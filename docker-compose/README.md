# docker-compose

English | [中文](README.zh.md)

deploying UniTAO with Docker Compose. the templates are not run where they are: a script copies a template into a directory of your choice (e.g. `~/infra/<name>`), fills in its configuration and starts it there. each deployed directory is one compose project; manage it with `docker compose` in that directory.

```
script/
  docker-deploy.sh     installs Docker Engine on Ubuntu (sudo)
  mongo_deploy.sh      deploys template/mongodb: MongoDB + mongo-express
  unitao_deploy.sh     deploys template/dataservice or template/inventory: one UniTAO instance
  unitao_vds.sh        adds / removes shared and mirrored vDS on a deployed Data Service
template/
  mongodb/             MongoDB for the Data Services (see its README)
  dataservice/         a Data Service with the default local vDS
  inventory/           an Inventory Service
```

the scripts run on the host and use the UniTAO image for the tools (`DataServiceAdmin`, `git`); nothing else has to be installed, except `python3` for `unitao_deploy.sh` and `unitao_vds.sh` (Ubuntu has it). until CI publishes an image with the current features, build it locally: `docker build -f ./docker/unitao/dockerfile -t ghcr.io/howeyhuo/unitao-dev:latest .`

## Deploy

one UniTAO system lives in one directory: its Inventory Service and its Data Services are subdirectories, each a compose project. MongoDB is separate (one per host, shared by the systems). for a new system, in this order:

```
S=docker-compose/script
sudo $S/docker-deploy.sh                       # 1. once per host: Docker Engine, your user in group docker
$S/mongo_deploy.sh ~/infra/mongodb             # 2. MongoDB, creates the network mongo_net (skip if you run one)
$S/unitao_deploy.sh inventory   ~/infra/unitao-howey   # 3. the Inventory Service -> ~/infra/unitao-howey/inv
$S/unitao_deploy.sh dataservice ~/infra/unitao-howey   # 4. a Data Service -> ~/infra/unitao-howey/ds (again: ds-2, ...)
$S/unitao_vds.sh ~/infra/unitao-howey/ds               # 5. optional: shared / mirrored vDS
```

```
~/infra/unitao-howey/        the UniTAO directory: one system
  inv/                       Inventory Service unitao-howey-inv
  ds/                        Data Service unitao-howey-ds
  ds-2/                      Data Service unitao-howey-ds-2
```

the Inventory comes first: a Data Service deployed after it gets its URL as the default, registers right away and can be queried through it. a Data Service deployed before it still works: it retries its registration every 10 seconds until the Inventory is up (deploy it later with the name the Data Service was given, in the same network), but writes that reference other types (`contentMediaType: "inventory/…"`) fail until then.

just running `$S/unitao_deploy.sh` walks you through it. it asks for the UniTAO directory first and looks for instances in its subdirectories (a `docker-compose.yml` with an `inventory` or `dataservice` service):

 - none: a new system. it offers to deploy the Inventory Service first (recommended), and after it, a Data Service.
 - no Inventory among them: it offers to deploy the Inventory first.
 - otherwise it asks what to deploy; the Data Service gets that Inventory's URL and network as defaults. a second Inventory in one system is refused unless you confirm.

`docker-deploy.sh` picks the official Docker repository or a domestic mirror (e.g. in China) by itself. `mongo_deploy.sh` asks for the MongoDB root password (enter generates one) and prints how to connect. see [`template/mongodb/README.md`](template/mongodb/README.md).

`unitao_deploy.sh [dataservice|inventory] [<UniTAO directory>]` asks for everything it needs; each question has a default (enter accepts it), only the MongoDB password must be typed. `<base>` is the name of the UniTAO directory (`unitao-howey`):

| question | default | |
|---|---|---|
| UniTAO directory | `~/infra/unitao` | the system; not an instance directory |
| deploy the Inventory first? | yes | only when the directory has no Inventory yet |
| instance name | `<base>-inv` / `<base>-ds`, `<base>-ds-2`, … | container name, compose project name and Data Service id |
| instance directory | `inv` / `ds`, `ds-2`, … in the UniTAO directory | must be new or empty |
| port on this host | first free from 8000 / 8001 | skips ports of running containers and of the instances in the directory |
| image | `ghcr.io/howeyhuo/unitao-dev:latest` | |
| Docker network | the network of the instances in the directory, else `<base>_net` | shared by the Data Services and the Inventory Service of one system, created if missing; they reach each other by container name |
| Inventory Service URL | `http://<Inventory in the directory>`, else `http://<running Inventory in that network>`, else `http://<base>-inv` | Data Service only. on another host: `http://<host>:<port>` |
| URL from other hosts | `http://<hostname>:<port>` | Data Service only. registered in Inventory next to `http://<name>`; `localhost` works only on this host |
| MongoDB network, endpoint, database, table name, user, password | `mongo_net`, `mongodb://mongodb:27017`, `<name>`, `<name>`, `root` | Data Service only |
| sync interval | `300` | Inventory only: seconds between schema syncs |

it then writes `.env` and `config/config.json` (both mode 600, the config holds the MongoDB password), creates `data/` and `logs/` (and `channel/` for a Data Service), and starts the instance. a Data Service starts with the default `local` vDS only; nothing about channels is asked here, that is `unitao_vds.sh`'s job.

```
~/infra/unitao-howey/ds/
  docker-compose.yml   from template/dataservice
  .env                 the answers: COMPOSE_PROJECT_NAME, UNITAO_NAME, UNITAO_PORT, UNITAO_UID/GID, UNITAO_IMAGE, networks, UNITAO_CHANNEL_DIR
  config/              config.json, state.json, and config/vds/ (keys of shared / mirrored vDS, see below)
  data/                clones of git channels
  channel/             the channel directory, until unitao_vds.sh is given another one
  logs/
```

the containers run as your user (`UNITAO_UID` / `UNITAO_GID`), so everything they write is yours; the image's entrypoint adds your UID to its `/etc/passwd` (ssh needs one). a Data Service keeps the ssh key it generates for git servers in the named volume `ssh` (`ssh.dir` = `/opt/UniTAO/ssh`), not in the directory.

to change an answer later, edit `.env` or `config/config.json` and run `docker compose up -d` (a new container name or port recreates the container). to remove an instance: `docker compose down -v` in its directory (`-v` also removes the ssh key volume), then delete the directory; the tables of a Data Service stay in MongoDB until you drop its database.

## Shared and mirrored vDS

a Data Service can share a vDS with other networks (`shared/<name>`, the publisher) or keep a read-only copy of one (`mirrored/<name>`, the subscriber), through a Communication Channel, usually a git repository both reach. `unitao_vds.sh` maintains them on a deployed Data Service:

```
unitao_vds.sh <directory>                         # interactive menu
unitao_vds.sh <directory> list
unitao_vds.sh <directory> add shared <name> --repo <url> [--folder <f>] [--branch <b>] [--publish-interval <s>] …
unitao_vds.sh <directory> add mirrored <name> --fingerprint <SHA256:…> --repo <url> [--folder <f>] [--alias <a>] [--pull-interval <s>] …
unitao_vds.sh <directory> show shared|mirrored <name>
unitao_vds.sh <directory> remove shared|mirrored <name>
unitao_vds.sh <directory> recipient list <name>
unitao_vds.sh <directory> recipient add <name> <recipient> [--note <text>]
unitao_vds.sh <directory> recipient remove <name> <recipient|subscriber id>
```

values not given on the command line (name, channel, fingerprint) are asked for; `-h` lists every option. `--dir <path>` instead of `--repo` makes a `dir` channel. it edits the `vds` section of `config/config.json`, keeps the files of each vDS in `config/vds/<kind>/<name>/`, and restarts the Data Service if it runs (`--no-restart` skips that; the config is read at start):

| kind | files | |
|---|---|---|
| shared | `signing.pem`, `fingerprint`, `recipients` | the signing key (private, never leaves the instance); its fingerprint, which every mirror needs; the subscriber whitelist (created by the Data Service, one recipient per line) |
| mirrored | `identity`, `recipient` | the decryption key (private, never leaves the instance); its public key, for the publisher's whitelist |

the branch of a git channel defaults to the repository's default branch (on Gitee often `master`), read from the remote with the Data Service's ssh key and credentials; an empty repository gives `main`. when the remote cannot be read (e.g. the Data Service's ssh key is not on the git server yet), the script prints the reason and the public key from `config/state.json`; interactively it waits for you to add the key and reads again (or takes a branch you type), otherwise it uses `main`. it is always written to the config; both sides must use the same branch, `--branch` sets it.

run the script as the user the Data Service runs as (`UNITAO_UID`, in group docker), not with sudo: `add` refuses to run as root, because the Data Service could not use the files root creates. `remove` may run with sudo, e.g. to clean up such files.

a git channel is cloned into `data/vds/<kind>/<name>/channel`. a `file:///opt/UniTAO/channel/<repo>.git` repository that does not exist yet is created as an empty bare repository in the channel directory.

a channel on this host (`file:///opt/UniTAO/channel/<repo>.git` or `--dir /opt/UniTAO/channel…`) lives in the host directory mounted at `/opt/UniTAO/channel`, by default `channel/` in the instance directory. the first such vDS asks for that directory (`--channel-dir <host path>`); when two sites on one host exchange data this way, give both the same directory. it is written to `UNITAO_CHANNEL_DIR` in `.env` and the container is recreated; once a vDS uses it, it stays. a git server needs none of this.

`remove` deletes the entry, its files and its clone (except what another vDS still uses); when the Data Service starts again it drops the table of that vDS and unregisters it from Inventory. after the restart it cleans up what the vDS left on the channel, through a temporary clone with the ssh key of the Data Service: a shared vDS deletes its `manifest.json`, `manifest.sig`, `queue/`, `history/` and `subscribers/` in its folder (not when the manifest is signed by another key, and never the folders below it), so a shared vDS added again under the same name, with a new key, can publish there; a mirrored vDS deletes its progress file `subscribers/<id>.json` in the folder of its source. with `--no-restart` while the Data Service runs, the channel is left as it is (the running Data Service would write the files back).

**the hand-over** between the two sides stays with you:

 - the publisher's `fingerprint` goes to every mirror: `add mirrored … --fingerprint "$(cat …/config/vds/shared/<name>/fingerprint)"`. it is the fingerprint of the key (`SHA256:` and 64 hex digits), not `signing.pem`.
 - each mirror's `recipient` (printed by its `add` / `show`) goes into the publisher's whitelist: `recipient add <name> <recipient>` on the publisher. until then the mirror stays `untrusted`.

`recipient` edits the whitelist `config/vds/shared/<name>/recipients` of a shared vDS and restarts the Data Service (the whitelist is read at start). `add` checks that the value is a recipient and skips one already there; `--note` puts a `# note:` line above it, shown by `list` next to the subscriber id. `remove` takes the recipient or its subscriber id (the id `list` and the channel's `subscribers/<id>.json` show) and removes its note too. a removed mirror cannot decrypt the batches published after that; its progress file is dropped after `subscriberTimeoutHours`.

### Example: two sites on one host

site-shared publishes `shared/catalog`, site-mirror mirrors it. each site is a UniTAO directory with its own Inventory, Data Service and Docker network (`site-shared_net`, `site-mirror_net`), so they cannot reach each other; the only path between them is a bare git repository in a directory both Data Services mount as their channel directory. both use the same MongoDB (different databases); in a real deployment each site has its own, and the repository is on a git server.

```
S=docker-compose/script
mkdir -p ~/demo
$S/mongo_deploy.sh ~/infra/mongodb                  # if not running yet

# site-shared: ~/demo/site-shared/{inv,ds}, ports 8034 / 8031 (answer them), sync interval 30
$S/unitao_deploy.sh inventory   ~/demo/site-shared
$S/unitao_deploy.sh dataservice ~/demo/site-shared
# site-mirror: ~/demo/site-mirror/{inv,ds}, ports 8044 / 8041
$S/unitao_deploy.sh inventory   ~/demo/site-mirror
$S/unitao_deploy.sh dataservice ~/demo/site-mirror

REPO=file:///opt/UniTAO/channel/exchange.git
# both Data Services mount ~/demo/channel at /opt/UniTAO/channel, where the repository is
$S/unitao_vds.sh ~/demo/site-shared/ds add shared catalog --repo $REPO --channel-dir ~/demo/channel --folder infra/catalog --publish-interval 5
$S/unitao_vds.sh ~/demo/site-mirror/ds add mirrored catalog --repo $REPO --channel-dir ~/demo/channel --folder infra/catalog --pull-interval 10 \
    --fingerprint "$(cat ~/demo/site-shared/ds/config/vds/shared/catalog/fingerprint)"
$S/unitao_vds.sh ~/demo/site-shared/ds recipient add catalog "$(cat ~/demo/site-mirror/ds/config/vds/mirrored/catalog/recipient)" --note site-mirror
```

try it:

```
# write into the shared vDS on site-shared
curl -X POST localhost:8031/shared/catalog -d '{"__type":"schema","__id":"rack","__ver":"0.0.1","data":{"name":"rack","version":"0.0.1","description":"rack","properties":{"name":{"type":"string"}}}}'
curl -X POST localhost:8031/shared/catalog -d '{"__type":"rack","__id":"r1","__ver":"0.0.1","data":{"name":"rack one"}}'

# what site-shared pushed
git -C ~/demo/channel/exchange.git ls-tree -r --name-only main   # infra/catalog/manifest.json, manifest.sig, subscribers/<site-mirror id>.json

# within ~15 seconds (publish every 5s, pull every 10s) it is on site-mirror, read-only
curl localhost:8041/mirrored/catalog/rack/r1
curl localhost:8041/mirrored/catalog/config      # subscription (waiting | active), cursor, sync status
curl localhost:8031/shared/catalog/config        # publisher view: subscribers with their state and cursor
curl localhost:8044/rack                         # ["catalog/r1"] in site-mirror's Inventory (after its next sync)
```

what happens on the channel:

 1. site-mirror pushes `subscribers/<its id>.json` with `state: subscribe` (the publisher does not need to exist yet).
 2. site-shared publishes the writes as `queue/…-….jsonl.gz.enc`, sees the subscription and writes `history/<id>/0001.jsonl.gz.aes` with all its records; the manifest grants it to site-mirror (its content key wrapped for site-mirror's public key).
 3. site-mirror loads the history (replacing its table), consumes the queue from there and reports `state: active` with its cursor.
 4. site-shared deletes the history and every batch site-mirror has passed.

if site-mirror stays away longer than `subscriberTimeoutHours` (24 by default, `--subscriber-timeout`), site-shared drops its progress file and stops keeping batches for it; when site-mirror comes back it subscribes again and reloads everything.

clean up: `docker compose down -v` in the four directories, `rm -rf ~/demo`, and drop the databases `site-shared-ds` and `site-mirror-ds` in MongoDB.

### How the git channel works

both sides use the same folder (`infra/catalog`) on branch `main`; other vDS can share the repository and branch under other folders (see also the main README, "How changes travel"):

```
infra/catalog/
  manifest.json, manifest.sig             # signed by the publisher: lastSeq, queue batches and history parts with their sha256, history grants
  queue/<from>-<to>.jsonl.gz.enc          # incremental batches, encrypted to all recipients
  history/<id>/<n>.jsonl.gz.aes           # parts of a full history; its content key is wrapped in the manifest for each grantee
  subscribers/<id>.json                   # written by each mirror: request, state, cursor
```

 - **every write, both sides**: `fetch --depth 1`, reset the clone to the remote branch, write only its own files, commit, push. a rejected push (someone else pushed first) is redone from the fetch, no rebase. an empty remote is fine, the first push creates the branch.
 - **the publisher** pushes batches and histories and prunes what every mirror has passed; at most every `compactIntervalHours` it squashes the branch into one commit (`push --force-with-lease`, so a concurrent push is never lost) and runs `gc` on its clone.
 - **a mirror** pushes its progress only when it changes, so an idle channel gets no commits. it needs write access to the repository.
 - git runs non-interactively (`GIT_TERMINAL_PROMPT=0`): credentials must be in place before the Data Service starts.

### Using a git server

pass the server's URL as `--repo` on both sides. the mirror needs push access too (it writes `subscribers/<id>.json`); the publisher additionally needs force-push for history compaction. if the server forbids force-push, compaction fails and is logged, everything else works.

 - **SSH** (`ssh://git@git.internal/unitao/exchange.git` or `git@git.internal:unitao/exchange.git`): the Data Service makes its own key. on first start it generates an ed25519 key `id_ed25519` in the named volume `ssh` (an existing key is reused) and writes the public key to `config/state.json` (`ssh.publicKey`, `ssh.fingerprint`), to its log and to `GET /{vDS}/config` (`instance.ssh`). add that public key to the git server (a deploy key with write access, or the account of a bot user); the private key never leaves the Data Service. git uses it with `StrictHostKeyChecking=accept-new`: the server's host key is recorded in `known_hosts` in the same volume on the first connection and checked afterwards. the key survives `docker compose down` and recreating the container, and is lost only with the volume (`docker compose down -v`). a `GIT_SSH_COMMAND` set in the container environment takes precedence, for bringing your own key:
   ```yaml
   environment:
     GIT_SSH_COMMAND: "ssh -i /opt/UniTAO/gitkey/id_ed25519 -o IdentitiesOnly=yes -o UserKnownHostsFile=/opt/UniTAO/gitkey/known_hosts"
   volumes:
     - ./gitkey:/opt/UniTAO/gitkey:ro     # id_ed25519 (mode 600, owned by your user) + known_hosts (ssh-keyscan git.internal)
   ```
 - **HTTPS** (`https://git.internal/unitao/exchange.git`): configure a credential helper in `$HOME` (`/tmp` in the containers):
   ```yaml
   volumes:
     - ./git/.gitconfig:/tmp/.gitconfig:ro              # [credential] helper = store
     - ./git/.git-credentials:/tmp/.git-credentials:ro  # https://unitao-bot:<token>@git.internal
   ```
   do not put the token in the repository URL: it is also stored in the clone's `.git/config`. the Data Service redacts credentials in the URL from `GET /{vDS}/config` and from git errors in its log, but the config file itself still holds them.
