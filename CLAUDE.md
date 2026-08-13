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

- **Data Service** — 数据节点，负责具体数据的 CRUD 操作、Schema 校验、变更日志记录和逐记录锁。每个 Data Service 实例连接一个数据库（DynamoDB、MongoDB 或本地文件），管理自己域内的数据。它是"存数据"的地方。
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
     "__ver": 1,
     "data": {
       "hostname": "web-01.example.com",
       "ip": "10.0.1.10",
       "rack": { "__ref__": { "__type": "Rack", "__id": "rack-a1" } }
     }
   }
   ```

3. **JSON Schema 扩展**：对 JSON Schema 的两项自定义扩展：
   - `contentMediaType: "inventory/{type}"` — 标记字段引用由 Inventory Service 管理的另一类型的数据
   - `indexTemplate` — 在被引用的 Record 创建时自动填充注册/反向引用属性

4. **可插拔数据层**：`src/Data/DbIface.Database` 接口，具有 DynamoDB、MongoDB 和基于文件的实现。工厂函数在 `src/Data/data.go` 中根据配置切换。

5. **DataServiceProxy**：Inventory Service 内部通过 `DataServiceProxy` 将跨 DS 的引用查询代理到目标 Data Service。上层应用提交查询后，Inventory Service 根据引用的 `__ref__` 记录自动路由到正确的 DS 实例。

6. **变更日志**：数据变更被记录日志，用于审计跟踪和跨服务同步。

### REST API 概览

Data Service 和 Inventory Service 均通过 HTTP JSON API 对外暴露。

| 服务 | 方法 | 端点 | 说明 |
|------|------|------|------|
| Data Service | `POST` | `/data/{type}` | 创建 Record |
| Data Service | `GET` | `/data/{type}/{id}` | 查询 Record |
| Data Service | `PUT` | `/data/{type}/{id}` | 替换 Record |
| Data Service | `DELETE` | `/data/{type}/{id}` | 删除 Record |
| Data Service | `PATCH` | `/data/{type}/{id}` | 部分更新 Record |
| Data Service | `GET` | `/schema/{type}` | 查询已注册的 Schema |
| Inventory Service | `GET` | `/inv/{type}/{id}` | 跨 DS 查询 Record |
| Inventory Service | `GET` | `/inv/schemas` | 列出所有已同步的 Schema |
| Inventory Service | `POST` | `/ds/register` | 注册新的 Data Service |
| Inventory Service | `POST` | `/ds/sync` | 同步所有 DS 的 Schema |

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
    DataJournal/               # 数据变更日志跟踪
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
```

### 辅助目录

```
dbSchemas/                     # 数据库表 Schema 定义（DynamoDB、MongoDB）
demo/                          # 分步演示脚本（PowerShell + Python）
docker/                        # Docker 构建脚本及各服务的 dockerfile
docker-compose/                # Docker Compose 环境配置
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
- **Docker** — 运行演示环境（docker-compose）或构建镜像
- **DynamoDB Local**（可选）— 本地运行 Data Service 的默认数据库；也可以使用 MongoDB 或文件模式（`SysDirFile`，适合测试）
- **Node.js**（可选）— 运行 `ui/WebServer/` 或 `schemaVisualizer/`

快速启动演示环境（自动拉起 DynamoDB Local + 两个 Data Service + 一个 Inventory Service）：
```bash
docker compose -f docker-compose/2data1inv/docker-compose.yml up -d
```

### Go 构建（所有目标）
```bash
# 构建 Data Service 二进制
go build ./app/DataService

# 构建 Inventory Service 二进制
go build ./app/InventoryService

# 构建管理工具
go build ./tool/DataServiceAdmin
go build ./tool/InventoryServiceAdmin

# 构建全部
go build ./app/...
```

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
```

### Docker
```bash
# 构建所有 Docker 镜像
./docker/buildAll.sh          # Linux/Mac
./docker/buildAll.ps1         # Windows

# 单镜像（一体化）
docker build -f ./docker/unitao/dockerfile -t unitao:latest .

# 启动演示环境
docker compose -f docker-compose/2data1inv/docker-compose.yml up -d

# CI 构建（GitHub Actions 推送到 ghcr.io）
```

