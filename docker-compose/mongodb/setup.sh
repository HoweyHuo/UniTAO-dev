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

# 准备并启动这个 MongoDB 实例和它的 mongo-express：补全 .env、建 data/、docker compose up，等它就绪。
# 可以重复运行：.env 里已有的值不改，只补缺的（旧版本写的 .env 会补上 mongo-express 的设置）；
# data/ 已有数据库时 root 账号保持不变。
#
# 用法: ./setup.sh
#   交互运行时询问 root 密码（直接回车则随机生成）；非交互时随机生成。密码只存在 .env 里。

set -euo pipefail
cd "$(dirname "$0")"

# 本机已有同名容器（另一个 MongoDB / mongo-express）时不动它
for container in mongodb mongo-express; do
    docker container inspect "$container" >/dev/null 2>&1 || continue
    project=$(docker container inspect "$container" --format '{{index .Config.Labels "com.docker.compose.project"}}')
    [ "$project" = unitao-mongodb ] && continue
    dir=$(docker container inspect "$container" --format '{{index .Config.Labels "com.docker.compose.project.working_dir"}}')
    echo "a container named [$container] already runs here${dir:+ (from $dir)}: use that MongoDB instead of starting this one" >&2
    exit 1
done

port_free() { ! ss -ltnH "sport = :$1" 2>/dev/null | grep -q .; }
free_port() { local port=$1; while ! port_free "$port"; do port=$((port + 1)); done; echo "$port"; }
secret() { openssl rand -base64 24 | tr -d '/+='; }
# 空值（KEY=）也算缺：compose 不再要求这些变量，空着的密码要在这里补上
has() { grep -q "^$1=." .env; }
add() { sed -i "/^$1=$/d" .env; echo "$1=$2" >> .env; echo "== .env: set $1"; }

# root 密码要拼进 mongo-express 的连接 URL，镜像不做编码，所以只允许 URL 安全的字符
root_password() {
    local password=
    if [ -t 0 ]; then
        while :; do
            read -rsp "root password for MongoDB (empty: generate one): " password; echo >&2
            [[ "$password" =~ ^[A-Za-z0-9._~-]*$ ]] && break
            echo "use only letters, digits and . _ ~ - (it goes into a connection URL)" >&2
        done
    fi
    if [ -n "$password" ]; then echo "$password"; else secret; fi
}

if [ -f .env ]; then
    echo "== using the existing .env, adding what is missing"
else
    (umask 077; : > .env)
fi
has UNITAO_UID || add UNITAO_UID "$(id -u)"
has UNITAO_GID || add UNITAO_GID "$(id -g)"
has MONGODB_ROOT_PASSWORD || add MONGODB_ROOT_PASSWORD "$(root_password)"
has MONGODB_PORT || add MONGODB_PORT "$(free_port 27017)"
has MONGO_EXPRESS_USER || add MONGO_EXPRESS_USER admin
has MONGO_EXPRESS_PASSWORD || add MONGO_EXPRESS_PASSWORD "$(secret)"
has MONGO_EXPRESS_SECRET || add MONGO_EXPRESS_SECRET "$(secret)"
has MONGO_EXPRESS_PORT || add MONGO_EXPRESS_PORT "$(free_port 8081)"

# bind-mount 源目录自己建，否则 docker 会建成 root 的
mkdir -p data
docker compose up -d --wait

# shellcheck disable=SC1091
. ./.env
cat <<INFO
== MongoDB is running (passwords are in $(pwd)/.env)
   Data Services: join network ${UNITAO_MONGODB_NETWORK:-mongo_net}, endpoint mongodb://mongodb:27017, user root, password MONGODB_ROOT_PASSWORD
   from this host: mongodb://127.0.0.1:$MONGODB_PORT
   mongo-express:  http://$(hostname -f 2>/dev/null || hostname):$MONGO_EXPRESS_PORT, user $MONGO_EXPRESS_USER, password MONGO_EXPRESS_PASSWORD
INFO
