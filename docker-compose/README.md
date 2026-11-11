# docker-compose

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

```
sudo docker-compose/script/docker-deploy.sh                         # once per host: Docker Engine, your user in group docker
docker-compose/script/mongo_deploy.sh ~/infra/mongodb               # MongoDB, creates the network mongo_net
docker-compose/script/unitao_deploy.sh inventory ~/infra/unitao-inv
docker-compose/script/unitao_deploy.sh dataservice ~/infra/unitao-ds
```

`docker-deploy.sh` picks the official Docker repository or a domestic mirror (e.g. in China) by itself. `mongo_deploy.sh` asks for the MongoDB root password (enter generates one) and prints how to connect; skip it if you already run a MongoDB. see [`template/mongodb/README.md`](template/mongodb/README.md).

`unitao_deploy.sh [dataservice|inventory] [<directory>]` asks for everything it needs; each question has a default (enter accepts it), only the MongoDB password must be typed:

| question | default | |
|---|---|---|
| instance name | `unitao-ds` / `unitao-inv` | container name, compose project name and Data Service id |
| target directory | `./<name>` | must be new or empty |
| port on this host | first free from 8001 / 8000 | |
| image | `ghcr.io/howeyhuo/unitao-dev:latest` | |
| Docker network | `unitao_net` | shared by the Data Services and the Inventory Service of one site, created if missing; they reach each other by container name |
| Inventory Service URL | `http://<running Inventory in that network>` | Data Service only. on another host: `http://<host>:<port>` |
| URL from other hosts | `http://<hostname>:<port>` | Data Service only. registered in Inventory next to `http://<name>` |
| MongoDB network, endpoint, database, table name, user, password | `mongo_net`, `mongodb://mongodb:27017`, `<name>`, `<name>`, `root` | Data Service only |
| channel directory | `<directory>/channel` | Data Service only. mounted at `/opt/UniTAO/channel` for `dir` channels and `file://` git repositories on this host |
| sync interval | `300` | Inventory only: seconds between schema syncs |

it then writes `.env` and `config/config.json` (both mode 600, the config holds the MongoDB password), creates `data/` and `logs/`, and starts the instance. a Data Service starts with the default `local` vDS only.

```
~/infra/unitao-ds/
  docker-compose.yml   from template/dataservice
  .env                 the answers: COMPOSE_PROJECT_NAME, UNITAO_NAME, UNITAO_PORT, UNITAO_UID/GID, UNITAO_IMAGE, networks, UNITAO_CHANNEL_DIR
  config/              config.json, state.json, and config/vds/ (keys of shared / mirrored vDS, see below)
  data/                clones of git channels
  logs/
```

the containers run as your user (`UNITAO_UID` / `UNITAO_GID`), so everything they write is yours; the image's entrypoint adds your UID to its `/etc/passwd` (ssh needs one). a Data Service keeps the ssh key it generates for git servers in the named volume `ssh` (`ssh.dir` = `/opt/UniTAO/ssh`), not in the directory.

to change an answer later, edit `.env` or `config/config.json` and run `docker compose up -d` (a new container name or port recreates the container). to remove an instance: `docker compose down -v` in its directory, then delete the directory; the tables of a Data Service stay in MongoDB until you drop its database.

## Shared and mirrored vDS

a Data Service can share a vDS with other networks (`shared/<name>`, the publisher) or keep a read-only copy of one (`mirrored/<name>`, the subscriber), through a Communication Channel, usually a git repository both reach. `unitao_vds.sh` maintains them on a deployed Data Service:

```
unitao_vds.sh <directory>                         # interactive menu
unitao_vds.sh <directory> list
unitao_vds.sh <directory> add shared <name> --repo <url> [--folder <f>] [--branch <b>] [--publish-interval <s>] …
unitao_vds.sh <directory> add mirrored <name> --fingerprint <SHA256:…> --repo <url> [--folder <f>] [--alias <a>] [--pull-interval <s>] …
unitao_vds.sh <directory> show shared|mirrored <name>
unitao_vds.sh <directory> remove shared|mirrored <name>
```

values not given on the command line (name, channel, fingerprint) are asked for; `-h` lists every option. `--dir <path>` instead of `--repo` makes a `dir` channel. it edits the `vds` section of `config/config.json`, keeps the files of each vDS in `config/vds/<kind>/<name>/`, and restarts the Data Service if it runs (`--no-restart` skips that; the config is read at start):

| kind | files | |
|---|---|---|
| shared | `signing.pem`, `fingerprint`, `recipients` | the signing key (private, never leaves the instance); its fingerprint, which every mirror needs; the subscriber whitelist (created by the Data Service, one recipient per line) |
| mirrored | `identity`, `recipient` | the decryption key (private, never leaves the instance); its public key, for the publisher's whitelist |

a git channel is cloned into `data/vds/<kind>/<name>/channel`. a `file:///opt/UniTAO/channel/<repo>.git` repository that does not exist yet is created as an empty bare repository in the channel directory.

`remove` deletes the entry, its files and its clone (except what another vDS still uses); when the Data Service starts again it drops the table of that vDS and unregisters it from Inventory. what the vDS left on the channel is not touched.

**the hand-over** between the two sides stays with you:

 - the publisher's `fingerprint` goes to every mirror: `add mirrored … --fingerprint "$(cat …/config/vds/shared/<name>/fingerprint)"`. it is the fingerprint of the key (`SHA256:` and 64 hex digits), not `signing.pem`.
 - each mirror's `recipient` is appended to the publisher's `recipients`, then the publisher is restarted. until then the mirror stays `untrusted`.

### Example: two sites on one host

site-shared publishes `shared/catalog`, site-mirror mirrors it. each site has its own Inventory, Data Service and Docker network, so they cannot reach each other; the only path between them is a bare git repository in a directory both Data Services mount as their channel directory. both use the same MongoDB (different databases); in a real deployment each site has its own, and the repository is on a git server.

```
S=docker-compose/script
mkdir -p ~/demo
$S/mongo_deploy.sh ~/infra/mongodb                  # if not running yet

# site-shared: network unitao_shared, ports 8034 / 8031, channel directory ~/demo/channel
$S/unitao_deploy.sh inventory   ~/demo/site-shared-inv      # name site-shared-inv, sync interval 30
$S/unitao_deploy.sh dataservice ~/demo/site-shared-ds       # name site-shared-ds, Inventory http://site-shared-inv
# site-mirror: network unitao_mirror, ports 8044 / 8041, the same channel directory ~/demo/channel
$S/unitao_deploy.sh inventory   ~/demo/site-mirror-inv
$S/unitao_deploy.sh dataservice ~/demo/site-mirror-ds

REPO=file:///opt/UniTAO/channel/exchange.git
$S/unitao_vds.sh ~/demo/site-shared-ds add shared catalog --repo $REPO --folder infra/catalog --publish-interval 5
$S/unitao_vds.sh ~/demo/site-mirror-ds add mirrored catalog --repo $REPO --folder infra/catalog --pull-interval 10 \
    --fingerprint "$(cat ~/demo/site-shared-ds/config/vds/shared/catalog/fingerprint)"
cat ~/demo/site-mirror-ds/config/vds/mirrored/catalog/recipient >> ~/demo/site-shared-ds/config/vds/shared/catalog/recipients
(cd ~/demo/site-shared-ds && docker compose restart)
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
