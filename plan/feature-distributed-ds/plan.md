# Feature Plan：虚拟 Data Service 与跨隔离网络共享

> 状态：草案 v3（以虚拟 DS 模型重写）。所有决定均已确定。

## 1. 背景

在现阶段 UniTAO 的设计中，Data Service 和 Inventory Service 默认都在同一个可以互相访问的网络环境里。

但在实际需求中，经常有互相隔离的网络环境之间需要共享信息的情况。已知的解决方法有两种：

1. **建立可互访的环境。** 对系统透明，但构建和维护都很复杂；而且因为暴露在外部环境中，有一定的安全隐患。这也违反了 UniTAO "整个系统只部署在内网、信任内网"的安全模型。
2. **通过加密渠道（如 git）共享数据。** 复用 git 的安全机制，更敏感的数据还可以二次加密。但数据通过 git 下载到对面后，数据服务需要重新搭建；而且共享数据的仓库比较集中，每次加载更新都很慢。

## 2. 方案概述

- 把 **Communication Channel（下称 Channel）做成插件**，让系统更灵活。第一阶段先做**共享 git 仓库**插件。
- 引入**虚拟 Data Service（vDS）**。DS 实例只是一个进程，真正的数据单元是 vDS。一个实例可以同时承载多个 vDS：

| vDS | 路径 | 读写 | 说明 |
|---|---|---|---|
| local | `local` | 可写 | 本地数据，不共享 |
| shared | `shared/{name}` | 可写 | 本地要共享出去的数据，变更经 Channel 发布。一个实例可以有多个，比如面向不同的接收方 |
| mirrored | `mirrored/{name}` | 对外只读 | 某个远端 shared vDS 的镜像，所有变化都来自 Channel |

- 部署很灵活：一个实例可以只放 `local`，可以是 `local` 加若干个 `shared/*`，也可以只放若干个 `mirrored/*`。
- **每个 vDS 在 Inventory 里都是一个独立的 DS。** Inventory 依然是总的查询入口。一个类型分布在多个 DS 上时，Inventory 本来就会按 `{ds}/{id}` 区分各 DS 的记录，所以可以直接复用这套机制。

## 3. 原则

1. **来源是 record 身份的一部分。** 一条 record 由 `(vDS, __type, __id)` 唯一确定。不同 vDS 的记录即使 `__type`、`__id` 相同，也是两条不同的 record，永远不会互相覆盖。
2. **shared vDS 有自己的签名身份**，mirrored vDS 同步这个签名。签名就是一个 vDS 跨网络、跨实例的全局身份（见 D3）。
3. **一个 shared vDS 只对应一个 Channel**；一个 mirrored vDS 只对应一个来源。一个实例可以承载多个 shared vDS 和多个 mirrored vDS。
4. **同名同版本的 schema，内容必须相同**（见 D6）。

## 4. 目标与非目标

**目标（第一阶段）**

- DS 实例支持 vDS 模型和新的 URL 方案。
- 网络 A 的 shared vDS 经 git 仓库单向同步到网络 B 的 mirrored vDS，同步内容包括 schema 和数据。
- 网络 B 的应用通过本网 Inventory 查询镜像数据，体验和查询本地数据一致。
- mirrored vDS 启动后能自动从仓库完成首次全量同步，之后持续接收增量；增量加载不随仓库历史变长而变慢。
- 同一个来源被多个实例镜像时，Inventory 能识别出它们是同一个 vDS，不会重复计入。
- Channel 接口与 git 解耦。

**非目标（第一阶段不做）**

- 双向同步和冲突合并：mirrored 只读，每份数据只有一个写入方。
- 实时性：分钟级延迟可以接受。
- 兼容旧 URL：新的 URL 方案不保留旧路径（见 D1）。
- 本网数据引用镜像数据（见 D8）。

## 5. 术语

| 术语 | 含义 |
|---|---|
| DS 实例 | 一个 Data Service 进程，有自己的 `ds.id` / `instanceId`，承载一个或多个 vDS |
| vDS | 虚拟 Data Service，有独立的数据表、schema、Journal、读写规则，在 Inventory 中注册为一个 DS |
| 共享名（name） | shared vDS 的名字，例如 `A01`，路径为 `shared/A01`。镜像侧沿用同一个名字：`mirrored/A01` |
| 签名身份 | shared vDS 的签名密钥对。公钥指纹就是这个 vDS 的全局身份 |
| 共享集 | shared vDS 中所有的类型。shared vDS 里的数据全部都要共享，不再单独配置类型列表 |
| Outbox | shared vDS 库里持久化的待发布变更，带单调递增的全局序号 `seq` |
| Cursor | mirrored vDS 已应用到的最大 `seq` |
| Snapshot / Segment | 某个 `seq` 时刻的全量数据 / 一段连续 `seq` 的增量文件 |

## 6. DS 的 REST API（D1：不兼容旧 URL）

所有路径都以 vDS 开头：`/{vDS}/{__type}/{__id}`。

| 方法 | 端点 | 说明 |
|---|---|---|
| `GET` | `/` | 列出本实例承载的所有 vDS：`["local", "shared/A01", "mirrored/C02", ...]` |
| `GET` | `/shared` | 列出所有 shared vDS 的名字 |
| `GET` | `/mirrored` | 列出所有 mirrored vDS 的名字 |
| `GET` | `/{vDS}` | 列出该 vDS 下的条目：`["config", "schema", <各数据类型>...]` |
| `GET` | `/{vDS}/config` | 该 vDS 的设置和运行信息（见下） |
| `GET` | `/{vDS}/schema[/{type}]` | 列出 / 查询该 vDS 的 schema |
| `POST` | `/{vDS}` | 创建 record（类型取自 body 的 `__type`），也用来提交 schema 新版本 |
| `GET` | `/{vDS}/{type}` | 列出该类型的所有 `__id` |
| `GET` | `/{vDS}/{type}/{id}[/{attrPath}]` | 查询 record，可带属性路径；`?schema`、`?flat`、`?iterator`、`[*]` 保持不变 |
| `PUT` | `/{vDS}/{type}/{id}` | 替换 record |
| `PATCH` | `/{vDS}/{type}/{id}[/{attrPath}]` | 部分更新 |
| `DELETE` | `/{vDS}/{type}/{id}` | 删除 record |

- 对 `mirrored/*` 发出的写请求（`POST/PUT/PATCH/DELETE`）一律返回 `405 vDS [mirrored/A01] is read-only`，包括写 `schema` 和 `cmtIdx`。
- `config` 和 `schema` 是保留名，元 schema 校验时不允许注册成类型。
- `/{vDS}/config` 的内容：
  - **通用**：vDS 路径、角色、是否只读、在 Inventory 中的 id、所属 DS 实例的 `ds.id` / `instanceId`；
  - **shared**：共享名、公钥指纹、Channel 类型和地址、outbox 最新 `seq`、最后发布的 `seq` 和时间、最新 snapshot 的 `seq`；
  - **mirrored**：来源共享名、配置里固定的可信指纹、实际同步到的公钥指纹、Channel 类型和地址、cursor、最后同步时间、状态（正常 / 暂停及原因）。
  - **不输出**私钥、Channel 凭据、加解密密钥的内容，只给出是否已配置。

**影响面**：现有的 DS 路由、`DataServiceAdmin`、`InventoryServiceAdmin`、测试（`test/src/DataServiceTest`、`test/data`）、demo 脚本、docker-compose 配置、`schemaVisualizer` 和 `ui` 里写死的 DS URL，以及 README / CLAUDE.md 的 API 表，都要一起迁移。

## 7. 数据流

```
网络 A：实例 dsA                                   │  网络 B：实例 dsB
                                                   │
 应用 ──写──▶ /shared/A01/...                       │
              │ Handler 写入成功                    │
              ▼                                    │
           AddJournal(before, after)               │
              │ JournalWorker                      │
              ▼                                    │
           ChannelPublish process（仅 shared）       │
              │ 写入 outbox（分配 seq）              │
              ▼                                    │
           Channel Publish（签名）──push──▶ git 仓库 ──pull──▶ Channel Subscribe(cursor)（验签）
                                                   │                  │
                                                   │                  ▼
                                                   │    /mirrored/A01 ApplyChange（内部路径）
                                                   │                  │ 更新 cursor
                                                   │                  ▼
                                                   │     注册到本网 Inventory（带签名身份）
                                                   │                  ▲
                                                   │   应用 ──查询──▶ Inventory
```

**为什么需要 outbox，而不是直接用 Journal**：Journal 处理完就删除，也没有全局序号（见 CLAUDE.md 核心概念第 6 条），所以只把它当作变更来源的挂载点。新增一个 `ChannelPublish` process，只处理 shared vDS 的 entry，把变更写入 outbox：

- **幂等**：Journal entry 处理失败会重试，所以 outbox 记录的 id 用 `(type, id, journalPage, journalIdx)`，重复写入不会产生新的 seq。
- **全局序号**：seq 由 shared vDS 单点分配。

## 8. 关键决定

### D1 URL 方案（已定）

不兼容旧 URL，统一改成 `/{vDS}/{__type}/{__id}`，详见第 6 节。

### D2 vDS 的隔离

每个 vDS 都有自己独立的：
- **数据表**：同一个数据库连接，每个 vDS 一张表。实现：`local` 沿用 `table.data`，`shared/{name}` 为 `{table.data}_shared_{name}`，`mirrored/{name}` 为 `{table.data}_mirrored_{name}`（见第 15 节）。
- **schema**：存在本 vDS 的数据表里，`/{vDS}/schema` 只显示本 vDS 的。
- **Journal**：键里带上 vDS。process 只在 `local` / `shared/*` 上运行（见 D7）。
- **逐记录锁**：锁的键带上 vDS。
- **初始化状态**：`state.json` 里的 `initialized` 改成按 vDS 记录。在 config 里新增一个 mirrored 时，只初始化这个 vDS，不影响其他 vDS。

**local 和 shared 不共用 schema（已定）。**
- 即使两边的 schema 完全相同，也各自保留一份。这和 URL 模型一致，shared 的 schema 集合就等于要发布出去的 schema 集合，闭合检查（D5）也简单。
- 两份 schema 各自独立升级、归档，互不影响。同名同版本时内容必须相同（原则 4），由 Inventory sync 检查（D6）。
- 同一个类型要在两个 vDS 上各注册一次。可以由 `DataServiceAdmin` 提供一个在 vDS 之间复制 schema 的命令。

### D3 签名身份（已定方向）

- **shared vDS**：在 config 中配置共享名和签名私钥，公钥指纹就是它的全局身份。Channel 上的 manifest、snapshot、segment 都用这把私钥签名。
- **mirrored vDS**：
  - config 中**固定可信指纹**，作为信任锚点；
  - 实际的公钥从 Channel 的 manifest 同步过来；
  - 指纹和可信指纹不一致时，拒绝应用，并把状态设为暂停。
  - 签名不能只从通道里读来就信，否则任何能写 git 仓库的人都能换掉公钥、注入数据。
- **local vDS**：没有签名身份，沿用实例现有的 `ds.id` / `instanceId`。

### D4 Inventory 注册与去重

每个 vDS 通过 `PUT /` 在 Inventory 注册一条 `inventory` 记录。现有记录有 `dsId`、`instanceId`、`url`（数组）三个字段，可以直接复用：

| vDS | `dsId` | `instanceId` | `url` |
|---|---|---|---|
| local | `{ds.id}`，例如 `dsB` | 实例的 `instanceId` | `http://dsB:8002/local` |
| shared | 共享名，例如 `A01` | 公钥指纹 | `http://dsA:8002/shared/A01` |
| mirrored | 来源共享名，例如 `A01` | 同步到的公钥指纹 | `http://dsB:8002/mirrored/A01` |

- **ds id 里不能带 `/`**，因为 Inventory 的多 DS 路径是 `/{type}/{ds}/{id}`。vDS 的 URL 路径可以带 `/`，但注册时用的 `dsId` 必须是扁平的。
- **同一来源被多个实例镜像**（`dsId` 相同，指纹也相同）：Inventory 认为这是**同一个 vDS 的多个副本**，把新实例的 URL 合并进这条记录的 `url` 数组，而不是新增一个 DS。这样 sync 时不会把同一份镜像计入两次，查询结果也不会重复。现在相同 `instanceId` 的注册会被当成"重注册"并替换 URL，对签名身份要改成"合并 URL"。
- **同名但指纹不同**：两个不同的来源撞了名。处理方式是**自动改名 + 告警**，不需要人工干预就能继续工作：
  - 先注册的保留原名；后注册的由 Inventory 分配 `{name}-{指纹前 8 位}`，例如 `prod-3f2a9c1b`，通过注册响应返回给实例。
  - 实例把分配到的名字存进 `state.json`，之后 vDS 的路径和 `dsId` 都用这个名字，例如 `mirrored/prod-3f2a9c1b`。这和现在 ds.id 撞名时 `dsId` / `dsIdFrom` 的机制一致：config 里的名字改了，分配的名字就作废。
  - 在 `/{vDS}/config` 和 Inventory 日志里记录告警，说明这个来源因撞名被改了名，以及和谁冲突（对方的指纹）。
  - **为什么用指纹派生，而不用递增后缀**：派生出来的名字只取决于指纹，和注册顺序、次数无关；不同实例镜像同一个来源时会得到同一个名字，仍然能合并成副本；Inventory 记录删除后重建，名字也不会变。
  - 谁保留原名取决于第一次注册的先后。撞名一旦发生，结果由 Inventory 持久化的记录固定，重启不会互换。
- **同指纹但不同名**：同一个来源在本网用了两个名字，比如两个实例给它配置了不同的 `alias`。Inventory 以指纹为准，把后来的实例合并进已有的那条记录，作为副本，通过注册响应把已有的名字返回给实例（同样存进 `state.json`），并告警提示配置不一致。这样同一个来源不会被计入两次。
- **可选的人工手段**（不处理也能正常工作）：
  - **本地别名**：mirrored 配置 `alias`，给来源在本网起一个好记的名字（见第 11 节）。`name` 仍然是来源的共享名，用来定位仓库目录、校验消息里的 `source`。
  - **固定指纹**：在 Inventory 的 config 写 `"pinnedSources": {"prod": "SHA256:KA..."}`。被固定的名字只给这个指纹使用；如果名字已经被别的指纹占着，就把占用者改成指纹派生的名字。
  - **移除错误接入的来源**：用 `DELETE /inventory/{dsId}` 删掉它的记录，或者从镜像实例的 config 里去掉它。这是唯一必须人工处理的情况，属于配置错误，不是正常运行时会碰到的。
- **shared 一侧撞名**：同一个网络里两个 shared vDS 起了同一个共享名，也按上面的规则自动改名，但改的只是它在本网 Inventory 里的 `dsId`。共享名经 Channel 传给所有镜像方，是这个来源对外的名字，不会因此改变。如果要彻底解决，应该改 shared 的 config。
- **URL 的移除**：实例从 config 里删掉某个 mirrored，或者永久下线时，从 `url` 数组里删掉它的 URL；数组为空时删除整条记录。实例只是暂时连不上时不用删，`GetUrl` 会自动跳过它。
- **local vDS 不参与合并**：local 没有签名身份，保持现有逻辑，即 `instanceId` 相同时替换 URL、撞名时分配后缀。
- **副本之间的一致性：优先选 cursor 最新的副本。** 各副本各自从 Channel 拉取，cursor 可能不同。
  - Inventory 在 sync 时（以及周期性地）读取每个副本 URL 的 `/{vDS}/config`，取出 cursor，按 cursor 从大到小给 `url` 数组排序并缓存；不是每次查询都去读。
  - `GetUrl` 按这个顺序选第一个可达的 URL，所以正常情况下查询总落在最新的副本上，最新的副本不可达时才退到次新的。
  - cursor 只在 sync 时刷新，两次刷新之间副本的先后可能已经变了，所以这是"尽量最新"，不保证严格最新。
  - local vDS 只有一个 URL，shared vDS 没有 cursor（它就是源头），都不需要排序。
- 在同一个 Inventory 里，shared vDS 和它自己的镜像如果同时出现，指纹相同，也会合并成同一条记录。写入本来就直接发给 DS、不经过 Inventory，所以不受影响。

### D5 引用完整性

**问题**：shared 中的 `contentMediaType: "inventory/{type}"` 字段，如果引用了不共享的数据，到了镜像网络就会悬空。

**规则**：
- **shared 不能引用 local，也不能引用别的 shared**。在 shared vDS 写入时做引用校验：被引用的记录必须也在**同一个 shared vDS** 里。每个 shared vDS 会被不同的接收方镜像，引用一旦跨出本 vDS，到了镜像侧就会悬空。这个检查要放在写入路径上，只靠配置检查是不够的。
- shared 的 schema 注册或升级时，检查它引用的类型是否在本 shared vDS 中有 schema，没有就拒绝。
- **local 可以引用本实例的任一 shared**（都是本地数据）。
- mirrored 应用变更时**跳过引用校验**（源侧已经校验过），以免因为到达顺序不同而失败。

### D6 Schema 同步与同名冲突

- **同步完整历史**：mirrored 要收到 shared 的所有 schema 版本，包括 archived id。否则 `__ver` 为旧版本的记录在镜像上找不到对应的 schema。`ApplyChange` 要支持直接写入 archived 版本。
- **同名同版本必须同内容**：
  - Inventory sync 时，对同一个类型在各 DS 上的同版本 schema，把 `data` 规范化（key 排序）后计算 hash 并比较；
  - 不一致就不把该 vDS 加入这个类型的 referral，并告警。
  - 同名同内容（比如两边都按同一份标准定义部署）则正常合并，数据按 `{ds}/{id}` 区分。
- 后续阶段可以考虑按 mirrored 配置类型别名（如 `"typeAlias": {"Server": "A01_Server"}`），应用变更时改写 schema id、`__type` 和引用的 `contentMediaType`。

### D7 Journal 与派生数据（indexTemplate / cmtIdx）

- `local` / `shared/*`：照常运行 `SchemaChanges`、`CmtIndexChanges`；`shared/*` 额外运行 `ChannelPublish`，每个 shared vDS 有自己的 outbox。
- `mirrored`：经 Channel 写入时不挂任何 process。源侧回填的反向索引本来就是被引用记录 `data` 的一部分，会作为那条记录的变更一起同步过来，镜像原样存储即可，不需要重算。
- 禁止 `local` / `shared/*` 的 schema 用 `indexTemplate` 订阅只存在于 mirrored 的类型，因为回填需要写镜像记录，而镜像只读。

### D8 引用解析按 vDS 优先

**问题**：现在 Inventory 用裸 id 查询时（`GetRecord` → `GetDataServiceData`），会按 referral 里 DS 的顺序逐个查，第一个命中的就返回。来源 B 的记录引用 `srv-001` 时，可能被解析成来源 A 的 `srv-001`，违反原则 1。

**规则**：
- 解析引用时，**先在发起引用的那条记录所在的 vDS 里查**，找不到再回退到全局查找。Inventory 展开引用时，要把当前记录所在的 vDS 一路传下去。
- DS 侧经 Inventory 校验引用时，带上自己的 vDS 的 `dsId`。
- D5 要求引用闭合，所以镜像数据的引用总能在同一个 vDS 内解析到。
- **本网数据引用镜像数据**（例如 local 的 `Rack` 引用 `mirrored/A01` 的 `Server`）：裸 id 无法说明是哪个来源。第一阶段不支持；后续可以考虑允许引用值写成 `{ds}/{id}` 的限定形式。

### D9 Inventory 列表格式与按 id 查询（已定）

现在的行为：一个类型只在一个 DS 上时，Inventory 返回裸 id；分布在多个 DS 上时，返回 `{ds}/{id}`，按 id 查询时也必须带 ds 前缀。vDS 模型下多 DS 会很常见（同一个类型同时在 `local` 和某个 `shared/*`，或者有了镜像），客户端看到的格式会随部署情况变化。

**决定：不管一个类型分布在几个 DS 上，统一使用带 ds 的形式。** D1 已经不兼容旧 URL，这次一起改掉。

- `GET /{type}`：始终返回 `["{ds}/{id}", ...]`，不去重。
- `GET /{type}/{ds}/{id}[/{attrPath}]`：始终要求带 ds；裸的 `GET /{type}/{id}` 返回 400。
- 引用字段里存的仍然是裸 `__id`（数据格式不变）。展开引用时，按 D8 的规则先在发起引用的那条记录所在的 vDS 里解析。
- Inventory 的查询结果里，记录的 `__id` 保持原样，不改写成 `{ds}/{id}`。ds 只出现在路径和列表里，记录内容和 DS 上的一致。
- 影响面：`DataHandler.List` / `Get` 中按 `multiDs` 分支的逻辑（`src/InventoryService/DataHandler/dataHandler.go`）合并成一条路径；Inventory 的调用方（DS 侧 `Inventory.Get`、cmtIdx 回填、`schemaVisualizer`、`ui`、测试和 demo）都改为带 ds 查询。

### D10 消息格式、顺序与幂等

每条变更（一行 JSON）：

```json
{
  "seq": 1024,
  "source": "A01",
  "op": "upsert",
  "type": "Server",
  "id": "srv-001",
  "record": { "__type": "Server", "__id": "srv-001", "__ver": "0.0.1", "data": { } },
  "ts": "2026-11-06T10:00:00Z"
}
```

- `op`：`upsert` | `delete`。
- 发**完整的 after 记录**，不发 patch。
- mirrored 按 `seq` 严格递增应用，`seq <= cursor` 的直接跳过（幂等）。发现跳号时停止应用、状态设为暂停并告警，然后从最近的 snapshot 重建。
- `source` 必须等于 mirrored 配置的来源共享名。

### D11 Snapshot、首次加载与删除

- shared 定期（或在 outbox 积压超过阈值时）生成 snapshot：先记下当前 `seq = S`，再扫描全部 schema（含 archived）和数据。镜像加载 snapshot 后从 `S+1` 开始回放，因为每条都是完整记录的 upsert，最终结果一致。
- mirrored 首次启动（没有 cursor）或检测到跳号时：清空本 vDS 的表 → 加载最新 snapshot → 回放之后的 segment。因为 vDS 的数据表是隔离的（D2），清空不会影响其他 vDS。
- 删除记录或 schema → `op: delete`，走正常流程。
- 不再共享：shared 里的数据全部都要共享，"取消共享"就是把记录从 shared 删除（或移到 local，即删除后在 local 重建），镜像会收到相应的 `delete`。不需要单独的 `unshare` 消息，也不需要比较共享集。
- **从 config 里删掉某个 mirrored：删除数据。** 实例启动时比较 config 和 `state.json` 里记录的 vDS 列表，对已经不在 config 中的 mirrored：停止同步、从 Inventory 注销它的 URL（D4）、删除它的数据表，再清掉它在 `state.json` 里的状态。
  - 如果只是改错了配置（比如改了 `alias`），数据可以从 Channel 的 snapshot 重新同步回来，所以删除的风险可控。
  - 从 config 里删掉某个 shared：同样删除数据表和 outbox，并从 Inventory 注销。Channel 上已发布的内容不会自动删除，由管理员清理仓库，镜像方也需要各自从 config 移除。

### D12 传输安全

- **传输**：复用 git 的认证（SSH key / token）。两侧网络只需要能访问 git 服务器，不需要互通。
- **签名**：必须开启，见 D3。
- **加密（可选）**：segment 和 snapshot 文件落盘前做文件级加密（候选 `age`），git 服务器只看到密文。
- 所有实例仍然部署在内网，不对外暴露。Channel 是唯一的跨网通道。

## 9. Channel 插件接口

```go
package Channel

type Change struct { /* 见 D10 */ }

type Snapshot struct {
    Seq     uint64
    Schemas []map[string]interface{}
    Records []map[string]interface{}
}

// shared vDS 侧
type Publisher interface {
    // 发布一批连续 seq 的变更；返回后即视为已持久化到通道
    Publish(changes []Change) error
    PublishSnapshot(s *Snapshot) error
    // 通道已确认的最大 seq，用于清理 outbox
    LastPublishedSeq() (uint64, error)
}

// mirrored vDS 侧
type Subscriber interface {
    // 来源公钥（从 manifest 同步），由调用方与可信指纹比对
    SourceKey() (PublicKey, error)
    // 返回 seq > cursor 的变更（已验签）；无新数据时返回空
    Fetch(cursor uint64, limit int) ([]Change, error)
    // 最新 snapshot（已验签），用于首次加载或重建
    LatestSnapshot() (*Snapshot, error)
}

// 工厂：按配置 channel.type 创建，如 "git"、"dir"
func NewPublisher(cfg Config, signer Signer) (Publisher, error)
func NewSubscriber(cfg Config) (Subscriber, error)
```

放在新模块 `src/Channel/`（接口 + 工厂 + 签名），具体实现放在 `src/Channel/GitChannel/` 和 `src/Channel/DirChannel/`（本地目录，用于测试）。组织方式和 `src/Data/DbIface` 加各实现一致。

> 实现与此不同：接口与实现都在 `src/DataService/Channel` 一个包里，git 通道复用目录通道的文件格式，只在读写前后挂 git 操作；`Subscriber` 用 `Refresh` + `SourceFingerprint` 代替 `SourceKey`。见第 15 节。

## 10. Git 仓库布局

```
<repo>/
  <共享名>/
    manifest.json                 # 公钥、最新 snapshot 的 seq、最新 segment 的范围；带签名
    snapshots/
      000000000000001000/
        schemas.jsonl.gz
        records-<type>.jsonl.gz
        SIGNATURE
    segments/
      000000000000001001-000000000000001100.jsonl.gz
      000000000000001001-000000000000001100.sig
```

- **增量更新不变慢**：镜像只读 `manifest.json` 和 cursor 之后的 segment，不需要遍历历史。
- **控制仓库体积**：新 snapshot 生成后，早于它的 snapshot 和 segment 可以删除；镜像侧用 `--depth 1` 浅拉取。
- **推送频率**：按时间（如 60 秒）或条数攒批，一批写成一个 segment，一次 commit + push。

> 实现的布局更简单：`snapshots/{seq}.jsonl.gz[.enc]` 单个文件（schema 排在数据前面）；不给每个文件单独签名，而是 `manifest.json` 记录每个文件的 sha256，`manifest.sig` 对整个 manifest 签名。见第 15 节。

## 11. 配置示例

`config.json` 新增 `vds` 段，出现了哪个 key 就启用哪个 vDS。`database`、`http`、`ds`、`inventory` 等段保持不变。

```json
{
  "vds": {
    "local": {},
    "shared": [
      {
        "name": "A01",
        "signingKeyFile": "/opt/UniTAO/keys/A01.key",
        "publishIntervalSec": 60,
        "snapshotIntervalSec": 86400,
        "channel": {
          "type": "git",
          "repo": "git@git.example.com:unitao/share.git",
          "branch": "main",
          "workDir": "/opt/UniTAO/channel/A01",
          "encrypt": { "recipientsFile": "/opt/UniTAO/keys/recipients.txt" }
        }
      }
    ],
    "mirrored": [
      {
        "name": "C02",
        "alias": "partnerC",
        "trustedFingerprint": "SHA256:Qm9...",
        "pullIntervalSec": 60,
        "channel": {
          "type": "git",
          "repo": "git@git.example.com:unitao/share.git",
          "branch": "main",
          "workDir": "/opt/UniTAO/channel/C02",
          "decrypt": { "identityFile": "/opt/UniTAO/keys/mirror.key" }
        }
      }
    ]
  }
}
```

约定：
- **vDS 配置只写在 `config.json` 中**，不存数据库，也不提供修改 API。改配置后需要重启。
- `shared` 和 `mirrored` 都是数组。每个 shared 有自己的共享名、签名密钥和 Channel；多个 shared 可以用同一个 git 仓库，仓库里按共享名分目录（第 10 节）。同一个实例里，各 shared 的 `name` 不能重复。
- mirrored 的 `name` 是来源的共享名，必须和来源一致；`alias` 可选，是在本网使用的名字（路径和 Inventory `dsId`），不写就等于 `name`。同一个实例里，各项生效的本地名字不能重复。
- 按"DS 不回写 `config.json`"的约定，DS 自己产生的状态写在 `state.json`，按 vDS 分开存放：本实例的 vDS 列表（用于发现被删除的 vDS，见 D11）、各 vDS 的 `initialized`、mirrored 的 cursor 和同步到的公钥指纹，以及 Inventory 分配的名字（D4）。

## 12. 第一阶段里程碑

1. **vDS 基础**：DS 按 vDS 路由（D1），实现 `/`、`/{vDS}`、`/{vDS}/config`；数据表、schema、Journal、锁按 vDS 隔离（D2）；每个 vDS 自注册到 Inventory。这一步只启用 `local`，让现有功能在新 URL 下跑通。
2. **迁移**：`DataServiceAdmin`、`InventoryServiceAdmin`、测试、demo、docker-compose、`schemaVisualizer`、`ui` 以及文档改用新 URL。
3. **Inventory**：签名身份的注册与 URL 合并（D4）、同名 schema 内容检查（D6）、引用按 vDS 优先解析（D8），以及 D9 的统一 `{ds}/{id}` 列表与查询。
4. **Channel 接口 + DirChannel + 签名**。
5. **shared vDS**：引用闭合（D5）、`ChannelPublish` process、outbox、snapshot。
6. **mirrored vDS**：只读拦截、`ApplyChange`、验签与可信指纹、cursor、首次加载与重建（D11）。
7. **GitChannel**：仓库布局、攒批 push、浅拉取、manifest。
8. **加密**（可选）。

## 13. 测试方案

- **单元测试**：vDS 路由与保留名、消息序列化、seq 幂等和跳号检测、签名和指纹校验、snapshot + segment 合并的结果、shared 不能引用 local 的检查。
- **集成测试**：新增 `docker-compose/shared-mirror/`，起两套互不连通的网络（两个 compose network），各自有 DynamoDB Local、DS 实例和 Inventory，中间用一个本地 bare git 仓库（volume 挂载）充当通道。覆盖这些场景：
  - 首次全量加载；增 / 删 / 改的增量同步；
  - schema 升级后旧版本数据仍可查；
  - 引用字段在同一 vDS 内展开；不同来源的同 type 同 id 记录同时存在，互不覆盖，引用也不串到别的来源；
  - mirrored 拒绝外部写入；
  - 镜像重启后从 cursor 续传；人为删掉一个 segment 后自动从 snapshot 重建；
  - 篡改签名或替换公钥时拒绝应用；
  - **网络 B 中两个实例镜像同一个来源，Inventory 只注册一个 DS，合并两个 URL，查询结果不重复**；
  - 同名不同指纹的两个来源：后注册的被自动改名为 `{name}-{指纹前 8 位}`，两边数据都能查到，重启后名字不变；
  - 两个实例给同一个来源配置了不同 alias：Inventory 合并成一个 vDS 并告警；
  - `pinnedSources` 固定的指纹能抢回原名。

## 14. 已确认的问题

- Inventory 不提供 `/{ds}/{type}/{id}` 形式，只用 D9 的 `/{type}/{ds}/{id}`。
- 从 config 里删掉 mirrored 时删除数据（D11）。
- Inventory 选副本时优先选 cursor 最新的那个（D4）。
- 一个实例可以有多个 shared，路径为 `shared/{name}`（第 2 节、D5、第 11 节）。

## 15. 实现与计划的差异

第一阶段的 8 个里程碑都已实现。和上文不同或上文没写到的地方：

**结构**
- **表名**：`local` 沿用 `table.data`，不加后缀，这样已有部署的数据原地可用；顶层 `initialized` 迁移到 `state.json` 的 `vds.local`。shared / mirrored 为 `{table.data}_{kind}_{name}`。
- **Channel 的位置**：`src/DataService/Channel` 一个包（`channel.go` 接口与工厂，`store.go` 目录格式，`git.go` git 钩子，`keys.go` 签名与加密）。git 通道用 git CLI，发布方完整 clone、commit、push（冲突时 rebase 重试 3 次），订阅方 `fetch --depth 1` 后 `reset --hard`。
- **共享逻辑**：`src/DataService/Share`（outbox、`ChannelPublish`、`Publisher`、`Mirror`）；镜像侧直接写入在 `DataHandler/apply.go`。
- **签名**：manifest 记录每个文件的 sha256，只对 manifest 签名一次（`manifest.sig`），不是每个文件一个 `.sig`。
- **加密**：没有用 `age`，用标准库实现 X25519 + HKDF-SHA256 + AES-256-GCM，支持多个接收方；密钥是一行 base64，`DataServiceAdmin enckey` 生成。

**行为**
- **撞名改名只改 Inventory 的 dsId**：D4 写的是 vDS 路径也改成派生名字。实现中 vDS 路径始终是 config 里的名字（例如仍是 `mirrored/prod`），Inventory 分配的 dsId 存在 `state.json`，在 `/{vDS}/config` 的 `inventoryId` 中可见。这样本地直连 DS 的客户端不会因为别处的撞名而改路径。
- **shared 和它自己的镜像在同一个 Inventory**：合并成一条记录后 kind 保持 `shared`；按 cursor 给副本排序时 shared 源头排在最前。DS 复查注册时对签名 vDS 只要求记录是签名身份且包含自己的 URL，避免两边来回改 kind。
- **发布前与通道对账**（第 9 节没写）：通道比本地进度新（写入通道后、保存进度前进程退出）就追上；通道比本地旧（仓库被重置）就先发一个当前 seq 的 snapshot，镜像方因 gap 从它重建。通道的 `Publish` 严格要求 seq 接着通道上的最大 seq。
- **snapshot 只按时间触发**（`snapshotIntervalSec`），没有按 outbox 积压量触发。通道上还没有 snapshot 时，第一次发布就会生成一个。
- **outbox 不需要去重**：seq 无空洞（先写 outbox 记录，再推进计数器），发布方只发已提交的连续 seq。
- **Inventory `GET /{type}/{ds}`**：列出某一个 DS 上该类型的 id（D9 之外新增）。
- **`DELETE /inventory/{dsId}?url=`**：DS 删除 vDS 时用它注销自己的 URL。
- **force-init**：仍然清空整个数据库，所以所有 vDS 都会重新初始化。

**顺带修复的已有问题**
- Inventory 判断名字是否被占用时把整个 record 信封当成 `data` 解析，`instanceId` 总是空，导致"不同 DS 撞名"一直被当成旧记录原地接管，从没分配过 `_n` 后缀。
- Inventory `DELETE` 的路径检查条件写反了（`/{type}/{id}` 被拒，多一段反而通过）。
- `DataServiceInfo.GetUrl` 取的是最后一个可达的 URL，改为第一个。
- `Http.SiteReachable` 没有关闭响应体。

**测试**
- 没有用 docker-compose 做集成测试，改为进程内测试 `test/src/VdsTest`：两个 DS + 一个 Inventory 跑在 `httptest` 上，用按表隔离的内存库 `MemDb`；覆盖第 13 节的大部分场景（全量与增量、只读、同 type 同 id 不覆盖、引用在 vDS 内展开、篡改与指纹不符、加密、git 通道用本地 bare 仓库、副本合并、撞名派生、alias 不一致合并、pinnedSources、删除 mirrored、通道被重置）。
- `docker-compose/shared-mirror/` 已提供（dir 通道，两个 compose network），但还没有在 docker 里实际跑过。

**未做**
- 在 vDS 之间复制 schema 的 `DataServiceAdmin` 命令（D2 提到）。
- mirrored 的类型别名（D6 的后续阶段）。
