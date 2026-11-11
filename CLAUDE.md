# CLAUDE.md

本文件为 Claude Code (claude.ai/code) 提供指引，帮助其在处理本仓库代码时理解上下文。

## UniTAO 是什么？

UniTAO 是一个 **Universal Inventory（通用清单）系统**。

### 要解决的问题

传统的 inventory 系统通常为特定场景硬编码数据模型，导致：
- 新增一种资产类型需要修改代码、重新部署
- 跨系统的数据结构难以统一理解和查询
- 信息之间的关联关系（引用、聚合）不直观

### UniTAO 的解法

**由 Schema（数据模型）主导一切。** 数据的结构、校验规则、类型之间的关联关系都由 JSON Schema 定义，存储在数据库中。系统本身不做任何数据模型的假设——而是根据 Schema 动态理解数据。

这意味着：
- 新增数据类型只需定义一个新的 Schema，无需修改代码
- 数据结构自带说明（self-documenting），Schema 本身就是对数据最精确的描述
- 支持跨 Data Service 的数据引用和查询，信息关联一目了然

> 更多设计理念和解决的问题将陆续补充。

## 项目架构

UniTAO 是一个基于 Schema 驱动的多节点异构基础设施清单管理系统。数据由 JSON Schema 定义，服务提供 CRUD + 跨节点交叉引用查询，新增数据类型无需编码。

### Data Service 与 Inventory Service

UniTAO 由两类服务组成：

- **Data Service** — 数据节点，负责具体数据的 CRUD 操作、Schema 校验、写入后处理（Journal）和逐记录锁。每个 Data Service 实例连接一个数据库（DynamoDB、MongoDB 或本地文件），管理自己域内的数据。它是"存数据"的地方。
- **Inventory Service** — 聚合/查询节点，负责注册多个 Data Service、同步它们的 Schema、并提供跨 Data Service 的交叉引用查询。上层应用只需与 Inventory Service 对话，不需要知道数据分散在多少个 DS 上。它是"查数据"的地方。

这种分离带来了几个关键优势：
- **Data Service 可水平扩展**，每个 DS 独立管理自己的数据域，互不干扰
- **Inventory Service 提供统一视图**，屏蔽底层存储细节
- **跨 DS 引用**通过 `contentMediaType: "inventory/{type}"` 扩展字段实现，Inventory Service 负责解析和路由

### 核心架构概念

1. **元 Schema（Schema-of-schema）**：元 Schema 定义在 `lib/Schema/data/schema.json`。所有数据类型都作为存储在数据库中的 JSON Schema Record 定义，使得系统无需代码修改即可完全扩展。

2. **Record 格式**：所有数据存储为带信封字段的 Record：
   - `__type` — Schema/数据类型名称
   - `__id` — 记录标识符
   - `__ver` — Schema 版本
   - `data` — 载荷（根据该类型的 Schema 校验）

   示例：
   ```json
   {
     "__type": "Server",
     "__id": "srv-001",
     "__ver": "0.0.1",
     "data": {
       "hostname": "web-01.example.com",
       "ip": "10.0.1.10",
       "rack": "rack-a1"
     }
   }
   ```

   `__ver` 是 `xxx.xxx.xxx` 形式的版本串（至少三段数字），不是整数。跨类型引用直接存目标记录的 `__id` 字符串，没有额外的引用包装结构。

   `__id`、`__type`、`__ver` **不能包含空白字符**（空格、制表符、换行等）。`Record.ValidateEnvelope` 负责检查，Data Service 写入时经 `SchemaOps.ValidateRecord` 调用，Inventory 的 `PUT /` 在 `PutData` 里调用；违反返回 400 `record field [__id]=[a b] must not contain whitespace`。`data` 里的属性名和值不受这条限制。

3. **JSON Schema 扩展**：对 JSON Schema 的两项自定义扩展。

   **`contentMediaType: "inventory/{type}"`** — 标记字段引用由 Inventory Service 管理的另一类型的数据。要点：

   - **只认 `inventory/` 前缀**。标准 JSON Schema 的取值（`json`、`application/json`、`text/plain`）和裸类型名（`actor`）都会在 schema 预处理阶段被拒——`[contentMediaType]=[json] not supported`，schema 根本注册不上去。值按第一个 `/` 切分，所以 `application/json` 报的是 `[application]`。
   - **只能挂在 `type: "string"` 的字段上**（`SchemaDoc.IsCmtRef`）；数组引用写成 `items` 下的字符串。
   - **写入时校验**：保存前会经 Inventory Service 查目标记录，不存在则 400 `reference inventory:{type} with value=[{id}] does not exists`。所以**被引用的记录必须先存在**；两个记录互相引用时，若两侧字段都是必填就会死锁（字段**默认必填**，不写 `required` 就是必填），需把至少一侧设为 `"required": false` 再分步补写。
   - **依赖 referral 表**：类型要先被 Inventory Service 的 sync 登记才能被引用。sync 在启动时、新 DS 注册事件、以及周期（`sync.intervalSec`，默认 300 秒）触发，所以刚注册的 schema 不能立即被引用；手动触发用 `InventoryServiceAdmin sync`。referral 记录存在 Inventory Service 自己的库里（`dataSync.go` 的 `Db.Replace`），**重启不会丢失**，所以这个等待只在首次引入某类型时出现，重启任一服务都不会重新引入。

   完整用法与 demo 实例见 `README.md` 的 contentMediaType 一节。

   **`indexTemplate`** — 在被引用的 Record 创建时自动填充注册/反向引用属性

4. **可插拔数据层**：`src/Data/DbIface.Database` 接口，具有 DynamoDB、MongoDB 和基于文件的实现。工厂函数在 `src/Data/data.go` 中根据配置切换。可选接口 `DbIface.Scanner` 逐条读取一个类型的记录（MongoDB 用游标、DynamoDB 逐页），`DbIface.Scan` 在数据库没实现它时退回 `Get`。DynamoDB 的 `Query` 会按 `LastEvaluatedKey` 翻页读完（单次 Query 上限 1MB）。

5. **DataServiceProxy**：Inventory Service 内部通过 `DataServiceProxy` 将跨 DS 的引用查询代理到目标 Data Service。上层应用提交查询后，Inventory Service 根据引用的 `__ref__` 记录自动路由到正确的 DS 实例。

6. **Journal（变更队列）**：Data Service 内部的异步任务队列，用来执行写入后的副作用，**不是审计日志**。
   - `DataHandler` 每次增 / 删 / 改 / PATCH 成功后调用 `AddJournal(type, id, before, after)`，按 `(type, id)` 分页存进 DS 自己的库。
   - `JournalHandler` 为每个 `(type, id)` 起一个 `JournalWorker`，按顺序把 entry 交给 `LoadProcesses` 注册的 `JournalProcess`。顺序只在单条记录内保证，没有全局序号。
   - 一页 entry 全部处理完、head 前移时，这一页会被删除（`JournalHeadForward` → `removeJournal`），不保留历史。
   - 内置两个 process，都服务于 `indexTemplate`：`SchemaChanges` 在 schema 注册或删除时，经 Inventory 在被引用类型的 `cmtIdx` 里登记或撤销订阅；`CmtIndexChanges` 在数据变化时回填或清理被引用记录上的反向索引，可能跨 DS PATCH。shared vDS 另外注册 `Share.ChannelPublish`（排在内置 process 之后），把变更写进 outbox 供发布。mirrored vDS 没有 Journal。
   - 要给写入加新的后续处理（如向外推送变更），就实现一个 `JournalProcess` 并在 `LoadProcesses` 里注册。不过 Journal 处理完就删，不能用来回放或审计。

7. **虚拟 Data Service（vDS）**：一个 DS 实例上承载多个 vDS，每个 vDS 有独立的表、schema、Journal、锁，在 Inventory 中各自注册为一个 DS。
   - `local`（表 `table.data`）：本网数据，不共享。config 没有 `vds` 段时默认只有它。
   - `shared/{name}`（表 `{table.data}_shared_{name}`）：经 Communication Channel 发布给其它网络，可写。只能引用同一个 shared vDS 内的记录（`Handler.RefLocalOnly`）。
   - `mirrored/{name}`（表 `{table.data}_mirrored_{name}`）：别的网络的 shared 的只读副本（`Handler.ReadOnly`，写返回 405），没有 Journal。
   - 记录身份是 `(vDS, __type, __id)`；local 与各 shared 的 schema 各存一份，互不共用。
   - 代码：`DataServer/vds.go`（每个 vDS 的初始化、删除、`/{vDS}/config`）、`Share/`（outbox、`ChannelPublish` JournalProcess、发布器 `Publisher`、镜像同步器 `Mirror`）、`Channel/`（`dir` / `git` 通道、ed25519 签名的 manifest、必选的 X25519 加密）、`DataHandler/apply.go`（镜像侧直接写入，跳过引用校验）。
   - 通道是单 publisher 的低频队列：vDS 在通道上的地址是 `repo + branch + folder`（dir 为 `path + folder`，`folder` 默认是 vDS 名，可多级）；一个仓库可容纳多个 vDS，同一 DS 内 `workDir` 相同的 vDS 共用一份克隆（`sharedGitRepo`，必须同 repo + branch）。folder 下有签名的 `manifest.json`（version 3：lastSeq、queue、histories 及各文件 sha256、history 的授权）、`queue/` 增量 batch、`history/<hid>/<n>.jsonl.gz.aes` 全量 history 的分片、`subscribers/<id>.json` 各 mirror 自己写的进度（request、`subscribe|active`、cursor）。
   - 加密是必选：shared 的 `encrypt.recipientsFile` 就是 subscriber 白名单（可以为空，文件不存在时 DS 启动时在配置路径上建一个只有注释的空文件——目录必须存在；为空时为空时 `fileStore.Publish` 不写 queue batch，只推进 manifest 的 `lastSeq`，新订阅方从 history 拿全量；`/config` 的 `recipients` 显示数量、`channel.recipientsFile` 显示路径），subscriber id = X25519 公钥的 sha256（`Channel.SubscriberId`）；queue 对所有接收方加密；history 有自己的随机内容密钥，manifest 的 `grants` 里为每个被授权的 subscriber 各包装一份（`HistoryRef.Grants`）。
   - 发布：写入 → Journal → outbox（无空洞的 seq，先写记录再推进计数器）→ `Publisher.PublishPending` 按连续 seq 发 queue batch；发布前与通道对账（通道领先则追上，通道落后或旧格式则 `Reset` 到本地 seq；通道上还没有本来源的 manifest 时也 `Reset`，所以 publisher 启动后不等第一条数据就在 folder 下写出签名的 manifest，订阅方由此能找到来源）。`Maintain` 为订阅请求提供 history（`provideHistory`：`historyReuseHours`（默认 24）内最新的 history 只加授权 `GrantHistory`，不重写分片；否则 `PublishHistory` 打包一份新的，本轮所有新请求共用；内容密钥只在 publisher 内存里，重启后旧 history 不能再授权）、判定死亡（第一个未处理 batch 或授权给它而未加载的 history 超过 `subscriberTimeoutHours`，默认 24，删其进度文件）、删除所有存活 subscriber 都处理过的 batch，以及所有授权都不再等待的 history；`CompactIfDue` 每 `compactIntervalHours`（默认 24）把 git 分支压成一个提交（`--force-with-lease`）并本地 gc。
   - history 分片：按 gzip 后大小切片（`DefaultHistoryPartBytes` 32MB，低于 GitHub 单文件 100MB 上限），每片 nonce + AES-GCM 二进制，AAD 绑定来源、history id、分片序号；两边都流式：publisher 经 `Handler.EachSyncedRecord`（schema 一次读出，各类型数据走 `DbIface.Scan`）边读边压缩加密，分片暂存在系统临时目录（`TMPDIR`，不在通道工作目录里，推送被拒重做时重新复制），内存里最多一个分片；mirror 用 `HistoryReader.Each` 逐片读取，`ClearSynced` 只收集记录的键。
   - 镜像：`Mirror` 先登记订阅（来源未发布时也可以），拿到授权给自己的 history 后整体替换本表，再从 history 的 seq 消费 queue 并汇报进度；本地的 request/loaded/cursor 存在 `state.json`，进度文件只是汇报。进度文件被删、请求不符、cursor 不在通道上（`ErrGap`）或本地状态丢失时重新订阅，等新 history 期间继续提供旧数据。
   - git 走 SSH 时用 DS 自己生成的身份：启动时 `setupSSHIdentity` 在 config 的 `ssh.dir`（缺省 config 同目录的 `ssh/`，相对路径相对 config 目录）下生成（已有则沿用）ed25519 私钥 `id_ed25519`（OpenSSH 格式，0600，`Channel/sshkey.go`），公钥写进 `state.json` 的 `ssh.publicKey` / `ssh.fingerprint` 和 `GET /{vDS}/config` 的 `instance.ssh`，由人加到 git 服务器上。git 经 `GIT_SSH_COMMAND` 使用它（`StrictHostKeyChecking=accept-new`，host key 记在 `ssh/known_hosts`；`ConnectTimeout=30`、`ServerAliveInterval=15`/`ServerAliveCountMax=4`，连接停在半路约 1 分钟内断开）；环境里已设 `GIT_SSH_COMMAND` 时以环境为准。ssh 要求运行 UID 在 `/etc/passwd` 里有记录：镜像的入口脚本 `docker/unitao/entrypoint.sh` 在容器启动时给非 root UID 补一条（镜像里 `/etc/passwd` 为 666，没有 setuid 程序，不会因此提权），不用再挂 `passwd`。私钥不从外部挂入，所以该目录必须可写、持久；容器部署把 `ssh.dir` 设为 `/opt/UniTAO/ssh` 并挂 Docker 命名卷，私钥就不会出现在宿主机的 bind-mount 目录里（镜像把该目录建成 1777，新命名卷继承这个权限，非 root UID 也能写）。
   - 每条 git 命令有总超时（`DefaultGitTimeout` 10 分钟，`gitRepo.run`），超时连同 git 启动的 ssh / git-remote-https 整个进程组一起杀掉（`procgroup_unix.go`），否则停在半路的连接会让 git 永远挂着、一直占着工作目录的锁。
   - git 写入：fetch → reset 到远端 → 只写自己的文件 → commit → push，被拒（`errRetry`）就从头重做（`fileStore.write`），不做 rebase。mirror 也要写通道（进度），所以要有写权限。
   - 运行时状态在 `state.json` 的 `vds` 下（每个 vDS 的 `initialized`、`table`、Inventory 分配的 id、镜像 cursor）。从 config 删除 mirrored 并重启会删表并从 Inventory 注销其 URL：待注销的 URL 先记进 `state.json` 的 `unregister`，启动时注销完才开始注册当前的 vDS（否则改 alias 后的新名字会因指纹相同被合并进旧记录）。撞名被改名的镜像，给它配 `alias` 即可让 DS 路径和 Inventory 的 dsId 一致，但同一来源的所有副本要用同一个 alias。
   - 签名密钥与加密密钥用 `DataServiceAdmin keygen|fingerprint|enckey` 生成。

### REST API 概览

Data Service 和 Inventory Service 均通过 HTTP JSON API 对外暴露。

Data Service 的 URL 是 `/{vDS}/{type}[/{id}]`（`{vDS}` 为 `local`、`shared/{name}`、`mirrored/{name}`），不兼容旧的无前缀形式。Inventory Service 的 URL 是 `/{type}[/{ds}/{id}]`。两者都**没有 `/data` 或 `/inv` 前缀**，也无 `/ds/...` 之类的管理端点。

| 服务 | 方法 | 端点 | 说明 |
|------|------|------|------|
| Data Service | `GET` | `/` | 列出 vDS 路径；`/shared`、`/mirrored` 列出该组的名字 |
| Data Service | `GET` | `/{vDS}` | `["config", "schema", <data types>]` |
| Data Service | `GET` | `/{vDS}/config` | 该 vDS 的设置与状态（kind、表、通道、发布/同步进度），不含密钥；git 通道 `repo` 地址里的凭据显示为 `***`（`Channel.RedactRepo`） |
| Data Service | `POST` | `/{vDS}` | 创建 Record，类型取自 body 的 `__type` |
| Data Service | `GET` | `/{vDS}/{type}` | 列出该类型的所有 `__id` |
| Data Service | `GET` | `/{vDS}/{type}/{id}[/{attrPath}]` | 查询 Record，可带属性路径深入 |
| Data Service | `PUT` | `/{vDS}/{type}/{id}` | 替换 Record |
| Data Service | `PATCH` | `/{vDS}/{type}/{id}[/{attrPath}]` | 部分更新 Record；请求体是 JSON 值（字符串发 `"abc"`），不是合法 JSON 时按原文本处理 |
| Data Service | `DELETE` | `/{vDS}/{type}/{id}` | 删除 Record |
| Data Service | `GET` | `/{vDS}/schema[/{type}]` | 列出 / 查询已注册的 Schema |
| Inventory Service | `GET` | `/{type}` | 列出所有 DS 上该类型的 id，**始终**为 `{ds}/{id}` |
| Inventory Service | `GET` | `/{type}/{ds}[/{id}[/{attrPath}]]` | 跨 DS 查询 Record，必须带 ds；沿引用展开时先在起始记录的 vDS 内找，找不到再全局查找 |
| Inventory Service | `GET` | `/schema` | 列出所有已同步的类型 |
| Inventory Service | `GET` | `/referral[/{type}]` | 查询类型到 Data Service 的映射 |
| Inventory Service | `PUT` | `/` | 注册 / 更新 Data Service（body 为 `inventory` 类型的 Record） |
| Inventory Service | `POST` | `/referral` | Data Service 上报类型变更事件，触发同步 |
| Inventory Service | `DELETE` | `/{type}/{id}` | 删除 Inventory 本地记录（如 `inventory/{ds-id}`） |
| Inventory Service | `DELETE` | `/inventory/{dsId}?url={url}` | 只移除一个副本 URL，没有剩余 URL 时删除整条记录 |

**Inventory 中签名 vDS 的注册**（`allocateSignedDsId`）：shared / mirrored 以名字 + 来源公钥指纹（`instanceId`）注册。同名同指纹（镜像副本、shared 与它自己的镜像）合并 URL 成一条记录，有 shared 时 kind 保持 shared；同名不同指纹的后来者改名为 `{name}-{指纹前 8 位}`；同指纹不同名合并进已有记录；`pinnedSources` 把名字固定给某个指纹。sync 时按各副本 `/config` 的 cursor 给 URL 排序（shared 源头最前）。同一类型同版本的 schema 在不同 DS 上内容不同时，后来的 DS 不进入该类型的 referral（D6）。

Data Service 的路径查询引擎还支持 `?schema`（显示当前路径的 schema）、`?flat`（只显示当前层）、`?iterator`（列出各叶子可选值），以及用 `[*]` 通配数组/映射的下标。

**Schema 不能 PUT 或 PATCH，只能 POST 新版本升级。** 对 `/schema/...` 发 PUT（`Handler.Set` 拒绝所有 `InternalTypes`）或 PATCH（`Handler.Patch` 单独拦 `schema`）都会返回 400 `method[...] on type[schema] is not allowed`。要修改 schema，用 `POST /` 提交同 id、更高 `version` 的 schema record：`Handler.Add` 会把当前版本归档为 archived id 再写入新版本；版本相同或更低都会被拒，也不能直接 POST 一个 archived id。这样按旧版本写入的数据仍能找到它的 schema。`Handler.Patch` 不能拦全部 `InternalTypes`，因为日志处理器要经它（可能跨 DS 走 HTTP）PATCH `CmtIdx`。

Data Service 启动时会为每个 vDS 通过 `PUT /` 注册到 Inventory Service（`DataServer/selfRegister.go`），URL 为 `{ds.urls}/{vDS 路径}`，之后每 `SelfRegCheckSec` 复查一次；Inventory 侧不必也不存在 `POST /ds/register` 端点。

### 网络安全模型与访问控制

Inventory 系统（Inventory Service 与所有 Data Service）**必须完全部署在受保护的内网中，不能通过外网访问**。因此 Inventory 系统**信任内网中的其它系统**：

- **当前开发阶段：不做任何访问限制。** 没有认证/鉴权；两个服务的 REST API 对**任意来源开放 CORS**（`Access-Control-Allow-Origin: *`，`OPTIONS` 预检直接返回 204），内网的 UI（如不同端口上的 schema designer / 仪表盘）可直接从浏览器调用。
- **生产阶段：用白名单限制访问**，只允许名单内的内网系统调用。访问控制靠白名单，不靠 CORS，CORS 保持全开。

实现：`lib/Util/Http/cors.go` 的 `Http.Cors` 中间件包住两个服务的根 handler（`http.HandleFunc("/", Http.Cors(srv.handler))`）。**不要**再在服务配置的 `http.headers` 里设置 `Access-Control-Allow-Origin`——那里的值只会覆盖普通响应上的 `*`，预检响应仍是 `*`，浏览器会拿到不一致的结果。不要在代码里新增认证或来源校验，除非是在实现生产阶段的白名单。

### Go 工作区 (go.work)

```
lib/                           # 共享库（无 main）
  Schema/                      # JSON Schema 引擎、Record 类型、SchemaDoc 解析
  SchemaPath/                  # Schema 路径遍历与查询引擎
  Util/                        # HTTP 客户端、JSON 工具、模板引擎、线程控制、HashLock

src/                           # 源码包（无 main）
  Data/                        # 数据层 — 可插拔的数据库接口 + 实现
    DbIface/                   # 数据库接口（Get, Create, Replace, Delete, ...）
    DbDynamoDb/                # DynamoDB 实现
    Mongodb/                   # MongoDB 实现
    SysDirFile/                # 文件系统实现（用于测试/开发）
    DbConfig/                  # 数据库连接配置类型
  DataService/                 # Data Service 核心 — CRUD 处理器、日志、锁、配置、HTTP 服务
    DataHandler/               # Record 校验、CRUD 操作、跨 DS 代理
    DataServer/                # HTTP 服务器及 REST 端点路由
    DataJournal/               # 写入后的异步处理队列（驱动 indexTemplate / cmtIdx / 共享发布）
    Channel/                   # Communication Channel：dir / git 上的队列（manifest v3、queue、分片 history 与授权、subscribers），签名与加密
    Share/                     # shared 发布（outbox + Publisher）与 mirrored 同步（Mirror）
    DataLock/                  # 并发访问的逐记录锁
  InventoryService/            # Inventory Service 核心 — 跨 DS 查询路由
    DataHandler/               # DS 注册、Schema 同步、交叉引用查询
    InventoryServer/           # HTTP 服务器
    InvRecord/                 # Data Service 信息记录类型
    RefRecord/                 # 引用/交叉引用记录类型

app/                           # 可执行入口
  DataService/                 # main.go — 运行 Data Service 服务器
  InventoryService/            # main.go — 运行 Inventory Service 服务器

tool/                          # 管理 CLI 工具
  DataServiceAdmin/            # 数据库表创建与数据导入
  InventoryServiceAdmin/       # DS 注册、Schema 同步

test/                          # 集成测试与单元测试
  src/
    SchemaTest/                # Schema 引擎单元测试
    SchemaPathTest/            # 路径查询引擎测试
    UtilTest/                  # 工具函数测试
    DataServiceTest/           # 集成测试（需要运行中的服务）
    VdsTest/                   # vDS / Channel 测试：进程内起 DS + Inventory（内存库 MemDb），含 git 通道
```

### 辅助目录

```
dbSchemas/                     # 数据库表 Schema 定义（DynamoDB、MongoDB）
demo/                          # 分步演示脚本（PowerShell + Python）
docker/                        # Docker 构建脚本：唯一的镜像 docker/unitao（发布为 unitao-dev）+ UIServer
docker-compose/                # 部署：script/ 下的脚本把 template/ 下的模板（mongodb、dataservice、inventory）复制到指定目录、生成配置并启动；见 docker-compose/README.md
  script/                      # docker-deploy.sh（Ubuntu 上装 Docker Engine）、mongo_deploy.sh、unitao_deploy.sh（交互部署一个 DS 或 Inventory 实例，只带 local vDS）、unitao_vds.sh（交互增删 shared / mirrored vDS，维护 config/vds/<kind>/<name>/ 下的密钥）
  template/                    # mongodb/、dataservice/、inventory/：每个是一个 compose 项目的模板，不在原地运行
ui/WebServer/                  # Node.js 静态文件服务器（用于仪表盘）
schemaVisualizer/              # Express.js Schema 可视化前端服务
javascript/Schema/             # Schema 库的 JavaScript 移植版
.docker/                       # Docker 元数据
```

### Go 模块布局

工作区使用 `replace` 指令（或本地 `go.work` use）连接模块。内部包使用简短模块名（如 `module Data`、`module DataService`、`module InventoryService`），而库使用规范路径 `github.com/salesforce/UniTAO/lib/...`。应用/工具入口通过 `go.work` 的 `use` 指令使用 `github.com/salesforce/UniTAO/app/...` / `UniTao/DataServiceAdmin` 等。

所有 Go 源码目标版本为 Go 1.18（库）到 1.24（Data 模块）。

## 构建与运行命令

### 开发环境前提

开始之前需要准备好以下依赖：

- **Go 1.18+**（部分模块需要 1.24）— 构建所有 Go 二进制
- **Docker** — 部署（docker-compose/）或构建镜像
- **DynamoDB Local**（可选）— 本地运行 Data Service 的默认数据库；也可以使用 MongoDB 或文件模式（`SysDirFile`，适合测试）
- **Node.js**（可选）— 运行 `ui/WebServer/` 或 `schemaVisualizer/`

用 Docker 部署（MongoDB + Inventory + Data Service，各自一个目录、一个 compose 项目；详见 `docker-compose/README.md`）：
```bash
docker-compose/script/mongo_deploy.sh ~/infra/mongodb
docker-compose/script/unitao_deploy.sh inventory ~/infra/unitao-inv
docker-compose/script/unitao_deploy.sh dataservice ~/infra/unitao-ds
docker-compose/script/unitao_vds.sh ~/infra/unitao-ds          # 可选：shared / mirrored vDS
```

**Data Service 不写 `config.json`，运行时状态存在旁边的 `state.json`。** 三个值由 DS 自己产生、需要跨重启保留，由 `Config.LoadState` / `State.Save`（`src/DataService/Config/state.go`）读写 config 同目录下的 `state.json`：

- `initialized` — 数据库已初始化。`InitDatabase` 在它为真时直接返回（`src/DataService/DataInit/init.go`）。想重新建表、导入元 schema，把 `state.json` 里的 `initialized` 改成 `false`；**不要删整个文件**，否则 `instanceId` 也会丢。`force-init` 只在 `initialized` 为 false 时才会生效。
- `instanceId` — 自动生成的实例 UUID，用于区分「同 DS 重注册」与「不同 DS 撞名」。**丢了它，Inventory 会把这个 DS 当成撞名的另一个 DS**，分配带后缀的新 ds.id。
- `dsId` / `dsIdFrom` — config 中 `ds.id` 为空时自动生成的 id，或 Inventory 撞名时分配的 id。只有 config 的 `ds.id` 仍等于 `dsIdFrom` 时才生效；人工改了 config 的 `ds.id`，旧值自动作废。

**迁移**：旧版本把这些值回写进了 `config.json`。`state.json` 不存在时，`LoadState` 会以 config 里的 `initialized` / `ds.instanceId` 为初值，立即写出 `state.json`，之后 `config.json` 里这两个字段不再起作用。所以本地被旧版本改脏的 `config.json`，**要先用新版本启动一次**、生成 `state.json`，再 `git checkout` 还原；以前加过 skip-worktree 的，先用 `git update-index --no-skip-worktree <file>` 撤销。

`unitao_deploy.sh` 部署的实例把目标目录的 `config/` bind-mount 到 `/opt/UniTAO/config`，`state.json` 就写在那里（模板不在仓库里原地运行）。InventoryService 也不写自己的配置。

### Go 构建（所有目标）
```bash
# 构建 Data Service 二进制
go build ./app/DataService

# 构建 Inventory Service 二进制
go build ./app/InventoryService

# 构建管理工具
go build ./tool/DataServiceAdmin
go build ./tool/InventoryServiceAdmin

# 构建全部（每个 app / tool 是独立 module，`./app/...` 跨不了 module，必须逐个列出）
go build ./app/DataService ./app/InventoryService ./tool/DataServiceAdmin ./tool/InventoryServiceAdmin
```

### 按地区选 GOPROXY 的构建脚本

`scripts/build-cn.sh` 和 `scripts/build-us.sh` 只设置 `GOPROXY`，再调用公共的 `scripts/build.sh`。`build.sh` 编译上面四个目标，并跑无外部依赖的单元测试；加 `--all` 跑 `test/src` 下的全部测试。

```bash
./scripts/build-cn.sh        # 在中国：GOPROXY=https://goproxy.cn,direct
./scripts/build-us.sh        # 在美国：GOPROXY=https://goproxy.io,direct
./scripts/build-us.sh --all
```

在美国，goproxy.cn、proxy.golang.org、golang.org 都可能连不上（`golang.org/x/*` 用 `GOPROXY=direct` 也拉不到），表现为 `TLS handshake timeout`。这是代理的问题，不是代码的问题，换 goproxy.io 即可。`GOPROXY` 只在脚本进程内生效，不改 shell 环境。

### 运行服务
```bash
# Data Service（需要数据库连接）
go run ./app/DataService -id <service-id> -config <config.json>

# Inventory Service
go run ./app/InventoryService -config <config.json>

# 管理工具：初始化数据库表
go run ./tool/DataServiceAdmin table -config <config.json> -table <tables.json>

# 管理工具：导入数据
go run ./tool/DataServiceAdmin data -config <config.json> -table <table> -data <data.json>

# Inventory 管理：注册 Data Service
go run ./tool/InventoryServiceAdmin add -config <config.json> -ds <url> -id <ds-id>

# Inventory 管理：同步所有 Data Service 的 Schema
go run ./tool/InventoryServiceAdmin sync -config <config.json>
```

### 测试
```bash
# 运行所有测试模块
go test ./test/src/...

# Schema 单元测试（无外部依赖）
go test ./test/src/SchemaTest/...
go test ./test/src/SchemaPathTest/...
go test ./test/src/UtilTest/...

# DataService 集成测试（需要运行中的 DynamoDB + 服务）
go test ./test/src/DataServiceTest/...

# 运行单个测试函数
go test ./test/src/SchemaTest/... -run TestSchemaOps
go test ./test/src/DataServiceTest/... -run TestDataHandler

# 需要真实 DynamoDB 的测试（如 TestDynamoDbPaging：一个类型超过 1MB 时 Get / Scan 要翻页读完）
# 起一个内存模式的 DynamoDB Local 临时容器（默认 127.0.0.1:18000），跑完删除；需要 Docker
./scripts/test-dynamodb.sh

# 需要真实 MongoDB 的测试（如 TestMongoDbObjectId：读取不返回 MongoDB 的 _id）
# 起一个数据在 tmpfs 里的 MongoDB 临时容器（默认 127.0.0.1:27018），跑完删除；需要 Docker
./scripts/test-mongodb.sh
```

需要真实 DynamoDB / MongoDB 的测试分别读 `UNITAO_TEST_DYNAMODB_ENDPOINT`、`UNITAO_TEST_MONGODB_ENDPOINT`（MongoDB 另读 `UNITAO_TEST_MONGODB_USER` / `UNITAO_TEST_MONGODB_PASSWORD`），没设置时 skip，所以 `go test ./test/src/...` 和 `build.sh --all` 照常不依赖外部服务。指向已有的实例也可以：测试每次用新名字的表（MongoDB 是新 database），结束时删掉。

MongoDB 后端读取时用投影去掉文档的内部主键 `_id`（`MongoDb.ObjectIdField`），写入时忽略客户端带回的 `_id`，所以 Record 里不会出现它。

### Docker
```bash
# 构建所有 Docker 镜像
./docker/buildAll.sh          # Linux/Mac
./docker/buildAll.ps1         # Windows

# 单镜像（一体化）
docker build -f ./docker/unitao/dockerfile -t unitao:latest .

# 部署（见 docker-compose/README.md）
docker-compose/script/unitao_deploy.sh dataservice ~/infra/unitao-ds

# CI 构建（GitHub Actions 推送到 ghcr.io）
```

CI 发布到 `ghcr.io/howeyhuo/unitao-dev` 的 tag 规则（`latest` / `<version>` / `sha-<short>` 各自何时前移、为什么未 bump 的 version tag 不能当回滚点）、以及查询各 tag 当前镜像摘要的命令，见 `docker/README.md`。

