# Feature Plan：跨隔离网络的分布式 Data Service

> 状态：草案 v2。标 **【待定】** 的地方是需要拍板的决定，后面都附了建议方案。

## 1. 背景

在现阶段 UniTAO 的设计中，Data Service 和 Inventory Service 默认都在同一个可以互相访问的网络环境里。

但在实际需求中，经常有互相隔离的网络环境之间需要共享信息的情况。已知的解决方法有两种：

1. **建立可互访的环境。** 对系统透明，但构建和维护都很复杂；而且因为暴露在外部环境中，有一定的安全隐患。这也违反了 UniTAO "整个系统只部署在内网、信任内网"的安全模型。
2. **通过加密渠道（如 git）共享数据。** 复用 git 的安全机制，更敏感的数据还可以二次加密。但数据通过 git 下载到对面后，数据服务需要重新搭建；而且共享数据的仓库比较集中，每次加载更新都很慢。

## 2. 方案概述

- 把 **Communication Channel（下称 Channel）做成插件**，让系统更灵活。第一阶段先做**共享 git 仓库**插件。
- 在 UniTAO 中增加两种 Data Service 角色，用来加载远端共享过来的 schema 和数据：
  - **共享 Data Service（Shared DS）**：正常读写数据，不同之处在于它会把库里的数据变化通过 Channel 发送出去。
  - **镜像 Data Service（Mirror DS）**：对外只读不写，所有变化都来自 Channel。
- **Inventory Service 依然是总的查询入口。** 镜像 DS 像普通 DS 一样自注册到本网的 Inventory。referral 本来就支持一个类型对应多个 DS（`RefRecord.DataServices` 是数组），所以查询路由不需要改。

## 3. 目标与非目标

**目标（第一阶段）**

- 网络 A 的共享 DS 把一组指定类型（共享集）的 schema 和数据，经 git 仓库单向同步到网络 B 的镜像 DS。
- 网络 B 的应用通过本网 Inventory 查询镜像数据，体验和查询本地 DS 一致。
- 不需要重新搭建数据服务：镜像 DS 启动后能自动从仓库完成首次全量同步，之后持续接收增量。
- 增量加载不随仓库历史变长而变慢。
- Channel 接口与 git 解耦，以后加别的通道不用改 DS 核心。

**非目标（第一阶段不做）**

- 双向同步和冲突合并：镜像只读，同一份数据只有一个写入方。
- 实时性：分钟级延迟可以接受。
- 一个镜像 DS 同时接收多个来源。
- 本网 DS 的 `indexTemplate` 订阅镜像类型（见 D2）。

## 4. 术语

| 术语 | 含义 |
|---|---|
| 共享 DS | 配置了 `share` 段的普通 DS，额外把共享集的变更写入 outbox，并经 Channel 发布 |
| 镜像 DS | 配置了 `mirror` 段的 DS，拒绝外部写入，只通过 Channel 应用变更 |
| 共享集（share set） | 共享 DS 配置中列出的、要对外共享的类型集合 |
| Outbox | 共享 DS 库里持久化的待发布变更，带单调递增的全局序号 `seq` |
| Cursor | 镜像 DS 已应用到的最大 `seq`，存在 `state.json` |
| Snapshot | 某个 `seq` 时刻共享集的全量数据，用于镜像首次加载和重建 |
| Segment | 一段连续 `seq` 的增量变更文件 |

## 5. 数据流

```
网络 A                                         │  网络 B
                                               │
 应用 ──写──▶ 共享 DS                           │
              │ Handler 写入成功                │
              ▼                                │
           AddJournal(before, after)           │
              │ JournalWorker                  │
              ▼                                │
           ChannelPublish process              │
              │ 写入 outbox（分配 seq）          │
              ▼                                │
           Channel 插件 Publish ──push──▶ git 仓库 ──pull──▶ Channel 插件 Subscribe(cursor)
                                               │                  │
                                               │                  ▼
                                               │            镜像 DS Apply（内部路径）
                                               │                  │ 更新 cursor
                                               │                  ▼
                                               │            自注册到本网 Inventory
                                               │                  ▲
                                               │   应用 ──查询──▶ Inventory
```

### 5.1 为什么需要 outbox，而不是直接用 Journal

Journal 是写入后的异步处理队列（见 CLAUDE.md 核心概念第 6 条）：
- entry 处理完、所在页翻过去后就被删除，不能回放；
- 顺序只在单条记录内保证，没有全局序号。

所以只把 Journal 当作**变更来源的挂载点**：新增一个 `ChannelPublish` 的 `JournalProcess`，在 `LoadProcesses` 里注册，把共享集类型的变更写入 outbox。

- **幂等**：Journal entry 处理失败会重试，所以 outbox 记录的 id 用 `(type, id, journalPage, journalIdx)`，重复写入不会产生新的 seq。
- **全局序号**：seq 由共享 DS 单点分配。第一阶段假设共享 DS 是单实例写入；多实例共享同一个库的情况放到【待定】问题里。
- **清理**：outbox 在已经被某个 snapshot 覆盖、且超过保留期后清理。

## 6. 关键决定

### D1 引用完整性 【待定】

**问题**：共享记录里的 `contentMediaType: "inventory/{type}"` 字段，可能引用了没有被共享的类型。到了镜像网络，这个引用就是悬空的；而且字段默认必填，按正常写入路径会被引用校验拒绝。

**建议**：
- **共享集必须在引用上闭合**：共享集中任一类型（含其所有历史 schema 版本）引用的类型，也必须在共享集内。
- 共享 DS 启动时，以及共享集内的 schema 升级时都要做这个检查；不闭合时启动失败，或者拒绝这次 schema 升级。
- 引用的目标类型可以在源网络的另一个 DS 上。共享 DS 只发布自己库里的数据，所以闭合检查要求被引用类型由**同一个共享 DS** 持有。第一阶段先这样限制，跨多个共享 DS 的共享放到后续阶段。
- 镜像 DS 应用变更时**跳过引用校验**（源侧已经校验过），以免因为到达顺序不同（先收到引用方、后收到被引用方）而失败。

### D2 `indexTemplate` / cmtIdx 等派生数据 【待定】

**问题**：`CmtIndexChanges` 会把反向索引回填进被引用记录的 `data`，并经 Inventory 跨 DS PATCH `cmtIdx`。如果镜像侧再跑一遍，会重复写入，或者去写本网其他 DS。

**建议**：
- 源侧回填的反向索引本来就是被引用记录 `data` 的一部分，被引用记录变化时会产生自己的 Journal，从而进入 outbox。所以**镜像原样存储记录即可，不需要重算**。
- 镜像 DS 经 Channel 写入时**不产生 Journal**（或者 Journal 不挂任何 process），不触发 `SchemaChanges` / `CmtIndexChanges`。
- 第一阶段**禁止本网 DS 的 schema 用 `indexTemplate` 订阅镜像类型**，因为回填需要写镜像记录，而镜像只读。本网 DS 注册这类 schema 时，`subscribeIndex` 会发现目标类型来自镜像 DS，直接报错。注意：普通的 `contentMediaType` 引用（不带 `indexTemplate`）是允许的。

### D3 镜像 DS 的"只读"边界

- 对外 HTTP 的 `POST / PUT / PATCH / DELETE` 一律返回 `405 mirror data service is read-only`，包括对 `schema` 和 `cmtIdx` 的写入（和 D2 一致）。在 `Handler` 入口统一拦截，和现在拦截 `InternalTypes` 的位置相同。
- Channel 应用走 `Handler` 的内部方法（如 `ApplyChange`）：做 schema 校验和 envelope 校验，跳过引用校验、跳过锁以外的副作用，并且不经过 HTTP。
- 镜像 DS 的读接口（`GET`、`?schema`、`?flat` 等）行为和普通 DS 一致。

### D4 Schema 同步与冲突 【待定】

- **同步完整历史**：镜像要收到共享集所有 schema 版本，包括 archived id。否则 `__ver` 为旧版本的记录在镜像上找不到对应 schema。schema 升级在源侧表现为"旧版本写成 archived id + 新版本覆盖"，两次写入都会进入 outbox，镜像按顺序应用即可。
- **同名类型冲突**：如果本网已有 DS 提供同名类型，referral 会把两边合并成同一类型查询。
  - 建议第一阶段**检测并拒绝**：镜像 DS 自注册后，Inventory sync 发现同名类型的 schema 内容不一致时，不把镜像加入该 referral，并记录告警。需要先确认 sync 现有逻辑在多 DS 同名类型下的行为。
  - 同名且 schema 完全一致（比如两边都是从同一份定义部署的）时，允许合并。
  - 命名空间 / 前缀方案放到后续阶段。

### D5 消息格式、顺序与幂等

每条变更（一行 JSON）：

```json
{
  "seq": 1024,
  "source": "ds-A01",
  "op": "upsert",
  "type": "Server",
  "id": "srv-001",
  "beforeVer": "0.0.1",
  "record": { "__type": "Server", "__id": "srv-001", "__ver": "0.0.1", "data": { } },
  "ts": "2026-11-06T10:00:00Z"
}
```

- `op`：`upsert` | `delete` | `unshare`（类型被移出共享集，见 D8）。
- 发**完整的 after 记录**，不发 patch。这样应用逻辑简单，丢弃中间状态也不影响最终结果。
- 镜像按 `seq` 严格递增应用，`seq <= cursor` 的直接跳过（幂等）。发现跳号时停止应用并告警，然后从最近的 snapshot 重建。

### D6 Snapshot 与首次加载

- 共享 DS 定期（或在 outbox 积压超过阈值时）生成 snapshot：先记下当前 `seq = S`，再扫描共享集的全部 schema（含 archived）和数据。
- 扫描期间写入还在继续，所以 snapshot 里的数据可能比 S 新。镜像加载 snapshot 后从 `S+1` 回放 segment，因为是 upsert 全量记录，最终结果一致。
- 镜像首次启动（cursor 为空）或检测到跳号时：清空本地共享数据 → 加载最新 snapshot → 回放之后的 segment。

### D7 身份

- 镜像 DS 自己的 `ds.id` / `instanceId` 照常由 config 和 `state.json` 管理，在本网 Inventory 里是一个普通 DS。
- `mirror.source` 配置写来源 DS 的 id（如 `ds-A01`），用来在仓库里定位目录，并校验消息里的 `source`。
- 第一阶段一个镜像 DS 只对应一个来源。

### D8 删除与撤销共享

- 记录删除 → `op: delete`。
- 类型移出共享集 → 共享 DS 启动时对比上次的共享集，对被移除的类型发一条 `op: unshare`；镜像收到后删除该类型的数据和 schema。
- schema 删除 → 作为 `schema` 类型记录的 `delete` 传过去。

### D9 安全

- **传输**：复用 git 的认证（SSH key / token），两侧网络只需要能访问 git 服务器，不需要互通。
- **加密（可选）**：segment 和 snapshot 文件落盘前做文件级加密（候选 `age`），密钥在两侧各自配置，git 服务器只看到密文。
- **签名（建议默认开启）**：manifest 和每个文件带共享 DS 的签名，镜像校验失败就拒绝应用，防止有人往仓库里注入数据。
- 镜像 DS 依旧部署在内网，不对外暴露。Channel 是唯一的跨网通道。

## 7. Channel 插件接口

```go
package Channel

type Change struct { /* 见 D5 */ }

type Snapshot struct {
    Seq     uint64
    Schemas []map[string]interface{}
    Records []map[string]interface{}
}

// 共享 DS 侧
type Publisher interface {
    // 发布一批连续 seq 的变更；返回后即视为持久化到通道
    Publish(changes []Change) error
    PublishSnapshot(s *Snapshot) error
    // 通道已确认的最大 seq，用于清理 outbox
    LastPublishedSeq() (uint64, error)
}

// 镜像 DS 侧
type Subscriber interface {
    // 返回 seq > cursor 的变更；无新数据时返回空
    Fetch(cursor uint64, limit int) ([]Change, error)
    // 最新 snapshot，用于首次加载或重建
    LatestSnapshot() (*Snapshot, error)
}

// 工厂：按配置 channel.type 创建，如 "git"
func NewPublisher(cfg Config) (Publisher, error)
func NewSubscriber(cfg Config) (Subscriber, error)
```

建议放在新模块 `src/Channel/`（接口 + 工厂），具体实现放 `src/Channel/GitChannel/`，跟 `src/Data/DbIface` + 各实现的组织方式一致。

## 8. Git 仓库布局

```
<repo>/
  <sourceDsId>/
    manifest.json                 # 最新 snapshot 的 seq、最新 segment 的范围、签名
    snapshots/
      000000000000001000/
        schemas.jsonl.gz
        records-<type>.jsonl.gz
    segments/
      000000000000001001-000000000000001100.jsonl.gz
      000000000000001101-000000000000001150.jsonl.gz
```

- **增量更新不变慢**：镜像只读 `manifest.json` 和 cursor 之后的 segment，不需要遍历历史。
- **控制仓库体积**：新 snapshot 生成后，早于它的 snapshot 和 segment 可以删除；git 历史可以定期用浅克隆或换分支的方式压缩。镜像侧用 `--depth 1` 拉取即可。
- **推送频率**：按时间（如 60 秒）或条数攒批，一批写成一个 segment，一次 commit + push。

## 9. 配置示例

共享 DS（在现有 DS config 上加 `share` 段）：

```json
{
  "share": {
    "types": ["Server", "Rack"],
    "snapshotIntervalSec": 86400,
    "publishIntervalSec": 60,
    "channel": {
      "type": "git",
      "repo": "git@git.example.com:unitao/share.git",
      "branch": "main",
      "workDir": "/opt/UniTAO/channel",
      "sign": { "keyFile": "/opt/UniTAO/keys/share.key" },
      "encrypt": { "recipientsFile": "/opt/UniTAO/keys/recipients.txt" }
    }
  }
}
```

镜像 DS：

```json
{
  "mirror": {
    "source": "ds-A01",
    "pullIntervalSec": 60,
    "channel": {
      "type": "git",
      "repo": "git@git.example.com:unitao/share.git",
      "branch": "main",
      "workDir": "/opt/UniTAO/channel",
      "verify": { "pubKeyFile": "/opt/UniTAO/keys/share.pub" },
      "decrypt": { "identityFile": "/opt/UniTAO/keys/mirror.key" }
    }
  }
}
```

`share` 和 `mirror` 互斥。镜像的 cursor 存在 `state.json`，和 `initialized`、`instanceId` 放在一起。

## 10. 第一阶段范围与里程碑

1. **Channel 接口 + 本地目录实现**（不经过 git，用于单测和集成测试）。
2. **共享 DS**：`share` 配置、共享集闭合检查（D1）、`ChannelPublish` process、outbox、snapshot。
3. **镜像 DS**：`mirror` 配置、只读拦截（D3）、`ApplyChange`、cursor、首次加载和重建（D6）。
4. **GitChannel**：仓库布局、攒批 push、浅拉取、manifest。
5. **Inventory**：同名类型冲突检测（D4），禁止对镜像类型做 `indexTemplate` 订阅（D2）。
6. **签名 / 加密**（D9）。
7. **文档**：CLAUDE.md、README，以及 demo 的 docker-compose。

## 11. 测试方案

- **单元测试**：消息序列化、seq 幂等和跳号检测、共享集闭合检查、snapshot + segment 合并的结果。
- **集成测试**：新增 `docker-compose/shared-mirror/`，起两套互不连通的网络（两个 compose network），各自有 DynamoDB Local + DS + Inventory，中间用一个本地 bare git 仓库（volume 挂载）充当通道。覆盖这些场景：
  - 首次全量加载；
  - 增 / 删 / 改的增量同步；
  - schema 升级后旧版本数据仍可查；
  - 引用字段跨类型展开查询；
  - 镜像拒绝外部写入；
  - 镜像重启后从 cursor 续传；
  - 人为删掉一个 segment 后自动从 snapshot 重建；
  - 签名被篡改时拒绝应用。

## 12. 待确认的问题

- 共享 DS 是否可能多实例共享同一个库？如果可能，seq 分配需要用数据库的原子计数器。
- Inventory sync 现在遇到多个 DS 提供同名类型、但 schema 不一致时是什么行为？（影响 D4 的实现位置）
- 是否需要支持只共享某类型的一部分记录（按条件过滤）？第一阶段建议不做。
- 镜像数据要不要在查询结果里标注来源，比如加一个 `__source` 响应头或字段？
- git 仓库的保留策略和体积上限由谁来维护？
