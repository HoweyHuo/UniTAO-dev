#!/usr/bin/env bash

# ************************************************************************************************************
# Copyright (c) 2022 Salesforce, Inc.
# All rights reserved.

# UniTAO was originally created in 2022 by Shai Herzog & Yi Huo as an 
# Universal No-Coding Heterogeneous Infrastructure Maintenance & Inventory system that is holistically driven by open/community-developed semantic models/schemas.

# This program is free software: you can redistribute it and/or modify
# it under the terms of the GNU Affero General Public License as published
# by the Free Software Foundation, either version 3 of the License, or
# (at your option) any later version.

# This program is distributed in the hope that it will be useful,
# but WITHOUT ANY WARRANTY; without even the implied warranty of
# MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
# GNU Affero General Public License for more details.

# You should have received a copy of the GNU Affero General Public License
# along with this program.  If not, see <https://www.gnu.org/licenses/>

# This copyright notice and license applies to all files in this directory or sub-directories, except when stated otherwise explicitly.


# 在 Ubuntu 上安装 Docker Engine（含 compose 插件），设置开机启动，并让用户不用 sudo 就能用 docker。
# 步骤与 https://docs.docker.com/engine/install/ubuntu/ 一致。可以重复运行。
#
# 自动判断网络（如美国 / 中国）：
#   - 能访问 download.docker.com 就用官方 apt 仓库，否则用国内镜像里第一个能访问的。
#     镜像提供的是同一把 Docker 签名公钥，包照样按它校验。
#   - 访问不了 Docker Hub 时，把能访问的镜像站写进 /etc/docker/daemon.json（已有该文件则不改，只提示）。
#
# 用法: sudo ./docker-deploy.sh [用户]     默认是调用 sudo 的用户
#
# 注意：docker 组的成员可以通过 docker 拿到 root 权限，只把可信的用户加进去。

set -euo pipefail

[ "$(id -u)" -eq 0 ] || exec sudo "$0" "$@"

user="${1:-${SUDO_USER:-}}"
[ -n "$user" ] && [ "$user" != root ] || { echo "usage: sudo $0 [user]" >&2; exit 1; }
id "$user" >/dev/null

# shellcheck disable=SC1091
. /etc/os-release
[ "$ID" = ubuntu ] || { echo "only Ubuntu is supported, this is [$ID]" >&2; exit 1; }

APT_SOURCES=(
    https://download.docker.com
    https://mirrors.tuna.tsinghua.edu.cn/docker-ce
    https://mirrors.ustc.edu.cn/docker-ce
    https://mirrors.aliyun.com/docker-ce
)
REGISTRY_MIRRORS=(
    https://docker.m.daocloud.io
    https://docker.1ms.run
)

# 有 HTTP 应答就算能访问（registry 的 /v2/ 未登录时返回 401）
reachable() { [ "$(curl -s -o /dev/null -m 10 -w '%{http_code}' "$1")" != 000 ]; }

apt-get update
apt-get install -y ca-certificates curl

# 1. Docker 的 apt 仓库：官方，或第一个能访问的镜像
source=
for url in "${APT_SOURCES[@]}"; do
    if reachable "$url/linux/ubuntu/gpg"; then source=$url; break; fi
done
[ -n "$source" ] || { echo "none of ${APT_SOURCES[*]} is reachable" >&2; exit 1; }
echo "== Docker apt repository: $source"

install -m 0755 -d /etc/apt/keyrings
curl -fsSL "$source/linux/ubuntu/gpg" -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc
cat > /etc/apt/sources.list.d/docker.sources <<SOURCES
Types: deb
URIs: $source/linux/ubuntu
Suites: ${UBUNTU_CODENAME:-$VERSION_CODENAME}
Components: stable
Signed-By: /etc/apt/keyrings/docker.asc
SOURCES
apt-get update

# 2. Docker Engine 与插件，开机启动
apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
systemctl enable --now docker.service containerd.service

# 3. 访问不了 Docker Hub 时用镜像站拉取（如 mongo）
if reachable https://registry-1.docker.io/v2/; then
    echo "== Docker Hub is reachable"
else
    mirrors=()
    for url in "${REGISTRY_MIRRORS[@]}"; do
        reachable "$url/v2/" && mirrors+=("\"$url\"")
    done
    if [ ${#mirrors[@]} -eq 0 ]; then
        echo "warning: Docker Hub and its mirrors (${REGISTRY_MIRRORS[*]}) are unreachable, pulling images such as mongo will fail" >&2
    else
        list=$(IFS=,; echo "${mirrors[*]}")
        if grep -qs registry-mirrors /etc/docker/daemon.json; then
            echo "== Docker Hub is unreachable, registry mirrors are already set in /etc/docker/daemon.json"
        elif [ -s /etc/docker/daemon.json ]; then
            echo "warning: Docker Hub is unreachable and /etc/docker/daemon.json exists, not changed: add \"registry-mirrors\": [$list] to it and run 'systemctl restart docker'" >&2
        else
            echo "== Docker Hub is unreachable, pulling through: $list"
            printf '{\n    "registry-mirrors": [%s]\n}\n' "$list" > /etc/docker/daemon.json
            systemctl restart docker.service
        fi
    fi
fi

# 4. 用户加入 docker 组，不用 sudo 就能访问 docker
usermod -aG docker "$user"

docker --version
docker compose version
echo "[$user] is in group docker: log in again (or run 'newgrp docker') for it to take effect"
