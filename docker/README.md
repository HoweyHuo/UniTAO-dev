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
 docker run -it --rm --network {network_name} -v "$(pwd)"/config:/opt/UniTAO/config ghcr.io/howeyhuo/unitao-dev:latest DataServiceAdmin {data service admin arguments}
 ```
 - InventoryServiceAdmin, e.g. sync schema from all Data Services
 ```
 docker run -it --rm --network unitao_net -v "$(pwd)"/config:/opt/UniTAO/config ghcr.io/howeyhuo/unitao-dev:latest InventoryServiceAdmin sync -config /opt/UniTAO/config/config.json
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
| `sha-<short>` | git commit | never — unique per build, deleted once it carries no other tag (see below) |

### Release Model

**`docker/meta_data.json` holds the version currently under development, not the last released one.**

While a version is still in development, its tag and `latest` point at the **same image**, and both advance on every main build. In that window the version tag is **not** a rollback target — it *is* `latest`, and there is nothing older behind it.

Bumping `version` for the next release is what freezes the previous one: from that build onward the old version tag stops advancing and stays pinned to its last image, while `latest` moves on to the new version. Only then does the version tag become an immutable, usable reference to that release.

To pin a build of the current version, use its `sha-<short>` tag or digest only until the next merge to `main`: the cleanup below deletes it.

### Cleanup

after pushing, the main-branch workflow runs `scripts/cleanup-images.sh`. it keeps every build tagged `latest` or with a version (`x.y.z`, the frozen rollback points) together with their child manifests (platform image, attestation), and deletes everything else: builds that only carry a `sha-<short>` tag and orphaned untagged manifests. set `KEEP_RECENT` in the workflow to also keep the newest N sha-only builds. main builds run one at a time (`concurrency`), so the cleanup never sees a half-pushed build.

check what it would delete:

```bash
scripts/cleanup-images.sh HoweyHuo unitao-dev --dry-run   # needs gh with read:packages, python3, docker buildx
```

### Image Digests

every push to `main` rebuilds the image, so a digest written down here would be stale after the next merge. look up what a tag resolves to now:

```bash
for t in 1.1.0 latest; do
  echo -n "$t  "
  docker manifest inspect ghcr.io/howeyhuo/unitao-dev:$t \
    | jq -r '.manifests[] | select(.platform.architecture=="amd64") | .digest'
done
```

all builds before 1.1.0 (the `1.0.x` tags and their `sha-<short>` tags) were deleted from the registry on 2026-11-08; 1.1.0 changed the channel protocol incompatibly (queue channel, mandatory encryption). to run a 1.0.x version, build it from its git tag or commit.
