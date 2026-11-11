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

# 删除一个 site 的部署后清理目录。拷贝到 site 目录（docker-compose.yml 所在目录）里运行，与 setup_env.sh 配套：
#   docker compose down -v，然后删掉目录里不在 .setup_env.keep（setup_env.sh 第一次运行时记下的模板文件）里的所有文件
#   （由模板生成的配置、.env、密钥、state.json、ssh/、数据、日志），最后删 .setup_env.keep 本身。
# 不动目录外的东西：指向目录外的 bind mount（如共用的通道）和数据库里的表只列出来，由人处理。
#
# 用法: ./clean_env.sh [-y]    不带 -y 时先列出要删的内容并确认。

set -euo pipefail
cd "$(dirname "$0")"
[ -f docker-compose.yml ] || { echo "no docker-compose.yml in $(pwd): copy this script into a site directory" >&2; exit 1; }
KEEP=.setup_env.keep
[ -f "$KEEP" ] || { echo "no $KEEP here: setup_env.sh never ran, nothing to clean" >&2; exit 1; }

here=$(pwd)
generated() { find . ! -type d ! -name "$KEEP" | sort | comm -23 - <(sort "$KEEP"); }
mapfile -t files < <(generated)
outside=$(docker compose config 2>/dev/null | awk '/^ *- type: bind$/{b=1;next} b&&/^ *source:/{sub(/^ *source: */,""); print; b=0}' | sort -u | while read -r d; do [[ $d == "$here" || $d == "$here"/* ]] || echo "$d"; done)
databases=$(find . -name '*.json' ! -name '*.template*' -path '*/config/*' -exec grep -ho '"database"[[:space:]]*:[[:space:]]*"[^"]*"' {} + 2>/dev/null | sed 's/.*"\([^"]*\)"$/\1/' | sort -u || true)

if [ "${1:-}" != -y ]; then
    echo "== docker compose down -v, then delete ${#files[@]} generated file(s) in $here:"
    printf '   %s\n' "${files[@]}" | head -n 40
    [ ${#files[@]} -gt 40 ] && echo "   ..."
    [ -t 0 ] || { echo "not interactive: run with -y" >&2; exit 1; }
    read -rp "continue? [y/N] " answer
    [[ $answer == [yY]* ]] || exit 1
fi

docker compose down -v --remove-orphans

for f in "${files[@]}"; do rm -f "$f" 2>/dev/null || true; done
find . -mindepth 1 -depth -type d -empty -delete 2>/dev/null || true
left=$(generated)
if [ -n "$left" ]; then
    echo "!! not deleted (owned by another user, e.g. root when the containers ran without .env); remove them with sudo:" >&2
    echo "$left" | sed 's/^/   /' >&2
    exit 1
fi
rm -f "$KEEP"
echo "== cleaned $here (back to the template; run setup_env.sh to prepare it again)"

if [ -n "$outside" ]; then
    echo "-> bind mounts outside this directory are left alone (shared with other sites?):"
    echo "$outside" | sed 's/^/   /'
fi
if [ -n "$databases" ]; then
    echo "-> the tables stay in the database; drop these databases if they are no longer needed: $(echo $databases)"
fi
