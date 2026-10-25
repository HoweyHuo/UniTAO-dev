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

package InventoryServiceTest

import (
	"testing"

	"UniTao/Test/DataServiceTest"

	"InventoryService/DataHandler"

	"github.com/salesforce/UniTAO/lib/Schema/JsonKey"
	"github.com/salesforce/UniTAO/lib/Schema/Record"
)

// Inventory 的 /schema 列表必须包含 record：它不是 referral 类型，但描述了
// 所有数据记录共有的信封 __id/__type/__ver/data。
func TestSchemaListIncludesRecordFramework(t *testing.T) {
	db := &DataServiceTest.MockDatabase{Data: map[string]interface{}{}}
	handler := &DataHandler.Handler{Db: db}
	schemaList, err := handler.List(JsonKey.Schema)
	if err != nil {
		t.Fatalf("failed to list schema: %s", err)
	}
	found := false
	for _, idObj := range schemaList {
		if id, ok := idObj.(string); ok && id == Record.KeyRecord {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("schema list %v should include [%s]", schemaList, Record.KeyRecord)
	}
}

// /schema/record 在 Inventory 上可直接返回信封框架 schema，不需要 referral。
func TestGetRecordSchemaFramework(t *testing.T) {
	db := &DataServiceTest.MockDatabase{Data: map[string]interface{}{}}
	handler := &DataHandler.Handler{Db: db}
	record, err := handler.GetSchemaRecord(Record.KeyRecord)
	if err != nil {
		t.Fatalf("failed to get schema of [%s]: %s", Record.KeyRecord, err)
	}
	if record.Id != Record.KeyRecord || record.Type != JsonKey.Schema {
		t.Fatalf("unexpected record schema identity: id=[%s] type=[%s]", record.Id, record.Type)
	}
	props, ok := record.Data[JsonKey.Properties].(map[string]interface{})
	if !ok {
		t.Fatalf("[%s] schema should declare properties", Record.KeyRecord)
	}
	for _, key := range []string{Record.DataId, Record.DataType, Record.Version} {
		if _, ok := props[key]; !ok {
			t.Fatalf("[%s] schema should declare [%s]", Record.KeyRecord, key)
		}
	}
}
