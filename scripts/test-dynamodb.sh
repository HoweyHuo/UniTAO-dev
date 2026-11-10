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


# 起一个内存模式的 DynamoDB Local（临时容器，结束时删除），跑需要真实 DynamoDB 的测试。
# 需要 Docker。GOPROXY 由调用方决定（模块已缓存时不需要）。
#
# 用法: test-dynamodb.sh [端口]     默认端口 18000，只监听 127.0.0.1

set -euo pipefail

cd "$(dirname "$0")/.."

port="${1:-18000}"
name="unitao-test-dynamodb-$$"

docker run -d --rm --name "$name" -p "127.0.0.1:${port}:8000" \
    amazon/dynamodb-local:latest -jar DynamoDBLocal.jar -inMemory -sharedDb >/dev/null
trap 'docker stop "$name" >/dev/null 2>&1 || true' EXIT

# 就绪时对 GET / 返回 400（缺少请求参数）
for _ in $(seq 1 60); do
    if [ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${port}/" || true)" = "400" ]; then
        break
    fi
    sleep 1
done

UNITAO_TEST_DYNAMODB_ENDPOINT="http://127.0.0.1:${port}" \
    go test ./test/src/DataServiceTest/ -run 'DynamoDb' -count=1 -v
