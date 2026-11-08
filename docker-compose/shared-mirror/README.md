# shared-mirror

two isolated networks (`siteA`, `siteB`), each with its own Data Service and Inventory Service. siteA publishes `shared/catalog`; siteB keeps `mirrored/catalog`. the only path between them is the `./exchange` directory (a `dir` Communication Channel); with a `git` channel it would be a repository instead, see [`shared-mirror-git`](../shared-mirror-git).

the image must contain the vDS feature; until CI publishes it, build it locally (or set `UNITAO_IMAGE` to another tag for both `setup.sh` and `docker compose`):

```
docker build -f ./docker/unitao/dockerfile -t ghcr.io/howeyhuo/unitao-dev:latest .
```

```
cd docker-compose/shared-mirror
./setup.sh                      # signing key, trusted fingerprint for siteB, .env with your UID/GID, passwd
docker compose up -d

# write into the shared vDS on siteA
curl -X POST localhost:8011/shared/catalog -d '{"__type":"schema","__id":"rack","__ver":"0.0.1","data":{"name":"rack","version":"0.0.1","description":"rack","properties":{"name":{"type":"string"}}}}'
curl -X POST localhost:8011/shared/catalog -d '{"__type":"rack","__id":"r1","__ver":"0.0.1","data":{"name":"rack one"}}'

# a few seconds later it is on siteB, read-only
curl localhost:8021/mirrored/catalog/rack/r1
curl localhost:8021/mirrored/catalog/config      # cursor, source fingerprint, sync status
curl localhost:8024/rack                         # ["catalog/r1"] in siteB's Inventory
```

the UniTAO containers run as your user (`UNITAO_UID` / `UNITAO_GID` in `.env`, written by `setup.sh`), so everything they write is yours and can be deleted normally. without `.env` they run as root. since your UID is not in the image's `/etc/passwd`, `setup.sh` also writes `./passwd` (the image's file plus your user) and the containers mount it at `/etc/passwd`; tools such as ssh refuse to run for a UID without an entry.

`keys/`, `exchange/`, `SiteB/ds/config.json`, `passwd`, `.env`, every `state.json`, the Inventory data and the logs are generated and ignored by git. `./clean.sh` stops the example and removes all of them; files left owned by root by an older run are removed through a container.
