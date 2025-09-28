<#
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
#>

param (
    [Parameter(Mandatory=$true)]
    [string]$DockerRegUrl
)

$scriptRoot = $PSScriptRoot

$dirName = $(Split-Path -Path $scriptRoot -Leaf)

$imageName = "UniTAO/$($dirName)".ToLower()

$srcImageName = "$($imageName):localbuild"

$tgtImageName = "$DockerRegUrl/$($imageName):latest"

$img = docker images --format "{{.ID}}\t{{.Repository}}:{{.Tag}}" |
       Select-String $srcImageName |
       ForEach-Object { $_.ToString().Split("`t")[0] }

if (-not $img) {
    Write-Host "镜像不存在"
    # 这里写不存在时的逻辑
    exit 1
} else {
    Write-Host "镜像存在,ID: $img"
}

tgtImg = docker images --format "{{.ID}}\t{{.Repository}}:{{.Tag}}" |
       Select-String $tgtImageName |
       ForEach-Object { $_.ToString().Split("`t")[0] }

if ($tgtImg) {
    Write-Host "目标镜像已存在，ID: $tgtImg"
    if ($tgtImg.ID -ne $img.ID) {
        Write-Host "目标镜像Outdated, 重新生成新的镜像"
        docker tag $srcImageName $tgtImageName
    }
}

Write-Host "推送镜像到 $tgtImageName"
docker push $tgtImageName
