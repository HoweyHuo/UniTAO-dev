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

3. **JSON Schema extensions**: Two custom extensions on JSON Schema:
   - `contentMediaType: "inventory/{type}"` — marks a field as referencing data of another type managed by Inventory Service
   - `indexTemplate` — auto-populates registry/back-reference attributes when a referenced record is created

4. **Pluggable data layer**: `src/Data/DbIface.Database` interface with DynamoDB, MongoDB, and file-based implementations. The factory in `src/Data/data.go` switches by config.

5. **Data Service + Inventory Service**: Data Services handle local CRUD. The Inventory Service aggregates schema registrations from multiple Data Services and enables cross-service data references via the `DataServiceProxy`.

6. **Journaling**: Data changes are journaled for audit trail and cross-service synchronization.

### Go module layout

The workspace uses `replace` directives (or local `go.work` use) to link modules. Internal packages use short module names (e.g., `module Data`, `module DataService`, `module InventoryService`) while libraries use the canonical `github.com/salesforce/UniTAO/lib/...` path. App/tool entrypoints use `github.com/salesforce/UniTAO/app/...` / `UniTao/DataServiceAdmin` etc. via the go.work `use` directive.

All Go source targets Go 1.18 (libs) to 1.24 (Data module).
