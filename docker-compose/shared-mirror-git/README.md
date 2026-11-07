# shared-mirror-git

same as [`shared-mirror`](../shared-mirror), but the Communication Channel is a `git` repository instead of a directory. two isolated networks (`siteA`, `siteB`), each with its own Data Service and Inventory Service. siteA publishes `shared/catalog` by committing and pushing to the repository; siteB keeps `mirrored/catalog` by fetching from it.

the remote is a bare repository on the host, `./remote/exchange.git`, mounted into both Data Services and used as `file:///opt/UniTAO/remote/exchange.git`, so no git server or SSH key is needed. in a real deployment point `channel.repo` of both sides at a git server both networks can reach (see [Using a real git server](#using-a-real-git-server)).

the image must contain the vDS feature and the `git` CLI (`docker/unitao/dockerfile` has both); until CI publishes it, build it locally (or set `UNITAO_IMAGE` to another tag for `setup.sh`, `clean.sh` and `docker compose`):

```
docker build -f ./docker/unitao/dockerfile -t ghcr.io/howeyhuo/unitao-dev:latest .
```

```
cd docker-compose/shared-mirror-git
./setup.sh                      # signing key, trusted fingerprint for siteB, bare repository, .env with your UID/GID
docker compose up -d

# write into the shared vDS on siteA
curl -X POST localhost:8031/shared/catalog -d '{"__type":"schema","__id":"rack","__ver":"0.0.1","data":{"name":"rack","version":"0.0.1","description":"rack","properties":{"name":{"type":"string"}}}}'
curl -X POST localhost:8031/shared/catalog -d '{"__type":"rack","__id":"r1","__ver":"0.0.1","data":{"name":"rack one"}}'

# what siteA pushed
git -C remote/exchange.git log --oneline main
git -C remote/exchange.git ls-tree -r --name-only main   # catalog/manifest.json, manifest.sig, segments/, snapshots/

# within ~15 seconds (publish every 5s, pull every 10s) it is on siteB, read-only
curl localhost:8041/mirrored/catalog/rack/r1
curl localhost:8041/mirrored/catalog/config      # cursor, source fingerprint, sync status
curl localhost:8044/rack                         # ["catalog/r1"] in siteB's Inventory
```

## How the git channel works

the repository has the same layout as a `dir` channel, one folder per shared vDS:

```
catalog/
  manifest.json        # segments and snapshots on the channel, with the sha256 of every file
  manifest.sig         # ed25519 signature of the manifest by keys/catalog.pem
  segments/<from>-<to>.jsonl.gz
  snapshots/<seq>.jsonl.gz
```

 - **publisher (siteA)**: keeps a full clone in `channel.workDir` (`./work/siteA/catalog`). before reading or writing it fetches and rebases onto the remote branch; after writing it commits only its own `catalog/` folder and pushes (retried 3 times, rebasing in between). an empty remote is fine, the first push creates the branch. several shared vDS may share one repository, each writes its own folder.
 - **mirror (siteB)**: keeps a shallow clone in `./work/siteB/catalog`; every `pullIntervalSec` it runs `fetch --depth 1` + `reset --hard`, verifies the manifest signature against `trustedFingerprint` and the sha256 of each file, then applies the segments after its cursor. it never pushes, so siteB mounts `./remote` read-only.
 - git runs non-interactively (`GIT_TERMINAL_PROMPT=0`): credentials must be in place before the Data Service starts.

## Using a real git server

replace `channel.repo` on both sides, drop the `./remote` mount, and give each Data Service its credentials:

 - **SSH** (`git@git.internal:unitao/exchange.git`): mount a private key and `known_hosts` for the container user, e.g. `- ./ssh:/tmp/.ssh:ro` (the containers run with `HOME=/tmp`). the mirror only needs read access, use a read-only deploy key there.
 - **HTTPS** (`https://git.internal/unitao/exchange.git`): put a token in the URL (`https://<token>@git.internal/...`) or configure a git credential helper in `$HOME/.gitconfig`.

the image built from `docker/unitao/dockerfile` contains `git` and `openssh-client`.

`keys/`, `remote/`, `work/`, `SiteB/ds/config.json`, `.env`, every `state.json` and the logs are generated and ignored by git. `./clean.sh` stops the example and removes all of them.
