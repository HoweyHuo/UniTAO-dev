# shared-mirror

two isolated networks (`siteA`, `siteB`), each with its own Data Service and Inventory Service. siteA publishes `shared/catalog`; siteB keeps `mirrored/catalog`. the only path between them is the `./exchange` directory (a `dir` Communication Channel); with a `git` channel it would be a repository instead.

the image must contain the vDS feature; until CI publishes it, build it locally (or set `UNITAO_IMAGE` to another tag for both `setup.sh` and `docker compose`):

```
docker build -f ./docker/unitao/dockerfile -t ghcr.io/howeyhuo/unitao-dev:latest .
```

```
cd docker-compose/shared-mirror
./setup.sh                      # signing key + trusted fingerprint for siteB
docker compose up -d

# write into the shared vDS on siteA
curl -X POST localhost:8011/shared/catalog -d '{"__type":"schema","__id":"rack","__ver":"0.0.1","data":{"name":"rack","version":"0.0.1","description":"rack","properties":{"name":{"type":"string"}}}}'
curl -X POST localhost:8011/shared/catalog -d '{"__type":"rack","__id":"r1","__ver":"0.0.1","data":{"name":"rack one"}}'

# a few seconds later it is on siteB, read-only
curl localhost:8021/mirrored/catalog/rack/r1
curl localhost:8021/mirrored/catalog/config      # cursor, source fingerprint, sync status
curl localhost:8024/rack                         # ["catalog/r1"] in siteB's Inventory
```

`keys/`, `exchange/`, `SiteB/ds/config.json` and every `state.json` are generated and ignored by git.
