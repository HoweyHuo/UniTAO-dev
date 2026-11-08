# shared-mirror

two isolated networks (`siteA`, `siteB`), each with its own Data Service and Inventory Service. siteA publishes `shared/catalog`; siteB keeps `mirrored/catalog`. the only path between them is the `./exchange` directory (a `dir` Communication Channel); with a `git` channel it would be a repository instead, see [`shared-mirror-git`](../shared-mirror-git).

the image must contain the vDS feature; until CI publishes it, build it locally (or set `UNITAO_IMAGE` to another tag for both `setup.sh` and `docker compose`):

```
docker build -f ./docker/unitao/dockerfile -t ghcr.io/howeyhuo/unitao-dev:latest .
```

```
cd docker-compose/shared-mirror
./setup.sh                      # signing key (siteA), encryption identity (siteB) + recipients (siteA), trusted fingerprint, .env, passwd
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
 2. siteA publishes the two writes as `queue/…-….jsonl.gz.enc`, sees the subscription and writes `history/<id>-…` with all its records, encrypted only to siteB.
 3. siteB loads the history (replacing its table), consumes the queue from there and reports `state: active` with its cursor.
 4. siteA deletes the history and every batch siteB has passed.

if siteB stays away longer than `subscriberTimeoutHours` (720 in `SiteA/ds/config.json`), siteA drops its progress file and stops keeping batches for it; when siteB comes back it subscribes again and reloads everything.

`keys/siteA/` (signing key, recipients) is mounted only into siteA and `keys/siteB/` (identity) only into siteB. to add another mirror, run `DataServiceAdmin enckey` for it, append its recipient to `keys/siteA/recipients` and restart siteA. both sides mount `./exchange` read-write: siteB writes its progress file.

the UniTAO containers run as your user (`UNITAO_UID` / `UNITAO_GID` in `.env`, written by `setup.sh`), so everything they write is yours and can be deleted normally. without `.env` they run as root. since your UID is not in the image's `/etc/passwd`, `setup.sh` also writes `./passwd` (the image's file plus your user) and the containers mount it at `/etc/passwd`; tools such as ssh refuse to run for a UID without an entry.

`keys/`, `exchange/`, `SiteB/ds/config.json`, `passwd`, `.env`, every `state.json`, the Inventory data and the logs are generated and ignored by git. `./clean.sh` stops the example and removes all of them; files left owned by root by an older run are removed through a container.
