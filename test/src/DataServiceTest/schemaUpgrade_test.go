/*
************************************************************************************************************
Copyright (c) 2022 Salesforce, Inc.
All rights reserved.

UniTAO was originally created in 2022 by Shai Herzog & Yi Huo as an
Universal No-Coding Heterogeneous Infrastructure Maintenance & Inventory system that is holistically driven by open/community-developed semantic models/schemas.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published
by the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>

This copyright notice and license applies to all files in this directory or sub-directories, except when stated otherwise explicitly.
************************************************************************************************************
*/

package DataServiceTest

import (
	"net/http"
	"testing"

	"github.com/salesforce/UniTAO/lib/Schema/JsonKey"
	"github.com/salesforce/UniTAO/lib/Schema/Record"
)

const (
	upgradeSchemaV1 = `{
		"__id": "upgradable",
		"__type": "schema",
		"__ver": "0.0.1",
		"data": {
			"name": "upgradable",
			"version": "0.0.1",
			"properties": {
				"attr1": {
					"type": "string"
				},
				"attr2": {
					"type": "string"
				}
			}
		}
	}`
	upgradeSchemaV2 = `{
		"__id": "upgradable",
		"__type": "schema",
		"__ver": "0.0.1",
		"data": {
			"name": "upgradable",
			"version": "0.0.2",
			"properties": {
				"attr1": {
					"type": "string"
				},
				"attr2": {
					"type": "string"
				},
				"attr3": {
					"type": "string",
					"required": false
				}
			}
		}
	}`
	upgradeSchemaLower = `{
		"__id": "upgradable",
		"__type": "schema",
		"__ver": "0.0.1",
		"data": {
			"name": "upgradable",
			"version": "0.0.0",
			"properties": {
				"attr1": {
					"type": "string"
				}
			}
		}
	}`
	upgradeDataV1 = `{
		"__id": "item01",
		"__type": "upgradable",
		"__ver": "0.0.1",
		"data": {
			"attr1": "a",
			"attr2": "b"
		}
	}`
	upgradeSchemaArchivedPost = `{
		"__id": "upgradable__0.0.1",
		"__type": "schema",
		"__ver": "0.0.1",
		"data": {
			"name": "upgradable__0.0.1",
			"version": "0.0.3",
			"properties": {
				"attr1": {
					"type": "string"
				}
			}
		}
	}`
)

// 升级契约：同一 __id + 严格更高 data.version，旧版本归档为 <type>__<old version>，
// 旧版本数据仍按归档 schema 校验；相同/更低版本被拒绝（400，而不是落到 duplicate key 500）。
func TestSchemaUpgradeFlow(t *testing.T) {
	handler, ex := MockHandler()
	if ex != nil {
		t.Fatalf(ex.Error())
	}
	assertAddOK := func(recordStr string) {
		t.Helper()
		if err := AddData(handler, recordStr); err != nil {
			t.Fatalf("failed to add record. Error: %s", err)
		}
	}
	assertAddRejected400 := func(recordStr string, what string) {
		t.Helper()
		err := AddData(handler, recordStr)
		if err == nil {
			t.Fatalf("%s should be rejected", what)
		}
		if err.Status != http.StatusBadRequest {
			t.Fatalf("%s should return %d, got %d: %s", what, http.StatusBadRequest, err.Status, err)
		}
	}

	assertAddOK(upgradeSchemaV1)
	assertAddOK(upgradeDataV1)

	// 相同版本不能升级
	assertAddRejected400(upgradeSchemaV1, "schema with the same version")
	// 更低版本不能升级
	assertAddRejected400(upgradeSchemaLower, "schema with a lower version")

	assertAddOK(upgradeSchemaV2)

	schemaList, err := handler.List(JsonKey.Schema)
	if err != nil {
		t.Fatalf("failed to list schema: %s", err)
	}
	seen := map[string]bool{}
	for _, idObj := range schemaList {
		seen[idObj.(string)] = true
	}
	if !seen["upgradable"] {
		t.Fatalf("current schema [upgradable] should be listed, got %v", schemaList)
	}
	if !seen["upgradable__0.0.1"] {
		t.Fatalf("upgraded schema should be archived as [upgradable__0.0.1], got %v", schemaList)
	}

	current, err := handler.LocalSchema("upgradable", "")
	if err != nil {
		t.Fatalf("failed to load current schema: %s", err)
	}
	if current.Schema.Version != "0.0.2" {
		t.Fatalf("current schema version = [%s], want [0.0.2]", current.Schema.Version)
	}

	archived, err := handler.LocalSchema("upgradable", "0.0.1")
	if err != nil {
		t.Fatalf("failed to load archived schema: %s", err)
	}
	if archived.Schema.Version != "0.0.1" {
		t.Fatalf("archived schema version = [%s], want [0.0.1]", archived.Schema.Version)
	}
	oldRecord, e := Record.LoadStr(upgradeDataV1)
	if e != nil {
		t.Fatalf("failed to load old record: %s", e)
	}
	if e := archived.ValidateRecord(oldRecord); e != nil {
		t.Fatalf("record written under the archived version should still validate. Error: %s", e)
	}

	// 新写入必须使用当前版本；归档后的 schema id 不能再直接 POST
	assertAddRejected400(upgradeDataV1, "new data under the archived version")
	assertAddRejected400(upgradeSchemaArchivedPost, "POST of the archived schema id [upgradable__0.0.1]")
}
