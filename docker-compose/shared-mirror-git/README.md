# shared-mirror-git

same as [`shared-mirror`](../shared-mirror), but the Communication Channel is a `git` repository instead of a directory. two isolated networks (`site-shared`, `site-mirror`), each with its own Data Service and Inventory Service. site-shared publishes `shared/catalog` by committing and pushing to the repository; site-mirror keeps `mirrored/catalog` by fetching from it.

the remote is a bare repository on the host, `./remote/exchange.git`, mounted into both Data Services and used as `file:///opt/UniTAO/remote/exchange.git`, so no git server or SSH key is needed. in a real deployment point `channel.repo` of both sides at a git server both networks can reach (see [Using a real git server](#using-a-real-git-server)).

the image must contain the vDS feature and the `git` CLI (`docker/unitao/dockerfile` has both); until CI publishes it, build it locally (or set `UNITAO_IMAGE` to another tag for `docker compose`, and `IMAGE` below):

```
docker build -f ./docker/unitao/dockerfile -t ghcr.io/howeyhuo/unitao-dev:latest .
```

## Setup

these are the steps an operator does in a real deployment, part on site-shared and part on site-mirror; here both sites are directories next to each other on one host. the tools run from the image, nothing has to be installed on the host.

```
cd docker-compose/shared-mirror-git
IMAGE=ghcr.io/howeyhuo/unitao-dev:latest
printf 'UNITAO_UID=%s\nUNITAO_GID=%s\n' "$(id -u)" "$(id -g)" > .env   # containers run as you, see below
mkdir -p remote site-shared/ds/data site-mirror/ds/data site-shared/inv/data site-mirror/inv/data
                                  # bind-mount sources: create them yourself, docker would create them owned by root
```

each service has a `config` directory (mounted at `/opt/UniTAO/config`) and a `data` directory (`/opt/UniTAO/data`). a Data Service keeps its `config.json`, `state.json`, `ssh/` and keys in `config`, and the channel's working files in `data` (the clone, `channel.workDir`); an Inventory Service keeps its records there (`sysdirfile`) and does not start until the directory exists.

**MongoDB**: the Data Services store their tables in MongoDB, which is not part of this compose file. each site normally has its own; for the example both may use the same one: they use different databases, `smg-site-shared` and `smg-site-mirror`. the Data Services join the Docker network the MongoDB container is on, `mongo_net` by default (`UNITAO_MONGODB_NETWORK=<network>` in `.env` for another one). create both configs from their templates and fill in the endpoint as seen from that network (`mongodb://<container name>:27017`), the user and the password:

```
cp site-shared/ds/config/config.template.json site-shared/ds/config/config.json
cp site-mirror/ds/config/config.template.json site-mirror/ds/config/config.json
```

the configs then hold the MongoDB password; they are ignored by git. the user needs to create collections and indexes in its database (e.g. the `readWrite` role on `smg-site-shared` / `smg-site-mirror`, or root).

**site-shared** (publisher) creates the signing key of `shared/catalog`. `keygen` prints its fingerprint, which every mirror needs:

```
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD/site-shared/ds/config:/keys" $IMAGE DataServiceAdmin keygen -out /keys/catalog.pem
```

**site-mirror** (subscriber) creates its encryption identity. the private key (`identity`) stays on site-mirror, the public key (`recipient`) goes to the publisher; `enckey` also prints site-mirror's subscriber id:

```
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD/site-mirror/ds/config:/keys" $IMAGE DataServiceAdmin enckey -identity /keys/identity -recipient /keys/recipient
```

**hand over** what each side needs from the other:

 - site-mirror's recipient into site-shared's subscriber whitelist: `cat site-mirror/ds/config/recipient >> site-shared/ds/config/recipients`. without it site-shared starts with an empty whitelist and site-mirror stays `untrusted`.
 - site-shared's fingerprint into site-mirror's config: in `site-mirror/ds/config/config.json` replace `<fingerprint of the site-shared signing key>` in `trustedFingerprint` with the fingerprint printed by `keygen` (print it again with `docker run --rm -v "$PWD/site-shared/ds/config:/keys:ro" $IMAGE DataServiceAdmin fingerprint -key /keys/catalog.pem`).

**the repository** both sides reach. here it is an empty bare repository on the host (the publisher's first push creates branch `main`); in a real deployment it is a repository on a git server, see [Using a real git server](#using-a-real-git-server):

```
docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp -v "$PWD/remote:/remote" --entrypoint git $IMAGE init -q --bare --initial-branch=main /remote/exchange.git
```

## Try it

```
docker compose up -d

# write into the shared vDS on site-shared
curl -X POST localhost:8031/shared/catalog -d '{"__type":"schema","__id":"rack","__ver":"0.0.1","data":{"name":"rack","version":"0.0.1","description":"rack","properties":{"name":{"type":"string"}}}}'
curl -X POST localhost:8031/shared/catalog -d '{"__type":"rack","__id":"r1","__ver":"0.0.1","data":{"name":"rack one"}}'

# what site-shared pushed
git -C remote/exchange.git log --oneline main
git -C remote/exchange.git ls-tree -r --name-only main   # infra/catalog/manifest.json, manifest.sig, subscribers/<site-mirror id>.json

# within ~15 seconds (publish every 5s, pull every 10s) it is on site-mirror, read-only
curl localhost:8041/mirrored/catalog/rack/r1
curl localhost:8041/mirrored/catalog/config      # subscription (waiting | active), cursor, sync status
curl localhost:8031/shared/catalog/config        # publisher view: subscribers with their state and cursor
curl localhost:8044/rack                         # ["catalog/r1"] in site-mirror's Inventory
```

## How the git channel works

both sides use `"folder": "infra/catalog"`, so the vDS lives under that folder on branch `main`; other vDS can share the repository and branch under other folders. the layout is the same as a `dir` channel (see the main README, "How changes travel"):

```
infra/catalog/
  manifest.json, manifest.sig             # signed by site-shared: lastSeq, queue batches and history parts with their sha256, history grants
  queue/<from>-<to>.jsonl.gz.enc          # incremental batches, encrypted to all recipients
  history/<id>/<n>.jsonl.gz.aes           # parts of a full history; its content key is wrapped in the manifest for each grantee
  subscribers/<id>.json                   # written by site-mirror: request, state, cursor
```

 - **every write, both sides**: `fetch --depth 1`, reset the clone in `channel.workDir` (`site-shared/ds/data/channel`, `site-mirror/ds/data/channel`) to the remote branch, write only its own files, commit, push. a rejected push (someone else pushed first) is redone from the fetch, no rebase. an empty remote is fine, the first push creates the branch.
 - **site-shared** pushes batches and histories and prunes what site-mirror has passed; at most every `compactIntervalHours` it squashes the branch into one commit (`push --force-with-lease`, so a concurrent push is never lost) and runs `gc` on its clone. `git -C remote/exchange.git log --oneline main` shows the commits since the last squash.
 - **site-mirror** pushes its progress only when it changes, so an idle channel gets no commits. it needs write access to the repository; `./remote` is mounted read-write on both sides.
 - git runs non-interactively (`GIT_TERMINAL_PROMPT=0`): credentials must be in place before the Data Service starts.

## Using a real git server

replace `channel.repo` on both sides, drop the `./remote` mount, and give each Data Service its credentials. git runs non-interactively, so they must be in place before the Data Service starts. the mirror needs push access too (it writes `subscribers/<id>.json`); the publisher additionally needs force-push for history compaction. if the server forbids force-push, compaction fails and is logged, everything else works.

 - **SSH** (`ssh://git@git.internal/unitao/exchange.git` or `git@git.internal:unitao/exchange.git`): the Data Service makes its own key. on first start it generates an ed25519 key `id_ed25519` (mode 600; an existing key is reused) in `ssh.dir` of its config, by default `ssh/` next to the config and writes the public key to `state.json` (`ssh.publicKey`, `ssh.fingerprint`), to its log and to `GET /{vDS}/config` (`instance.ssh`). add that public key to the git server (a deploy key with write access, or the account of a bot user); the private key never leaves the Data Service. git uses it through `GIT_SSH_COMMAND` with `StrictHostKeyChecking=accept-new`: the server's host key is recorded in `ssh/known_hosts` on the first connection and checked afterwards. to pin it in advance, put `ssh-keyscan git.internal` output into that file before the first start. the key directory must be writable and persistent, or a new key is generated on every start. to keep the private key out of bind-mounted host directories, put it in a Docker named volume: the image has `/opt/UniTAO/ssh` with mode 1777, which a new named volume inherits, so a Data Service running as your UID can write into it:
   ```json
   "ssh": { "dir": "/opt/UniTAO/ssh" }
   ```
   ```yaml
   volumes:
     - ds-ssh:/opt/UniTAO/ssh
   # top level
   volumes:
     ds-ssh:
   ```
   the key then survives `docker compose down` / recreating the container and is lost only with the volume (`docker compose down -v`). a `GIT_SSH_COMMAND` set in the container environment takes precedence, for bringing your own key:
   ```yaml
   environment:
     GIT_SSH_COMMAND: "ssh -i /opt/UniTAO/gitkey/id_ed25519 -o IdentitiesOnly=yes -o UserKnownHostsFile=/opt/UniTAO/gitkey/known_hosts"
   volumes:
     - ./gitkey:/opt/UniTAO/gitkey:ro     # id_ed25519 (mode 600, owned by your user) + known_hosts (ssh-keyscan git.internal)
   ```
   ssh refuses to run for a UID that has no `/etc/passwd` entry (`No user exists for uid 1000`), and the containers run as your UID. the image's entrypoint adds an entry for the running UID (home `$HOME`, `/tmp` here) when the container starts, so nothing has to be mounted. if you mount your own `/etc/passwd` read-only, it must already contain that UID.
 - **HTTPS** (`https://git.internal/unitao/exchange.git`): configure a credential helper in `$HOME` (`/tmp` in the containers):
   ```yaml
   volumes:
     - ./git/.gitconfig:/tmp/.gitconfig:ro              # [credential] helper = store
     - ./git/.git-credentials:/tmp/.git-credentials:ro  # https://unitao-bot:<token>@git.internal
   ```
   do not put the token in `channel.repo`: the URL is also stored in the clone's `.git/config`. the Data Service redacts credentials in the URL from `GET /{vDS}/config` and from git errors in its log, but the config file itself still holds them.

the image built from `docker/unitao/dockerfile` contains `git` and `openssh-client`.

the UniTAO containers run as your user (`UNITAO_UID` / `UNITAO_GID` in `.env`), so everything they write is yours; without `.env` they run as root.

## Clean up

```
docker compose down
rm -rf remote .env site-*/ds/data site-*/inv/data logs/*.log
rm -rf site-*/ds/config/{config.json,state.json,ssh,catalog.pem,recipients,identity,recipient}
```

these are all generated and ignored by git. files left owned by root (containers started without `.env`) need `sudo rm`.

the tables stay in MongoDB. drop the two databases, or a next run finds the old data there (e.g. with `mongosh` in the MongoDB container):

```
docker exec <mongodb container> mongosh -u <user> -p <password> --quiet --eval 'db.getSiblingDB("smg-site-shared").dropDatabase(); db.getSiblingDB("smg-site-mirror").dropDatabase()'
```
