#!/usr/bin/env bash
# 清理 GHCR 上的旧镜像，只保留：
#   - 带 latest 或版本号（x.y.z）tag 的构建（冻结的版本 tag 是回滚点，见 docker/README.md 的 Release Model）
#   - 最近 KEEP_RECENT 个只有 sha-<short> tag 的构建（默认 0）
#   - 以上构建的子 manifest（每个构建是一个带 tag 的 index + 不带 tag 的平台镜像与 attestation）
# 其余全部删除：只有 sha tag 的旧构建，以及不属于任何保留构建的无 tag manifest。
#
# 用法：scripts/cleanup-images.sh <owner> <package> [--dry-run]
# 需要：gh（GH_TOKEN 有 packages 读/删权限）、python3、docker buildx（已登录 ghcr.io）。
set -euo pipefail

OWNER="${1:?usage: $0 <owner> <package> [--dry-run]}"
PACKAGE="$(echo "${2:?usage: $0 <owner> <package> [--dry-run]}" | tr "[:upper:]" "[:lower:]")"   # GHCR 的包名总是小写
DRY_RUN=false
[ "${3:-}" = "--dry-run" ] && DRY_RUN=true
KEEP_RECENT="${KEEP_RECENT:-0}"
IMAGE="ghcr.io/$(echo "$OWNER/$PACKAGE" | tr '[:upper:]' '[:lower:]')"

case "$(gh api "/users/$OWNER" --jq .type)" in
    Organization) BASE="/orgs/$OWNER/packages/container/$PACKAGE/versions" ;;
    *)            BASE="/users/$OWNER/packages/container/$PACKAGE/versions" ;;
esac

# id \t digest \t tags（逗号分隔），新的在前
VERSIONS=$(gh api --paginate "$BASE?per_page=100" --jq '.[] | "\(.id)\t\(.name)\t\(.metadata.container.tags | join(","))"')

declare -A KEEP=()
recent=0
while IFS=$'\t' read -r id digest tags; do
    [ -z "$tags" ] && continue
    keep=false
    if echo ",$tags," | grep -Eq ',(latest|[0-9]+\.[0-9]+\.[0-9]+),'; then
        keep=true
    elif [ "$recent" -lt "$KEEP_RECENT" ]; then
        keep=true
        recent=$((recent + 1))
    fi
    $keep || continue
    KEEP[$digest]="$tags"
    # 子 manifest；单平台镜像（不是 index）没有 manifests 字段
    for child in $(docker buildx imagetools inspect --raw "$IMAGE@$digest" |
        python3 -c 'import json, sys; [print(m["digest"]) for m in json.load(sys.stdin).get("manifests", [])]'); do
        KEEP[$child]="(child of $tags)"
    done
done <<< "$VERSIONS"

if ! printf '%s\n' "${KEEP[@]}" | grep -q 'latest'; then
    echo "no version tagged latest found, refusing to clean up" >&2
    exit 1
fi

echo "keep:"
for digest in "${!KEEP[@]}"; do echo "  ${digest:0:19}  ${KEEP[$digest]}"; done

# 先删带 tag 的 index，再删无 tag 的 manifest
DELETE=$(echo "$VERSIONS" | awk -F'\t' '$3!=""'; echo "$VERSIONS" | awk -F'\t' '$3==""')
deleted=0
while IFS=$'\t' read -r id digest tags; do
    [ -z "$id" ] && continue
    [ -n "${KEEP[$digest]:-}" ] && continue
    echo "delete: ${digest:0:19}  ${tags:-(untagged)}"
    if ! $DRY_RUN; then
        gh api -X DELETE "$BASE/$id" >/dev/null
    fi
    deleted=$((deleted + 1))
done <<< "$DELETE"
$DRY_RUN && echo "dry run: would delete $deleted version(s)" || echo "deleted $deleted version(s)"
