# build docker images

### Image List

 - DataService
 - InventoryService

### Local Build
 - build image into local registry
 ```
 cd docker/{service name}
 ./build.sh
 ```
 - this will generate a image in local docker registry
   - REPOSITORY=unitao/{service name} TAG=localbuild
 - list images
 ```
 docker images
 ```
 - to check content of the image
 ```
 docker run -it --rm -v "$(pwd)"/DataService01:/opt/UniTAO/config unitao/dataservice:localbuild 
 ```

 - run DataServiceAdmin
 ```
 docker run -it --rm --network {network_name} -v "$(pwd)"/DataService01:/opt/UniTAO/config unitao/dataservice:localbuild DataServiceAdmin {data service admin arguments}
 ```
   - example - (init table):
   ```
   docker run -it --rm --network 2data1inv_default -v "$(pwd)"/DataService01:/opt/UniTAO/config unitao/dataservice:localbuild initTable.sh 
   ```

  - reset data for data service
  ```
   docker container restart {data service admin container name}
   example:
   docker container restart unitao-data-service01-admin
  ```

- run InventoryServiceAdmin manually
```
docker run -it --rm --network 2data1inv_default -v "$(pwd)"/InventoryService/config:/opt/UniTAO/config -v "$(pwd)"/InventoryService/data:/opt/UniTAO/data unitao/inventoryservice:localbuild InventoryServiceAdmin sync -config /opt/UniTAO/config/config.json
```
  - Sync schema from all Data Sources
  ```
  docker container restart unitao-inv-service-admin
  ```


### Clean Up
 - Clean up stopped Containers:
 ```
 docker container prune -f
 ```

## Published Images (CI)

`.github/workflows/docker-build.yml` builds `docker/unitao/dockerfile` on every push to `main` and publishes it to **`ghcr.io/howeyhuo/unitao-dev`** with three tags:

| Tag | Source | Advances? |
|-----|--------|-----------|
| `<version>` | `docker/meta_data.json` | depends on the release model below |
| `latest` | — | every push to `main` |
| `sha-<short>` | git commit | never — unique per build |

### Release Model

**`docker/meta_data.json` holds the version currently under development, not the last released one.**

While a version is still in development, its tag and `latest` point at the **same image**, and both advance on every main build. In that window the version tag is **not** a rollback target — it *is* `latest`, and there is nothing older behind it.

Bumping `version` for the next release is what freezes the previous one: from that build onward the old version tag stops advancing and stays pinned to its last image, while `latest` moves on to the new version. Only then does the version tag become an immutable, usable reference to that release.

To pin a build independently of any bump, use the `sha-<short>` tag or an image digest.

### Image Digests (snapshot)

A point-in-time record of what each tag resolved to. Nothing keeps this up to date, and it goes stale on the next version bump — regenerate it with the command below rather than trusting the table.

**as of 2026-09-10:**

| Tag | amd64 manifest digest | Status |
|-----|----------------------|--------|
| `1.0.0` | `sha256:51c722ccfd367f86ff145e87e2007248d92cb0ba56a5b2a2b7f0f952b65085c8` | frozen |
| `1.0.1` | `sha256:0058c3f8909ccd94b3600d1ee2e12f1fd7cda46483f0d2efb68a6276bf8ffce8` | frozen |
| `1.0.2` | `sha256:accce9f130c6fd8b4037d179a598aeb2660da4a954737c7783ba0286893347d9` | frozen |
| `1.0.3` | `sha256:5a0074d727a8e137af1364a696214062ab8cc56d4b0b53595db0b9ee5cc681cb` | in development (= `latest`) |
| `latest` | `sha256:5a0074d727a8e137af1364a696214062ab8cc56d4b0b53595db0b9ee5cc681cb` | — |

Regenerate:

```bash
for t in 1.0.0 1.0.1 1.0.2 1.0.3 latest; do
  echo -n "$t  "
  docker manifest inspect ghcr.io/howeyhuo/unitao-dev:$t \
    | jq -r '.manifests[] | select(.platform.architecture=="amd64") | .digest'
done
```




