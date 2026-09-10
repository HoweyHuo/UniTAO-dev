# CLAUDE.en.md

> English version of CLAUDE.md. Keep in sync with the Chinese master file (CLAUDE.md).

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What is UniTAO?

UniTAO is a **Universal Inventory system**.

### The problem it solves

Traditional inventory systems hardcode data models for specific scenarios, leading to:
- Adding a new asset type requires code changes and redeployment
- Cross-system data structures are hard to unify, understand, and query
- Relationships between data (references, aggregations) are not intuitive

### UniTAO's approach

**Everything is driven by Schema (data model).** The structure, validation rules, and relationships between data types are all defined as JSON Schema records stored in the database. The system makes no assumptions about data models — it dynamically interprets data based on its Schema.

This means:
- Adding a new data type only requires defining a new Schema — no code changes
- Data structures are self-documenting; the Schema itself is the most accurate description of the data
- Cross-Data-Service data references and queries are supported out of the box, making data relationships clear

> More design philosophy and problem statements will be added over time.

## Build & Run Commands

### Prerequisites

The following are needed before you start:

- **Go 1.18+** (some modules need 1.24) — to build all Go binaries
- **Docker** — to run the demo environment (docker-compose) or build images
- **DynamoDB Local** (optional) — the default database for running a Data Service locally; MongoDB or file mode (`SysDirFile`, good for testing) also work
- **Node.js** (optional) — to run `ui/WebServer/` or `schemaVisualizer/`

Quick start for the demo environment (brings up DynamoDB Local + two Data Services + one Inventory Service):
```bash
docker compose -f docker-compose/2data1inv/docker-compose.yml up -d
```

**Note: a Data Service rewrites its own config file.** At startup, `Config.Write` (`src/DataService/Config/config.go`) serializes the whole configuration struct back to `config.json`, so `docker-compose/**/DataService01/config.json` goes dirty after the first start. What it writes is runtime state, not configuration:

- `initialized: true` — the database has been initialized. **This field must not be committed**: `InitDatabase` returns immediately when it is true (`src/DataService/DataInit/init.go`), so once committed, a fresh clone would skip table creation and base meta-schema import.
- `ds.instanceId` — the auto-generated instance UUID, used to tell "same DS re-registering" apart from "a different DS colliding on the name".
- The extra empty `dynamodb` / `sysdirfile` blocks, the reordered fields, and the lost trailing newline all come from the same write-back: `MarshalIndent` emits every field of the struct (zero values included), in struct-definition order.

These files are bind-mounted into the container (compose mounts `./DataService01` at `/opt/UniTAO/config`), so the write-back lands directly in the working tree. Use skip-worktree to make git ignore that local modification:

```bash
git update-index --skip-worktree docker-compose/data_inv/DataService01/config.json
```

The flag lives in the local index only, so **a fresh clone must run it again**; undo it with `--no-skip-worktree`. If upstream genuinely changes this file, drop the flag before pulling, then re-add it once the change has landed. The InventoryService does not rewrite its config — only Data Services need this.

### Go (all targets)
```bash
# Build Data Service binary
go build ./app/DataService

# Build Inventory Service binary
go build ./app/InventoryService

# Build admin tools
go build ./tool/DataServiceAdmin
go build ./tool/InventoryServiceAdmin

# Build all
go build ./app/...
```

### Run services
```bash
# Data Service (requires DB connection)
go run ./app/DataService -id <service-id> -config <config.json>

# Inventory Service
go run ./app/InventoryService -config <config.json>

# Admin: initialize DB tables
go run ./tool/DataServiceAdmin table -config <config.json> -table <tables.json>

# Admin: import data
go run ./tool/DataServiceAdmin data -config <config.json> -table <table> -data <data.json>

# Inventory Admin: register a Data Service
go run ./tool/InventoryServiceAdmin add -config <config.json> -ds <url> -id <ds-id>

# Inventory Admin: sync schemas across Data Services
go run ./tool/InventoryServiceAdmin sync -config <config.json>
```

### Tests
```bash
# Run all test modules
go test ./test/src/...

# Schema unit tests (no external dependencies)
go test ./test/src/SchemaTest/...
go test ./test/src/SchemaPathTest/...
go test ./test/src/UtilTest/...

# DataService integration tests (requires running DynamoDB + services)
go test ./test/src/DataServiceTest/...

# Run a single test function
go test ./test/src/SchemaTest/... -run TestSchemaOps
go test ./test/src/DataServiceTest/... -run TestDataHandler
```

### Docker
```bash
# Build all Docker images
./docker/buildAll.sh          # Linux/Mac
./docker/buildAll.ps1         # Windows

# Single image (all-in-one)
docker build -f ./docker/unitao/dockerfile -t unitao:latest .

# Bring up demo environment
docker compose -f docker-compose/2data1inv/docker-compose.yml up -d

# Build image via CI (GitHub Actions pushes to ghcr.io)
```

## Project Architecture

UniTAO is a schema-driven, multi-node heterogeneous infrastructure inventory system. Data is JSON-schema defined; services provide CRUD + cross-reference queries with zero coding for new data types.

### Data Service and Inventory Service

UniTAO is made of two kinds of service:

- **Data Service** — the data node. It owns CRUD for concrete data, schema validation, change journaling, and per-record locks. Each Data Service instance connects to one database (DynamoDB, MongoDB, or local files) and manages its own domain of data. It is where data is *stored*.
- **Inventory Service** — the aggregation/query node. It registers multiple Data Services, syncs their schemas, and provides cross-Data-Service reference queries. Upper-layer applications only ever talk to the Inventory Service and do not need to know how many Data Services the data is spread over. It is where data is *queried*.

This split brings a few key advantages:
- **Data Services scale horizontally** — each DS owns its own data domain, independently of the others
- **The Inventory Service gives a unified view**, hiding the storage details underneath
- **Cross-DS references** are expressed with the `contentMediaType: "inventory/{type}"` extension, which the Inventory Service resolves and routes

### Go workspace (go.work)

```
lib/                           # Shared libraries (no main)
  Schema/                      # JSON schema engine, Record type, SchemaDoc parsing
  SchemaPath/                  # Schema path traversal & query engine
  Util/                        # HTTP client, JSON utils, Template engine, Thread control, HashLock

src/                           # Source packages (no main)
  Data/                        # Data layer — pluggable DB interface + implementations
    DbIface/                   # Database interface (Get, Create, Replace, Delete, ...)
    DbDynamoDb/                # DynamoDB implementation
    Mongodb/                   # MongoDB implementation
    SysDirFile/                # File-system implementation (for testing/dev)
    DbConfig/                  # DB connection configuration types
  DataService/                 # Data Service core — CRUD handler, Journal, Lock, Config, HTTP server
    DataHandler/               # Record validation, CRUD operations, cross-DS proxy
    DataServer/                # HTTP server with REST endpoint routing
    DataJournal/               # Journaling for data change tracking
    DataLock/                  # Per-record locking for concurrent access
  InventoryService/            # Inventory Service core — cross-DS query routing
    DataHandler/               # DS registration, schema sync, cross-ref queries
    InventoryServer/           # HTTP server
    InvRecord/                 # Data Service info record type
    RefRecord/                 # Referral/cross-reference record type

app/                           # Executable entrypoints
  DataService/                 # main.go — runs Data Service server
  InventoryService/            # main.go — runs Inventory Service server

tool/                          # Admin CLI tools
  DataServiceAdmin/            # DB table creation & data import
  InventoryServiceAdmin/       # DS registration, schema synchronization

test/                          # Integration & unit tests
  src/
    SchemaTest/                # Schema engine unit tests
    SchemaPathTest/            # Path query engine tests
    UtilTest/                  # Utility tests
    DataServiceTest/           # Integration tests (require running services)
```

### Supporting directories

```
dbSchemas/                     # DB table schema definitions (DynamoDB, MongoDB)
demo/                          # Step-by-step demo scripts (PowerShell + Python)
docker/                        # Docker build scripts & dockerfiles per service
docker-compose/                # Docker Compose environment configs
ui/WebServer/                  # Node.js static file server (for dashboard)
schemaVisualizer/              # Express.js schema visualizer frontend server
javascript/Schema/             # JavaScript port of schema library (jsonSchema.js, schema.js, record.js)
.docker/                       # Docker metadata
```

### Key architectural concepts

1. **Schema-of-schema**: The meta-schema is defined in `lib/Schema/data/schema.json`. All data types are defined as JSON Schema records stored in the database, making the system fully extensible without code changes.

2. **Record format**: All data is stored as Records with envelope fields:
   - `__type` — schema/data type name
   - `__id` — record identifier
   - `__ver` — schema version
   - `data` — the payload (validated against the type's schema)

   Example:

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

   `__ver` is a `xxx.xxx.xxx` version string (at least three numeric parts), not an integer. A cross-type reference stores the target record's `__id` string directly — there is no extra reference wrapper structure.

3. **JSON Schema extensions**: Two custom extensions on JSON Schema.

   **`contentMediaType: "inventory/{type}"`** — marks a field as referencing data of another type managed by Inventory Service. Key points:

   - **Only the `inventory/` prefix is accepted.** Standard JSON Schema values (`json`, `application/json`, `text/plain`) and bare type names (`actor`) are rejected during schema preprocess — `[contentMediaType]=[json] not supported` — so the schema cannot even be registered. The value is split on the first `/`, so `application/json` is reported as `[application]`.
   - **Only on `type: "string"` attributes** (`SchemaDoc.IsCmtRef`); an array of references is a string array under `items`.
   - **Validated on write**: before saving, the target record is looked up through the Inventory Service; if missing, the write fails with 400 `reference inventory:{type} with value=[{id}] does not exists`. The referenced record must therefore exist first, and two records referencing each other deadlock when both fields are required (attributes are **required by default** — omit `required` and it is required). Set at least one side to `"required": false` and PATCH the value in afterwards.
   - **Depends on the referral table**: a type is referenceable only after the Inventory Service sync has registered it. The sync runs at startup, on new-Data-Service events, and periodically (`sync.intervalSec`, 300s by default), so a freshly registered schema is not immediately referenceable; trigger it manually with `InventoryServiceAdmin sync`.

   See the contentMediaType section of `README.md` for full usage and the demo examples.

   **`indexTemplate`** — auto-populates registry/back-reference attributes when a referenced record is created

4. **Pluggable data layer**: `src/Data/DbIface.Database` interface with DynamoDB, MongoDB, and file-based implementations. The factory in `src/Data/data.go` switches by config.

5. **Data Service + Inventory Service**: Data Services handle local CRUD. The Inventory Service aggregates schema registrations from multiple Data Services and enables cross-service data references via the `DataServiceProxy`.

6. **Journaling**: Data changes are journaled for audit trail and cross-service synchronization.

### REST API Overview

Both the Data Service and the Inventory Service expose an HTTP JSON API.

Both services route as `/{type}[/{id}]` — there is **no `/data` or `/inv` prefix**, and no `/ds/...` admin endpoints.

| Service | Method | Endpoint | Description |
|---------|--------|----------|-------------|
| Data Service | `POST` | `/` | Create a Record; the type comes from `__type` in the body |
| Data Service | `GET` | `/{type}` | List all `__id` of that type |
| Data Service | `GET` | `/{type}/{id}[/{attrPath}]` | Query a Record, optionally deep into an attribute path |
| Data Service | `PUT` | `/{type}/{id}` | Replace a Record |
| Data Service | `PATCH` | `/{type}/{id}[/{attrPath}]` | Partially update a Record |
| Data Service | `DELETE` | `/{type}/{id}` | Delete a Record |
| Data Service | `GET` | `/schema[/{type}]` | List / query registered Schemas |
| Inventory Service | `GET` | `/{type}[/{id}]` | Cross-DS Record query; routes automatically and expands references |
| Inventory Service | `GET` | `/schema` | List all synced types |
| Inventory Service | `GET` | `/referral[/{type}]` | Query the type-to-Data-Service mapping |
| Inventory Service | `PUT` | `/` | Register / update a Data Service (body is an `inventory` Record) |
| Inventory Service | `POST` | `/referral` | A Data Service reports a type change event, triggering sync |
| Inventory Service | `DELETE` | `/{type}/{id}` | Delete a local Inventory record (e.g. `inventory/{ds-id}`) |

The Data Service path query engine also supports `?schema` (show the schema at the current path), `?flat` (show only the current layer), and `?iterator` (list the options at each leaf), plus `[*]` to wildcard an array/map index.

A Data Service registers itself with the Inventory Service via `PUT /` at startup (`DataServer/selfRegister.go`); there is no `POST /ds/register` endpoint on the Inventory side.

### Go module layout

The workspace uses `replace` directives (or local `go.work` use) to link modules. Internal packages use short module names (e.g., `module Data`, `module DataService`, `module InventoryService`) while libraries use the canonical `github.com/salesforce/UniTAO/lib/...` path. App/tool entrypoints use `github.com/salesforce/UniTAO/app/...` / `UniTao/DataServiceAdmin` etc. via the go.work `use` directive.

All Go source targets Go 1.18 (libs) to 1.24 (Data module).
