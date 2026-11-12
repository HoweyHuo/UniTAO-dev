# UniTao

## Main Repo Moved To
https://github.com/TuringCompute/UniTAO

## NOTE from Author (Yi Huo):

Apparently, This opensource project is terminated by salesforce.

**All further development will be fully based on my own requirement now.**

**Thus, I decided to fork this project to my own Organization (TuringCompute) for further development**

https://github.com/TuringCompute

**The new repo will be a fork to this repo.**

https://github.com/TuringCompute/UniTAO

all future development will happens to that repo first. 

I will try to sync all future development back here, as long as I have the permission to do so.

## Description

UniTAO was originally created in 2022 by Shai Herzog & Yi Huo as an 
Universal No-Coding Heterogeneous Infrastructure Maintenance & Inventory system that is holistically driven by open/community-developed semantic models/schemas.

It is designed to be self-contained and self-documenting to serve as foundation for human-free automation of heterogeneous computing infrastructure.

The model/schema uses the standard JSON schema format as basis, with additional enhancements. Those can be found in the documentation. 

**schema of schema is defined in path:**
```
lib/Schema/data/schema.json
```
### Demo

#### **Docker**
We can build DataService and Inventory Service into docker container images. so we can run these in a docker environment for demo.

to build all images, run one of the following command based on your target environment.

```
# Powershell script for Windows environment
./docker/buildAll.ps1

# Bash script for Mac or Linux
./docker/buildAll.sh
```

these are local builds. the images published by CI to **ghcr.io** — their tags (`latest`, `<version>`, `sha-<short>`), how the tags move, and the digests behind them — are documented in [`docker/README.md`](./docker/README.md).

#### **Environment**
deploy MongoDB, then an Inventory Service and a Data Service into subdirectories of one UniTAO directory (the Inventory first), with the scripts in [`docker-compose/script`](./docker-compose/script) (they ask for the settings and start the instance):
```
docker-compose/script/mongo_deploy.sh ~/infra/mongodb
docker-compose/script/unitao_deploy.sh inventory ~/infra/unitao      # first the Inventory -> ~/infra/unitao/inv
docker-compose/script/unitao_deploy.sh dataservice ~/infra/unitao    # then a Data Service -> ~/infra/unitao/ds
```
shared / mirrored vDS between networks are added afterwards with `docker-compose/script/unitao_vds.sh`. see [`docker-compose/README.md`](./docker-compose/README.md).

#### **Run**
to run demo:
 - bring up demo environment. wait all instance running stable after multiple restart for data setup
 - login to demo folder
 ```
 ./demo
 ```
 - choose a demo folder and run demo script depending on demo environment.
 ```
 # Windows environment:
 demo.ps1

 # Mac/Linux environment
 demo.sh
 ```


## Components
 
### Data Layer
 - Supported Databases: DynamoDb, MongoDb
 - plugin-able data layer to support multiple types of database
 - Language: GoLang
 - sub folder: ./data


#### REST API Service
 - Language: GOLANG
 - Workspace: all project folders are included in go.work
 - install golang on MacOS
 ```
 brew install go
 ```
 - follow src/README.md to init and run the Inventory Service
 - build all Go targets and run the unit tests with the script for your region (add `--all` to run every test under `test/src`):
 ```
 # in China: GOPROXY=https://goproxy.cn,direct
 ./scripts/build-cn.sh

 # in the US: goproxy.cn and proxy.golang.org may be unreachable, use GOPROXY=https://goproxy.io,direct
 ./scripts/build-us.sh
 ```
 - a schema cannot be changed in place: **PUT** or **PATCH** on `schema` returns 400. to upgrade, **POST** a schema record with the same `__id` and a **strictly higher** `data.version` (equal or lower is rejected). the current version is archived automatically as `__id = <type>__<old version>`; archived ids cannot be POSTed directly, and records that declare the old `__ver` keep validating against the archived schema. the schema record's own `__ver` is the current version of the schema of schema (not `data.version`), while a data record's `__ver` is the version of its type's schema at write time. the same contract is documented on `GET /schema/schema`
 - the body of a **PATCH** on an attribute path is a JSON value: send `"rack one"` (quoted) for a string, `42` for a number, an object or array as usual. a body that is not valid JSON is taken as raw text, so the older unquoted form (`rack one`) still works, but a raw `123` or `true` is now a number / boolean, quote it to keep it a string

## Virtual Data Services (vDS)

one Data Service instance hosts several **virtual Data Services**. each vDS has its own table, schemas, journal and locks, and registers in the Inventory Service as a DS of its own:

| vDS | path | what it holds |
|---|---|---|
| `local` | `/local` | data of this network, never shared. the default when the config has no `vds` section |
| `shared/{name}` | `/shared/{name}` | data this network publishes to other networks through a Communication Channel. writable |
| `mirrored/{name}` | `/mirrored/{name}` | a read-only copy of another network's `shared/{name}`. writes return `405` |

records from different vDS never overwrite each other: the identity of a record is `(vDS, __type, __id)`. `local` and every `shared` keep their **own** schema copies, even when the content is identical.

### URLs

the Data Service URL is `/{vDS}/{__type}/{__id}` - there is no un-prefixed form any more.

```
GET  /                         # list vDS: ["local", "shared/prod", "mirrored/lab"]
GET  /shared                   # names of the shared vDS: ["prod"]
GET  /local                    # ["config", "schema", <data types>]
GET  /local/config             # settings and status of the vDS (kind, table, channel, publish/sync progress; no secrets)
POST /local                    # create a record, type taken from __type
GET  /local/VirtualMachine/vm-01
GET  /mirrored/lab/schema
```

the Inventory Service always qualifies data ids with the DS they live in, so the same `__type`/`__id` from different vDS stays apart:

```
GET http://localhost:8004/VirtualMachine                     # ["DataService01/vm-01", "prod/vm-01", ...]
GET http://localhost:8004/VirtualMachine/DataService01/vm-01 # the ds segment is required
GET http://localhost:8004/VirtualMachine/prod                # ids of one DS
```

when the Inventory walks a reference it looks in the vDS of the starting record first and falls back to a global lookup.

### Configuration

everything lives in the Data Service `config.json`:

```json
"vds": {
    "local": {},
    "shared": [{
        "name": "prod",
        "publishIntervalSec": 10,
        "subscriberTimeoutHours": 24,
        "compactIntervalHours": 24,
        "historyReuseHours": 24,
        "channel": {
            "type": "git", "repo": "git@git.internal:unitao/exchange.git", "branch": "infra", "folder": "dc1/prod",
            "workDir": "/opt/UniTAO/channel/exchange-infra"
        }
    }],
    "mirrored": [{
        "name": "lab",
        "alias": "lab-west",
        "pullIntervalSec": 60,
        "channel": { "type": "dir", "path": "/mnt/exchange" }
    }]
}
```

 - table names: `local` uses `table.data`; `shared/{name}` uses `{table.data}_shared_{name}`; `mirrored/{name}` uses `{table.data}_mirrored_{name}`.
 - removing a `mirrored` entry from the config and restarting **drops its table** and removes its URL from the Inventory. a removed `local` is only logged, its table is kept.
 - channel types: `dir` (a directory, e.g. a shared mount) and `git` (the `git` CLI must be installed). both sides write to the channel: the publisher its batches, every mirror its own progress file, so the mirror needs write access too.
 - a vDS's address on the channel is `repo + branch + folder` (`path + folder` for `dir`); `folder` defaults to the vDS name and may have several levels (`dc1/prod`). one repository can hold many vDS; putting related vDS on one branch lets a site pull them together. vDS in one Data Service that use the same `workDir` share one clone, so they must name the same repo and branch.
 - `subscriberTimeoutHours` (default 24): a mirror whose oldest unprocessed batch has been on the channel this long is considered dead, see below. `compactIntervalHours` (default 24): how often the publisher squashes the git branch history. `historyReuseHours` (default 24): how long after a history is published new subscribers are given it instead of a freshly packaged one, see below.
 - git authentication is left to git itself: the Data Service runs `git` non-interactively (`GIT_TERMINAL_PROMPT=0`) with its own environment, so use SSH or an HTTPS credential helper. for SSH the Data Service generates its own ed25519 key on first start (`id_ed25519` in `ssh.dir` of its config, default `ssh/` next to the config; in containers point it at a Docker named volume such as `/opt/UniTAO/ssh` so the private key stays out of bind-mounted host directories) and publishes the public key in `state.json` (`ssh.publicKey`) and `GET /{vDS}/config` (`instance.ssh`); add it to the git server. a `GIT_SSH_COMMAND` in the environment overrides it. every git command has a 10-minute timeout (the whole process group, ssh included, is killed), and the generated ssh command drops a stalled connection after about a minute (`ServerAliveInterval=15`, `ServerAliveCountMax=4`). the publisher additionally needs to force-push (history compaction); if the server forbids it, compaction fails and is logged, everything else keeps working. ssh needs an `/etc/passwd` entry for the UID the Data Service runs as; the image's entrypoint adds one for a non-root UID when the container starts. avoid credentials in `channel.repo`: they are redacted from `GET /{vDS}/config` and from git errors in the log, but stay in the config file and the clone's `.git/config`. see `docker-compose/README.md` for examples.

step-by-step onboarding of a shared vDS and its mirrors (git access, config, how to watch a subscription come up, troubleshooting): [`documentation/vds/README.md`](documentation/vds/README.md) (Chinese).

### Identity, access and rebuilds

the channel is neither encrypted nor signed: who can read and write it is up to the platform it lives on (access to the git repository, file permissions of a `dir` channel). keep the repository private; anyone who can read it can read the data.

 - **identity of a source**: its channel address. `SHA256:` + the sha256 of the normalized repository (`git@host:a/b.git`, `ssh://git@host/a/b` and `https://user@host/a/b.git` are the same), the branch and the folder; `dir:` + path + folder for a `dir` channel. a shared vDS and every mirror configured with the same address compute the same id (`instanceId` in `GET /{vDS}/config`), which is how the Inventory merges them.
 - **subscriber id**: a random id a mirror generates on first start and keeps in `state.json` (`vds.mirrored/{name}.subscriberId`); it names its progress file. there is no whitelist: the publisher serves every progress file in the folder.
 - **owner of a folder**: the manifest names the Data Service instance that writes it (`instance`, the instance id in `state.json`). a Data Service rebuilt from scratch (new `state.json`, empty database) with the same config finds another instance's manifest, logs `take it over` and restarts the folder in a new **epoch** from its own data; the same happens when a database restored from an older backup is behind what the channel already has. mirrors keep their config: they see the new epoch, subscribe again and replace their copy, serving the old one until then. if two running instances are configured with the same folder by mistake, they keep taking it from each other, logged on every round.
 - older configs still start: `signingKeyFile`, `trustedFingerprint`, `channel.encrypt` and `channel.decrypt` are ignored and listed in the log and in `ignoredConfig` of `GET /{vDS}/config`. the `keygen`, `fingerprint` and `enckey` commands of `DataServiceAdmin` are gone.

### How changes travel

the channel works like a low-frequency Kafka topic with one producer. each vDS folder holds:

```
<folder>/manifest.json                    # written by the publisher (version 4): instance, epoch, lastSeq, queue batches and
                                          # history parts with their sha256, which requests each history serves
<folder>/queue/<from>-<to>.jsonl.gz       # incremental batches
<folder>/history/<id>/<n>.jsonl.gz        # part n of a full history
<folder>/subscribers/<subscriber>.json    # written by each mirror: request, state (subscribe | active), cursor, epoch
```

 - **publish**: every write in a shared vDS goes through the journal into an outbox with a gap-free sequence number; the publisher pushes contiguous runs as queue batches. a change is always a full record upsert or a delete, so replaying is idempotent.
 - **subscribe**: a new mirror writes `state: subscribe` with a fresh request id. the publisher gives it a history: the newest one published within `historyReuseHours` if there is one (only the manifest records the new request, no data is rewritten), otherwise a new one with all current records at the published seq, shared by every request seen in that round. the mirror replaces its whole table with the history, reading it part by part, then consumes the queue from the history's seq and reports `state: active` with its cursor and epoch (kept locally in `state.json`, the progress file is only a report).
 - **history parts**: a history is split into parts of about 32 MB after gzip, well below the per-file limits of git hosts. both sides stream it: the publisher reads the records type by type (MongoDB cursor, DynamoDB page by page), compresses as it goes and spools finished parts in the system temp directory (`TMPDIR`), so it holds at most one part in memory; the mirror loads it part by part.
 - **prune**: a batch is deleted once every live subscriber's cursor has passed it; a history is deleted once none of the requests it serves is still waiting (every one is active, re-subscribed, dead or gone). with no subscribers the queue is emptied, new subscribers always start from a history.
 - **death**: if a subscriber's oldest unprocessed batch (or a history given to it and not loaded) is older than `subscriberTimeoutHours`, the publisher deletes its progress file and stops keeping batches for it. when the mirror comes back and finds its progress file gone (or its cursor no longer on the channel, or a new epoch), it subscribes again and replaces its data from a new history; it keeps serving the old data until then.
 - **git**: every write fetches the branch, resets to it, writes only its own files, commits and pushes; a rejected push is redone from the start. the publisher squashes the branch to one commit every `compactIntervalHours` (`--force-with-lease`, so a concurrent push is never lost) and garbage-collects its clone.
 - **epoch**: every restart of a folder (the channel was emptied or reset, an old-format manifest, another instance's manifest, a channel ahead of or behind the local outbox) writes a new manifest with a new random epoch at the publisher's current seq. a mirror's cursor only counts within its epoch, so a restarted folder whose seq happens to pass the old cursor is still noticed; the mirror subscribes again.
 - upgrading from manifest version 3 (encrypted and signed) or older: upgrade the shared side and every mirror. the new publisher restarts the folder (removing `manifest.sig` and the old queue and history files) and mirrors subscribe again; a mirror still on the old version sees an unknown format and waits.

a shared vDS may only reference records inside the **same** shared vDS (checked when the schema is registered and on every write), otherwise the reference would dangle on the mirror side. `local` may reference any shared vDS. `indexTemplate` cannot target a type that is only available from a mirror.

### Registration in the Inventory

 - `local` registers as `ds.id` with the instance UUID, as before.
 - `shared` and `mirrored` register with their name and the source id (the channel address, see above). instances mirroring the same source (same source id) are merged into **one** record with several URLs (replicas); a shared vDS and its own mirror in the same Inventory merge the same way and stay `kind: shared`. the URL list is ordered by mirror cursor, newest first, and refreshed on every sync.
 - the same name with a different source: the later one is renamed `{name}-{first 8 hex of the source id}` and a warning is logged. the same source under another name (inconsistent `alias`) is merged into the existing record.
 - `pinnedSources` in the Inventory config (`{"prod": "SHA256:..."}`, the `instanceId` from `GET /{vDS}/config`) reserves a name for one source.
 - `DELETE /inventory/{dsId}?url={url}` removes one replica URL; the record is deleted when no URL is left.
 - the same type with the same schema version but different content on two DS is a conflict: the later DS is left out of that type's referral and a warning is logged.

## Network Security Model

the Inventory system (Inventory Service + every Data Service) is meant to live **entirely inside a protected internal network**. it must **never be reachable from the public internet**.

because of that, the Inventory system **trusts the other systems on the internal network**:

 - **development stage (now)**: there is no access restriction at all. no authentication, no authorization, and the REST API of both services answers **CORS from any origin** (`Access-Control-Allow-Origin: *`, `OPTIONS` pre-flight answered with `204`). any internal UI or tool, e.g. the schema designer / dashboard served from a different port, can call it directly from the browser.
 - **production stage (later)**: access will be limited by an **allow-list (whitelist)** of the internal systems that may call it. CORS is not the access control mechanism and stays open; the allow-list is.

the CORS handling is a middleware in `lib/Util/Http/cors.go` (`Http.Cors`) that wraps the root handler of both servers. do not restrict `Access-Control-Allow-Origin` through the `http.headers` section of a service config - a value set there overrides the `*` on normal responses but not on pre-flight, which leaves the browser with an inconsistent answer.

**do not expose either service to the internet** (no public load balancer, no public port mapping) - with the current open model, anyone who can reach the port can read and change all data.

## JSON Schema Extensions
the JSON schema of schema to define data format is the key feature of this Data Service.
it give enough flexibility to define JSON data in order to support any coding requirement.

in order to achieve certain feature of Data Service, we have extended JSON schema as following:

### **contentMediaType**
in the standard JSON schema, the original meaning of contentMediaType defines how to parse value of the attached attribute. the following code means value of testAttr is a json string that we can use json library to parse it
```
{
    "name": "testAttr",
    "type": "string",
    "contentMediaType": "json"
}
```

**this implementation does NOT support those standard values.** the only accepted form is the one we introduced:

**contentMediaType = [inventory/{dataType}]**

The following attribute definition means, value of testId is the id of type **test** from **inventory** service
```
{
    "name": "testId",
    "type": "string",
    "contentMediaType": "inventory/test"
}
```

#### **what gets rejected**

every **contentMediaType** on a **string** attribute is parsed, and only the **inventory** prefix is understood. anything else fails the schema preprocess, so the schema cannot even be registered:

```
# the standard values are rejected
"contentMediaType": "json"              -> [contentMediaType]=[json] not supported
"contentMediaType": "application/json"  -> [contentMediaType]=[application] not supported
"contentMediaType": "text/plain"        -> [contentMediaType]=[text] not supported

# a bare type name is rejected too, the prefix is mandatory
"contentMediaType": "actor"             -> [contentMediaType]=[actor] not supported
"contentMediaType": "actor@1.0.0"       -> [contentMediaType]=[actor@1.0.0] not supported
```

note the value is split on the first **/**, so only the part before it is reported - **application/json** shows up as **[application]**.

once the form is correct, the target record is looked up through the Inventory Service, and a reference to a record that does not exist is rejected on write:

```
reference inventory:VmHost with value=[vmhost-test01] does not exists. @path=[VmLink/vm-srv-wireguard-01-link-ext/host]
```

#### **Usage in the demo**

the demo defines a mesh of virtual machine entities spread over two Data Services. it is the best place to see **contentMediaType** in action:

| demo file | what it shows |
|---|---|
| `demo/demo01/data/vmComputeSchema.json` | all schema definitions used below |
| `demo/demo01/steps/06-AcrossDataServiceRef.py` | reference across Data Service, and validation of it |
| `demo/demo01/steps/09-InventoryServiceExplorerData.py` | walking the reference graph by path |
| `demo/demo02/steps/05-CmtIndexAuto.py` | contentMediaType + indexTemplate for auto registered reverse reference |

**1, a reference that crosses the Data Service boundary**

VmLink is defined in DataService01, VmHost is defined in DataService02. a single attribute is enough to point from one into the other:

```
"host": {
    "type": "string",
    "contentMediaType": "inventory/VmHost"
}
```

the value stored is simply the **__id** of the target record, no URL, no DS name:

```
{
    "__id": "vm-srv-wireguard-01-link-ext",
    "__type": "VmLink",
    "__ver": "0.0.2",
    "data": {
        "vm": "vm-srv-wireguard-01",
        "name": "ext",
        "host": "vmhost-test01"
    }
}
```

so the writer of a record never needs to know which Data Service holds the target - the Inventory Service resolves **inventory/VmHost** through its referral table and routes the lookup to the right DS.

that referral table is maintained by the **Inventory Service**, not by the Data Service that owns the type. a type becomes resolvable only after a schema sync has registered it:

```
[sync] referral type[chat-channel-member] to DS[[DataService01]] set
```

the sync runs at Inventory Service startup, when a new Data Service registers itself, and periodically (**sync.intervalSec**, 300 seconds by default). a schema that was just registered on a Data Service is therefore not immediately referenceable - wait for the next sync, or trigger one manually with the admin tool:

```
go run ./tool/InventoryServiceAdmin sync -config <config.json> [-id <ds-id>]
```

the referral entries live in the Inventory Service's own database, so they survive restarts: once a type has been synced it stays referenceable, and restarting either service does **not** re-introduce the wait. the gap above only shows up the first time a type is introduced.

**2, the reference is validated on write**

because the target type is declared, DataService validates the value before saving. a reference to an entity that does not exist yet is rejected:

```
# NetworkCard[nic-eth1000-aef] not created yet, rejected by DataService02
PATCH http://localhost:8002/local/VmHost/vmhost-test01/nic nic-eth1000-aef
    -> 400 reference inventory:NetworkCard with value=[nic-eth1000-aef] does not exists

# create the entity in its own DataService01 first
POST http://localhost:8001/local { "__id": "nic-eth1000-aef", "__type": "NetworkCard", ... }

# same PATCH now succeeds
PATCH http://localhost:8002/local/VmHost/vmhost-test01/nic nic-eth1000-aef
```

note the value is a plain string id, so the validation can only be a [does this record exist] check - the type of the referenced value itself is not inspected here.

**3, array of references**

**contentMediaType** sits on a **string** attribute, so a list of references is simply an array of strings declared under **items**:

```
"virtualMachine": {
    "type": "array",
    "items": {
        "type": "string",
        "contentMediaType": "inventory/VirtualMachine"
    }
}
```

the attribute can also be a nested one, reached through a local **$ref** definition. in demo01 the **VirtualMachine** schema keeps its nic attributes in **definitions/nic**, and the reference inside it is declared exactly the same way:

```
"network": {
    "type": "array",
    "items": {
        "type": "object",
        "$ref": "#/definitions/nic"
    }
},
...
"definitions": {
    "nic": {
        "properties": {
            "link": {
                "type": "string",
                "contentMediaType": "inventory/VmLink"
            }
        }
    }
}
```

**4, walking the reference graph**

after the references are in place, the Inventory Service follows them and lets you walk the graph as if it were one local document. every step of the path below is a **contentMediaType** hop, crossing Data Service borders several times:

```
GET http://localhost:8004/VirtualMachine/DataService01/vm-srv-wireguard-01/network[eth0]/link/bridge/links[vm-srv-wireguard-02-link-ext]/vm
```

the helper commands of the path engine make the walk explorable, and all of them are demoed in step 09:
 - **?schema** - show the schema of the current path, so you can see the options available
 - **?flat** - show only the current layer instead of the whole referenced complex
 - **[*]** - replace an index with a wildcard to walk all options at that level
 - **?iterator** - list all the options on each leaf, for later filtering

**5, it also drives the reverse reference**

when **contentMediaType** is combined with **indexTemplate**, the same declaration that finds the target type is also used to register the reverse reference on the target record automatically. in step 05 of demo02 adding a **VirtualHardDisk** record makes its id appear under **VmHost/vmhost-test01/virtualHardDisk** without any extra write, and deleting the record removes it again.

#### **Consequences**

declaring a reference is not free - the write time validation puts constraints on the order records can be created in.

**1, the target has to exist first**

since a referencing value is validated on write, the record being pointed at must already be there. a pair that references each other therefore cannot simply be written in either order:

```
write A -> B : rejected if B does not exist yet
write B -> A : rejected if A does not exist yet
```

**2, mutual references**

there is no cycle detection in the code, the deadlock falls straight out of the existence check above combined with **required**. an attribute is **required by default** - it is only optional when it says **"required": false**. so:

 - both directions on **required** attributes = the pair can never be created, there is no valid first write
 - to actually have a mutual reference, make at least one side **"required": false** and write it in two steps: create the record without the attribute, create the other one pointing back at it, then **PATCH** the attribute in
 - the simpler design, and the one used in the demo, is to declare the reference in **one** direction only and keep the other side a plain string

#### **Reason：**
in JSON schema, there is already a key **$ref** that can reference remote schema.

but after close look, we found **$ref** is just a simple include method. it does not include the meaning of parsing and validation. 

so we decide **conentMediaType** are closer to what we really means here. 

that is:
```
we only specify how to find the data that referenced by the value, but we don't really tell you how to use it.
this will seperate the logic of parsing and using of the schema
```

### **indexTemplate**
this attribute is for DataService automation in order to auto fill in reference value in registry attribute of other record.

**Example**

what we want to achieve here is to automatically fill m01 into machines attribute from listA of type machineList when add record machine **id=m01**
```
{
    "machine": [
        {
            "id": "m01"
            "listId": "listA"
        }
    ],
    "machineList": [
        {
            "id": "listA",
            "machines": [
                "m01"
            ]
        }
    ]
}
```


