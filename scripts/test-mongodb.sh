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


# 起一个 MongoDB（临时容器，数据在 tmpfs 里，结束时删除），跑需要真实 MongoDB 的测试。
# 需要 Docker。GOPROXY 由调用方决定（模块已缓存时不需要）。
#
# 用法: test-mongodb.sh [端口]     默认端口 27018，只监听 127.0.0.1（不和本机已有的 27017 冲突）

set -euo pipefail

cd "$(dirname "$0")/.."

port="${1:-27018}"
name="unitao-test-mongodb-$$"
user=root
password=test

docker run -d --rm --name "$name" -p "127.0.0.1:${port}:27017" --tmpfs /data/db \
    -e MONGO_INITDB_ROOT_USERNAME="$user" -e MONGO_INITDB_ROOT_PASSWORD="$password" \
    mongo:latest >/dev/null
trap 'docker stop "$name" >/dev/null 2>&1 || true' EXIT

# 带账号能 ping 通才算就绪（初始化 root 账号时 mongod 会重启一次）
for _ in $(seq 1 60); do
    if docker exec "$name" mongosh --quiet -u "$user" -p "$password" --eval "db.adminCommand('ping').ok" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done

UNITAO_TEST_MONGODB_ENDPOINT="mongodb://127.0.0.1:${port}" \
UNITAO_TEST_MONGODB_USER="$user" UNITAO_TEST_MONGODB_PASSWORD="$password" \
    go test ./test/src/DataServiceTest/ -run 'MongoDb' -count=1 -v
