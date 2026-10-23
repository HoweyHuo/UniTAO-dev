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

**A Data Service never writes `config.json`; its runtime state lives in `state.json` next to it.** Three values are produced by the DS itself and must survive restarts. `Config.LoadState` / `State.Save` (`src/DataService/Config/state.go`) read and write them in `state.json` in the config directory:

- `initialized` — the database has been initialized. `InitDatabase` returns immediately when it is true (`src/DataService/DataInit/init.go`). To create tables and import the meta-schema again, set `initialized` to `false` in `state.json`; **do not delete the whole file**, or `instanceId` is lost too. `force-init` only takes effect while `initialized` is false.
- `instanceId` — the auto-generated instance UUID that tells "the same DS re-registering" apart from "a different DS with a clashing name". **Lose it and the Inventory treats this DS as a clashing one** and assigns it a new, suffixed ds.id.
- `dsId` / `dsIdFrom` — the ds.id generated when `ds.id` is empty in config, or the one the Inventory assigned on a name clash. It applies only while config's `ds.id` still equals `dsIdFrom`; edit `ds.id` in config and the old value is dropped.

**Migration**: older versions wrote these values back into `config.json`. When `state.json` does not exist, `LoadState` seeds it from `initialized` / `ds.instanceId` in config and writes it out right away; after that those two config fields have no effect. So for a local `config.json` dirtied by an older version, **start the new version once first** to create `state.json`, then `git checkout` the file. If you added skip-worktree earlier, undo it first with `git update-index --no-skip-worktree <file>`.

Compose bind-mounts `./DataService01` at `/opt/UniTAO/config`, so `state.json` shows up in the working tree; `.gitignore` already ignores `docker-compose/**/state.json`. The InventoryService does not write its config either.

### Go (all targets)
```bash
# Build Data Service binary
go build ./app/DataService

# Build Inventory Service binary
go build ./app/InventoryService

# Build admin tools
go build ./tool/DataServiceAdmin
go build ./tool/InventoryServiceAdmin

# Build all (each app / tool is its own module; `./app/...` cannot span modules, so list them)
go build ./app/DataService ./app/InventoryService ./tool/DataServiceAdmin ./tool/InventoryServiceAdmin
```

### Build scripts with per-region GOPROXY

`scripts/build-cn.sh` and `scripts/build-us.sh` only set `GOPROXY` and call the shared `scripts/build.sh`, which builds the four targets above and runs the unit tests that need no external services; pass `--all` to run everything under `test/src`.

```bash
./scripts/build-cn.sh        # in China: GOPROXY=https://goproxy.cn,direct
./scripts/build-us.sh        # in the US: GOPROXY=https://goproxy.io,direct
./scripts/build-us.sh --all
```

From the US, goproxy.cn, proxy.golang.org and golang.org may all be unreachable (`golang.org/x/*` cannot be fetched with `GOPROXY=direct` either), which shows up as `TLS handshake timeout`. That is a proxy problem, not a code problem; switch to goproxy.io. `GOPROXY` is set only inside the script process and does not change your shell.

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

The tag rules for what CI publishes to `ghcr.io/howeyhuo/unitao-dev` (when `latest` / `<version>` / `sha-<short>` each advance, and why an un-bumped version tag is not a rollback target), plus the digest behind each tag, are documented in `docker/README.md`.

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
   - **Depends on the referral table**: a type is referenceable only after the Inventory Service sync has registered it. The sync runs at startup, on new-Data-Service events, and periodically (`sync.intervalSec`, 300s by default), so a freshly registered schema is not immediately referenceable; trigger it manually with `InventoryServiceAdmin sync`. Referral entries live in the Inventory Service's own database (`Db.Replace` in `dataSync.go`) and **survive restarts**, so the wait only occurs the first time a type is introduced — restarting either service does not re-introduce it.

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

**A schema cannot be PUT or PATCHed; it is upgraded by POSTing a new version.** PUT (`Handler.Set` rejects every `InternalTypes` entry) and PATCH (`Handler.Patch` blocks `schema` explicitly) on `/schema/...` both return 400 `method[...] on type[schema] is not allowed`. To change a schema, `POST /` a schema record with the same id and a higher `version`: `Handler.Add` archives the current version under its archived id, then writes the new one. An equal or lower version is rejected, and an archived id cannot be POSTed directly. This keeps data written under an older version tied to its schema. `Handler.Patch` cannot block all `InternalTypes`, because journal processing PATCHes `CmtIdx` through it (possibly across DSs over HTTP).

A Data Service registers itself with the Inventory Service via `PUT /` at startup (`DataServer/selfRegister.go`); there is no `POST /ds/register` endpoint on the Inventory side.

### Go module layout

The workspace uses `replace` directives (or local `go.work` use) to link modules. Internal packages use short module names (e.g., `module Data`, `module DataService`, `module InventoryService`) while libraries use the canonical `github.com/salesforce/UniTAO/lib/...` path. App/tool entrypoints use `github.com/salesforce/UniTAO/app/...` / `UniTao/DataServiceAdmin` etc. via the go.work `use` directive.

All Go source targets Go 1.18 (libs) to 1.24 (Data module).
