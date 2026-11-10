# shared-mirror

two isolated networks (`siteA`, `siteB`), each with its own Data Service and Inventory Service. siteA publishes `shared/catalog`; siteB keeps `mirrored/catalog`. the only path between them is the `./exchange` directory (a `dir` Communication Channel); with a `git` channel it would be a repository instead, see [`shared-mirror-git`](../shared-mirror-git).

the image must contain the vDS feature; until CI publishes it, build it locally (or set `UNITAO_IMAGE` to another tag for `docker compose`, and `IMAGE` below):

```
docker build -f ./docker/unitao/dockerfile -t ghcr.io/howeyhuo/unitao-dev:latest .
```

## Setup

these are the steps an operator does in a real deployment, part on siteA and part on siteB; here both sites are directories next to each other on one host. the tools run from the image, nothing has to be installed on the host.

```
cd docker-compose/shared-mirror
IMAGE=ghcr.io/howeyhuo/unitao-dev:latest
printf 'UNITAO_UID=%s\nUNITAO_GID=%s\n' "$(id -u)" "$(id -g)" > .env   # containers run as you, see below
mkdir -p keys/siteA keys/siteB exchange SiteA/inv/data SiteB/inv/data
                                  # bind-mount sources and the Inventory data dirs: create them yourself, docker would create them owned by root
```

the Inventory Services store their records as files (`sysdirfile`) under `Site*/inv/data`; they do not start until that directory exists.

**siteA** (publisher) creates the signing key of `shared/catalog`. `keygen` prints its fingerprint, which every mirror needs:

```
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD/keys/siteA:/keys" $IMAGE DataServiceAdmin keygen -out /keys/catalog.pem
```

**siteB** (subscriber) creates its encryption identity. the private key (`identity`) stays on siteB, the public key (`recipient`) goes to the publisher; `enckey` also prints siteB's subscriber id:

```
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD/keys/siteB:/keys" $IMAGE DataServiceAdmin enckey -identity /keys/identity -recipient /keys/recipient
```

**hand over** what each side needs from the other:

 - siteB's recipient into siteA's subscriber whitelist: `cat keys/siteB/recipient >> keys/siteA/recipients`. without it siteA starts with an empty whitelist and siteB stays `untrusted`.
 - siteA's fingerprint into siteB's config: `cp SiteB/ds/config.template.json SiteB/ds/config.json`, then replace `<fingerprint of siteA signing key>` in `trustedFingerprint` with the fingerprint printed by `keygen` (print it again with `docker run --rm -v "$PWD/keys/siteA:/keys:ro" $IMAGE DataServiceAdmin fingerprint -key /keys/catalog.pem`).

## Try it

```
docker compose up -d

# write into the shared vDS on siteA
curl -X POST localhost:8011/shared/catalog -d '{"__type":"schema","__id":"rack","__ver":"0.0.1","data":{"name":"rack","version":"0.0.1","description":"rack","properties":{"name":{"type":"string"}}}}'
curl -X POST localhost:8011/shared/catalog -d '{"__type":"rack","__id":"r1","__ver":"0.0.1","data":{"name":"rack one"}}'

# ~10 seconds later it is on siteB, read-only
curl localhost:8021/mirrored/catalog/rack/r1
curl localhost:8021/mirrored/catalog/config      # subscription (waiting | active), cursor, sync status
curl localhost:8011/shared/catalog/config        # publisher view: subscribers with their state and cursor
curl localhost:8024/rack                         # ["catalog/r1"] in siteB's Inventory
find exchange -type f                            # once siteB caught up: only manifest + siteB's progress file
```

what happens on the channel (`exchange/catalog/`):

 1. siteB starts first and writes `subscribers/<its id>.json` with `state: subscribe` (the publisher does not need to exist yet).
 2. siteA publishes the two writes as `queue/…-….jsonl.gz.enc`, sees the subscription and writes `history/<id>/0001.jsonl.gz.aes` with all its records; the manifest grants it to siteB (its content key wrapped for siteB's public key).
 3. siteB loads the history (replacing its table), consumes the queue from there and reports `state: active` with its cursor.
 4. siteA deletes the history and every batch siteB has passed.

if siteB stays away longer than `subscriberTimeoutHours` (24 in `SiteA/ds/config.json`), siteA drops its progress file and stops keeping batches for it; when siteB comes back it subscribes again and reloads everything.

`keys/siteA/` (signing key, recipients) is mounted only into siteA and `keys/siteB/` (identity) only into siteB. to add another mirror, run `DataServiceAdmin enckey` for it, append its recipient to `keys/siteA/recipients` and restart siteA. both sides mount `./exchange` read-write: siteB writes its progress file.

both Data Services store their tables in one MongoDB container (`sm-mongodb`, databases `SiteA` and `SiteB`, user `root` / `example`, demo only). its data lives on a tmpfs, so removing the container empties it; to look inside: `docker exec -it sm-mongodb mongosh -u root -p example SiteA`. the Inventory Services keep their few records in files under `Site*/inv/data`.

the UniTAO containers run as your user (`UNITAO_UID` / `UNITAO_GID` in `.env`), so everything they write is yours and can be deleted normally. without `.env` they run as root. your UID is not in the image's `/etc/passwd`, so the image's entrypoint adds an entry for it when the container starts; tools such as ssh refuse to run for a UID without one.

## Clean up

```
docker compose down
rm -rf keys exchange .env SiteB/ds/config.json Site*/ds/state.json Site*/ds/ssh Site*/inv/data logs/*.log
```

these are all generated and ignored by git. files left owned by root (containers started without `.env`) need `sudo rm`.
