# build docker images

### Image

there is a single image, built from `docker/unitao/dockerfile`. it contains `DataService`, `InventoryService`, `DataServiceAdmin`, `InventoryServiceAdmin`, plus `git` and `openssh-client` for the vDS `git` Communication Channel. every compose environment under `docker-compose/` runs both services from it, choosing the program with the container `command`. CI publishes it as `ghcr.io/howeyhuo/unitao-dev` (see below).

the former per-service images (`docker/DataService`, `docker/InventoryService`) were removed; `docker/UIServer` is a separate Node.js image.

### Local Build
 - build image into local registry as `unitao:localbuild`
 ```
 ./docker/unitao/build.sh
 ```
 - or tag it like the published image, so the compose environments pick it up without changes
 ```
 docker build -f ./docker/unitao/dockerfile -t ghcr.io/howeyhuo/unitao-dev:latest .
 ```
 - pass a Go module proxy when the default one is unreachable, e.g. in the US
 ```
 docker build --build-arg GOPROXY=https://goproxy.io,direct -f ./docker/unitao/dockerfile -t unitao:localbuild .
 ```

### Run the admin tools
 - DataServiceAdmin
 ```
 docker run -it --rm --network {network_name} -v "$(pwd)"/DataService01:/opt/UniTAO/config ghcr.io/howeyhuo/unitao-dev:latest DataServiceAdmin {data service admin arguments}
 ```
 - InventoryServiceAdmin, e.g. sync schema from all Data Services
 ```
 docker run -it --rm --network 2data1inv_default -v "$(pwd)"/InventoryService/config:/opt/UniTAO/config ghcr.io/howeyhuo/unitao-dev:latest InventoryServiceAdmin sync -config /opt/UniTAO/config/config.json
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

**as of 2026-10-26:**

| Tag | amd64 manifest digest | Status |
|-----|----------------------|--------|
| `1.0.0` | `sha256:51c722ccfd367f86ff145e87e2007248d92cb0ba56a5b2a2b7f0f952b65085c8` | frozen |
| `1.0.1` | `sha256:0058c3f8909ccd94b3600d1ee2e12f1fd7cda46483f0d2efb68a6276bf8ffce8` | frozen |
| `1.0.2` | `sha256:accce9f130c6fd8b4037d179a598aeb2660da4a954737c7783ba0286893347d9` | frozen |
| `1.0.3` | `sha256:f8df3a379c521d62f1c6f52ac4e6a52bb1a3ccf4a03733eaf7b96055a9397254` | in development (= `latest`) |
| `latest` | `sha256:f8df3a379c521d62f1c6f52ac4e6a52bb1a3ccf4a03733eaf7b96055a9397254` | — |

Regenerate:

```bash
for t in 1.0.0 1.0.1 1.0.2 1.0.3 latest; do
  echo -n "$t  "
  docker manifest inspect ghcr.io/howeyhuo/unitao-dev:$t \
    | jq -r '.manifests[] | select(.platform.architecture=="amd64") | .digest'
done
```




