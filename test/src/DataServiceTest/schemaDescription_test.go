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
	"path/filepath"
	"strings"
	"testing"

	"InventoryService/InvRecord"
	"InventoryService/RefRecord"

	"github.com/salesforce/UniTAO/lib/Schema/JsonKey"
	"github.com/salesforce/UniTAO/lib/Schema/Record"
	"github.com/salesforce/UniTAO/lib/SchemaPath"
	"github.com/salesforce/UniTAO/lib/Util"
	"github.com/salesforce/UniTAO/lib/Util/Json"
)

// 所有内置 schema 的每个字段都必须有 description，REST /schema 才能给出字段说明。
func TestBuiltinSchemaFieldsHaveDescription(t *testing.T) {
	rootDir, err := Util.RootDir()
	if err != nil {
		t.Fatalf("failed to get running dir: %s", err)
	}
	schemaFile, err := filepath.Abs(filepath.Join(rootDir, "lib/Schema/data/schema.json"))
	if err != nil {
		t.Fatalf("failed to resolve schema file path: %s", err)
	}
	raw, err := Json.LoadJSONMap(schemaFile)
	if err != nil {
		t.Fatalf("failed to load [%s]: %s", schemaFile, err)
	}
	records, ok := raw["data"].([]interface{})
	if !ok {
		t.Fatalf("[%s] has no [data] list", schemaFile)
	}
	for _, recObj := range records {
		record, e := Record.LoadMap(recObj.(map[string]interface{}))
		if e != nil {
			t.Fatalf("failed to load schema record from [%s]: %s", schemaFile, e)
		}
		checkFieldDescriptions(t, "schema.json/"+record.Id, record.Data)
	}

	embedded := []struct {
		label  string
		record string
	}{
		{"record.go", Record.Schema},
		{"invRecord.go", InvRecord.SchemaRecord},
		{"refRecord.go", RefRecord.SchemaRecord},
		{"cmdQueryPathName.go", SchemaPath.PathDataSchema},
	}
	for _, src := range embedded {
		record, e := Record.LoadStr(src.record)
		if e != nil {
			t.Fatalf("%s: failed to load embedded schema record: %s", src.label, e)
		}
		checkFieldDescriptions(t, src.label+"/"+record.Id, record.Data)
	}
}

// schema-of-schema 的 description 必须把升级契约写清楚：严格更高版本、归档 id、
// 旧版本数据继续可校验，以及 __ver 到底是谁的版本。
func TestSchemaUpgradeContractDocumented(t *testing.T) {
	rootDir, err := Util.RootDir()
	if err != nil {
		t.Fatalf("failed to get running dir: %s", err)
	}
	schemaFile, err := filepath.Abs(filepath.Join(rootDir, "lib/Schema/data/schema.json"))
	if err != nil {
		t.Fatalf("failed to resolve schema file path: %s", err)
	}
	raw, err := Json.LoadJSONMap(schemaFile)
	if err != nil {
		t.Fatalf("failed to load [%s]: %s", schemaFile, err)
	}
	var schemaRecord *Record.Record
	for _, recObj := range raw["data"].([]interface{}) {
		record, e := Record.LoadMap(recObj.(map[string]interface{}))
		if e != nil {
			t.Fatalf("failed to load schema record from [%s]: %s", schemaFile, e)
		}
		if record.Id == JsonKey.Schema {
			schemaRecord = record
			break
		}
	}
	if schemaRecord == nil {
		t.Fatalf("[%s] does not contain the [%s] schema record", schemaFile, JsonKey.Schema)
	}

	schemaDesc, _ := schemaRecord.Data[JsonKey.Description].(string)
	if !strings.Contains(schemaDesc, "upgrade") {
		t.Errorf("schema description should mention the upgrade contract, got: %s", schemaDesc)
	}

	props, ok := schemaRecord.Data[JsonKey.Properties].(map[string]interface{})
	if !ok {
		t.Fatalf("[%s] schema should declare properties", JsonKey.Schema)
	}
	versionDef, ok := props["version"].(map[string]interface{})
	if !ok {
		t.Fatalf("[%s] schema should declare [version]", JsonKey.Schema)
	}
	versionDesc, _ := versionDef[JsonKey.Description].(string)
	for _, want := range []string{"strictly higher", "<type>__<old version>", "archived", "__ver", "No-Record-Framework"} {
		if !strings.Contains(versionDesc, want) {
			t.Errorf("[version] description should mention [%s], got: %s", want, versionDesc)
		}
	}
}

// checkFieldDescriptions 检查一个 schema 的每个属性及其每个 definition 的属性都有非空 description。
func checkFieldDescriptions(t *testing.T, label string, data map[string]interface{}) {
	t.Helper()
	props, ok := data[JsonKey.Properties].(map[string]interface{})
	if !ok {
		t.Fatalf("%s: missing or invalid [%s]", label, JsonKey.Properties)
	}
	for name, defObj := range props {
		def, ok := defObj.(map[string]interface{})
		if !ok {
			t.Fatalf("%s/%s: property definition is not an object", label, name)
		}
		if desc, _ := def[JsonKey.Description].(string); desc == "" {
			t.Errorf("%s/%s: missing description", label, name)
		}
	}
	defs, ok := data[JsonKey.Definitions].(map[string]interface{})
	if !ok {
		return
	}
	for name, defObj := range defs {
		def, ok := defObj.(map[string]interface{})
		if !ok {
			t.Fatalf("%s/definitions/%s: definition is not an object", label, name)
		}
		checkFieldDescriptions(t, label+"/definitions/"+name, def)
	}
}
