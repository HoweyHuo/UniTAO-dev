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

# 维护一个已部署的 Data Service 实例（unitao_deploy.sh 部署的目录）上的 shared / mirrored vDS：
# 改 config/config.json 的 vds 段，生成 / 删除每个 vDS 附带的文件，然后重启 Data Service 让配置生效。需要 python3。
#
# 用法: unitao_vds.sh [--service <服务>] [--no-restart] [<目标目录>] [<命令> ...]
#   不给命令时进入交互菜单。目标目录默认当前目录。
#   list
#   add shared [<name>] [--repo <url> | --dir <path>] [--channel-dir <宿主机目录>] [--branch <b>] [--folder <f>] [--workdir <path>]
#       [--publish-interval <秒>] [--subscriber-timeout <小时>] [--compact-interval <小时>] [--history-reuse <小时>]
#   add mirrored [<name>] [--fingerprint <SHA256:...>] [--repo <url> | --dir <path>] [--channel-dir <宿主机目录>] [--alias <a>]
#       [--branch <b>] [--folder <f>] [--workdir <path>] [--pull-interval <秒>]
#   show shared|mirrored <name>      打印要交给其它 site 的东西
#   remove shared|mirrored <name>    mirrored 有 alias 时用 alias
#   缺少的值（名字、通道、指纹）交互询问；非交互时必须在命令行给出。
#
#   --channel-dir 通道在本机上时（file:///opt/UniTAO/channel/... 或 --dir /opt/UniTAO/channel...）挂到 /opt/UniTAO/channel 的
#                 宿主机目录，两个 site 在同一台主机上就给同一个目录；默认是实例目录里的 channel/。写进 .env 的 UNITAO_CHANNEL_DIR，
#                 已有别的 vDS 用本机通道时不能再改
#   --service     compose 里不止一个 Data Service 时指定是哪个
#   --no-restart  add / remove 后不重启 Data Service（配置在启动时生效，默认在它运行时重启它）
#
# add 在 config/vds/<kind>/<name>/ 下生成该 vDS 的文件：
#   shared:   signing.pem（签名私钥，不离开本实例）、fingerprint（交给 mirror 作 --fingerprint），
#             recipients（subscriber 白名单，Data Service 启动时建；把每个 mirror 的 recipient 追加进去再重启）
#   mirrored: identity（解密私钥，不离开本实例）、recipient（公钥，交给 shared 追加进它的 recipients）
#   git 通道的克隆默认在 data/vds/<kind>/<name>/channel。仓库是 /opt/UniTAO/channel 下的 file:// 且不存在时，建一个空的裸仓库。
# remove 从 config.json 删掉这一项，删除它的文件和克隆（其它 vDS 还在用的除外）。重启后 Data Service 删除该 vDS 的表
#   并从 Inventory 注销；通道上的内容（manifest、进度文件）不动。

set -euo pipefail
SELF="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
command -v python3 >/dev/null || { echo "unitao_vds.sh needs python3 (to edit config.json)" >&2; exit 1; }

usage() { sed -n '/^# 用法:/,/^# add 在/s/^# \{0,1\}//p' "$SELF" | sed '$d'; }
die() { echo "$*" >&2; exit 1; }
interactive=0; [ -t 0 ] && interactive=1
ask() {
    local prompt=$1 def=${2:-} val=
    [ "$interactive" = 1 ] || { [ -n "$def" ] && { printf '%s' "$def"; return; }; die "$prompt: missing, give it on the command line"; }
    while :; do
        read -rp "$prompt${def:+ [$def]}: " val
        val=${val:-$def}
        [ -n "$val" ] && break
    done
    printf '%s' "$val"
}
# 交互输入的 ~ 不会被 shell 展开，这里展开后转成绝对路径
abs_path() { local p=$1; case $p in "~") p=$HOME ;; "~/"*) p=$HOME/${p#"~/"} ;; esac; realpath -m "$p"; }

service= restart=1 recreate=0
while [ $# -gt 0 ]; do
    case $1 in
        --service) service=${2:?--service needs a value}; shift 2 ;;
        --no-restart) restart=0; shift ;;
        -h|--help) usage; exit 0 ;;
        *) break ;;
    esac
done
case ${1:-} in
    ""|list|add|show|remove) target=$PWD ;;
    *) target=$1; shift ;;
esac
[ -f "$target/docker-compose.yml" ] || die "no docker-compose.yml in $target: give the directory of a Data Service deployed with unitao_deploy.sh"
cd "$target"

if [ -f .env ]; then set -a; . ./.env; set +a; fi
RUN_AS="${UNITAO_UID:-$(id -u)}:${UNITAO_GID:-$(id -g)}"

# ---- compose 的设置：B 服务 源 目标（bind mount）、C 服务 路径（-config）、D 服务（Data Service）、I 服务 镜像 ----
load_facts() {
mapfile -t facts < <(docker compose config | awk '
    /^[^ ]/ { sec = $1; next }
    sec != "services:" { next }
    /^  [^ ]/ { svc = $1; sub(/:$/, "", svc); next }
    /^    command:$/ { cmd = 1; next }
    cmd { w = $0; sub(/^ *- */, "", w); if (w == "DataService") print "D\t" svc; cmd = 0 }
    /^    image:/ { print "I\t" svc "\t" $2; next }
    /^      - type: bind$/ { b = 1; next }
    /^      - type:/ { b = 0; next }
    b && /^        source:/ { src = $0; sub(/^ *source: */, "", src); next }
    b && /^        target:/ { tgt = $0; sub(/^ *target: */, "", tgt); print "B\t" svc "\t" src "\t" tgt; b = 0; next }
    /^      - -config$/ { c = 1; next }
    c { p = $0; sub(/^ *- */, "", p); print "C\t" svc "\t" p; c = 0; next }')
}
load_facts
fact() { local f; for f in "${facts[@]}"; do [[ $f == "$1"$'\t'* ]] && echo "${f#*$'\t'}"; done; true; }

mapfile -t dss < <(fact D)
if [ -n "$service" ]; then
    printf '%s\n' "${dss[@]}" | grep -qx "$service" || die "[$service] is not a Data Service in docker-compose.yml (Data Services: ${dss[*]:-none})"
    svc=$service
elif [ ${#dss[@]} -eq 1 ]; then
    svc=${dss[0]}
elif [ ${#dss[@]} -eq 0 ]; then
    die "no Data Service (command DataService) in $target/docker-compose.yml"
else
    die "several Data Services in docker-compose.yml (${dss[*]}): choose one with --service"
fi
IMAGE=$(fact I | awk -F'\t' -v s="$svc" '$1 == s { print $2 }')

# 容器内路径 -> "宿主机路径<TAB>挂载源<TAB>挂载目标"（按该服务的 bind mount）
resolve() {
    local s src tgt
    while IFS=$'\t' read -r s src tgt; do
        [ "$s" = "$svc" ] || continue
        if [ "$1" = "$tgt" ] || [[ $1 == "$tgt"/* ]]; then
            printf '%s\t%s\t%s\n' "$src${1#"$tgt"}" "$src" "$tgt"
            return 0
        fi
    done < <(fact B)
    return 1
}
host_of() { local h; IFS=$'\t' read -r h _ _ < <(resolve "$1") || return 1; echo "$h"; }
rel() { realpath -m --relative-to=. "$1"; }
mounted() { local src tgt; IFS=$'\t' read -r _ src tgt < <(resolve "$1"); echo "$src:$tgt"; }
admin() { local m; m=$(mounted "$1"); shift; docker run --rm --user "$RUN_AS" -v "$m" "$IMAGE" DataServiceAdmin "$@"; }

cfg=$(fact C | awk -F'\t' -v s="$svc" '$1 == s { print $2 }')
[ -n "$cfg" ] || die "$svc has no -config argument"
cfg_host=$(host_of "$cfg") || die "$svc: $cfg is not in a bind mount"
[ -f "$cfg_host" ] || die "$(rel "$cfg_host") does not exist"
KEYROOT="$(dirname "$cfg")/vds"
DATAROOT=/opt/UniTAO/data/vds
CHANNEL=/opt/UniTAO/channel

# ---- config.json ----
cfgpy() {
    python3 - "$cfg_host" "$@" <<'PY'
import json, sys
path, op, *args = sys.argv[1:]
with open(path) as f:
    cfg = json.load(f)
vds = cfg.get("vds") or {}

def lname(kind, e):
    return (e.get("alias") or e["name"]) if kind == "mirrored" else e["name"]

def find(kind, name):
    for i, e in enumerate(vds.get(kind) or []):
        if lname(kind, e) == name:
            return i, e
    return None, None

def files(e):
    ch = e.get("channel") or {}
    for field, value in (("signingKeyFile", e.get("signingKeyFile")),
                         ("recipientsFile", (ch.get("encrypt") or {}).get("recipientsFile")),
                         ("identityFile", (ch.get("decrypt") or {}).get("identityFile")),
                         ("workDir", ch.get("workDir"))):
        if value:
            print(field + "\t" + value)

def save():
    with open(path, "w") as f:
        json.dump(cfg, f, indent=4, ensure_ascii=False)
        f.write("\n")

if op == "list":
    for kind in ("shared", "mirrored"):
        for e in vds.get(kind) or []:
            ch = e.get("channel") or {}
            where = ch.get("repo") or ch.get("path") or ""
            if ch.get("folder"):
                where += " " + ch["folder"]
            print("%s\t%s\t%s" % (kind, lname(kind, e), where))
elif op == "get":
    _, e = find(args[0], args[1])
    if e is None:
        sys.exit(3)
    print(json.dumps(e))
elif op == "field":
    _, e = find(args[0], args[1])
    for p in args[2].split("."):
        e = (e or {}).get(p)
    print(e or "")
elif op == "files":
    _, e = find(args[0], args[1])
    files(e)
elif op == "otherfiles":
    _, mine = find(args[0], args[1])
    for kind in ("shared", "mirrored"):
        for e in vds.get(kind) or []:
            if e is not mine:
                files(e)
elif op == "add":
    kind, pairs = args[0], args[1:]
    e = {}
    for pair in pairs:
        key, value = pair.split("=", 1)
        if key.endswith("Sec") or key.endswith("Hours"):
            value = int(value)
        node = e
        parts = key.split(".")
        for p in parts[:-1]:
            node = node.setdefault(p, {})
        node[parts[-1]] = value
    if "vds" not in cfg:
        cfg["vds"] = vds = {"local": {}}  # 加 shared / mirrored 时保留 local（没有 vds 段时它是默认）
    vds.setdefault(kind, []).append(e)
    save()
elif op == "remove":
    i, _ = find(args[0], args[1])
    del vds[args[0]][i]
    if not vds[args[0]]:
        del vds[args[0]]
    save()
PY
}

check_kind() { [ "${1:-}" = shared ] || [ "${1:-}" = mirrored ] || die "expect shared or mirrored, got [${1:-}]"; }
check_name() { [[ ${1:-} =~ ^[A-Za-z0-9_-]+$ ]] || die "invalid vDS name [${1:-}]: only [A-Za-z0-9_-]"; }
exists() { cfgpy get "$1" "$2" >/dev/null 2>&1; }

restart_ds() {
    if [ "$restart" = 0 ]; then
        if [ "$recreate" = 1 ]; then echo "-> recreate the Data Service to apply (the channel mount changed): docker compose up -d $svc (in $target)"
        else echo "-> restart the Data Service to apply: docker compose restart $svc (in $target)"; fi
    elif [ -n "$(docker compose ps -q --status running "$svc" 2>/dev/null)" ]; then
        if [ "$recreate" = 1 ]; then
            docker compose up -d "$svc" >/dev/null
            echo "== recreated $svc (the channel mount changed)"
        else
            docker compose restart "$svc" >/dev/null
            echo "== restarted $svc"
        fi
    else
        echo "-> $svc is not running; the change applies when it starts (docker compose up -d in $target)"
    fi
}

do_list() {
    local out; out=$(cfgpy list)
    if [ -z "$out" ]; then echo "no shared / mirrored vDS in $(rel "$cfg_host")"; else echo "$out" | sed 's|\t|/|' | column -t -s $'\t'; fi
}

do_show() {
    local kind=$1 name=$2 key list host fp r n
    exists "$kind" "$name" || die "$kind/$name is not in $(rel "$cfg_host")"
    if [ "$kind" = shared ]; then
        key=$(cfgpy field shared "$name" signingKeyFile)
        host=$(host_of "$key") || die "$key is not in a bind mount"
        fp="$(dirname "$host")/fingerprint"
        [ -s "$fp" ] || admin "$key" fingerprint -key "$key" > "$fp"
        echo "-> shared/$name: fingerprint (in $(rel "$fp")), every mirror needs it (add mirrored $name --fingerprint ...):"
        echo "   $(cat "$fp")"
        list=$(cfgpy field shared "$name" channel.encrypt.recipientsFile)
        host=$(host_of "$list") || return 0
        n=0; [ -f "$host" ] && n=$(grep -cv '^[[:space:]]*\(#\|$\)' "$host" || true)
        echo "-> shared/$name: subscriber whitelist $(rel "$host") has $n recipient(s); append each mirror's recipient, then restart"
    else
        key=$(cfgpy field mirrored "$name" channel.decrypt.identityFile)
        host=$(host_of "$key") || die "$key is not in a bind mount"
        r="$(dirname "$host")/recipient"
        [ -s "$r" ] || die "$(rel "$r") is missing"
        echo "-> mirrored/$name: recipient (in $(rel "$r")), append it to the recipients of the shared vDS on the publishing site:"
        echo "   $(cat "$r")"
        echo "   subscriber id: $(base64 -d <"$r" | sha256sum | cut -d' ' -f1)"
    fi
}

# 通道是否在本机的通道目录里：file:///opt/UniTAO/channel/... 的仓库，或 /opt/UniTAO/channel 下的 dir 通道
local_channel() { [[ $1 == file://$CHANNEL/* || $1 == "$CHANNEL" || $1 == "$CHANNEL"/* ]]; }

# 定下挂到 /opt/UniTAO/channel 的宿主机目录（.env 的 UNITAO_CHANNEL_DIR）。已有别的 vDS 用本机通道时沿用，不能改
choose_channel_dir() {
    local want=$1 cur new
    cur=$(host_of "$CHANNEL") || die "$CHANNEL is not mounted into $svc: add the mount to docker-compose.yml"
    if cfgpy list | cut -f3 | grep -qE "^(file://)?$CHANNEL(/| |\$)"; then
        [ -z "$want" ] || [ "$(abs_path "$want")" = "$cur" ] || die "other vDS here use the channel directory $cur, it cannot change to $want"
        echo "-> channel directory on this host: $cur (used by the other vDS here too)"
        return 0
    fi
    new=$want
    [ -n "$new" ] || new=$(ask "host directory mounted at $CHANNEL (on one host, give both sites the same directory)" "$cur")
    new=$(abs_path "$new")
    mkdir -p "$new"
    [ "$new" != "$cur" ] || return 0
    python3 - "$new" <<'PY'
import sys
lines = open(".env").read().splitlines() if __import__("os").path.exists(".env") else []
lines = [l for l in lines if not l.startswith("UNITAO_CHANNEL_DIR=")] + ["UNITAO_CHANNEL_DIR=" + sys.argv[1]]
open(".env", "w").write("\n".join(lines) + "\n")
PY
    export UNITAO_CHANNEL_DIR=$new   # 开头 source 过 .env，环境变量优先于 .env，要一起改
    load_facts
    recreate=1
    echo "== channel directory on this host: $new (UNITAO_CHANNEL_DIR in .env)"
}

# file:///opt/UniTAO/channel/... 的仓库不存在时建一个空的裸仓库（发布方第一次 push 建分支）
init_local_repo() {
    local repo=$1 branch=${2:-main} path host
    [[ $repo == file://$CHANNEL/* ]] || return 0
    path=${repo#file://}
    host=$(host_of "$path") || return 0
    [ -e "$host" ] && return 0
    docker run --rm --user "$RUN_AS" -e HOME=/tmp -v "$(mounted "$path")" --entrypoint git "$IMAGE" init -q --bare --initial-branch="$branch" "$path"
    echo "== created the empty bare repository $(rel "$host")"
}

do_add() {
    local kind=$1; shift
    local name= repo= dir= chdir= branch= folder= workdir= fingerprint= alias= pairs=() opt val asked=0
    check_kind "$kind"
    if [ $# -gt 0 ] && [[ $1 != --* ]]; then name=$1; shift; fi
    while [ $# -gt 0 ]; do
        opt=$1; val=${2:-}; [ $# -ge 2 ] || die "$opt needs a value"
        case $kind:$opt in
            *:--repo) repo=$val ;;
            *:--dir) dir=$val ;;
            *:--channel-dir) chdir=$val ;;
            *:--branch) branch=$val ;;
            *:--folder) folder=$val ;;
            *:--workdir) workdir=$val ;;
            shared:--publish-interval) pairs+=("publishIntervalSec=$val") ;;
            shared:--subscriber-timeout) pairs+=("subscriberTimeoutHours=$val") ;;
            shared:--compact-interval) pairs+=("compactIntervalHours=$val") ;;
            shared:--history-reuse) pairs+=("historyReuseHours=$val") ;;
            mirrored:--fingerprint) fingerprint=$val ;;
            mirrored:--alias) check_name "$val"; alias=$val ;;
            mirrored:--pull-interval) pairs+=("pullIntervalSec=$val") ;;
            *) die "unknown option [$opt] for add $kind (-h)" ;;
        esac
        case $opt in --*-interval|--*-timeout|--history-reuse) [[ $val =~ ^[0-9]+$ ]] || die "$opt needs a number" ;; esac
        shift 2
    done
    [ -n "$name" ] || name=$(ask "name of the $kind vDS" "")
    check_name "$name"
    local lname=${alias:-$name}
    exists "$kind" "$lname" && die "$kind/$lname is already in $(rel "$cfg_host")"
    [ -z "$repo" ] || [ -z "$dir" ] || die "use either --repo or --dir"
    if [ -z "$repo" ] && [ -z "$dir" ]; then
        [ "$interactive" = 1 ] || die "add $kind needs --repo <git url> or --dir <path>"
        asked=1
        case $(ask "channel type, git or dir" "git") in
            git) repo=$(ask "git repository (a git server, or file://$CHANNEL/<repo>.git on this host)" "file://$CHANNEL/exchange.git") ;;
            dir) dir=$(ask "channel directory in the container" "$CHANNEL") ;;
            *) die "channel type must be git or dir" ;;
        esac
    fi
    if [ -z "$folder" ] && [ "$asked" = 1 ]; then
        folder=$(ask "folder of the vDS in the channel (the same on both sites)" "$name")
    fi
    if [ "$kind" = mirrored ] && [ -z "$fingerprint" ]; then
        fingerprint=$(ask "fingerprint of shared/$name (its fingerprint file on the publishing site, SHA256:...)" "")
    fi
    [ "$kind" = shared ] || [[ $fingerprint =~ ^SHA256:[0-9a-f]{64}$ ]] || die "fingerprint must be SHA256: and 64 hex digits, got [$fingerprint]"
    if local_channel "${repo:-$dir}"; then
        choose_channel_dir "$chdir"
    else
        [ -z "$chdir" ] || die "--channel-dir is for a channel on this host (file://$CHANNEL/... or --dir $CHANNEL...)"
    fi

    local keydir="$KEYROOT/$kind/$lname" keyhost
    if [ "$kind" = shared ]; then
        pairs=("name=$name" "signingKeyFile=$keydir/signing.pem" "${pairs[@]}")
    else
        pairs=("name=$name" ${alias:+"alias=$alias"} "trustedFingerprint=$fingerprint" "${pairs[@]}")
    fi
    if [ -n "$repo" ]; then
        workdir=${workdir:-$DATAROOT/$kind/$lname/channel}
        pairs+=("channel.type=git" "channel.repo=$repo")
        [ -z "$branch" ] || pairs+=("channel.branch=$branch")
        [ -z "$folder" ] || pairs+=("channel.folder=$folder")
        pairs+=("channel.workDir=$workdir")
        host_of "$workdir" >/dev/null || echo "!! $workdir is not in a bind mount: the clone is lost when the container is recreated (it is fetched again)" >&2
    else
        [ -z "$branch$workdir" ] || die "--branch / --workdir are for a git channel (--repo)"
        pairs+=("channel.type=dir" "channel.path=$dir")
        [ -z "$folder" ] || pairs+=("channel.folder=$folder")
        host_of "$dir" >/dev/null || echo "!! $dir is not in a bind mount of $svc: mount the channel directory into the container" >&2
    fi

    keyhost=$(host_of "$keydir") || die "$keydir is not in a bind mount"
    mkdir -p "$keyhost"
    if [ "$kind" = shared ]; then
        [ -e "$keyhost/signing.pem" ] || admin "$keydir" keygen -out "$keydir/signing.pem" >/dev/null
        admin "$keydir" fingerprint -key "$keydir/signing.pem" > "$keyhost/fingerprint"
        pairs+=("channel.encrypt.recipientsFile=$keydir/recipients")
    else
        [ -e "$keyhost/identity" ] || admin "$keydir" enckey -identity "$keydir/identity" -recipient "$keydir/recipient" >/dev/null
        pairs+=("channel.decrypt.identityFile=$keydir/identity")
    fi
    [ -z "$repo" ] || init_local_repo "$repo" "$branch"
    cfgpy add "$kind" "${pairs[@]}"
    echo "== added $kind/$lname to $(rel "$cfg_host"); its files are in $(rel "$keyhost")"
    do_show "$kind" "$lname"
    restart_ds
}

do_remove() {
    local kind=$1 name=$2 line field path host others failed=0 d
    exists "$kind" "$name" || die "$kind/$name is not in $(rel "$cfg_host")"
    local -a mine; mapfile -t mine < <(cfgpy files "$kind" "$name")
    others=$(cfgpy otherfiles "$kind" "$name" | cut -f2)
    cfgpy remove "$kind" "$name"
    echo "== removed $kind/$name from $(rel "$cfg_host")"
    for line in "${mine[@]}" "keydir"$'\t'"$KEYROOT/$kind/$name"; do
        field=${line%%$'\t'*}; path=${line#*$'\t'}
        if grep -qxF "$path" <<<"$others"; then echo "-> kept $path (used by another vDS)"; continue; fi
        host=$(host_of "$path") || { echo "-> $path ($field) is not in a bind mount, nothing to delete"; continue; }
        [ -e "$host" ] || continue
        if rm -rf "$host" 2>/dev/null; then echo "== deleted $(rel "$host")"; else echo "!! could not delete $(rel "$host") (owned by another user?)" >&2; failed=1; fi
    done
    for d in "$KEYROOT/$kind" "$KEYROOT" "$DATAROOT/$kind/$name" "$DATAROOT/$kind" "$DATAROOT"; do
        host=$(host_of "$d") && rmdir "$host" 2>/dev/null || true
    done
    restart_ds
    echo "-> the Data Service drops the table of $kind/$name and unregisters it from Inventory when it starts; the channel is left as it is"
    [ "$failed" = 0 ]
}

# 交互菜单里选一个已有的 vDS，输出 "kind name"
pick() {
    local -a items; mapfile -t items < <(cfgpy list | cut -f1,2 | tr '\t' '/')
    [ ${#items[@]} -gt 0 ] || die "no shared / mirrored vDS yet"
    local PS3="which vDS: " item
    select item in "${items[@]}"; do [ -n "$item" ] && break; done
    echo "${item%%/*} ${item#*/}"
}

menu() {
    [ "$interactive" = 1 ] || { usage >&2; exit 1; }
    echo "vDS of $svc in $target"
    while :; do
        local PS3="action: " action kn
        select action in list "add shared" "add mirrored" show remove quit; do [ -n "$action" ] && break; done
        case $action in
            list) (do_list) || true ;;
            "add shared") (do_add shared) || true ;;
            "add mirrored") (do_add mirrored) || true ;;
            show) (kn=$(pick) && do_show $kn) || true ;;
            remove) (kn=$(pick) && read -rp "remove ${kn/ //}, its keys and its table? [y/N] " a && [[ $a == [yY]* ]] && do_remove $kn) || true ;;
            quit) break ;;
        esac
        echo
    done
}

[ $# -gt 0 ] || { menu; exit 0; }
cmd=$1; shift
case $cmd in
    list) do_list ;;
    show) check_kind "${1:-}"; check_name "${2:-}"; do_show "$1" "$2" ;;
    add) [ $# -gt 0 ] || die "add shared|mirrored ..."; do_add "$@" ;;
    remove) check_kind "${1:-}"; check_name "${2:-}"; do_remove "$1" "$2" ;;
    *) usage >&2; exit 1 ;;
esac
