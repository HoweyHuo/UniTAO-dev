#!/usr/bin/env bash
# 生成 siteA 共享 catalog 的签名密钥，并把它的指纹写进 siteB 的 mirrored 配置（trustedFingerprint）。
# 实际部署时密钥只留在共享方，镜像方只拿到指纹。
set -euo pipefail
cd "$(dirname "$0")"
IMAGE="${UNITAO_IMAGE:-ghcr.io/howeyhuo/unitao-dev:latest}"
# 容器以当前用户运行，生成的文件归当前用户（docker compose 自动读取 .env）
USER_ID="$(id -u):$(id -g)"
printf 'UNITAO_UID=%s\nUNITAO_GID=%s\n' "$(id -u)" "$(id -g)" > .env
mkdir -p keys exchange logs
if [ ! -f keys/catalog.pem ]; then
    docker run --rm --user "$USER_ID" -v "$PWD/keys:/keys" "$IMAGE" DataServiceAdmin keygen -out /keys/catalog.pem
fi
FINGERPRINT=$(docker run --rm --user "$USER_ID" -v "$PWD/keys:/keys:ro" "$IMAGE" DataServiceAdmin fingerprint -key /keys/catalog.pem)
sed "s|__TRUSTED_FINGERPRINT__|${FINGERPRINT}|" SiteB/ds/config.template.json > SiteB/ds/config.json
echo "trusted fingerprint of siteA/catalog: ${FINGERPRINT}"
