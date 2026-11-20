# Design Record：记录变化订阅系统（DS report + Inventory subscribe）

> 状态：**已实现**（PR #70，2026-11-20 合入 main；测试文档见 PR #71）。与需求方确认过的三个关键决策记录在第 3 节，实现红线在第 5 节。日常使用与运维细节以 CLAUDE.md 第 8 条「记录变化订阅」为准，本文只记决策与理由。

## 1. 背景

Inventory 此前没有任何"数据变了"的通知机制：只有 schema 级事件（`POST /referral`）和 cmtIdx 反向索引，上层系统想知道哪条记录变了只能轮询全量数据。本功能分两部分：

1. **Report**：Data Service 把每条记录的增/删/改以元数据上报给 Inventory。
2. **Subscribe**：客户端在 Inventory 上注册订阅，拉取变更（cursor + ack），Inventory 维护订阅者状态并清理已消费的变更。

## 2. 目标与非目标

**目标**：跨 DS 的统一变更流；订阅者断点续传；Inventory 端存储可控；DS 端零新增基础设施。

**非目标**（有意不做）：

- 不做实时推送（无 webhook）；不做变更内容分发（只报元数据）；不做跨记录全局因果序；不保证恰好一次（至少一次 + 幂等）。

## 3. 关键决策（需求方确认）

| # | 决策 | 选定 | 否决的备选 | 否决理由 |
|---|---|---|---|---|
| D1 | 投递方式 | **拉取**：订阅者 GET + cursor + ack，实时性靠轮询间隔 | webhook 推送：订阅者注册回调 URL，Inventory 主动 POST | 推送需要重试、退避、死信、订阅者可达性判定一整套机制，复杂度显著更高；内网低频场景轮询足够。若未来确需实时，另立提案 |
| D2 | 变更负载 | **仅元数据**：`{ds, vds, type, id, op, ver, ts}`，订阅者按需经 Inventory 查询 API 回查记录内容 | 完整记录（upsert 带 after 全量，delete 只带 id）；订阅时可选 includeData | 完整记录的上报流量与 Inventory 存储大；"可选"方案存储仍是全量，只是拉取侧裁剪，两头复杂。delete 后回查不到记录正好由 op 表达 |
| D3 | 订阅者状态 | **有状态**：Inventory 存订阅记录（过滤条件 + cursor + updated），支持断点续传与按最小 cursor 清理 | 无状态拉取：`GET ?after=seq` 自存 cursor，Inventory 只按时间清理 | 无状态方案 Inventory 不知道消费进度，落后太多的订阅者会静默丢数据 |

## 4. 架构总览

```
DS (每个有 Journal 的 vDS: local + shared)                     Inventory
  写入 → Journal entry → InvReport 进程 ──POST /report────→  Manager.Ingest
  (失败 entry 留 head, ~5s 重试; 成功才归档)                    分配全局 seq（先写行再推进计数器）
                                                              写 change 行（Inventory 自己的库，202 = 已持久）
客户端
  POST /subscription ──→ 订阅记录 {id, types, ds, cursor}
  GET  /subscription/{id}/changes?after=&limit= ──→ 变更 + next（扫描水位）
  POST /subscription/{id}/ack {cursor: next} ──→ 推进 cursor + 活跃时间戳
  后台 Prune：删 seq <= min(活跃订阅者 cursor) 或超过保留期的行；判死清理订阅者
```

- 代码：`src/DataService/InvReport/`（DS 侧上报进程）、`src/InventoryService/Subscription/`（change log、订阅登记、pull/ack、清理）、路由在 `InventoryServer`。
- 投递可靠性**完全复用 journal 重试语义**：InvReport 以 `JournalProcess` 形式经 `setupJournal` 的 extra 参数挂载（同 `Share.ChannelPublish` 机制），至少一次、单记录内有序、DS 重启不丢，不另造队列。
- seq 分配复刻 Share outbox 的「先写行再推进计数器」，seq 永无空洞；行 id 为 20 位补零。
- 清理参数默认值：保留 7 天（`retentionHours: 168`）、订阅者 24 小时无活动判死（`subscriberTimeoutHours`）、清理循环 60 秒一轮（`pruneIntervalSec`）。

## 5. 实现红线（改动前必须知道）

1. **`POST /report` 必须同步落库**（202 = 已持久）。不采用 `handleTypeEvent` 的异步 channel 模式——DS 的 journal 重试语义依赖 2xx 表示可靠存储，异步化会让 DS 在 Inventory 崩溃时误归档条目造成丢数据。
2. **所有读侧（Pull / Prune / floor 计算）必须过滤 `seq <= head`**：计数器是提交点，崩溃半截的 batch 会留下未提交行，重试会用相同 seq 覆盖；提前暴露它们等于发出从未提交的变更。
3. **游标落后到已清理区间返回显式 410**，绝不静默跳过，也不要改成"自动从 floor 重放"——慢订阅者必须知道丢了数据。
4. **`change` / `subscription` 不进 Inventory 的 `InvTypes`**：进表会污染 `GET /schema` 的类型列表和通用 `GET /{type}` 访问。运维可见性走订阅端点自身。
5. **订阅路由在通用 method 分发之前拦截**（`handler()` 开头的 `handleSubscriptionRoute`）：`GET /{type}` 是 catch-all、POST 只认 `/referral`，不前置拦截新路由会被吞掉。
6. **schema 等 DS 内部类型不上报**（`Common.InternalTypes` 全排除），否则 journal/outbox 的行会经 `/report` 反馈回 Inventory 形成回环。

## 6. 语义小结

- **至少一次**：重复变更幂等（每条都是完整 upsert/delete 元数据，订阅者回查内容按最后写入生效）。
- **顺序**：全局按 seq 有序；单记录内有序（per `(type, id)` journal worker）；跨记录无序（与 journal 现状一致）。
- **mirrored vDS 不产生变更**（无 Journal）；mirror 副本的变更从 shared 源头侧上报。
- 共享 dsId 的多副本各自上报：元数据层面重复无害。

## 7. 测试与文档

- 测试（全部无外部依赖）：`test/src/DataServiceTest/InvReportTest/`（reporter 单测 + journal 集成）、`test/src/InventoryServiceTest/`（协议单测：seq 连续性、扫描水位、410 gap、ack 语义、判死清理）、`test/src/VdsTest/subscription_test.go`（端到端：上报→订阅→拉取→ack→宕机补收→判死）。命令见 CLAUDE.md 测试章节。
- 文档：CLAUDE.md 第 8 条「记录变化订阅」+ REST API 表 7 行 + 测试章节；README.md「Record Change Subscription」小节（curl 示例）。
