# shared-mirror

two isolated networks (`site-shared`, `site-mirror`), each with its own Data Service and Inventory Service. site-shared publishes `shared/catalog`; site-mirror keeps `mirrored/catalog`. the only path between them is the `./exchange` directory (a `dir` Communication Channel); with a `git` channel it would be a repository instead, see [`shared-mirror-git`](../shared-mirror-git).

needs Docker Engine with the compose plugin, usable without sudo; on a fresh Ubuntu host `sudo ../docker-deploy.sh` installs it and adds you to group `docker`. where docker.com is unreachable (e.g. in China) it installs from a domestic mirror and sets registry mirrors for Docker Hub by itself.

the image must contain the vDS feature; until CI publishes it, build it locally (or set `UNITAO_IMAGE` to another tag for `docker compose`, and `IMAGE` below):

```
docker build -f ./docker/unitao/dockerfile -t ghcr.io/howeyhuo/unitao-dev:latest .
```

## Setup

these are the steps an operator does in a real deployment, part on site-shared and part on site-mirror; here both sites are directories next to each other on one host. the tools run from the image, nothing has to be installed on the host.

```
cd docker-compose/shared-mirror
IMAGE=ghcr.io/howeyhuo/unitao-dev:latest
printf 'UNITAO_UID=%s\nUNITAO_GID=%s\n' "$(id -u)" "$(id -g)" > .env   # containers run as you, see below
mkdir -p exchange site-shared/ds/data site-mirror/ds/data site-shared/inv/data site-mirror/inv/data
                                  # bind-mount sources: create them yourself, docker would create them owned by root
```

each service has a `config` directory (mounted at `/opt/UniTAO/config`) and a `data` directory (`/opt/UniTAO/data`). a Data Service keeps its `config.json`, `state.json`, `ssh/` and keys in `config`, and the channel's working files in `data` (none for a `dir` channel); an Inventory Service keeps its records there (`sysdirfile`) and does not start until the directory exists.

**MongoDB**: the Data Services store their tables in MongoDB, which is not part of this compose file: start one from [`../mongodb`](../mongodb), or use one you already run. each site normally has its own; for the example both may use the same one: they use different databases, `sm-site-shared` and `sm-site-mirror`. the Data Services join the Docker network the MongoDB container is on, `mongo_net` by default (`UNITAO_MONGODB_NETWORK=<network>` in `.env` for another one). create both configs from their templates and fill in the endpoint as seen from that network (`mongodb://<container name>:27017`), the user and the password:

```
cp site-shared/ds/config/config.template.json site-shared/ds/config/config.json
cp site-mirror/ds/config/config.template.json site-mirror/ds/config/config.json
```

the configs then hold the MongoDB password; they are ignored by git. the user needs to create collections and indexes in its database (e.g. the `readWrite` role on `sm-site-shared` / `sm-site-mirror`, or root).

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

## Try it

```
docker compose up -d

# write into the shared vDS on site-shared
curl -X POST localhost:8011/shared/catalog -d '{"__type":"schema","__id":"rack","__ver":"0.0.1","data":{"name":"rack","version":"0.0.1","description":"rack","properties":{"name":{"type":"string"}}}}'
curl -X POST localhost:8011/shared/catalog -d '{"__type":"rack","__id":"r1","__ver":"0.0.1","data":{"name":"rack one"}}'

# ~10 seconds later it is on site-mirror, read-only
curl localhost:8021/mirrored/catalog/rack/r1
curl localhost:8021/mirrored/catalog/config      # subscription (waiting | active), cursor, sync status
curl localhost:8011/shared/catalog/config        # publisher view: subscribers with their state and cursor
curl localhost:8024/rack                         # ["catalog/r1"] in site-mirror's Inventory
find exchange -type f                            # once site-mirror caught up: only manifest + site-mirror's progress file
```

what happens on the channel (`exchange/catalog/`):

 1. site-mirror starts first and writes `subscribers/<its id>.json` with `state: subscribe` (the publisher does not need to exist yet).
 2. site-shared publishes the two writes as `queue/…-….jsonl.gz.enc`, sees the subscription and writes `history/<id>/0001.jsonl.gz.aes` with all its records; the manifest grants it to site-mirror (its content key wrapped for site-mirror's public key).
 3. site-mirror loads the history (replacing its table), consumes the queue from there and reports `state: active` with its cursor.
 4. site-shared deletes the history and every batch site-mirror has passed.

if site-mirror stays away longer than `subscriberTimeoutHours` (24 in `site-shared/ds/config/config.json`), site-shared drops its progress file and stops keeping batches for it; when site-mirror comes back it subscribes again and reloads everything.

the signing key and the recipients are in `site-shared/ds/config/`, mounted only into site-shared; the identity is in `site-mirror/ds/config/`, mounted only into site-mirror. to add another mirror, run `DataServiceAdmin enckey` for it, append its recipient to `site-shared/ds/config/recipients` and restart site-shared. both sides mount `./exchange` read-write: site-mirror writes its progress file.

the UniTAO containers run as your user (`UNITAO_UID` / `UNITAO_GID` in `.env`), so everything they write is yours and can be deleted normally. without `.env` they run as root. your UID is not in the image's `/etc/passwd`, so the image's entrypoint adds an entry for it when the container starts; tools such as ssh refuse to run for a UID without one.

## Clean up

```
docker compose down
rm -rf exchange .env site-*/ds/data site-*/inv/data logs/*.log
rm -rf site-*/ds/config/{config.json,state.json,ssh,catalog.pem,recipients,identity,recipient}
```

these are all generated and ignored by git. files left owned by root (containers started without `.env`) need `sudo rm`.

the tables stay in MongoDB. drop the two databases, or a next run finds the old data there (e.g. with `mongosh` in the MongoDB container):

```
docker exec <mongodb container> mongosh -u <user> -p <password> --quiet --eval 'db.getSiblingDB("sm-site-shared").dropDatabase(); db.getSiblingDB("sm-site-mirror").dropDatabase()'
```
