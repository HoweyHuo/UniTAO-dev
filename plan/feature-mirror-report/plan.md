# Design Record：mirrored vDS 向本网 Inventory 上报记录变化

> 状态：**已实现**（分支 `feat/mirror-report`）。需求来自 UniTao-Modeling（`plan/feature-foreman/node-link-inventory.md`「已定（第五组）」75–79），维护者已确认。日常使用以 CLAUDE.md 第 8 条「记录变化订阅」为准，本文只记决策与理由。

## 1. 背景

记录变化上报（`InvReport.Reporter`）以 JournalProcess 形式挂在有 Journal 的 vDS 上（local + shared）。mirrored vDS 没有 Journal，所以 mirror 应用的变化**从不上报到 mirror 所在网络的 Inventory**。原文档说「其副本变更从 shared 源头侧上报」，但源头报给的是源头网络的 Inventory，mirror 那一侧的订阅者什么也收不到。Modeling 只靠推送事件发现记录变化，需要补上。

## 2. 决策

| # | 决策 | 选定 | 理由 |
|---|---|---|---|
| M1 | 增量谁来报（75） | `Share.Mirror` 持有一个 Reporter（`setupMirrored` 在 `srv.invLinked()` 时 `SetReporter`），`syncOnce` 每批 apply 完、`saveState` 之后，把这批的 `{type,id}` 去重、排除 `Common.InternalTypes`，一次 `ProcessChanges` 上报；dsId 是 mirrored vDS 自己的 `InvId` | 给 mirrored 加 Journal 太重（它只是副本）；Mirror 本来就是唯一写入者，在这里报最直接。先存进度再报：没报出去的就丢了，与本系统 best-effort 一致 |
| M2 | 整体替换怎么报（76） | `load(history)` 成功（`saveState` 之后）只报一个重置信号：`POST /report` body `{"dsId","changes":[],"reset":true}`；Inventory 推送体加 `"resets":["{ds}"]` | history 可能有大量记录，逐条报会冲垮分发队列（容量 1024，满则丢）；整体替换本来就意味着「这个 ds 什么都可能变了」，一个信号足够，订阅者自己决定怎么处理（对账） |
| M3 | resets 的过滤 | 按订阅的 `ds` 过滤，**不按 `types` 过滤** | 重置不知道涉及哪些类型，所有类型都可能变了 |
| M4 | 推送体格式 | `{"changes":[{ds,type,id}...],"resets":["dsId"...]}`；没有重置时省略 `resets`；只有重置时 `changes` 为 `[]`（不是 `null`） | 字段都是新增的，旧订阅者照常解析 `changes`、忽略 `resets`；`changes` 永远是数组，订阅者不用判空 |
| M5 | Ingest 校验 | `reset` 为 true 时允许 `changes` 为空 | 重置信号本身没有位置 |
| M6 | `?ver=1` 的含义（78） | 文档改正：ver 是记录的 schema 版本 `__ver`，内容修改时不变；对账只能发现新增/删除与格式升级，发现不了内容修改。发现修改靠推送，需要更强保证的订阅者自己回查内容（如比较内容指纹） | 原文档说「ver 对不上 = 更新、其余跳过无需回查」，与实现不符 |

## 3. 不做

- 不重试、不补发：上报失败（dsId 未分配、Inventory 不可达、非 2xx）只记日志，与 local/shared 的上报相同。订阅者另有一致性检查。
- 整体替换后不逐条发增量；同一轮 `syncOnce` 在 history 之后继续消费的 queue 批次照常按 M1 上报。
- schema 等 DS 内部类型不上报（`FilterChanges` 与 `Reporter.HandleType` 用同一张 `Common.InternalTypes`）。

## 4. 代码

- `src/DataService/InvReport/reporter.go`：`reportRequest.Reset`、`ProcessReset()`、`FilterChanges()`，发送抽成 `send()`。
- `src/DataService/Share/mirror.go`：`ChangeReporter` 接口与 `SetReporter`；`syncOnce` 每批上报增量，`load` 成功后上报重置。
- `src/DataService/DataServer/vds.go`：`setupMirrored` 挂 Reporter。
- `src/InventoryService/Subscription/manager.go`：`ReportRequest.Reset`、`Ingest` 放宽、`PushRequest.Resets`、`Dispatch` 按 ds 过滤重置。

## 5. 测试

- `test/src/DataServiceTest/InvReportTest`：`TestProcessResetPayload`（载荷 `{"dsId":"ds1","changes":[],"reset":true}`、增量不带 reset、失败不重试、无 dsId / 无 Inventory 不发）、`TestFilterChanges`。
- `test/src/InventoryServiceTest`：`TestIngestReset`（空 changes 的 reset 被受理、推送体 `{"changes":[],"resets":["dsM"]}`）、`TestDispatchResetFilters`（按 ds 过滤、忽略 types、普通推送省略 resets、重置与增量同批）。
- `test/src/VdsTest/mirror_report_test.go` `TestMirrorReportsResetAndChanges`：dsA(shared) 与 dsB(mirrored) 各连一个 Inventory，在 dsB 的 Inventory 上订阅；首次加载 history 只收到一个重置推送，之后 shared 写入、`syncAll` 后收到一批去重的 changes。
