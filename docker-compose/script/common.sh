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

# unitao_deploy.sh、unitao_vds.sh、unitao_update.sh 共用的函数，由它们 source（调用方先定义 die）。

# compose_up [<服务>]：docker compose up -d，镜像拉不下来时用本地的。
# 模板是 pull_policy: always，registry 连不上或下载卡住时 up 会一直挂着；这里先自己拉，
# PULL_STALL_SEC 秒没有任何进展（输出里没有出现新的一行）或拉取失败就放弃，有本地镜像就用它启动，没有就退出。
PULL_STALL_SEC=90
pull_image() {
    local log pid size last=-1 idle=0 rc=0
    log=$(mktemp)
    docker compose --progress plain pull "$@" >"$log" 2>&1 &
    pid=$!
    while kill -0 "$pid" 2>/dev/null; do
        sleep 5
        # 下载卡住时 compose 仍会反复打印同一行进度（Downloading 12.58MB），所以按不同的行数判断有没有进展
        size=$(sort -u "$log" | wc -l)
        if [ "$size" = "$last" ]; then idle=$((idle + 5)); else idle=0; last=$size; fi
        if [ "$idle" -ge "$PULL_STALL_SEC" ]; then
            kill "$pid" 2>/dev/null || true
            wait "$pid" 2>/dev/null || true
            rm -f "$log"
            echo "no progress for ${PULL_STALL_SEC}s"
            return 1
        fi
    done
    wait "$pid" || rc=$?
    [ "$rc" = 0 ] || grep -v '^ *$' "$log" | tail -n 1
    rm -f "$log"
    return "$rc"
}
compose_up() {
    local img reason
    img=$(docker compose config --images "$@" | head -n 1)
    if ! reason=$(pull_image "$@"); then
        docker image inspect "$img" >/dev/null 2>&1 || die "cannot pull $img ($reason) and there is no local copy of it"
        echo "!! cannot pull $img ($reason): starting with the local copy" >&2
    fi
    docker compose --progress quiet up -d --pull never "$@"
}
