#!/usr/bin/env bash
# 停掉示例并删除所有生成文件：密钥、通道内容、SiteB 的配置、state.json、ssh 身份、日志、.env。
# 旧版本以 root 运行容器，留下的文件当前用户删不掉，这时借一个容器删除。
set -euo pipefail
cd "$(dirname "$0")"
IMAGE="${UNITAO_IMAGE:-ghcr.io/howeyhuo/unitao-dev:latest}"
docker compose down --remove-orphans >/dev/null 2>&1 || true
GENERATED=(keys exchange SiteA/ds/state.json SiteB/ds/state.json SiteA/ds/ssh SiteB/ds/ssh SiteA/inv/data SiteB/inv/data SiteB/ds/config.json logs/*.log .env passwd)
if ! rm -rf "${GENERATED[@]}" 2>/dev/null; then
    echo "some files are owned by root (left by an older run), removing them through a container"
    docker run --rm -v "$PWD:/w" -w /w --entrypoint sh "$IMAGE" -c 'rm -rf keys exchange SiteA/ds/state.json SiteB/ds/state.json SiteA/ds/ssh SiteB/ds/ssh SiteA/inv/data SiteB/inv/data SiteB/ds/config.json logs/*.log .env passwd'
fi
echo "cleaned"
