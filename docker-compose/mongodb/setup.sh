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


# 准备并启动这个 MongoDB 实例：写 .env（UID/GID、root 密码、端口）、建 data/、docker compose up，等它就绪。
# 可以重复运行：已有的 .env 不改；data/ 已有数据库时 root 账号保持不变。
#
# 用法: ./setup.sh
#   交互运行时询问 root 密码（直接回车则随机生成）；非交互时随机生成。密码只存在 .env 里。

set -euo pipefail
cd "$(dirname "$0")"

CONTAINER=mongodb

# 本机已有名叫 mongodb 的容器（另一个 MongoDB）时不动它
if docker container inspect "$CONTAINER" >/dev/null 2>&1; then
    project=$(docker container inspect "$CONTAINER" --format '{{index .Config.Labels "com.docker.compose.project"}}')
    if [ "$project" != unitao-mongodb ]; then
        dir=$(docker container inspect "$CONTAINER" --format '{{index .Config.Labels "com.docker.compose.project.working_dir"}}')
        echo "a container named [$CONTAINER] already runs here${dir:+ (from $dir)}: use that MongoDB instead of starting this one" >&2
        exit 1
    fi
fi

port_free() { ! ss -ltnH "sport = :$1" 2>/dev/null | grep -q .; }

if [ -f .env ]; then
    echo "== using the existing .env"
else
    password=
    if [ -t 0 ]; then
        read -rsp "root password for MongoDB (empty: generate one): " password; echo
    fi
    if [ -z "$password" ]; then
        password=$(openssl rand -base64 24 | tr -d '/+=')
        echo "== generated a root password"
    fi
    port=27017
    while ! port_free "$port"; do port=$((port + 1)); done
    [ "$port" = 27017 ] || echo "== port 27017 is taken, using $port"
    umask 077
    {
        echo "UNITAO_UID=$(id -u)"
        echo "UNITAO_GID=$(id -g)"
        echo "MONGODB_ROOT_PASSWORD=$password"
        echo "MONGODB_PORT=$port"
    } > .env
    umask 022
    echo "== wrote .env"
fi

# bind-mount 源目录自己建，否则 docker 会建成 root 的
mkdir -p data
docker compose up -d --wait

# shellcheck disable=SC1091
. ./.env
cat <<INFO
== MongoDB is running
   Data Services: join network ${UNITAO_MONGODB_NETWORK:-mongo_net}, endpoint mongodb://mongodb:27017
   user root, password MONGODB_ROOT_PASSWORD in $(pwd)/.env
   from this host: mongodb://127.0.0.1:${MONGODB_PORT:-27017}
INFO
