#!/usr/bin/env bash
# 生成 siteA 共享 catalog 的签名密钥，并把它的指纹写进 siteB 的 mirrored 配置（trustedFingerprint）。
# 实际部署时密钥只留在共享方，镜像方只拿到指纹。
set -euo pipefail
cd "$(dirname "$0")"
IMAGE="${UNITAO_IMAGE:-ghcr.io/howeyhuo/unitao-dev:latest}"
mkdir -p keys exchange logs
if [ ! -f keys/catalog.pem ]; then
    docker run --rm -v "$PWD/keys:/keys" "$IMAGE" DataServiceAdmin keygen -out /keys/catalog.pem
fi
FINGERPRINT=$(docker run --rm -v "$PWD/keys:/keys:ro" "$IMAGE" DataServiceAdmin fingerprint -key /keys/catalog.pem)
sed "s|__TRUSTED_FINGERPRINT__|${FINGERPRINT}|" SiteB/ds/config.template.json > SiteB/ds/config.json
echo "trusted fingerprint of siteA/catalog: ${FINGERPRINT}"
