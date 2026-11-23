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

## Project Architecture

UniTAO is a schema-driven, multi-node heterogeneous infrastructure inventory system. Data is defined by JSON Schema; the services provide CRUD plus cross-node reference queries, and new data types need no code.

### Data Service and Inventory Service

UniTAO is made of two kinds of service:

- **Data Service** — the data node. It owns CRUD for concrete data, schema validation, post-write processing (Journal), and per-record locks. Each Data Service instance connects to one database (DynamoDB, MongoDB, or local files) and manages its own domain of data. It is where data is *stored*.
- **Inventory Service** — the aggregation/query node. It registers multiple Data Services, syncs their schemas, and provides cross-Data-Service reference queries. Upper-layer applications only ever talk to the Inventory Service and do not need to know how many Data Services the data is spread over. It is where data is *queried*.

This split brings a few key advantages:
- **Data Services scale horizontally** — each DS owns its own data domain, independently of the others
- **The Inventory Service gives a unified view**, hiding the storage details underneath
- **Cross-DS references** are expressed with the `contentMediaType: "inventory/{type}"` extension, which the Inventory Service resolves and routes

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

   `__id`, `__type` and `__ver` **must not contain whitespace** (spaces, tabs, newlines, ...). `Record.ValidateEnvelope` checks this; the Data Service calls it on write through `SchemaOps.ValidateRecord`, and the Inventory's `PUT /` calls it in `PutData`. A violation returns 400 `record field [__id]=[a b] must not contain whitespace`. Attribute names and values inside `data` are not restricted by this rule.

3. **JSON Schema extensions**: Two custom extensions on JSON Schema.

   **`contentMediaType: "inventory/{type}"`** — marks a field as referencing data of another type managed by the Inventory Service. Key points:

   - **Only the `inventory/` prefix is accepted.** Standard JSON Schema values (`json`, `application/json`, `text/plain`) and bare type names (`actor`) are rejected during schema preprocess — `[contentMediaType]=[json] not supported` — so the schema cannot even be registered. The value is split on the first `/`, so `application/json` is reported as `[application]`.
   - **Only on `type: "string"` attributes** (`SchemaDoc.IsCmtRef`); an array of references is a string array under `items`.
   - **Validated on write**: before saving, the target record is looked up through the Inventory Service; if missing, the write fails with 400 `reference inventory:{type} with value=[{id}] does not exists`. **The referenced record must therefore exist first**, and two records referencing each other deadlock when both fields are required (attributes are **required by default** — omit `required` and it is required). Set at least one side to `"required": false` and fill the value in afterwards.
   - **Depends on the referral table**: a type is referenceable only after the Inventory Service sync has registered it. The sync runs at startup, on new-DS registration events, and periodically (`sync.intervalSec`, 300 s by default), so a freshly registered schema is not immediately referenceable; trigger it manually with `InventoryServiceAdmin sync`. Referral records live in the Inventory Service's own database (`Db.Replace` in `dataSync.go`) and **survive restarts**, so the wait only occurs the first time a type is introduced — restarting either service does not re-introduce it.

   See the contentMediaType section of `README.md` for full usage and the demo examples.

   **`indexTemplate`** — auto-populates registry/back-reference attributes when a referenced record is created

4. **Pluggable data layer**: `src/Data/DbIface.Database` interface with DynamoDB, MongoDB, and file-based implementations. The factory in `src/Data/data.go` switches by config. The optional interface `DbIface.Scanner` reads the records of one type one by one (a cursor in MongoDB, page by page in DynamoDB); `DbIface.Scan` falls back to `Get` when a database does not implement it. DynamoDB's `Query` pages through `LastEvaluatedKey` until done (a single Query returns at most 1MB).

5. **DataServiceProxy**: inside the Inventory Service, `DataServiceProxy` proxies cross-DS reference queries to the target Data Service. An upper-layer application submits a query, and the Inventory Service routes it to the right DS instance according to the reference's `__ref__` record.

6. **Journal (change queue)**: an asynchronous task queue inside the Data Service that runs the side effects of writes. **It is not an audit log.**
   - After every successful add / delete / update / PATCH, `DataHandler` calls `AddJournal(type, id, before, after)`, stored in pages per `(type, id)` in the DS's own database.
   - `JournalHandler` starts a `JournalWorker` per `(type, id)`, which hands the entries in order to the `JournalProcess`es registered by `LoadProcesses`. Order is guaranteed only within one record; there is no global sequence.
   - When every entry of a page is processed and the head moves forward, the page is deleted (`JournalHeadForward` → `removeJournal`); no history is kept.
   - Two built-in processes, both serving `indexTemplate`: `SchemaChanges` registers or withdraws a subscription in the referenced type's `cmtIdx` (through the Inventory) when a schema is registered or deleted; `CmtIndexChanges` fills or cleans the back-reference index on referenced records when data changes, possibly PATCHing across DSs. A shared vDS additionally registers `Share.ChannelPublish` (after the built-in processes), which writes changes to the outbox for publishing. A DS linked to an Inventory (config has `inventory.url`) also registers `InvReport.Reporter` on every vDS that has a Journal (also in the extra position, mounted by `setupJournal`), notifying the record change subscription system of each change's record position (best-effort, failures are only logged; see item 8). mirrored vDS have no Journal; `Share.Mirror` reports their changes itself (see item 8).
   - To add new post-write processing (e.g. pushing changes out), implement a `JournalProcess` and register it in `LoadProcesses`. But the Journal is deleted once processed and cannot be used for replay or audit.

7. **Virtual Data Service (vDS)**: one DS instance hosts several vDS. Each vDS has its own table, schemas, Journal and locks, and registers in the Inventory as a separate DS.
   - `local` (table `table.data`): this network's data, not shared. Without a `vds` section in config it is the only one.
   - `shared/{name}` (table `{table.data}_shared_{name}`): published to other networks through a Communication Channel, writable. It may only reference records inside the same shared vDS (`Handler.RefLocalOnly`).
   - `mirrored/{name}` (table `{table.data}_mirrored_{name}`): a read-only copy of another network's shared (`Handler.ReadOnly`, writes return 405), without a Journal.
   - A record's identity is `(vDS, __type, __id)`; local and each shared keep their own copy of the schemas, never shared.
   - Code: `DataServer/vds.go` (per-vDS init, removal, `/{vDS}/config`), `Share/` (outbox, the `ChannelPublish` JournalProcess, the `Publisher`, the mirror syncer `Mirror`), `Channel/` (`dir` / `git` channels, manifest, source identity `SourceId`), `DataHandler/apply.go` (direct writes on the mirror side, skipping reference validation).
   - A channel is a low-frequency, single-publisher queue. A vDS's address on a channel is `repo + branch + folder` (for dir: `path + folder`; `folder` defaults to the vDS name and may be multi-level). One repository can hold several vDS; vDS in the same DS with the same `workDir` share one clone (`sharedGitRepo`, must be the same repo + branch). The folder holds `manifest.json` (version 4: instance, epoch, lastSeq, queue, histories with each file's sha256, and which requests each history is granted to, `HistoryRef.Grants`), `queue/` incremental batches (`.jsonl.gz`), `history/<hid>/<n>.jsonl.gz` parts of full histories, and `subscribers/<id>.json` with each mirror's own progress (request, `subscribe|active`, cursor, epoch).
   - **Channels are neither encrypted nor signed**; confidentiality and read/write permissions are left to the channel platform (git repository permissions, dir file permissions); DSs exchange no keys. Identities: source = channel address (`Channel.SourceId`: `SHA256:` + sha256 of the normalized repo (`NormalizeRepo`: scp / ssh / https forms, user names and the `.git` suffix are unified) + branch + folder; for dir, `dir:` + path + folder). A shared and its mirrors compute the same value, used as the Inventory `instanceId`. The folder owner = the manifest's `instance` (the DS's `state.json` `instanceId`, generated by `setupVds` when missing). Subscriber id = a random id a mirror generates on first start (`Channel.NewSubscriberId`, stored in `state.json` `vds[path].subscriberId`); there is no whitelist. The old `signingKeyFile`, `trustedFingerprint`, `channel.encrypt` / `decrypt` are ignored (`Ignored()`, logged as a warning, `ignoredConfig` in `/config`).
   - Publishing: write → Journal → outbox (gap-free seq; the record is written before the counter advances) → `Publisher.PublishPending` publishes queue batches of consecutive seqs. Before publishing it reconciles with the channel (`reconcileLocked`): when the manifest was written by another instance (taken over after a rebuild, logged as `take it over`), is in an old format, is missing from the channel, the channel is behind, or the channel is ahead beyond the outbox's committed seq (local state lost / restored from an old backup), it `Reset`s to the local seq; when it is ahead only within the committed range (crash after publishing, before saving), it catches up. Every `Reset` writes a new manifest with a random **epoch**; a mirror that sees the epoch change re-subscribes, and the publisher marks active subscribers whose progress belongs to an old epoch as `gap` — each check is sufficient on its own. The publisher writes the manifest right after start, without waiting for the first data, so subscribers can find the source. `Maintain` provides histories for subscribe requests (`provideHistory`: the newest history within `historyReuseHours` (default 24) only gets a grant added to the manifest, `GrantHistory`, without rewriting parts; otherwise `PublishHistory` packs a new one shared by all new requests of that round), judges subscribers dead (the first unprocessed batch, a history granted to it but not loaded, or the progress file's `updated` older than `subscriberTimeoutHours` (default 24) while in gap state: its progress file is deleted; `Prune` checks before deleting that the file is still the one judged dead, and keeps it if the subscriber wrote progress since; a caught-up active subscriber is never judged dead by `updated`, as an idle mirror writes no progress), deletes batches every live subscriber has processed, and histories no grant is still waiting for. `CompactIfDue` squashes the git branch into one commit every `compactIntervalHours` (default 24) (`--force-with-lease`) and runs gc locally.
   - History parts: split by gzipped size (`DefaultHistoryPartBytes` 32MB, below GitHub's 100MB per-file limit), each part an independent gzip JSONL. Both sides stream: the publisher reads through `Handler.EachSyncedRecord` (schemas read at once, each type's data through `DbIface.Scan`) while compressing; parts are staged in the system temp directory (`TMPDIR`, not inside the channel working directory, copied again when a rejected push is redone), with at most one part in memory. The mirror reads part by part with `HistoryReader.Each`, and `ClearSynced` only collects record keys.
   - Mirroring: `Mirror` first registers a subscription (also possible before the source has published), replaces its whole table with the history granted to it, then consumes the queue from the history's seq and reports progress. Local request/loaded/cursor live in `state.json`; the progress file is only a report. It re-subscribes when its progress file was deleted, the request does not match, the cursor is no longer on the channel (`ErrGap`), the epoch changed, or local state was lost, and keeps serving the old data while waiting for the new history.
   - git over SSH uses an identity the DS generates itself: at startup `setupSSHIdentity` generates (or reuses) an ed25519 private key `id_ed25519` (OpenSSH format, 0600, `Channel/sshkey.go`) under config's `ssh.dir` (default `ssh/` next to the config; relative paths are relative to the config directory). The public key goes into `state.json` `ssh.publicKey` / `ssh.fingerprint` and `instance.ssh` of `GET /{vDS}/config`, for a human to add to the git server. git uses it through `GIT_SSH_COMMAND` (`StrictHostKeyChecking=accept-new`, host keys in `ssh/known_hosts`; `ConnectTimeout=30`, `ServerAliveInterval=15`/`ServerAliveCountMax=4`, so a connection stuck halfway drops within about a minute); a `GIT_SSH_COMMAND` already set in the environment wins. ssh needs the running UID to have an entry in `/etc/passwd`: the image's entrypoint `docker/unitao/entrypoint.sh` adds one for a non-root UID at container start (`/etc/passwd` is 666 in the image, which has no setuid programs, so this grants no privilege), so `passwd` no longer needs to be mounted. The private key is never mounted from outside, so the directory must be writable and persistent; container deployments set `ssh.dir` to `/opt/UniTAO/ssh` and mount a Docker named volume, keeping the key out of the host's bind-mounted directories (the image creates the directory as 1777, a new named volume inherits it, so non-root UIDs can write).
   - Every git command has an overall timeout (`DefaultGitTimeout` 10 minutes, `gitRepo.run`); on timeout the whole process group, including the ssh / git-remote-https git started, is killed (`procgroup_unix.go`), otherwise a connection stuck halfway would leave git hanging forever, holding the working directory's lock.
   - git writes: fetch → reset to the remote → write only its own files → commit → push; a rejected push (`errRetry`) is redone from the start (`fileStore.write`), no rebase. A mirror also writes to the channel (progress), so it needs write access.
   - Runtime state lives under `vds` in `state.json` (each vDS's `initialized`, `table`, the id the Inventory assigned, the mirror cursor). Removing a mirrored from config and restarting drops its table and unregisters its URL from the Inventory: the URLs to unregister are first recorded in `state.json` `unregister`, and at startup they are unregistered before the current vDS register (otherwise a new name after an alias change would be merged into the old record because the source identity is the same). For a mirror renamed on a name clash, configure an `alias` to make the DS path match the Inventory dsId; all replicas of the same source must use the same alias.
   - Backup and rebuild: channels need no backup. A shared rebuilt from scratch with the same config takes the folder over automatically (new epoch); mirrors keep their config, re-subscribe and replace their data wholesale. A rebuilt mirror re-subscribes with a new subscriber id, and its old progress file is deleted after the timeout. Two running instances configured with the same folder take it over from each other every round.

8. **Record change subscription (report / subscription, push model)**: Data Services notify the Inventory of record changes as **record positions** (`{ds, type, id}`, no content, no operation), and the Inventory pushes them to the endpoints subscribers provide. Events are **best-effort dirty markers**: "pushes speed up convergence, reconciliation guarantees correctness" — the Inventory is the source of truth, a subscriber re-reads the record on receiving a position (`GET /{type}/{ds}/{id}`, 404 = deleted), and missed events are made up by the subscriber's own periodic reconciliation. Code: `DataService/InvReport/` (DS-side notification process), `InventoryService/Subscription/` (registry + push fan-out + lease sweep); the routes are in `InventoryServer` (intercepted before the generic method dispatch).
   - **Reporting**: every vDS with a Journal (local + shared) turns journal entries into `{dsId, changes: [{type, id}]}` through `InvReport.Reporter` and `POST {inventory.url}/report` (5 s timeout per request). **Failures are dropped**: dsId not assigned, Inventory unreachable, non-2xx are only logged and return nil — the journal entry is archived as usual, **no retry, the head is not held** (a report is a notification, not post-write processing; an Inventory outage does not slow down other JournalProcesses). DS-internal types such as schema are not reported; a DS not linked to an Inventory (no `inventory.url` in config) does not mount it. op/ver/ts are never sent: op is derived by re-reading (present = current state, 404 = deleted), ver/time are replaced by reconciliation (the DS side no longer has `normalizeTs`).
   - **mirrored vDS reporting**: mirrored vDS have no Journal, so `Share.Mirror` reports to **its own network's** Inventory itself (`setupMirrored` calls `SetReporter` when the DS links an Inventory; the dsId is the mirrored vDS's own `InvId`). After each batch `syncOnce` applies and `saveState`, it deduplicates the batch's `{type, id}`, removes `Common.InternalTypes` (`InvReport.FilterChanges`) and sends one `ProcessChanges`. After `load(history)` replaces the whole table and `saveState`, it sends only a single **reset signal** (`ProcessReset`): `{"dsId", "changes": [], "reset": true}`, not one entry per record (a history can be large and would flood the dispatch queue). Queue batches consumed after the history in the same round are reported per batch as usual. Failures are dropped here too. Reports from the shared source only reach the source network's Inventory and cannot replace the mirror side.
   - **Push**: the Inventory receives a report → validates it, stamps `ds` with the dsId → puts it on a bounded dispatch queue (capacity 1024; when full the whole batch is dropped and logged — dropping is an explicit design choice in a best-effort channel) → returns 202 `{accepted}` immediately (accepted ≠ delivered). The dispatch goroutine filters by the registry (empty `types`/`ds` = no filter), then **one goroutine per subscriber with a 5 s timeout** POSTs `{"changes": [{ds, type, id}...], "resets": ["{ds}"...]}` to its endpoint (`Ingest` allows empty `changes` when `reset` is true; `resets` lists DSs replaced wholesale, filtered only by the subscription's `ds`, **not by `types`**, and left out when there is no reset; when only resets are pushed, `changes` is an empty array `[]`, not null; both fields are new semantics, older subscribers can simply ignore `resets`). Forward and forget: nothing records what was pushed. A failed push is **never retried and reported to no one**; it only marks the subscriber record `unreachable` + `lastError` (throttled writes, at least 60 s apart); a successful push clears the mark (one write). Failures never disable pushing: the next event is pushed as usual.
   - **Registry**: the `subscription` table in the Inventory's own database. subscription is a **schema-described Inventory internal type** (in the same group as inventory / referral, `DataHandler.InvTypes`): its structure is defined by `Subscription.SchemaRecord`, self-documented through `GET /schema/subscription`, and registration payloads are validated by `lib/Schema` first (structure, types, unknown fields); semantic constraints the schema dialect cannot express, such as http(s) URLs and the `ttlSec` minimum, stay in Go. Adding a field = a new schema version. The record `__id` is a hash of the endpoint (`subId`: `sub-` + the first 16 hex digits of sha256; record ids must not contain `/`), the endpoint itself is in `data`, and `POST /subscription` register/heartbeat is an **idempotent upsert** (refreshes `registered`, updates `types`/`ds`/`ttlSec`, keeps `created` and the health marks — a heartbeat only proves the subscriber is alive, not that its endpoint is reachable). Subscribers declare a lease `ttlSec` (default 86400, minimum 60). **After an Inventory restart pushing resumes with zero wait** (the registry is persistent; heartbeats are only a GC signal).
   - **Sweep** (`sweepLoop`, `subscription.sweepIntervalSec`, default 60 s): deletes subscriptions without a heartbeat for longer than their TTL — pure garbage collection judged by the lease the subscriber declared itself; a wrong judgement only costs a re-registration on the next heartbeat. **Death is judged only by missing heartbeats, never by failed pushes**: a subscriber that is alive with a broken endpoint stays in the table, its `unreachable` mark visible to operators (`GET /subscription`). Records whose `Registered` cannot be parsed are skipped, not deleted (bad metadata does not destroy a registration; the next heartbeat rewrites it). Deletion always uses the record's real `__id` (the runtime view carries `recordId`), never a recomputed `subId(endpoint)`. **Migration of records from older versions** (run by `Sweep` every round and once at Inventory start, idempotent): records without an endpoint (the pull model, PR #70, created a new one on every registration, without endpoint or `registered`, so they can neither heartbeat nor unregister) are deleted; records whose `__id` is not `subId(endpoint)` (the first push-model version, PR #75, used the raw endpoint as `__id`) are re-saved under the hashed id (keeping the newer record if one with the same endpoint already exists) and the old record is deleted. Until migrated these records take no part in pushing, so the same endpoint is not pushed twice.
   - **Subscriber contract** (correctness rests entirely on this; the staleness bound = the reconciliation period): ① Periodic reconciliation — `GET /{type}?ver=1` returns a `{ds, id, ver}` list to diff against local state: id mismatch = added/deleted, ver mismatch = schema version upgrade. **ver is the record's schema version (`__ver`) and does not change when the content is edited**, so reconciliation only reveals format upgrades, never content edits; edits are found through pushes (on a `resets` entry, treat the whole DS as possibly modified), and subscribers that need a stronger guarantee re-read the content themselves (e.g. compare content fingerprints) (the DS side provides the versions through `GET /{vDS}/{type}?ver=1`; a list without `?ver=1` is still a `{ds}/{id}` string array, so older consumers are unaffected). ② Bootstrap order: register → full query → receive pushes; the overlap between pushes and the query snapshot is naturally idempotent (a position triggers a re-read, which returns the current state). ③ After a restart or downtime → reconcile once (there is no gap signal at all; it is pure self-discipline); pushes cannot tell "quiet" from "stopped", and heartbeats self-heal an Inventory restart.

### REST API Overview

Both the Data Service and the Inventory Service expose an HTTP JSON API.

Data Service URLs are `/{vDS}/{type}[/{id}]` (`{vDS}` is `local`, `shared/{name}` or `mirrored/{name}`); the old unprefixed form is not supported. Inventory Service URLs are `/{type}[/{ds}/{id}]`. Neither has a `/data` or `/inv` prefix, and there are no management endpoints like `/ds/...`.

| Service | Method | Endpoint | Description |
|------|------|------|------|
| Data Service | `GET` | `/` | List vDS paths; `/shared`, `/mirrored` list the names in that group |
| Data Service | `GET` | `/{vDS}` | `["config", "schema", <data types>]` |
| Data Service | `GET` | `/{vDS}/config` | The vDS's settings and status (kind, table, channel, publish/sync progress), no secrets; credentials in a git channel's `repo` address are shown as `***` (`Channel.RedactRepo`) |
| Data Service | `POST` | `/{vDS}` | Create a Record; the type comes from `__type` in the body |
| Data Service | `GET` | `/{vDS}/{type}` | List all `__id` of that type |
| Data Service | `GET` | `/{vDS}/{type}/{id}[/{attrPath}]` | Query a Record, optionally deep into an attribute path |
| Data Service | `PUT` | `/{vDS}/{type}/{id}` | Replace a Record |
| Data Service | `PATCH` | `/{vDS}/{type}/{id}[/{attrPath}]` | Partially update a Record; the body is a JSON value (send a string as `"abc"`), and is taken as raw text when it is not valid JSON |
| Data Service | `DELETE` | `/{vDS}/{type}/{id}` | Delete a Record |
| Data Service | `GET` | `/{vDS}/schema[/{type}]` | List / query registered Schemas |
| Inventory Service | `GET` | `/{type}` | List the ids of that type on all DSs, **always** as `{ds}/{id}`; `?ver=1` returns `[{ds, id, ver}]` instead (for subscriber reconciliation) |
| Inventory Service | `GET` | `/{type}/{ds}[/{id}[/{attrPath}]]` | Cross-DS Record query, ds required; when expanding references, look in the starting record's vDS first, then globally |
| Inventory Service | `GET` | `/schema` | List all synced types |
| Inventory Service | `GET` | `/referral[/{type}]` | Query the type-to-Data-Service mapping |
| Inventory Service | `PUT` | `/` | Register / update a Data Service (body is an `inventory` Record) |
| Inventory Service | `POST` | `/referral` | A Data Service reports a type change event, triggering sync |
| Inventory Service | `DELETE` | `/{type}/{id}` | Delete a local Inventory record (e.g. `inventory/{ds-id}`) |
| Inventory Service | `DELETE` | `/inventory/{dsId}?url={url}` | Remove only one replica URL; the whole record is deleted when no URL remains |
| Inventory Service | `POST` | `/report` | A DS reports record positions (`{dsId, changes: [{type, id}], reset?}`, see "Record change subscription"; `reset: true` means the DS was replaced wholesale, `changes` may then be empty, and subscribers receive it as `resets: [dsId]`); queued, returns 202 `{accepted}` immediately; accepted ≠ delivered |
| Inventory Service | `POST` | `/subscription` | Register / heartbeat a subscription (`{endpoint, name?, types?, ds?, ttlSec?}`, idempotent upsert per endpoint); the Inventory pushes change positions to the endpoint |
| Inventory Service | `GET` | `/subscription` | List all subscriptions: each item is a record envelope `{__id, __type, __ver, data}`, with the subscription fields (including the `unreachable`/`lastError` health marks) in `data` |
| Inventory Service | `GET` | `/schema/subscription` | The schema of subscription records |
| Inventory Service | `DELETE` | `/subscription?url={endpoint}` | Unsubscribe (idempotent) |

**Registration of shared / mirrored vDS in the Inventory** (`allocateSignedDsId`): they register with name + source identity (`instanceId` = `SourceId`, i.e. the channel address). Same name and same source (mirror replicas, a shared and its own mirror) merge their URLs into one record, keeping kind shared when a shared is present; a later arrival with the same name but a different source is renamed `{name}-{first 8 chars of the source id}`; the same source under a different name merges into the existing record; `pinnedSources` pins a name to a source id. On sync, URLs are ordered by each replica's `/config` cursor (the shared source first). When the same type and version of a schema differs in content between DSs, the later DS is left out of that type's referral (D6).

The Data Service path query engine also supports `?schema` (show the schema at the current path), `?flat` (show only the current layer), and `?iterator` (list the options at each leaf), plus `[*]` to wildcard an array/map index.

**A schema cannot be PUT or PATCHed; it is upgraded by POSTing a new version.** PUT (`Handler.Set` rejects every `InternalTypes` entry) and PATCH (`Handler.Patch` blocks `schema` explicitly) on `/schema/...` both return 400 `method[...] on type[schema] is not allowed`. To change a schema, `POST /` a schema record with the same id and a higher `version`: `Handler.Add` archives the current version under its archived id, then writes the new one. An equal or lower version is rejected, and an archived id cannot be POSTed directly. This keeps data written under an older version tied to its schema. `Handler.Patch` cannot block all `InternalTypes`, because journal processing PATCHes `CmtIdx` through it (possibly across DSs over HTTP).

At startup a Data Service registers each vDS with the Inventory Service via `PUT /` (`DataServer/selfRegister.go`), with URL `{ds.urls}/{vDS path}`, and re-checks every `SelfRegCheckSec`; the Inventory side neither needs nor has a `POST /ds/register` endpoint.

### Network security model and access control

The Inventory system (the Inventory Service and every Data Service) **must be deployed entirely inside a protected internal network and must not be reachable from outside**. The Inventory system therefore **trusts the other systems on the internal network**:

- **Current development stage: no access restrictions at all.** There is no authentication/authorization; both services' REST APIs **allow CORS from any origin** (`Access-Control-Allow-Origin: *`, `OPTIONS` preflight returns 204 directly), so internal UIs (e.g. a schema designer / dashboard on another port) can call them straight from the browser.
- **Production stage: restrict access with a whitelist**, allowing only listed internal systems. Access control relies on the whitelist, not on CORS; CORS stays fully open.

Implementation: the `Http.Cors` middleware in `lib/Util/Http/cors.go` wraps both services' root handler (`http.HandleFunc("/", Http.Cors(srv.handler))`). **Do not** set `Access-Control-Allow-Origin` in a service config's `http.headers` — that value only overrides the `*` on normal responses, preflight responses stay `*`, and browsers get inconsistent results. Do not add authentication or origin checks in code unless implementing the production whitelist.

### Go workspace (go.work)

```
lib/                           # Shared libraries (no main)
  Schema/                      # JSON Schema engine, Record type, SchemaDoc parsing
  SchemaPath/                  # Schema path traversal & query engine
  Util/                        # HTTP client, JSON utils, template engine, thread control, HashLock

src/                           # Source packages (no main)
  Data/                        # Data layer — pluggable DB interface + implementations
    DbIface/                   # Database interface (Get, Create, Replace, Delete, ...)
    DbDynamoDb/                # DynamoDB implementation
    Mongodb/                   # MongoDB implementation
    SysDirFile/                # File-system implementation (for testing/dev)
    DbConfig/                  # DB connection configuration types
  DataService/                 # Data Service core — CRUD handler, Journal, locks, config, HTTP server
    DataHandler/               # Record validation, CRUD operations, cross-DS proxy
    DataServer/                # HTTP server with REST endpoint routing
    DataJournal/               # Asynchronous post-write processing queue (drives indexTemplate / cmtIdx / shared publishing)
    Channel/                   # Communication Channel: a queue on dir / git (manifest v4, queue, history parts, subscribers, epoch), not encrypted or signed
    Share/                     # shared publishing (outbox + Publisher) and mirrored sync (Mirror)
    DataLock/                  # Per-record locking for concurrent access
  InventoryService/            # Inventory Service core — cross-DS query routing
    DataHandler/               # DS registration, schema sync, cross-ref queries
    InventoryServer/           # HTTP server
    InvRecord/                 # Data Service info record type
    RefRecord/                 # Referral/cross-reference record type

app/                           # Executable entrypoints
  DataService/                 # main.go — runs the Data Service server
  InventoryService/            # main.go — runs the Inventory Service server

tool/                          # Admin CLI tools
  DataServiceAdmin/            # DB table creation & data import
  InventoryServiceAdmin/       # DS registration, schema synchronization

test/                          # Integration & unit tests
  src/
    SchemaTest/                # Schema engine unit tests
    SchemaPathTest/            # Path query engine tests
    UtilTest/                  # Utility tests
    DataServiceTest/           # Integration tests (require running services); subdirectory InvReportTest/ has unit tests of the DS-side notification process of record change subscription (payload shape, failures never block the journal, journal integration)
    InventoryServiceTest/      # Inventory-side unit tests of record change subscription (registration validation and idempotent heartbeat, push fan-out and filters, failure marks without retry, lease sweep, legacy record migration, subscription schema)
    VdsTest/                   # vDS / Channel tests: DS + Inventory in-process (in-memory MemDb), including git channels; subscription_test.go is the subscription system end to end (report → push → failure mark/recovery → no backfill after an outage → reconciliation list ?ver=1), mirror_report_test.go covers mirrored vDS reporting (reset signal + increments)
```

### Supporting directories

```
dbSchemas/                     # DB table schema definitions (DynamoDB, MongoDB)
demo/                          # Step-by-step demo scripts (PowerShell + Python)
docker/                        # Docker build scripts: the single image docker/unitao (published as unitao-dev) + UIServer
docker-compose/                # Deployment: scripts in script/ copy the templates in template/ (mongodb, dataservice, inventory) into a directory, generate config and start them; see docker-compose/README.md (Chinese version README.zh.md, keep both in sync)
  script/                      # docker-deploy.sh (installs Docker Engine on Ubuntu), mongo_deploy.sh, unitao_deploy.sh (interactively deploys one DS or Inventory instance into a subdirectory of a UniTAO directory; install the Inventory first on a new system; a DS starts with only the local vDS), unitao_vds.sh (interactively adds / removes shared / mirrored vDS, maintaining the keys under config/vds/<kind>/<name>/)
  template/                    # mongodb/, dataservice/, inventory/: each a compose project template, not run in place
ui/WebServer/                  # Node.js static file server (for dashboard)
schemaVisualizer/              # Express.js schema visualizer frontend server
javascript/Schema/             # JavaScript port of the Schema library
.docker/                       # Docker metadata
```

### Go module layout

The workspace uses `replace` directives (or local `go.work` use) to link modules. Internal packages use short module names (e.g., `module Data`, `module DataService`, `module InventoryService`) while libraries use the canonical `github.com/salesforce/UniTAO/lib/...` path. App/tool entrypoints use `github.com/salesforce/UniTAO/app/...` / `UniTao/DataServiceAdmin` etc. via the go.work `use` directive.

All Go source targets Go 1.18 (libs) to 1.24 (Data module).

## Build & Run Commands

### Prerequisites

The following are needed before you start:

- **Go 1.18+** (some modules need 1.24) — to build all Go binaries
- **Docker** — to deploy (docker-compose/) or build images
- **DynamoDB Local** (optional) — the default database for running a Data Service locally; MongoDB or file mode (`SysDirFile`, good for testing) also work
- **Node.js** (optional) — to run `ui/WebServer/` or `schemaVisualizer/`

Deploy with Docker (MongoDB in its own directory; one UniTAO system per directory, its Inventory and Data Services in subdirectories, each a compose project; the Inventory first; see `docker-compose/README.md`):
```bash
docker-compose/script/mongo_deploy.sh ~/infra/mongodb
docker-compose/script/unitao_deploy.sh inventory ~/infra/unitao      # first the Inventory -> ~/infra/unitao/inv
docker-compose/script/unitao_deploy.sh dataservice ~/infra/unitao    # then a Data Service -> ~/infra/unitao/ds
docker-compose/script/unitao_vds.sh ~/infra/unitao/ds          # optional: shared / mirrored vDS
```

**A Data Service never writes `config.json`; its runtime state lives in `state.json` next to it.** Three values are produced by the DS itself and must survive restarts. `Config.LoadState` / `State.Save` (`src/DataService/Config/state.go`) read and write them in `state.json` in the config directory:

- `initialized` — the database has been initialized. `InitDatabase` returns immediately when it is true (`src/DataService/DataInit/init.go`). To create tables and import the meta-schema again, set `initialized` to `false` in `state.json`; **do not delete the whole file**, or `instanceId` is lost too. `force-init` only takes effect while `initialized` is false.
- `instanceId` — the auto-generated instance UUID that tells "the same DS re-registering" apart from "a different DS with a clashing name". **Lose it and the Inventory treats this DS as a clashing one** and assigns it a new, suffixed ds.id.
- `dsId` / `dsIdFrom` — the ds.id generated when `ds.id` is empty in config, or the one the Inventory assigned on a name clash. It applies only while config's `ds.id` still equals `dsIdFrom`; edit `ds.id` in config and the old value is dropped.

**Migration**: older versions wrote these values back into `config.json`. When `state.json` does not exist, `LoadState` seeds it from `initialized` / `ds.instanceId` in config and writes it out right away; after that those two config fields have no effect. So for a local `config.json` dirtied by an older version, **start the new version once first** to create `state.json`, then `git checkout` the file. If you added skip-worktree earlier, undo it first with `git update-index --no-skip-worktree <file>`.

An instance deployed by `unitao_deploy.sh` bind-mounts `config/` of its target directory at `/opt/UniTAO/config`, so `state.json` is written there (the templates are not run in place in the repository). The InventoryService does not write its config either.

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

`scripts/build-cn.sh` and `scripts/build-us.sh` only set `GOPROXY`, then call the shared `scripts/build.sh`. `build.sh` builds the four targets above and runs the unit tests that need no external services; add `--all` to run every test under `test/src`.

```bash
./scripts/build-cn.sh        # in China: GOPROXY=https://goproxy.cn,direct
./scripts/build-us.sh        # in the US: GOPROXY=https://goproxy.io,direct
./scripts/build-us.sh --all
```

In the US, goproxy.cn, proxy.golang.org and golang.org may all be unreachable (`golang.org/x/*` cannot be fetched with `GOPROXY=direct` either), showing up as `TLS handshake timeout`. That is a proxy problem, not a code problem; switch to goproxy.io. `GOPROXY` only applies inside the script process and does not change your shell environment.

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

# Record change subscription (no external dependencies at all; MemDb / httptest)
# DS-side notification process unit tests: HandleType matrix, payload carries positions only (no op/ver/ts),
# failures never block the journal (entries are archived as usual), reset payload and FilterChanges
go test ./test/src/DataServiceTest/InvReportTest/...
# Inventory-side unit tests: registration validation and idempotent heartbeat (same-endpoint upsert, ttl clamping),
# push fan-out and filters, failure marks (unreachable + lastError) without retry, success clears the mark,
# lease sweep (expired TTL deleted, heartbeat keeps alive),
# reset pushes (TestIngestReset / TestDispatchResetFilters: filtered by ds, not by types, changes is [])
go test ./test/src/InventoryServiceTest/...
# End to end (DS + Inventory in-process, through the real HTTP routes):
# TestReportPushFanout (report → push position → update/delete same shape), TestSubscriptionFilters (types filter),
# TestPushFailureAnnotationAndRecovery (failure mark → no retry → recovery clears the mark), TestRegisterHeartbeatUnregister,
# TestSubscriberLeaseSweep (TTL sweep), TestNoBackfillWhenInventoryComesBack (notifications dropped during an outage are not backfilled),
# TestListWithVer (reconciliation list ?ver=1), TestMirrorReportsResetAndChanges (shared / mirrored each linked to its own Inventory:
# the first history load pushes a single resets, later batches push deduplicated changes)
go test ./test/src/VdsTest/... -run 'TestReport|TestSubscription|TestPush|TestRegister|TestSubscriber|TestNoBackfill|TestListWithVer|TestMirrorReports'

# Run a single test function
go test ./test/src/SchemaTest/... -run TestSchemaOps
go test ./test/src/DataServiceTest/... -run TestDataHandler

# Tests that need a real DynamoDB (e.g. TestDynamoDbPaging: Get / Scan must page through a type larger than 1MB)
# starts a throwaway in-memory DynamoDB Local container (default 127.0.0.1:18000), removed afterwards; needs Docker
./scripts/test-dynamodb.sh

# Tests that need a real MongoDB (e.g. TestMongoDbObjectId: reads never return MongoDB's _id)
# starts a throwaway MongoDB container with its data in tmpfs (default 127.0.0.1:27018), removed afterwards; needs Docker
./scripts/test-mongodb.sh
```

Tests that need a real DynamoDB / MongoDB read `UNITAO_TEST_DYNAMODB_ENDPOINT` and `UNITAO_TEST_MONGODB_ENDPOINT` respectively (MongoDB also reads `UNITAO_TEST_MONGODB_USER` / `UNITAO_TEST_MONGODB_PASSWORD`) and skip when unset, so `go test ./test/src/...` and `build.sh --all` still need no external services. Pointing them at an existing instance works too: each run uses tables with fresh names (a fresh database for MongoDB) and drops them at the end.

The MongoDB backend strips the document's internal primary key `_id` with a projection on reads (`MongoDb.ObjectIdField`) and ignores an `_id` sent back by clients on writes, so it never appears in a Record.

### Docker
```bash
# Build all Docker images
./docker/buildAll.sh          # Linux/Mac
./docker/buildAll.ps1         # Windows

# Single image (all-in-one)
docker build -f ./docker/unitao/dockerfile -t unitao:latest .

# Deploy (see docker-compose/README.md)
docker-compose/script/unitao_deploy.sh dataservice ~/infra/unitao

# Build image via CI (GitHub Actions pushes to ghcr.io)
```

The tag rules for what CI publishes to `ghcr.io/howeyhuo/unitao-dev` (when `latest` / `<version>` / `sha-<short>` each advance, and why an un-bumped version tag is not a rollback target), plus the commands to look up the image digest behind each tag, are documented in `docker/README.md`.
