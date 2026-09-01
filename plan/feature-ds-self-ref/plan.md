# Data Service 自我注册 Inventory Service

## Context

**问题**：目前 Data Service 在 Inventory Service 中的注册完全手工。`InventoryServiceAdmin` CLI 必须直连 Inventory 数据库，`add` 写 `inventory/<ds-id>` 记录、`sync` 拉取各 DS 的 `/schema` 写 `referral/<dataType>` 记录。Mongo/Dynamo 模式下没有任何脚本能自动创建 `inventory` 记录——每起一个 DS 都要人工执行命令。

**目标**：DS 启动后自动向 `config.inventory.url` 链接的 Inventory Service 自我注册，无需手工命令。

**用户确认**：`InvSrv/inventory` 类型（`inventory/<ds-id>`，payload `{dsId, url[], lastSynctime}`）就是记录 DataService 注册信息的数据结构——自我注册的本质是自动化写入这条记录。

**用户决策**：**不新增任何 REST 端点**。复用 Inventory Service 现有的 `PUT /` 端点（`handleUpdate` → `PutData` → `Db.Replace`，本身就是幂等 upsert），只需把 `EditableTypes` 放开给 `inventory` 类型。**referral 同步不在本功能范围内**，仍由 admin `sync` 手工维护。

> **POST 已评估，维持 PUT**：POST 语义为"创建"，但 InvSrv 现不支持 POST（405）；且 `Db.Replace` 以 `__id` 为键，同 DS 重复注册只会覆盖同一条记录，PUT 本身已满足"不重复注册"。POST create-only 反而在 DS 重启地址变化时不更新记录 → 陈旧 URL。

**分支**：`feature-20060814-self-reg-ds`（已建，尚无本功能代码）。

## 设计概览

1. **Inventory Service：放开现有 PUT 端点** —— `EditableTypes` 增加 `inventory`，使现有 `PUT /` 能 upsert 该类记录。**不新增路由、不新增 handler 方法**。
2. **DS 启动后后台 goroutine 自我注册** —— 先 GET `{inv.url}/inventory/{ds.id}` 查询自身记录：已注册且 `url` 一致则跳过；未注册或不一致才构造自己的 `inventory` 记录（完整 Record 信封，含 dsId + url[]）PUT 到 `{inv.url}/`。查询/PUT 失败每 10s 重试（与 `WaitForDataHandler` 节奏一致）。
3. **复用最大化** —— 写入复用现有 `PutData`/`Db.Replace`；记录结构复用 `InvRecord.DataServiceInfo`；DS 侧复用 `Http.SubmitPayload`。

## 变更文件清单

| 文件 | 动作 |
|------|------|
| `plan/feature-ds-self-ref/plan.md` | 新建 — 保存本计划 |
| `src/InventoryService/DataHandler/dataHandler.go` | 修改 — `EditableTypes` 增加 `inventory` |
| `src/DataService/Config/config.go` | 修改 — 新增 `DsConfig{id, instanceId, urls}` 于 `Configuration.Ds`；`InvConfig` 仅保留 `url` |
| `src/DataService/DataServer/selfRegister.go` | 新建 — 后台注册 goroutine + payload 构造 |
| `src/DataService/DataServer/dataServer.go` | 修改 — `Run()` 中 `RunJournalHandler()` 之后触发 `StartSelfRegistration()` |
| `docker-compose/2data1inv*/DataService0X/config.json` | 修改 — `http.dns` 设为 compose 服务名、`urls` 加 localhost 别名 |

> 相比初版计划删除的内容：`dsRegister.go`、`dsSync.go`、`inventoryServer.go` POST 路由、admin 工具重构——全部不再需要。

## 关键实现细节

### 1. `InventoryService/DataHandler/dataHandler.go`（修改）

`EditableTypes`（当前仅 `SchemaPath.PathName`）增加一项：

```go
var EditableTypes = map[string]bool{
    SchemaPath.PathName: true,
    Schema.Inventory:    true,   // DS 自我注册写入 inventory 记录
}
```

- 现有 `PutData` 已对任意 EditableTypes 做：`Record.LoadMap` 校验信封 → `Db.CreateTable` → `Db.Replace(Table, {DataId: id}, record.Map())`，即**幂等 upsert**，无需改动。
- 副作用：`DeleteData` 同样放开 `inventory` 的删除（可做注销，可选用途）。
- （可选）对 `inventory` 的 payload 用 `InvRecord.CreateDsInfo(record.Data)` 做结构校验。

### 2. `DataService/Config/config.go`（修改）

**DsId/urls 归属本 DataService 自身，故放入 `Configuration.Ds`（`DsConfig`），不放在 `inventory` 下**：

```go
// DsConfig 描述本 DataService 自身身份与对外可访问地址
type DsConfig struct {
    Id   string   `json:"id"`   // DsId，注册身份；首次部署为空时自动生成并回写 config
    Urls []string `json:"urls"` // 本 DataService 可被 Inventory Service 访问的 URL 列表
}

type Confuguration struct {
    // ...
    Ds  DsConfig   `json:"ds"`        // 本 DataService 身份/地址
    Inv InvConfig  `json:"inventory"` // 所连接的 Inventory Service
    // ...
}

type InvConfig struct {
    Url string `json:"url"` // 所连接的 Inventory Service；非空即启用自我注册
}
```
`Register` 默认 false，存量部署不受影响。

**dsId 自动生成**（`StartSelfRegistration()` 内）：
- 若 `config.Ds.Id == ""`，用 `uuid.NewString()` 生成（复用 `github.com/google/uuid`，`DataService/go.mod` 已有该依赖），并通过 `Config.Write` 回写 config.json——保证重启后身份稳定，且与 `DataInit` 回写 `Initialized` 的既有模式一致。
- 与 `-id` flag 的关系：`-id` 仍是服务器标识（日志文件名等），保持不变；注册身份改为使用 `config.Ds.Id`。

### 3. `DataService/DataServer/selfRegister.go`（新）

```go
func (srv *Server) StartSelfRegistration()          // 确保 dsId 存在；InvLinked() 为 false 则直接返回；否则 go srv.selfRegister()
func (srv *Server) selfRegister()                   // 重试循环：先查询、一致则跳过、否则 PUT
func (srv *Server) checkRegistration() (bool, error) // 查询现有 inventory 记录，判断已注册且一致
func (srv *Server) dsUrls() []string                // 广播 URL 列表：config.Ds.Urls 优先，否则按 http.dns:port 推导
func (srv *Server) buildRegisterRecord() map[string]interface{}
func sameUrlList(a, b []string) bool                // 集合相等比较，忽略顺序
```

- `StartSelfRegistration`：
  1. `InvLinked()` 为 false → log 并 return（无 Inventory 可注册）。
  2. 若 `config.Ds.Id == ""` → `uuid.NewString()` 生成，`Config.Write` 回写 config.json。
  3. `go srv.selfRegister()`。
- `selfRegister` 循环（**先查询后注册，避免同 DS 重复注册**）：
  1. `GET {Inv.Url}/inventory/{ds.id}` 查询当前记录（`checkRegistration`）。
  2. 记录已存在且 `url` 与当前 `ds.urls` 集合一致 → 已注册，直接 return（跳过 PUT）。
  3. 记录不存在（404）或 url 不一致 → `PUT {Inv.Url}/` upsert 自己的 inventory 记录。
  4. 查询失败（Inventory 不可达）或 PUT 失败 → log 后 `time.Sleep(10 * time.Second)` 重试。
- `buildRegisterRecord`：
  - `dsInfo := InvRecord.DataServiceInfo{Id: srv.config.Ds.Id, URL: srv.dsUrls()}`
  - `dsMap, _ := Json.CopyToMap(dsInfo)`
  - `record := Record.NewRecord(Schema.Inventory, InvRecord.LatestVer, srv.config.Ds.Id, dsMap)`
  - 返回 `record.Map()`（完整信封 `{__type, __id, __ver, data}`）。
- **不调 `/ds/sync`**（无此端点）。

### 4. `DataService/DataServer/dataServer.go`（修改）

`Run()`（`RunJournalHandler()` 之后）：
```go
srv.RunJournalHandler()
if srv.config.Inv.Register {
    srv.StartSelfRegistration()
}
srv.RunHttp()
```

### 5. compose 配置

- `docker-compose/2data1inv/DataService01|02/config.json`（及 Mongo 版）：
  ```json
  "http": { "dns": "unitao-data-service01", "port": "80" },
  "ds": { "id": "DataService01", "urls": ["http://localhost:8001"] },
  "inventory": { "url": "http://unitao-inv-service" }
  ```
  `ds.id` 可不配（首次启动自动生成 UUID 并回写），也可显式指定（如 `"DataService01"`）。`ds.urls` 为 InvSrv 可达的地址列表（如宿主机 localhost 别名）。
  - **关键**：`http.dns` 必须是 InvSrv 能访问到的名字（compose 服务名）。DS 的 `ListenAndServe` 绑 `:port`，改 dns 不影响监听。
- Mongo/Dynamo 版是本次功能最大受益者（目前无预置 inventory 文件、无脚本可创建）。
- 预置的 `InventoryService/data/inventory/DataService01|02` 文件可保留（PUT upsert 会覆盖）。

## 复用点汇总

| 新代码 | 复用 |
|--------|------|
| `EditableTypes` 改动 | 现有 `PutData`/`DeleteData`/`Db.Replace` 全部沿用，零 handler 改动 |
| `buildRegisterRecord` | `InvRecord.DataServiceInfo`、`Record.NewRecord`、`Json.CopyToMap` |
| `StartSelfRegistration` | `srv.data.Inventory.InvLinked()`、`Config.Write`、`uuid.NewString()`、`Http.SubmitPayload` |

## 幂等与重试

- **服务端**：`PutData` → `Db.Replace` 以 `__id` 为 key upsert —— DS 重启安全，无重复记录。
- **客户端**：先 `GET /inventory/{ds.id}` 查询——已注册且 `url` 一致则跳过 PUT（同 DS 不重复注册）；未注册(404)或不一致才 PUT。查询失败或 PUT 失败 10s 重试；`Db.Replace` upsert 保证重试安全。

## 范围界定

- **inventory 记录（DS 自我身份注册）**：本功能自动化，DS 启动后 PUT 自己的 `inventory` 记录。
- **referral 记录（类型→DS 归属）**：**不在本功能范围内**，仍由 `InventoryServiceAdmin sync` 手工维护（用户决策）。后续如需完全免运维，可作为独立功能（DS 推送自己的 referral / InvSrv 提供同步端点）。

## 验证

### 构建
```bash
cd /opt/src/github/UniTAO-dev
go build ./app/DataService ./app/InventoryService ./tool/InventoryServiceAdmin
```

### 本地端到端（sysdirfile InvSrv + Dynamo-local DS）
1. 起 DynamoDB Local；DS 用 `extConfig.json`（`dns: localhost`）：
   `go run ./app/DataService -id DataService01 -config docker-compose/2data1inv/DataService01/extConfig.json -log /tmp/ds01.log`
2. 起 Inventory：`go run ./app/InventoryService -config docker-compose/2data1inv/InventoryService/config/config.json -port 8004 -log /tmp/inv.log`
3. 等 ~10-20s 后验证：
   - `curl http://localhost:8004/inventory` → 出现 `DataService01`
   - `curl http://localhost:8004/inventory/DataService01` → `data.url` 含 `http://localhost:8001`
4. 幂等：重启 DS01，`inventory/DataService01` 仍只有一条，url 列表完整。
5. 恢复：先停 InvSrv，起新 DS03，看日志重试，再起 InvSrv，~10s 内 DS03 出现在 inventory。

### Compose 端到端
```bash
cd docker-compose/2data1inv && docker compose up -d --build
sleep 30
curl http://localhost:8004/inventory
docker logs unitao-data-service01 | grep -i register
```

### Admin CLI 回归
```bash
go run ./tool/InventoryServiceAdmin sync -config docker-compose/2data1inv/InventoryService/config/config.json
```

## 实施顺序

1. 保存计划至 `plan/feature-ds-self-ref/plan.md`
2. `dataHandler.go`：`EditableTypes` 增加 `inventory` + `go build`
3. DS config + `selfRegister.go` + `Run()` hook
4. compose 配置更新
5. 验证（构建 / 本地端到端 / compose / CLI 回归）

## 风险与注意

- **可达性**：广播的 URL 必须 InvSrv 可达（compose 里需 `http.dns`=compose 服务名，`urls` 字段承载宿主机别名）。`InvRecord.GetUrl()` 会逐个探测，多 URL 时多一次 1s GET。
- **未认证写入**：放开 `EditableTypes` 后，`PUT /` 接受任意调用方写入 `inventory` 记录，与现有 REST 层同一信任模型。
- **信任边界扩大**：`EditableTypes` 放开意味着普通 PUT 也能改 `inventory`，比原来只允许 pathname 面更广；如后续需要可用 `Http.ParseHeaders` 的 header/token 加闸。
