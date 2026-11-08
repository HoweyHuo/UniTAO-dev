#!/usr/bin/env bash
# 生成 siteA 共享 catalog 的签名密钥，并把它的指纹写进 siteB 的 mirrored 配置（trustedFingerprint）；
# 创建充当远端的裸仓库 remote/exchange.git。
# 实际部署时密钥只留在共享方，镜像方只拿到指纹；远端是一个两边都能访问的 git 服务器。
set -euo pipefail
cd "$(dirname "$0")"
IMAGE="${UNITAO_IMAGE:-ghcr.io/howeyhuo/unitao-dev:latest}"
# 容器以当前用户运行，生成的文件归当前用户（docker compose 自动读取 .env）
USER_ID="$(id -u):$(id -g)"
printf 'UNITAO_UID=%s\nUNITAO_GID=%s\n' "$(id -u)" "$(id -g)" > .env
mkdir -p keys remote work/siteA work/siteB logs SiteA/inv/data SiteB/inv/data
# 容器以当前用户运行，镜像的 /etc/passwd 里没有这个 UID，ssh 会报 "No user exists for uid"（git 通道用 SSH 远端时）。
# 生成一份带当前用户的 passwd 挂进容器；home 与 compose 里的 HOME 一致（/tmp）。
docker run --rm --entrypoint cat "$IMAGE" /etc/passwd > passwd
if ! grep -q "^[^:]*:[^:]*:$(id -u):" passwd; then
    echo "unitao:x:$(id -u):$(id -g):UniTAO:/tmp:/sbin/nologin" >> passwd
fi
if [ ! -f keys/catalog.pem ]; then
    docker run --rm --user "$USER_ID" -v "$PWD/keys:/keys" "$IMAGE" DataServiceAdmin keygen -out /keys/catalog.pem
fi
FINGERPRINT=$(docker run --rm --user "$USER_ID" -v "$PWD/keys:/keys:ro" "$IMAGE" DataServiceAdmin fingerprint -key /keys/catalog.pem)
sed "s|__TRUSTED_FINGERPRINT__|${FINGERPRINT}|" SiteB/ds/config.template.json > SiteB/ds/config.json
# 空的裸仓库即可：发布方第一次 push 时创建 main 分支。用镜像里的 git 创建，宿主机不需要装 git。
if [ ! -d remote/exchange.git ]; then
    docker run --rm --user "$USER_ID" -e HOME=/tmp -v "$PWD/remote:/remote" --entrypoint git "$IMAGE" \
        init -q --bare --initial-branch=main /remote/exchange.git
fi
echo "trusted fingerprint of siteA/catalog: ${FINGERPRINT}"
