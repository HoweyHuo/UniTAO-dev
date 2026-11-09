# shared-mirror-git

same as [`shared-mirror`](../shared-mirror), but the Communication Channel is a `git` repository instead of a directory. two isolated networks (`siteA`, `siteB`), each with its own Data Service and Inventory Service. siteA publishes `shared/catalog` by committing and pushing to the repository; siteB keeps `mirrored/catalog` by fetching from it.

the remote is a bare repository on the host, `./remote/exchange.git`, mounted into both Data Services and used as `file:///opt/UniTAO/remote/exchange.git`, so no git server or SSH key is needed. in a real deployment point `channel.repo` of both sides at a git server both networks can reach (see [Using a real git server](#using-a-real-git-server)).

the image must contain the vDS feature and the `git` CLI (`docker/unitao/dockerfile` has both); until CI publishes it, build it locally (or set `UNITAO_IMAGE` to another tag for `setup.sh`, `clean.sh` and `docker compose`):

```
docker build -f ./docker/unitao/dockerfile -t ghcr.io/howeyhuo/unitao-dev:latest .
```

```
cd docker-compose/shared-mirror-git
./setup.sh                      # signing key (siteA), encryption identity (siteB) + recipients (siteA), trusted fingerprint, bare repository, .env, passwd
docker compose up -d

# write into the shared vDS on siteA
curl -X POST localhost:8031/shared/catalog -d '{"__type":"schema","__id":"rack","__ver":"0.0.1","data":{"name":"rack","version":"0.0.1","description":"rack","properties":{"name":{"type":"string"}}}}'
curl -X POST localhost:8031/shared/catalog -d '{"__type":"rack","__id":"r1","__ver":"0.0.1","data":{"name":"rack one"}}'

# what siteA pushed
git -C remote/exchange.git log --oneline main
git -C remote/exchange.git ls-tree -r --name-only main   # infra/catalog/manifest.json, manifest.sig, subscribers/<siteB id>.json

# within ~15 seconds (publish every 5s, pull every 10s) it is on siteB, read-only
curl localhost:8041/mirrored/catalog/rack/r1
curl localhost:8041/mirrored/catalog/config      # subscription (waiting | active), cursor, sync status
curl localhost:8031/shared/catalog/config        # publisher view: subscribers with their state and cursor
curl localhost:8044/rack                         # ["catalog/r1"] in siteB's Inventory
```

## How the git channel works

both sides use `"folder": "infra/catalog"`, so the vDS lives under that folder on branch `main`; other vDS can share the repository and branch under other folders. the layout is the same as a `dir` channel (see the main README, "How changes travel"):

```
infra/catalog/
  manifest.json, manifest.sig             # signed by siteA: lastSeq, queue and history files with their sha256
  queue/<from>-<to>.jsonl.gz.enc          # incremental batches, encrypted to all recipients
  history/<id>-<seq>-<request>.jsonl.gz.enc   # full data for one subscriber, encrypted only to it
  subscribers/<id>.json                   # written by siteB: request, state, cursor
```

 - **every write, both sides**: `fetch --depth 1`, reset the clone in `channel.workDir` (`./work/siteA`, `./work/siteB`) to the remote branch, write only its own files, commit, push. a rejected push (someone else pushed first) is redone from the fetch, no rebase. an empty remote is fine, the first push creates the branch.
 - **siteA** pushes batches and histories and prunes what siteB has passed; at most every `compactIntervalHours` it squashes the branch into one commit (`push --force-with-lease`, so a concurrent push is never lost) and runs `gc` on its clone. `git -C remote/exchange.git log --oneline main` shows the commits since the last squash.
 - **siteB** pushes its progress only when it changes, so an idle channel gets no commits. it needs write access to the repository; `./remote` is mounted read-write on both sides.
 - git runs non-interactively (`GIT_TERMINAL_PROMPT=0`): credentials must be in place before the Data Service starts.

## Using a real git server

replace `channel.repo` on both sides, drop the `./remote` mount, and give each Data Service its credentials. git runs non-interactively, so they must be in place before the Data Service starts. the mirror needs push access too (it writes `subscribers/<id>.json`); the publisher additionally needs force-push for history compaction. if the server forbids force-push, compaction fails and is logged, everything else works.

 - **SSH** (`ssh://git@git.internal/unitao/exchange.git` or `git@git.internal:unitao/exchange.git`): the Data Service makes its own key. on first start it generates an ed25519 key in `ssh/` next to its config (`ssh/id_ed25519`, mode 600; an existing key there is reused) and writes the public key to `state.json` (`ssh.publicKey`, `ssh.fingerprint`), to its log and to `GET /{vDS}/config` (`instance.ssh`). add that public key to the git server (a deploy key with write access, or the account of a bot user); the private key never leaves the Data Service. git uses it through `GIT_SSH_COMMAND` with `StrictHostKeyChecking=accept-new`: the server's host key is recorded in `ssh/known_hosts` on the first connection and checked afterwards. to pin it in advance, put `ssh-keyscan git.internal` output into that file before the first start. the config directory must be writable and persistent, or a new key is generated on every start. a `GIT_SSH_COMMAND` set in the container environment takes precedence, for bringing your own key:
   ```yaml
   environment:
     GIT_SSH_COMMAND: "ssh -i /opt/UniTAO/gitkey/id_ed25519 -o IdentitiesOnly=yes -o UserKnownHostsFile=/opt/UniTAO/gitkey/known_hosts"
   volumes:
     - ./gitkey:/opt/UniTAO/gitkey:ro     # id_ed25519 (mode 600, owned by your user) + known_hosts (ssh-keyscan git.internal)
   ```
   ssh refuses to run for a UID that has no `/etc/passwd` entry (`No user exists for uid 1000`), and the containers run as your UID. that is why `setup.sh` writes `./passwd` (the image's file plus your user, home `/tmp`) and every UniTAO container mounts it at `/etc/passwd`; keep that mount when you run as a non-root UID.
 - **HTTPS** (`https://git.internal/unitao/exchange.git`): configure a credential helper in `$HOME` (`/tmp` in the containers):
   ```yaml
   volumes:
     - ./git/.gitconfig:/tmp/.gitconfig:ro              # [credential] helper = store
     - ./git/.git-credentials:/tmp/.git-credentials:ro  # https://unitao-bot:<token>@git.internal
   ```
   do not put the token in `channel.repo`: the URL is also stored in the clone's `.git/config`. the Data Service redacts credentials in the URL from `GET /{vDS}/config` and from git errors in its log, but the config file itself still holds them.

the image built from `docker/unitao/dockerfile` contains `git` and `openssh-client`.

`keys/`, `remote/`, `work/`, `SiteB/ds/config.json`, `passwd`, `.env`, every `state.json`, the Inventory data and the logs are generated and ignored by git. `./clean.sh` stops the example and removes all of them.
