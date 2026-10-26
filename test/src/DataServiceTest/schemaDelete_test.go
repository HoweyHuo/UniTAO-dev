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
)

// 删除当前 schema 前必须先删掉该类型的全部数据，否则数据会变成无 schema 可用、也无法再删除的孤儿。
func TestSchemaDeleteWithData(t *testing.T) {
	handler, ex := MockHandler()
	if ex != nil {
		t.Fatalf(ex.Error())
	}
	if err := AddData(handler, upgradeSchemaV1); err != nil {
		t.Fatalf("failed to add schema. Error: %s", err)
	}
	if err := AddData(handler, upgradeDataV1); err != nil {
		t.Fatalf("failed to add data. Error: %s", err)
	}

	err := handler.Delete(JsonKey.Schema, "upgradable")
	if err == nil {
		t.Fatalf("delete schema with existing data should be rejected")
	}
	if err.Status != http.StatusBadRequest {
		t.Fatalf("delete schema with existing data should return %d, got %d: %s", http.StatusBadRequest, err.Status, err)
	}
	if _, err := handler.LocalSchema("upgradable", ""); err != nil {
		t.Fatalf("schema should still exist after rejected delete. Error: %s", err)
	}

	if err := handler.Delete("upgradable", "item01"); err != nil {
		t.Fatalf("failed to delete data. Error: %s", err)
	}
	if err := handler.Delete(JsonKey.Schema, "upgradable"); err != nil {
		t.Fatalf("delete schema without data should succeed. Error: %s", err)
	}
	_, err = handler.LocalSchema("upgradable", "")
	if err == nil || err.Status != http.StatusNotFound {
		t.Fatalf("schema should be gone after delete, got: %v", err)
	}
}

// 只有删除当前 schema（即移除该 data type）才通知 Inventory；删除归档版本不通知。
func TestSchemaDeleteNotifiesTypeChange(t *testing.T) {
	handler, ex := MockHandler()
	if ex != nil {
		t.Fatalf(ex.Error())
	}
	if err := AddData(handler, upgradeSchemaV1); err != nil {
		t.Fatalf("failed to add schema. Error: %s", err)
	}
	if err := AddData(handler, upgradeSchemaV2); err != nil {
		t.Fatalf("failed to upgrade schema. Error: %s", err)
	}
	notified := []string{}
	handler.TypeChangeNotify = func(dataType string) {
		notified = append(notified, dataType)
	}

	if err := handler.Delete(JsonKey.Schema, "upgradable__0.0.1"); err != nil {
		t.Fatalf("failed to delete archived schema. Error: %s", err)
	}
	if len(notified) != 0 {
		t.Fatalf("deleting an archived schema should not notify, got %v", notified)
	}
	if err := handler.Delete(JsonKey.Schema, "upgradable"); err != nil {
		t.Fatalf("failed to delete schema. Error: %s", err)
	}
	if len(notified) != 1 || notified[0] != "upgradable" {
		t.Fatalf("deleting the current schema should notify [upgradable], got %v", notified)
	}
}
