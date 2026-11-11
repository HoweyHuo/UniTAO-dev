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

# 准备一个 site 的部署目录。拷贝到 site 目录（docker-compose.yml 所在目录）里运行：
#   cp docker-compose/setup_env.sh docker-compose/clean_env.sh <site>/ && <site>/setup_env.sh
#
# 1. 由模板生成配置：每个 *.template* 文件生成去掉 .template 的同名文件（已有则保留），逐个询问其中的 <...> 占位符；
#    .env 的 <uid> / <gid> 自动填当前用户。占位符写成 <说明, e.g. 默认值> 时直接回车取默认值，说明里有 password 时输入不回显。
# 2. 建 docker-compose.yml 里 bind mount 的源目录（docker 自己建会归 root）。
# 3. 按各服务的 -config 文件补上缺少的密钥：signingKeyFile（DataServiceAdmin keygen）、identityFile（enckey，公钥写在旁边的 recipient）。
# 4. 打印要交给其它 site 的东西：签名密钥的指纹、加密身份的公钥。跨 site 的交换（指纹、recipient）仍由人完成。
#
# 可以重复运行：已有的文件和密钥不改，只补缺的。不启动服务。
# 第一次运行时把目录里已有的文件记进 .setup_env.keep，clean_env.sh 据此只删生成的文件。
#
# 用法: ./setup_env.sh    占位符需要交互输入；非交互运行时遇到未填的占位符就停下，可手工编辑后重跑。

set -euo pipefail
cd "$(dirname "$0")"
[ -f docker-compose.yml ] || { echo "no docker-compose.yml in $(pwd): copy this script into a site directory" >&2; exit 1; }

KEEP=.setup_env.keep
if [ ! -f "$KEEP" ]; then
    find . ! -type d ! -name "$KEEP" | sort > "$KEEP"
    echo "== recorded the files of the template in $KEEP (clean_env.sh keeps them)"
fi

# ---- 1. 模板 ----
fill() {
    local f=$1 content ph label def val
    content=$(<"$f")
    if [ "$(basename "$f")" = .env ]; then
        content=${content//"<uid>"/$(id -u)}
        content=${content//"<gid>"/$(id -g)}
    fi
    while ph=$(grep -v '^[[:space:]]*#' <<<"$content" | grep -o '<[^<>"]*>' | head -n 1) && [ -n "$ph" ]; do
        [ -t 0 ] || { echo "$f: $ph is not filled in; run interactively or edit the file, then run again" >&2; exit 1; }
        label=${ph#<}; label=${label%>}
        def=
        if [[ $label == *", e.g. "* ]]; then def=${label#*, e.g. }; label=${label%%, e.g. *}; fi
        while :; do
            if [[ $label == *password* ]]; then
                read -rsp "$f: $label: " val; echo
            else
                read -rp "$f: $label${def:+ [$def]}: " val
            fi
            val=${val:-$def}
            [ -n "$val" ] && break
        done
        if [[ $f == *.json ]]; then val=${val//\\/\\\\}; val=${val//\"/\\\"}; fi
        content=${content//"$ph"/"$val"}
    done
    printf '%s\n' "$content" > "$f"
}

while IFS= read -r t <&3; do
    out=${t/.template/}
    if [ ! -e "$out" ]; then
        (umask 077; cp "$t" "$out")
        echo "== created $out from $t"
    fi
    fill "$out"
done 3< <(find . -name '*.template*' ! -type d | sort)

if [ -f .env ]; then set -a; . ./.env; set +a; fi
RUN_AS="${UNITAO_UID:-$(id -u)}:${UNITAO_GID:-$(id -g)}"
rel() { realpath -m --relative-to=. "$1"; }

# ---- 2. compose 的设置：bind mount（B 服务 源 目标）、各服务的 -config（C 服务 路径）、外部网络（N 名字） ----
compose=$(docker compose config)
mapfile -t facts < <(awk '
    function flush() { if (ext && name != "") print "N\t" name; ext = 0; name = "" }
    /^[^ ]/ { flush(); sec = $1; next }
    sec == "services:" && /^  [^ ]/ { svc = $1; sub(/:$/, "", svc); next }
    sec == "services:" && /^      - type: bind$/ { b = 1; next }
    sec == "services:" && /^      - type:/ { b = 0; next }
    sec == "services:" && b && /^        source:/ { src = $0; sub(/^ *source: */, "", src); next }
    sec == "services:" && b && /^        target:/ { tgt = $0; sub(/^ *target: */, "", tgt); print "B\t" svc "\t" src "\t" tgt; b = 0; next }
    sec == "services:" && /^      - -config$/ { c = 1; next }
    sec == "services:" && c { p = $0; sub(/^ *- */, "", p); print "C\t" svc "\t" p; c = 0; next }
    sec == "networks:" && /^  [^ ]/ { flush(); next }
    sec == "networks:" && /^    name:/ { name = $2; next }
    sec == "networks:" && /^    external: true/ { ext = 1; next }
    END { flush() }' <<<"$compose")
IMAGE=$(docker compose config --images | head -n 1)

for fact in "${facts[@]}"; do
    IFS=$'\t' read -r kind a b c <<<"$fact"
    if [ "$kind" = B ] && [ ! -e "$b" ]; then
        mkdir -p "$b"
        echo "== created $(rel "$b")"
    fi
    if [ "$kind" = N ] && ! docker network inspect "$a" >/dev/null 2>&1; then
        echo "!! network [$a] does not exist yet: start the service that creates it (e.g. MongoDB), or set its name in .env" >&2
    fi
done

# 容器内路径 -> 宿主机路径（按该服务的 bind mount），输出 "宿主机路径<TAB>挂载源<TAB>挂载目标"
resolve() {
    local svc=$1 path=$2 fact kind s src tgt
    for fact in "${facts[@]}"; do
        IFS=$'\t' read -r kind s src tgt <<<"$fact"
        [ "$kind" = B ] && [ "$s" = "$svc" ] || continue
        if [ "$path" = "$tgt" ] || [[ $path == "$tgt"/* ]]; then
            printf '%s\t%s\t%s\n' "$src${path#"$tgt"}" "$src" "$tgt"
            return 0
        fi
    done
    return 1
}
json_values() { grep -o "\"$1\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" "$2" | sed 's/.*"\([^"]*\)"$/\1/' || true; }
admin() { local src=$1 tgt=$2; shift 2; docker run --rm --user "$RUN_AS" -v "$src:$tgt" "$IMAGE" DataServiceAdmin "$@"; }

# ---- 3./4. 密钥 ----
for fact in "${facts[@]}"; do
    IFS=$'\t' read -r kind svc cfg _ <<<"$fact"
    [ "$kind" = C ] || continue
    IFS=$'\t' read -r cfg_host _ _ < <(resolve "$svc" "$cfg") || { echo "!! $svc: $cfg is not in a bind mount, skipped" >&2; continue; }
    [ -f "$cfg_host" ] || { echo "!! $svc: $(rel "$cfg_host") does not exist" >&2; continue; }

    for key in $(json_values signingKeyFile "$cfg_host"); do
        IFS=$'\t' read -r host src tgt < <(resolve "$svc" "$key") || { echo "!! $svc: $key is not in a bind mount, skipped" >&2; continue; }
        if [ ! -e "$host" ]; then
            admin "$src" "$tgt" keygen -out "$key" >/dev/null
            echo "== $svc: created signing key $(rel "$host")"
        fi
        echo "-> $svc: fingerprint of $(rel "$host"), for trustedFingerprint of every mirror:"
        echo "   $(admin "$src" "$tgt:ro" fingerprint -key "$key")"
    done

    for id in $(json_values identityFile "$cfg_host"); do
        IFS=$'\t' read -r host src tgt < <(resolve "$svc" "$id") || { echo "!! $svc: $id is not in a bind mount, skipped" >&2; continue; }
        recipient="$(dirname "$id")/recipient"
        if [ ! -e "$host" ]; then
            admin "$src" "$tgt" enckey -identity "$id" -recipient "$recipient" | sed -n 's/^subscriber id: /   subscriber id: /p'
            echo "== $svc: created encryption identity $(rel "$host")"
        fi
        echo "-> $svc: recipient $(rel "$(dirname "$host")/recipient"), append it to encrypt.recipientsFile of the publisher:"
        echo "   $(cat "$(dirname "$host")/recipient")"
    done

    for list in $(json_values recipientsFile "$cfg_host"); do
        IFS=$'\t' read -r host _ _ < <(resolve "$svc" "$list") || continue
        n=0; [ -f "$host" ] && n=$(grep -cv '^[[:space:]]*\(#\|$\)' "$host" || true)
        echo "-> $svc: subscriber whitelist $(rel "$host") has $n recipient(s); append each mirror's recipient, then restart"
    done
done

echo "== done; start it with docker compose up -d in $(pwd)"
