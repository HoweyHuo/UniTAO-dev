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

$local_img_id = docker images --format "{{.ID}}\t{{.Repository}}:{{.Tag}}" |
       Select-String $srcImageName |
       ForEach-Object { $_.ToString().Split("`t")[0] }

if (-not $local_img_id) {
    Write-Host "Image does not exists"
    # 这里写不存在时的逻辑
    exit 1
} else {
    Write-Host "Image Exists,ID: $local_img_id"
}

$tgt_img_id = docker images --format "{{.ID}}\t{{.Repository}}:{{.Tag}}" |
       Select-String $tgtImageName |
       ForEach-Object { $_.ToString().Split("`t")[0] }

if ($tgt_img_id) {
    Write-Host "target image already exists,ID: $tgt_img_id"
    if ($tgt_img_id -ne $local_img_id) {
        Write-Host "target image outdated, retagging new image"
        docker tag $srcImageName $tgtImageName
    }
} else {
    Write-Host "target image does not exists, tagging new image"
    docker tag $srcImageName $tgtImageName
}

Write-Host "pushing image to $($tgtImageName)"
docker push $tgtImageName

Write-Host "remove target image $($tgtImageName) from local"
docker rmi $tgtImageName
