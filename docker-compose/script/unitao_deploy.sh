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

# 部署一个 UniTAO 实例（Data Service 或 Inventory Service）：复制 template/<kind>/ 到实例目录，按交互的回答生成 .env 和
# config/config.json，建好目录和 Docker 网络，然后启动。实例只带默认的 local vDS；shared / mirrored vDS 之后用 unitao_vds.sh 加。
#
# 用法: unitao_deploy.sh [dataservice|inventory] [<UniTAO 目录>]
#   一个 UniTAO 系统放在一个目录里，Inventory 和各 Data Service 是它的子目录（默认 inv/、ds/、ds-2/ ...）。
#   目录名也是默认的实例名前缀和网络名：~/infra/unitao-howey 下是 unitao-howey-inv、unitao-howey-ds，网络 unitao-howey_net。
#   目录里还没有 Inventory 时建议先部署它，部署完 Inventory 可以接着部署 Data Service。
#   没给的参数交互询问；每个问题都有默认值（回车接受），只有 MongoDB 密码必须输入。需要 python3（生成 config.json）。
#
# 同一台主机上，Data Service 与 Inventory Service 经 unitao 网络用容器名互访；
# 在不同主机上时，回答 Inventory 的 URL 和本 Data Service 从别的主机访问的 URL 即可。
# 部署后在目标目录里用 docker compose 管理；删除实例：docker compose down，再删掉目录（数据库里的表另行删除）。

set -euo pipefail
SCRIPTS="$(cd "$(dirname "$0")" && pwd)"
TEMPLATES="$(cd "$SCRIPTS/../template" && pwd)"
command -v python3 >/dev/null || { echo "unitao_deploy.sh needs python3 (to write config.json)" >&2; exit 1; }
die() { echo "$*" >&2; exit 1; }

USAGE="usage: $(basename "$0") [dataservice|inventory] [<UniTAO directory>]"
kind=${1:-}
root=${2:-}
[ $# -le 2 ] || die "$USAGE"
case $kind in
    -h|--help) sed -n '/^# 用法:/,/^$/s/^# \{0,1\}//p' "$0"; exit 0 ;;
    ""|dataservice|inventory) ;;
    *) [ $# -eq 1 ] || die "unknown kind [$kind]: dataservice or inventory"; root=$kind; kind= ;;
esac

interactive=0; [ -t 0 ] && interactive=1
# ask <提示> <默认值> [secret]：交互时询问（回车取默认），非交互时取默认；没有默认值又不能问就退出
ask() {
    local prompt=$1 def=${2:-} secret=${3:-} val=
    if [ "$interactive" = 1 ]; then
        while :; do
            if [ -n "$secret" ]; then read -rsp "$prompt: " val; echo >&2; else read -rp "$prompt${def:+ [$def]}: " val; fi
            val=${val:-$def}
            [ -n "$val" ] && break
        done
    else
        val=$def
        [ -n "$val" ] || die "$prompt: no default, run interactively"
    fi
    printf '%s' "$val"
}
taken_ports=" "   # 本目录里已部署（可能还没启动）的实例的端口
port_free() { [[ $taken_ports != *" $1 "* ]] && ! ss -ltnH "sport = :$1" 2>/dev/null | grep -q . && ! docker ps --format '{{.Ports}}' | grep -q ":$1->"; }
free_port() { local port=$1; while ! port_free "$port"; do port=$((port + 1)); done; echo "$port"; }
valid_name() { [[ $1 =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]]; }
# abs_path <路径>：交互输入的 ~ 不会被 shell 展开，这里展开后转成绝对路径
abs_path() { local p=$1; case $p in "~") p=$HOME ;; "~/"*) p=$HOME/${p#"~/"} ;; esac; realpath -m "$p"; }

# ---- UniTAO 目录：一个系统，Inventory 和 Data Service 在它的子目录里 ----
if [ -z "$root" ]; then
    echo "a UniTAO system lives in one directory, its Inventory Service and Data Services in subdirectories (inv/, ds/, ...)" >&2
    root=$(ask "UniTAO directory" "$HOME/infra/unitao")
fi
root=$(abs_path "$root")
[ ! -f "$root/docker-compose.yml" ] || die "$root is an instance directory: give the UniTAO directory that holds the instances (e.g. $(dirname "$root"))"
base=$(basename "$root"); valid_name "$base" || base=unitao
env_of() { [ -r "$1/.env" ] && sed -n "s/^$2=//p" "$1/.env" | tail -n 1; true; }

# 已部署的实例：子目录里的 docker-compose.yml，按其中的服务名区分
inv_name= sys_net= n_ds=0 listing=
for f in "$root"/*/docker-compose.yml; do
    [ -f "$f" ] || continue
    d=${f%/docker-compose.yml}
    if grep -q '^  inventory:' "$f"; then k=inventory; elif grep -q '^  dataservice:' "$f"; then k=dataservice; else continue; fi
    n=$(env_of "$d" UNITAO_NAME); net=$(env_of "$d" UNITAO_NETWORK); taken_ports+="$(env_of "$d" UNITAO_PORT) "
    listing+="   $k ${n:-?} in ${d#"$root"/}/"$'\n'
    if [ "$k" = inventory ]; then
        [ -n "$inv_name" ] || { inv_name=$n; [ -z "$net" ] || sys_net=$net; }
    else
        n_ds=$((n_ds + 1)); [ -n "$sys_net" ] || sys_net=$net
    fi
done
if [ -z "$listing" ]; then
    echo "== no UniTAO instance in $root yet: a new system" >&2
else
    printf '== instances in %s:\n%s' "$root" "$listing" >&2
fi

if [ -z "$kind" ]; then
    [ "$interactive" = 1 ] || die "$USAGE"
    if [ -z "$inv_name" ]; then
        if [ -z "$listing" ]; then q="deploy the Inventory Service of the new system first (recommended)? (y/n)"
        else q="no Inventory Service in $root: deploy it first (recommended)? (y/n)"; fi
        [[ $(ask "$q" y) == [yY]* ]] && kind=inventory
    fi
    if [ -z "$kind" ]; then
        PS3="what to deploy: "
        select kind in dataservice inventory; do [ -n "$kind" ] && break; done
    fi
fi
[ -d "$TEMPLATES/$kind" ] || die "unknown kind [$kind]: dataservice or inventory"
if [ "$kind" = inventory ] && [ -n "$inv_name" ]; then
    echo "!! $root already has an Inventory Service ($inv_name); a system has one" >&2
    [[ $(ask "deploy another one anyway? (y/n)" n) == [yY]* ]] || exit 1
fi

# 默认的子目录 inv / ds，已占用（目录或同名容器）时 inv-2 / ds-2 ...
prefix=ds; [ "$kind" = inventory ] && prefix=inv
sub=$prefix; i=1
while [ -e "$root/$sub" ] || docker container inspect "$base-$sub" >/dev/null 2>&1; do i=$((i + 1)); sub=$prefix-$i; done
while :; do
    name=$(ask "instance name (container and compose project name)" "$base-$sub")
    valid_name "$name" || { echo "only letters, digits and . _ - (starting with a letter or digit)" >&2; [ "$interactive" = 1 ] || exit 1; continue; }
    if docker container inspect "$name" >/dev/null 2>&1; then
        echo "a container named [$name] already exists" >&2; [ "$interactive" = 1 ] || exit 1; continue
    fi
    break
done
[ "$name" = "$base-$sub" ] || sub=${name#"$base-"}
target=$(ask "instance directory (relative: in $root)" "$root/$sub")
case $target in /*|"~"|"~/"*) ;; *) target=$root/$target ;; esac
target=$(abs_path "$target")
if [ -e "$target" ] && [ -n "$(ls -A "$target" 2>/dev/null)" ]; then
    die "$target is not empty: deploy into a new directory (an existing instance is managed with docker compose there)"
fi

def_port=8001; [ "$kind" = inventory ] && def_port=8000
port=$(ask "port on this host" "$(free_port $def_port)")
[[ $port =~ ^[0-9]+$ ]] || die "port must be a number"
image=$(ask "image" "ghcr.io/howeyhuo/unitao-dev:latest")
network=$(ask "Docker network shared by Data Services and Inventory Service" "${sys_net:-${base}_net}")
host=$(hostname -f 2>/dev/null || hostname)

declare -A values=( ["<instance name>"]=$name )
envs=( "COMPOSE_PROJECT_NAME=$name" "UNITAO_NAME=$name" "UNITAO_PORT=$port" "UNITAO_UID=$(id -u)" "UNITAO_GID=$(id -g)"
       "UNITAO_IMAGE=$image" "UNITAO_NETWORK=$network" )

if [ "$kind" = dataservice ]; then
    # Inventory：本目录里的，否则网络里正在运行的，否则按命名规则以后部署的
    inv=$inv_name
    [ -n "$inv" ] || inv=$(docker ps --filter "network=$network" --filter "label=com.docker.compose.service=inventory" --format '{{.Names}}' 2>/dev/null | head -n 1)
    values["<inventory url>"]=$(ask "Inventory Service URL (http://<container name> in the same network, or http://<host>:<port>)" "http://${inv:-$base-inv}")
    values["<url in the unitao network>"]="http://$name"
    values["<url from other hosts>"]=$(ask "URL of this Data Service from other hosts" "http://$host:$port")
    mongo_net=$(ask "Docker network of the MongoDB container" "mongo_net")
    docker network inspect "$mongo_net" >/dev/null 2>&1 ||
        echo "!! Docker network [$mongo_net] does not exist yet: deploy MongoDB first ($SCRIPTS/mongo_deploy.sh); the instance is set up but not started" >&2
    values["<mongodb endpoint>"]=$(ask "MongoDB endpoint (as seen from that network)" "mongodb://mongodb:27017")
    values["<mongodb database>"]=$(ask "MongoDB database" "$name")
    values["<table name>"]=$(ask "base name of the tables" "$name")
    values["<mongodb user>"]=$(ask "MongoDB user" "root")
    values["<mongodb password>"]=$(ask "MongoDB password" "" secret)
    envs+=( "UNITAO_MONGODB_NETWORK=$mongo_net" )
else
    values["<sync interval>"]=$(ask "seconds between schema syncs with the Data Services" "300")
    [[ ${values["<sync interval>"]} =~ ^[0-9]+$ ]] || die "sync interval must be a number"
fi
start=$(ask "start it now? (y/n)" "y")

# ---- 部署 ----
mkdir -p "$target"
cp -a "$TEMPLATES/$kind/." "$target/"
cd "$target"

(umask 077; : > .env)
while IFS= read -r line; do
    if [[ $line =~ ^([A-Za-z_][A-Za-z0-9_]*)= ]]; then
        key=${BASH_REMATCH[1]}
        for kv in "${envs[@]}"; do
            if [ "${kv%%=*}" = "$key" ]; then line=$kv; break; fi
        done
    fi
    printf '%s\n' "$line"
done < .env.template > .env

for p in "${!values[@]}"; do printf '%s\0%s\0' "$p" "${values[$p]}"; done |
python3 -c '
import json, sys
parts = sys.stdin.buffer.read().decode().split("\0")[:-1]
values = dict(zip(parts[0::2], parts[1::2]))
def fill(node):
    if isinstance(node, dict):
        return {k: fill(v) for k, v in node.items()}
    if isinstance(node, list):
        return [fill(v) for v in node]
    if isinstance(node, str) and node in values:
        v = values[node]
        return int(v) if v.isdigit() and node == "<sync interval>" else v
    if isinstance(node, str) and node.startswith("<") and node.endswith(">"):
        sys.exit("no value for " + node)
    return node
with open("config/config.template.json") as f:
    cfg = fill(json.load(f))
with open("config/config.json", "w") as f:
    json.dump(cfg, f, indent=4, ensure_ascii=False)
    f.write("\n")
'
chmod 600 config/config.json
rm -f .env.template config/config.template.json
mkdir -p data logs
# 通道目录先用目标目录里的 channel/（.env 的默认值）；要和别的 site 共用本机目录时，unitao_vds.sh 加 vDS 时再改
[ "$kind" = dataservice ] && mkdir -p channel
echo "== deployed $kind [$name] into $target"

for net in "$network" ${mongo_net:+"$mongo_net"}; do
    docker network inspect "$net" >/dev/null 2>&1 && continue
    if [ "$net" = "$network" ]; then
        docker network create "$net" >/dev/null
        echo "== created Docker network $net"
    else
        echo "!! Docker network [$net] does not exist: start MongoDB first (script/mongo_deploy.sh), then docker compose up -d in $target" >&2
        start=n
    fi
done

if [[ $start == [yY]* ]]; then
    docker compose up -d
    echo "== started; http://localhost:$port"
else
    echo "-> start it with: cd $target && docker compose up -d"
fi
[ "$kind" = dataservice ] && echo "-> shared / mirrored vDS: $SCRIPTS/unitao_vds.sh $target"
[ "$kind" = dataservice ] && echo "-> another Data Service: $SCRIPTS/unitao_deploy.sh dataservice $root"
echo "-> remove it: cd $target && docker compose down, then delete the directory${mongo_net:+ (and the database ${values["<mongodb database>"]} in MongoDB)}"
if [ "$kind" = inventory ] && [ "$n_ds" = 0 ]; then
    if [ "$interactive" = 1 ] && [[ $(ask "deploy a Data Service into $root now? (y/n)" y) == [yY]* ]]; then
        echo
        exec "$SCRIPTS/$(basename "$0")" dataservice "$root"
    fi
    echo "-> next, a Data Service: $SCRIPTS/unitao_deploy.sh dataservice $root"
fi
