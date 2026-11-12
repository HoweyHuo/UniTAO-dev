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

# 把已部署的 UniTAO 实例更新到当前仓库的模板和最新镜像。脚本本身在仓库里运行，git pull 就是新的；
# 部署时从 template/ 复制过去的文件不会自己更新，由这个脚本补上。
#
# 用法: unitao_update.sh [--no-start] [<目录>]
#   <目录> 是一个实例目录（inv/、ds/ ...），或放着多个实例的 UniTAO 目录（先 Inventory，后 Data Service），缺省是当前目录。
#   --no-start  只更新文件，不启动 / 重建容器
#
# 对每个实例：
#   docker-compose.yml  换成模板里的（它不含实例相关的内容，都在 .env 里）；内容不同时旧文件留作 docker-compose.yml.old
#   .env                模板里新增的变量追加进去，已有的值不动
#   config.json         不改；需要人处理的项（Inventory 还在用文件存储、Data Service 里旧的密钥字段）打印出来
#   容器                正在运行的用 compose_up 重建：先拉镜像，拉不下来用本地的（见 common.sh）；没在运行的不启动
# Inventory 还在用文件存储（sysdirfile）时整个跳过：新模板不再挂 data/，重建容器会丢掉它的记录。

set -euo pipefail
SCRIPTS="$(cd "$(dirname "$(readlink -f "$0")")" && pwd)"
TEMPLATES="$(cd "$SCRIPTS/../template" && pwd)"
command -v python3 >/dev/null || { echo "unitao_update.sh needs python3 (to read config.json)" >&2; exit 1; }
die() { echo "$*" >&2; exit 1; }
source "$SCRIPTS/common.sh"   # compose_up

start=1 dir=
for a in "$@"; do
    case $a in
        -h|--help) sed -n '/^# 用法:/,/^$/s/^# \{0,1\}//p' "$0"; exit 0 ;;
        --no-start) start=0 ;;
        -*) die "unknown option [$a]" ;;
        *) [ -z "$dir" ] || die "usage: $(basename "$0") [--no-start] [<directory>]"; dir=$a ;;
    esac
done
dir=$(realpath -m "${dir:-$PWD}")
[ -d "$dir" ] || die "$dir is not a directory"

kind_of() {   # 实例目录里 compose 文件的服务名就是类型
    if grep -q '^  inventory:' "$1/docker-compose.yml"; then echo inventory
    elif grep -q '^  dataservice:' "$1/docker-compose.yml"; then echo dataservice; fi
}

# 模板 .env.template 里有、实例 .env 里没有的变量，连同紧挨着它的注释追加进 .env；值是占位符 <...> 的不加，打印出来
update_env() {
    python3 - "$1/.env" "$2" <<'PY2'
import re, sys
env_path, tpl_path = sys.argv[1], sys.argv[2]
have = set()
with open(env_path) as f:
    for line in f:
        m = re.match(r"([A-Za-z_][A-Za-z0-9_]*)=", line)
        if m:
            have.add(m.group(1))
add, todo, comments = [], [], []
for line in open(tpl_path):
    line = line.rstrip("\n")
    m = re.match(r"([A-Za-z_][A-Za-z0-9_]*)=(.*)", line)
    if not m:
        comments = comments + [line] if line.startswith("#") else []
        continue
    key, val = m.groups()
    if key not in have:
        if re.fullmatch(r"<[^>]*>", val):
            todo.append(key)
        else:
            add += comments + [line]
            print("+" + key)
    comments = []
if add:
    with open(env_path) as f:
        text = f.read()
    with open(env_path, "a") as f:
        if text and not text.endswith("\n"):
            f.write("\n")
        f.write("\n".join(add) + "\n")
for key in todo:
    print("?" + key)
PY2
}

# config.json 里要人处理的项，一行一条
config_notes() {
    python3 - "$1/config/config.json" "$2" <<'PY2'
import json, sys
try:
    cfg = json.load(open(sys.argv[1]))
except OSError as e:
    print("cannot read config/config.json: %s" % e.strerror); sys.exit(0)
if sys.argv[2] == "inventory":
    t = (cfg.get("database") or {}).get("type")
    if t != "mongodb":
        print("sysdirfile" if t == "sysdirfile" else "database type is [%s], the template uses mongodb" % t)
    sys.exit(0)
old = []
vds = cfg.get("vds") or {}
for kind, fields in (("shared", ("signingKeyFile",)), ("mirrored", ("trustedFingerprint",))):
    for e in vds.get(kind) or []:
        ch = e.get("channel") or {}
        found = [f for f in fields if e.get(f)] + ["channel." + k for k in ("encrypt", "decrypt") if ch.get(k)]
        if found:
            old.append("%s/%s: %s" % (kind, e.get("alias") or e.get("name"), ", ".join(found)))
for o in old:
    print("old key fields (ignored since channel encryption was removed, can be deleted) in " + o)
PY2
}

update_one() {
    local d=$1 kind name tpl line notes
    kind=$(kind_of "$d")
    [ -n "$kind" ] || { echo "!! $d: not a UniTAO instance (no inventory / dataservice service in docker-compose.yml)" >&2; return 1; }
    tpl=$TEMPLATES/$kind
    name=$(sed -n 's/^UNITAO_NAME=//p' "$d/.env" 2>/dev/null | tail -n 1); name=${name:-$(basename "$d")}

    notes=$(config_notes "$d" "$kind")
    if [ "$notes" = sysdirfile ]; then
        echo "!! $name: skipped, its Inventory keeps its records in files (sysdirfile); move it to MongoDB first (docker-compose/README.md)" >&2
        return 0
    fi

    if cmp -s "$tpl/docker-compose.yml" "$d/docker-compose.yml"; then :
    else
        cp "$d/docker-compose.yml" "$d/docker-compose.yml.old"
        cp "$tpl/docker-compose.yml" "$d/docker-compose.yml"
        echo "== $name: docker-compose.yml updated (the old one is docker-compose.yml.old)"
    fi
    while IFS= read -r line; do
        case $line in
            +*) echo "== $name: added ${line#+} to .env" ;;
            \?*) echo "!! $name: .env has no ${line#?} and the template has no default for it; set it in $d/.env" >&2 ;;
        esac
    done < <(update_env "$d" "$tpl/.env.template")
    [ -z "$notes" ] || while IFS= read -r line; do echo "!! $name: $line" >&2; done <<< "$notes"

    if [ "$start" = 0 ]; then return 0; fi
    if [ -z "$(cd "$d" && docker compose ps -q --status running 2>/dev/null)" ]; then
        echo "-> $name is not running; start it with: docker compose up -d (in $d)"
        return 0
    fi
    (cd "$d" && compose_up >/dev/null) || return 1
    echo "== $name: running image $(docker inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$(cd "$d" && docker compose ps -q | head -n 1)" 2>/dev/null | cut -c1-7)"
}

if [ -f "$dir/docker-compose.yml" ]; then
    update_one "$dir"
else
    invs=() dss=()
    for f in "$dir"/*/docker-compose.yml; do
        [ -f "$f" ] || continue
        case $(kind_of "${f%/docker-compose.yml}") in
            inventory) invs+=("${f%/docker-compose.yml}") ;;
            dataservice) dss+=("${f%/docker-compose.yml}") ;;
        esac
    done
    [ $(( ${#invs[@]} + ${#dss[@]} )) -gt 0 ] || die "no UniTAO instance in $dir"
    failed=0
    for d in "${invs[@]}" "${dss[@]}"; do update_one "$d" || failed=1; done
    exit $failed
fi
