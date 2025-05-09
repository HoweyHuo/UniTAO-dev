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
docker-compose down

Write-Host "clear $PSScriptRoot\InventoryService\data\referral/*"
Remove-Item -Force -Recurse $PSScriptRoot\InventoryService\data\referral\*
Get-ChildItem $PSScriptRoot\InventoryService\data\referral\
Write-Host "clear $PSScriptRoot\InventoryService\data\schema\*"
Remove-Item -Force -Recurse $PSScriptRoot\InventoryService\data\schema\*
Get-ChildItem $PSScriptRoot\InventoryService\data\schema\
Write-Host "remove $PSScriptRoot\logs"
if (Test-Path -Path $PSScriptRoot\logs) {
    Write-Host "log path exists, delete []"
    Remove-Item -Path $PSScriptRoot\logs -Recurse
}
Write-Host "removed"
Write-Host "remove database file $PSScriptRoot\MongoDB\db\*"
Remove-Item -Force -Recurse $PSScriptRoot\MongoDB\db\*


