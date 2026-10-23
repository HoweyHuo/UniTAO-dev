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

# 构建所有 Go 目标并跑测试。GOPROXY 由调用方决定，一般不直接运行本脚本，
# 而是按所在地区运行 build-cn.sh / build-us.sh。
#
# 用法: build.sh [--all]
#   默认只跑无外部依赖的单元测试（SchemaTest、SchemaPathTest、UtilTest）
#   --all 跑 test/src 下的全部测试（含 DataServiceTest、InventoryServiceTest）

set -euo pipefail

cd "$(dirname "$0")/.."

echo "GOPROXY=${GOPROXY:-$(go env GOPROXY)}"

echo "== build"
go build ./app/DataService ./app/InventoryService ./tool/DataServiceAdmin ./tool/InventoryServiceAdmin

echo "== test"
if [ "${1:-}" = "--all" ]; then
    go test ./test/src/...
else
    go test ./test/src/SchemaTest/... ./test/src/SchemaPathTest/... ./test/src/UtilTest/...
fi
