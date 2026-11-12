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
#   recipient list <name>                               列出 shared/<name> 的 subscriber 白名单（recipient 和 subscriber id）
#   recipient add <name> [<recipient>] [--note <说明>]   把一个 mirror 的 recipient（它 add / show 时打印的）加进白名单
#   recipient remove <name> [<recipient|subscriber id>]  从白名单删除
#   缺少的值（名字、通道、指纹）交互询问；非交互时必须在命令行给出。
#
#   --branch      git 通道的分支，两边要相同；默认是仓库的默认分支（经 Data Service 的 ssh 密钥 / 凭据读远端 HEAD），读不到时 main
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
# recipient add / remove 改 shared 的 recipients 文件，然后重启 Data Service（白名单在启动时读取）。
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
    ""|list|add|show|remove|recipient) target=$PWD ;;
    *) target=$1; shift ;;
esac
[ -f "$target/docker-compose.yml" ] || die "no docker-compose.yml in $target: give the directory of a Data Service deployed with unitao_deploy.sh"
cd "$target"
target=$PWD   # 菜单的动作以子进程重新运行本脚本，要用绝对路径

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
        echo "-> shared/$name: subscriber whitelist $(rel "$host") has $n recipient(s); add each mirror's recipient with: recipient add $name <recipient>"
    else
        key=$(cfgpy field mirrored "$name" channel.decrypt.identityFile)
        host=$(host_of "$key") || die "$key is not in a bind mount"
        r="$(dirname "$host")/recipient"
        [ -s "$r" ] || die "$(rel "$r") is missing"
        echo "-> mirrored/$name: recipient (in $(rel "$r")), add it to the whitelist of the shared vDS on the publishing site:"
        echo "   $(cat "$r")"
        echo "   unitao_vds.sh <its directory> recipient add $(cfgpy field mirrored "$name" name) $(cat "$r")"
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

# 远端仓库的默认分支（HEAD 指向的分支）。读到了（空仓库、本机还不存在的 file:// 仓库时输出空）返回 0；
# 读不到（连不上、没有权限）时在 stderr 说明原因并返回 1。
# 用 docker compose run 起一个该服务的临时容器，所以和 Data Service 用同样的 ssh 密钥（ssh 卷）、凭据和挂载
remote_branch() {
    local repo=$1 h sshdir out err env=(-e GIT_TERMINAL_PROMPT=0)
    if [[ $repo == file://* ]]; then h=$(host_of "${repo#file://}") && [ -e "$h" ] || return 0; fi
    if ! docker compose config | grep -q GIT_SSH_COMMAND; then   # compose 里自带了 GIT_SSH_COMMAND 就用它的
        sshdir=$(python3 -c 'import json,sys; print((json.load(open(sys.argv[1])).get("ssh") or {}).get("dir") or "/opt/UniTAO/ssh")' "$cfg_host")
        env+=(-e "GIT_SSH_COMMAND=ssh -i $sshdir/id_ed25519 -o IdentitiesOnly=yes -o UserKnownHostsFile=$sshdir/known_hosts -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=15")
    fi
    err=$(mktemp)
    if ! out=$(docker compose run --rm --no-deps -T "${env[@]}" "$svc" timeout 30 git ls-remote --symref "$repo" HEAD 2>"$err"); then
        echo "!! cannot read $repo: $(grep -v '^[[:space:]]*$' "$err" | grep -vi '^warning:' | head -n 1)" >&2
        rm -f "$err"
        return 1
    fi
    rm -f "$err"
    sed -n 's|^ref: refs/heads/\([^[:space:]]*\)[[:space:]]*HEAD$|\1|p' <<<"$out" | head -n 1
}

# 读不到远端时，打印 Data Service 的 ssh 公钥（config 旁边的 state.json），要先把它加到 git 服务器上
show_ssh_key() {
    local repo=$1 state key
    [[ $repo == file://* || $repo == http://* || $repo == https://* ]] && return 0
    docker compose config | grep -q GIT_SSH_COMMAND && return 0
    state="$(dirname "$cfg_host")/state.json"
    key=$(python3 -c 'import json,sys; s=json.load(open(sys.argv[1])).get("ssh") or {}; print(s.get("publicKey") or "")' "$state" 2>/dev/null || true)
    if [ -n "$key" ]; then
        echo "-> add the ssh key of the Data Service (ssh.publicKey in $(rel "$state")) to the git server, with write access:" >&2
        echo "   $key" >&2
    else
        echo "-> $(rel "$state") has no ssh.publicKey yet: start the Data Service once, it generates the key" >&2
    fi
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
    # 以 root（sudo）add 时建出的目录和文件归 root，以 UNITAO_UID 运行的 Data Service 和密钥工具都写不进、读不到。
    # remove 只删除，用 root 正好能清理这种残留
    if [ "$(id -u)" = 0 ] && [ "${RUN_AS%%:*}" != 0 ]; then
        die "do not add a vDS as root (sudo): the Data Service runs as UID ${RUN_AS%%:*} and could not use the files root creates. run it as that user (in group docker)"
    fi
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
    # 分支默认取仓库的默认分支（Gitee 常是 master），总是写进配置；Data Service 自己的缺省是 main
    if [ -n "$repo" ] && [ -z "$branch" ]; then
        echo "-> reading the default branch of $repo ..." >&2
        local def=
        if ! def=$(remote_branch "$repo"); then
            show_ssh_key "$repo"
            while [ "$interactive" = 1 ]; do
                read -rp "press Enter to read it again (after adding the key), or type the branch to use: " val
                if [ -n "$val" ]; then branch=$val; break; fi
                def=$(remote_branch "$repo") && break
            done
        fi
        if [ -z "$branch" ] && [ "$asked" = 1 ]; then
            branch=$(ask "branch (the same on both sites)" "${def:-main}")
        elif [ -z "$branch" ]; then
            branch=${def:-main}
            echo "== branch $branch${def:+ (the default branch of the repository)}" >&2
        fi
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
        pairs+=("channel.branch=$branch")
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

# ---- shared 的 subscriber 白名单（recipients 文件：每行一个 base64 X25519 公钥，# 开头是注释） ----
recpy() {
    python3 - "$@" <<'PY2'
import base64, hashlib, os, sys
op, path, *args = sys.argv[1:]
HEADER = ("# subscriber whitelist of this shared vDS: one recipient (base64 X25519 public key from\n"
          "# `DataServiceAdmin enckey`) per line, # starts a comment. restart the Data Service after editing.\n")

def subid(r):
    try:
        raw = base64.b64decode(r, validate=True)
    except Exception:
        return None
    return hashlib.sha256(raw).hexdigest() if len(raw) == 32 else None

lines = open(path).read().splitlines() if os.path.exists(path) else []

def entries():
    for i, l in enumerate(lines):
        t = l.strip()
        if t and not t.startswith("#"):
            yield i, t

def save():
    with open(path, "w") as f:
        f.write("\n".join(lines) + "\n" if lines else HEADER)

if op == "list":
    for i, r in entries():
        prev = lines[i - 1].strip() if i > 0 else ""
        note = prev[len("# note:"):].strip() if prev.startswith("# note:") else ""
        print("%s\t%s\t%s" % (subid(r) or "(invalid)", r, note))
elif op == "add":
    r, note = args[0].strip(), args[1] if len(args) > 1 else ""
    sid = subid(r)
    if sid is None:
        sys.exit("[%s] is not a recipient (base64 of a 32-byte X25519 public key)" % r)
    if any(t == r for _, t in entries()):
        print("exists\t" + sid)
        sys.exit(0)
    if not lines:
        lines = HEADER.rstrip("\n").split("\n")
    if note:
        lines.append("# note: " + note.replace("\n", " "))
    lines.append(r)
    save()
    print("added\t" + sid)
elif op == "remove":
    key = args[0].strip()
    hit = [i for i, t in entries() if t == key or subid(t) == key.lower()]
    if not hit:
        sys.exit("[%s] is not in %s" % (key, path))
    drop = set(hit)
    for i in hit:  # 连同 add 时写在它上面的说明一起删
        if i > 0 and lines[i - 1].strip().startswith("# note:"):
            drop.add(i - 1)
    removed = [subid(lines[i].strip()) for i in hit]
    lines = [l for i, l in enumerate(lines) if i not in drop]
    save()
    print("\n".join("removed\t" + x for x in removed))
PY2
}

# 输出 shared/<name> 的 recipients 文件在宿主机上的路径
recipients_host() {
    local name=$1 list
    check_name "$name"
    exists shared "$name" || die "shared/$name is not in $(rel "$cfg_host")"
    list=$(cfgpy field shared "$name" channel.encrypt.recipientsFile)
    [ -n "$list" ] || die "shared/$name has no channel.encrypt.recipientsFile"
    host_of "$list" || die "$list is not in a bind mount"
}

do_recipient() {
    local op=${1:-} name=${2:-} host key= note= out n
    [ -n "$name" ] || die "recipient list|add|remove <name> ..."
    shift 2
    host=$(recipients_host "$name")
    case $op in
        list)
            [ $# -eq 0 ] || die "recipient list <name>"
            out=$(recpy list "$host")
            if [ -z "$out" ]; then echo "shared/$name: no recipients in $(rel "$host")"; return 0; fi
            echo "shared/$name: recipients in $(rel "$host")"
            { printf 'SUBSCRIBER ID\tRECIPIENT\tNOTE\n'; echo "$out"; } | column -t -s $'\t'
            ;;
        add|remove)
            if [ $# -gt 0 ] && [[ $1 != --* ]]; then key=$1; shift; fi
            while [ $# -gt 0 ]; do
                [ $# -ge 2 ] || die "$1 needs a value"
                case $op:$1 in
                    add:--note) note=$2 ;;
                    *) die "unknown option [$1] for recipient $op (-h)" ;;
                esac
                shift 2
            done
            if [ "$op" = add ]; then
                if [ "$(id -u)" = 0 ] && [ "${RUN_AS%%:*}" != 0 ] && [ ! -e "$host" ]; then
                    die "do not add a recipient as root (sudo) before the file exists: the Data Service runs as UID ${RUN_AS%%:*}"
                fi
                [ -n "$key" ] || key=$(ask "recipient of the mirror (printed by its add / show)" "")
                [ -n "$note" ] || [ "$interactive" = 0 ] || read -rp "note (which site / mirror, optional): " note
                out=$(recpy add "$host" "$key" "$note")
                if [ "${out%%$'\t'*}" = exists ]; then echo "-> already in $(rel "$host"): subscriber ${out#*$'\t'}"; return 0; fi
                echo "== added subscriber ${out#*$'\t'} to $(rel "$host")"
            else
                if [ -z "$key" ]; then
                    [ "$interactive" = 1 ] || die "recipient remove <name> <recipient|subscriber id>"
                    local -a items; mapfile -t items < <(recpy list "$host" | awk -F'\t' '{ print $1 ($3 == "" ? "" : "  (" $3 ")") }')
                    [ ${#items[@]} -gt 0 ] || die "shared/$name has no recipients"
                    local PS3="which subscriber: " item
                    select item in "${items[@]}"; do [ -n "$item" ] && break; done
                    key=${item%% *}
                fi
                out=$(recpy remove "$host" "$key")
                echo "$out" | while IFS=$'\t' read -r _ id; do echo "== removed subscriber $id from $(rel "$host")"; done
            fi
            n=$(recpy list "$host" | grep -c . || true)
            echo "-> shared/$name: $n recipient(s)"
            restart_ds
            ;;
        *) die "recipient list|add|remove <name> ..." ;;
    esac
}

# 交互菜单里选一个已有的 shared vDS，输出名字
pick_shared() {
    local -a items; mapfile -t items < <(cfgpy list | awk -F'\t' '$1 == "shared" { print $2 }')
    [ ${#items[@]} -gt 0 ] || die "no shared vDS yet"
    local PS3="which shared vDS: " item
    select item in "${items[@]}"; do [ -n "$item" ] && break; done
    echo "$item"
}

# 交互菜单里选一个已有的 vDS，输出 "kind name"
pick() {
    local -a items; mapfile -t items < <(cfgpy list | cut -f1,2 | tr '\t' '/')
    [ ${#items[@]} -gt 0 ] || die "no shared / mirrored vDS yet"
    local PS3="which vDS: " item
    select item in "${items[@]}"; do [ -n "$item" ] && break; done
    echo "${item%%/*} ${item#*/}"
}

# 菜单的每个动作在单独的进程里运行：在 `|| true` 里 bash 会关掉 set -e，出错后会接着往下做（写进半成品的配置）
sub() {
    local opts=()
    [ -z "$service" ] || opts+=(--service "$service")
    [ "$restart" = 1 ] || opts+=(--no-restart)
    "$SELF" "${opts[@]}" "$target" "$@"
}

menu() {
    [ "$interactive" = 1 ] || { usage >&2; exit 1; }
    echo "vDS of $svc in $target"
    while :; do
        local PS3="action: " action kn
        select action in list "add shared" "add mirrored" show recipients remove quit; do [ -n "$action" ] && break; done
        case $action in
            list) sub list || true ;;
            "add shared") sub add shared || true ;;
            "add mirrored") sub add mirrored || true ;;
            show) kn=$(pick) && sub show $kn || true ;;
            recipients)
                if kn=$(pick_shared) && sub recipient list "$kn"; then
                    local PS3="recipients of shared/$kn: " ra
                    select ra in add remove back; do [ -n "$ra" ] && break; done
                    [ "$ra" = back ] || sub recipient "$ra" "$kn" || true
                fi
                ;;
            remove) kn=$(pick) && read -rp "remove ${kn/ //}, its keys and its table? [y/N] " a && [[ $a == [yY]* ]] && sub remove $kn || true ;;
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
    recipient) do_recipient "$@" ;;
    *) usage >&2; exit 1 ;;
esac
