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
# ************************************************************************************************************


remoteDockerRegUrl=$1

SCRIPT_DIR="$( cd -- "$( dirname -- "${BASH_SOURCE[0]:-$0}"; )" &> /dev/null && pwd 2> /dev/null; )";
IMAGE_NAME="$(echo "UniTAO/${SCRIPT_DIR##*/}" | awk '{print tolower($0)}';)"

pushd $SCRIPT_DIR/../../

srcImageName="$IMAGE_NAME:localbuild"
tgtImageName="$remoteDockerRegUrl/$IMAGE_NAME:latest"

# 获取 ID（不存在时返回空串）
id_local=$(docker images -q "$srcImageName")
id_remote=$(docker images -q "$tgtImageName")

# 1. 本地镜像不存在
if [ -z "$id_local" ]; then
  echo "本地镜像 $srcImageName 不存在"
  exit 1
fi

# 2. 远程仓库不存在该镜像
if [ -z "$id_remote" ]; then
  echo "远程镜像不存在，直接打 tag"
  docker tag "$LOCAL_IMG" "$REMOTE_REPO"
  exit 0
fi

# 3. ID 比对
if [ "$id_local" != "$id_remote" ]; then
  echo "ID 不同，重新打 tag"
  docker tag "$LOCAL_IMG" "$REMOTE_REPO"
else
  echo "ID 相同，无需操作"
fi

# push to remote registry
docker push "$tgtImageName"

# remove the local image with tag localbuild
docker rmi $srcImageName

# remove the remote image with tag latest
docker rmi $tgtImageName

# remove the empty image from previous command
sudo docker images --filter "dangling=true" -q --no-trunc | xargs -r sudo docker rmi
