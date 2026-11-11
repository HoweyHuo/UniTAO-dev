# shared-mirror

two sites sharing data over a Communication Channel that is a `git` repository. two isolated networks (`site-shared`, `site-mirror`), each with its own Data Service and Inventory Service. site-shared publishes `shared/catalog` by committing and pushing to the repository; site-mirror keeps `mirrored/catalog` by fetching from it.

the two sites are separate deployments, each a compose project of its own: `site-shared/docker-compose.yml` and `site-mirror/docker-compose.yml`, each with its own `.env` and `logs/`. in a real deployment they run on different hosts in different networks; here they are directories next to each other on one host, and the channel (`./remote`) sits between them, mounted into both as `../remote`.

the remote is a bare repository on the host, `./remote/exchange.git`, mounted into both Data Services and used as `file:///opt/UniTAO/remote/exchange.git`, so no git server or SSH key is needed. in a real deployment point `channel.repo` of both sides at a git server both networks can reach (see [Using a real git server](#using-a-real-git-server)).

needs Docker Engine with the compose plugin, usable without sudo; on a fresh Ubuntu host `sudo ../docker-deploy.sh` installs it and adds you to group `docker`. where docker.com is unreachable (e.g. in China) it installs from a domestic mirror and sets registry mirrors for Docker Hub by itself.

the image must contain the vDS feature and the `git` CLI (`docker/unitao/dockerfile` has both); until CI publishes it, build it locally (or set `UNITAO_IMAGE` to another tag in each site's `.env`, and `IMAGE` below):

```
docker build -f ./docker/unitao/dockerfile -t ghcr.io/howeyhuo/unitao-dev:latest .
```

## Setup

each site is prepared in its own directory with [`../setup_env.sh`](../setup_env.sh): copy it (and `../clean_env.sh`, for later) into the site directory and run it there. in a real deployment each site directory is copied to its own host together with the two scripts; here both are next to each other on one host. the tools run from the image, nothing has to be installed on the host.

**MongoDB** first: the Data Services store their tables in MongoDB, which is not part of the sites' compose files: start one from [`../mongodb`](../mongodb), or use one you already run. each site normally has its own; for the example both may use the same one: they use different databases, `sm-site-shared` and `sm-site-mirror`. the Data Services join the Docker network the MongoDB container is on, `mongo_net` by default (uncomment `UNITAO_MONGODB_NETWORK` in the site's `.env` for another one). the user needs to create collections and indexes in its database (e.g. the `readWrite` role on `sm-site-shared` / `sm-site-mirror`, or root).

```
cd docker-compose/shared-mirror
for s in site-shared site-mirror; do cp ../setup_env.sh ../clean_env.sh $s/; done

site-shared/setup_env.sh   # asks for the MongoDB endpoint, user and password; creates the signing key, prints its fingerprint
site-mirror/setup_env.sh   # the same, plus the fingerprint of site-shared; creates the encryption identity, prints its recipient

# hand over: site-mirror's recipient into site-shared's subscriber whitelist
cat site-mirror/ds/config/recipient >> site-shared/ds/config/recipients
```

what `setup_env.sh` does in a site directory (it reads everything from the directory and its `docker-compose.yml`, so it works for other sites too):

 1. every `*.template*` file becomes the same file without `.template` (an existing one is kept): `.env.template` → `.env` with your UID/GID, `ds/config/config.template.json` → `ds/config/config.json`. it asks for every `<…>` placeholder left in them; `<text, e.g. value>` takes `value` on enter, a placeholder with `password` is read without echo. the generated files are mode 600 and ignored by git.
 2. it creates the bind-mount sources of `docker-compose.yml` that are missing (`ds/data`, `inv/data`, `../remote`); docker would create them owned by root.
 3. it reads the config of each service (its `-config` argument, mapped to the host through the service's bind mounts) and creates the keys that are missing: `signingKeyFile` with `DataServiceAdmin keygen`, `identityFile` with `DataServiceAdmin enckey` (the public key goes into `recipient` next to it).
 4. it prints what the other site needs: the fingerprint of each signing key, each recipient, and how many recipients the subscriber whitelist has.

it can be run again at any time: existing files and keys are not touched, and it prints the fingerprint and recipient again. it does not start anything and does not exchange anything between sites; the hand-over stays with you:

 - site-shared's fingerprint goes into site-mirror's `trustedFingerprint`: `site-mirror/setup_env.sh` asks for it (paste the line printed by `site-shared/setup_env.sh`).
 - site-mirror's recipient goes into site-shared's subscriber whitelist `ds/config/recipients` (the `cat … >>` above). without it site-shared starts with an empty whitelist and site-mirror stays `untrusted`; after a change, restart site-shared.

each `.env` holds the variables of its `docker-compose.yml`: `UNITAO_UID` / `UNITAO_GID`, and, commented out with their defaults, `UNITAO_MONGODB_NETWORK` and `UNITAO_IMAGE`.

each service has a `config` directory (mounted at `/opt/UniTAO/config`) and a `data` directory (`/opt/UniTAO/data`). a Data Service keeps its `config.json`, `state.json`, `ssh/` and keys in `config`, and the channel's working files in `data` (the clone, `channel.workDir`); an Inventory Service keeps its records there (`sysdirfile`) and does not start until the directory exists.

**the repository** both sides reach. here it is an empty bare repository on the host (the publisher's first push creates branch `main`); in a real deployment it is a repository on a git server, see [Using a real git server](#using-a-real-git-server):

```
IMAGE=ghcr.io/howeyhuo/unitao-dev:latest
docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp -v "$PWD/remote:/remote" --entrypoint git $IMAGE init -q --bare --initial-branch=main /remote/exchange.git
```

## Try it

```
(cd site-mirror && docker compose up -d)   # the mirror may start first, see below
(cd site-shared && docker compose up -d)

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

both sides use `"folder": "infra/catalog"`, so the vDS lives under that folder on branch `main`; other vDS can share the repository and branch under other folders. (see also the main README, "How changes travel"):

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

what happens on the channel in [Try it](#try-it):

 1. site-mirror starts first and pushes `subscribers/<its id>.json` with `state: subscribe` (the publisher does not need to exist yet).
 2. site-shared publishes the two writes as `queue/…-….jsonl.gz.enc`, sees the subscription and writes `history/<id>/0001.jsonl.gz.aes` with all its records; the manifest grants it to site-mirror (its content key wrapped for site-mirror's public key).
 3. site-mirror loads the history (replacing its table), consumes the queue from there and reports `state: active` with its cursor.
 4. site-shared deletes the history and every batch site-mirror has passed.

if site-mirror stays away longer than `subscriberTimeoutHours` (24 in `site-shared/ds/config/config.json`), site-shared drops its progress file and stops keeping batches for it; when site-mirror comes back it subscribes again and reloads everything.

the signing key and the recipients are in `site-shared/ds/config/`, mounted only into site-shared; the identity is in `site-mirror/ds/config/`, mounted only into site-mirror. to add another mirror, run `DataServiceAdmin enckey` for it, append its recipient to `site-shared/ds/config/recipients` and restart site-shared.

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

the UniTAO containers run as your user (`UNITAO_UID` / `UNITAO_GID` in each site's `.env`), so everything they write is yours and can be deleted normally; without `.env` they run as root.

## Clean up

```
site-shared/clean_env.sh
site-mirror/clean_env.sh
rm -rf remote          # the stand-in for the git server, shared by both sites
```

[`clean_env.sh`](../clean_env.sh) lists what it will delete and asks first (`-y` skips the question). it runs `docker compose down -v`, then deletes every file in the site directory that was not there when `setup_env.sh` first ran (it recorded them in `.setup_env.keep`): the generated configs and `.env`, the keys, `state.json`, `ssh/`, `data/` and the logs. the site directory is then back to the template, scripts included. it does not touch bind mounts outside the site directory (`../remote`) or the database; it prints both. files owned by root (containers started without `.env`) cannot be deleted; it lists them for `sudo rm`.

the tables stay in MongoDB. drop the two databases, or a next run finds the old data there (e.g. with `mongosh` in the MongoDB container):

```
docker exec <mongodb container> mongosh -u <user> -p <password> --quiet --eval 'db.getSiblingDB("sm-site-shared").dropDatabase(); db.getSiblingDB("sm-site-mirror").dropDatabase()'
```
