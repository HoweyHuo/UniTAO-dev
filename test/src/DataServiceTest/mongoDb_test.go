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
	"fmt"
	"os"
	"testing"
	"time"

	"Data/DbConfig"
	"Data/DbIface"
	Mongodb "Data/Mongodb"
)

// MongoDbEndpointEnv 给出 MongoDB 的地址（mongodb://host:port）；不设置时跳过需要真实 MongoDB 的测试。
// 账号来自 UNITAO_TEST_MONGODB_USER / UNITAO_TEST_MONGODB_PASSWORD。scripts/test-mongodb.sh 起一个临时 MongoDB 并设置它们。
const (
	MongoDbEndpointEnv = "UNITAO_TEST_MONGODB_ENDPOINT"
	MongoDbUserEnv     = "UNITAO_TEST_MONGODB_USER"
	MongoDbPasswordEnv = "UNITAO_TEST_MONGODB_PASSWORD"
)

// TestMongoDbObjectId：MongoDB 的内部主键 _id 不属于 Record。读取（Get / Scan）不返回它；
// 写入时客户端带回来的 _id 被忽略，不会因为改动不可变的 _id 而失败；Update 按主键替换，不产生第二份文档。
func TestMongoDbObjectId(t *testing.T) {
	endpoint := os.Getenv(MongoDbEndpointEnv)
	if endpoint == "" {
		t.Skipf("%s not set, skip test against a real MongoDB", MongoDbEndpointEnv)
	}
	// 每次用新的 database，结束时删掉唯一的表，database 随之消失
	database := fmt.Sprintf("unitao_test_%d", time.Now().UnixNano())
	db, err := Mongodb.Connect(DbConfig.DatabaseConfig{
		DbType: Mongodb.Name,
		Mongodb: DbConfig.MongoDbConfig{
			EndPoint: endpoint,
			Database: database,
			UserName: os.Getenv(MongoDbUserEnv),
			Password: os.Getenv(MongoDbPasswordEnv),
		},
	}, nil)
	if err != nil {
		t.Fatalf("connect to MongoDB at [%s]: %s", endpoint, err)
	}
	const table = "data"
	if err := db.CreateTable(table, nil); err != nil {
		t.Fatalf("create table: %s", err)
	}
	defer db.DeleteTable(table)

	keys := map[string]interface{}{"__type": "rack", "__id": "r1"}
	byId := map[string]interface{}{DbIface.Table: table, "__type": "rack", "__id": "r1"}
	byType := map[string]interface{}{DbIface.Table: table, "__type": "rack"}
	get := func(what string, query map[string]interface{}) []map[string]interface{} {
		t.Helper()
		records, err := db.Get(query)
		if err != nil {
			t.Fatalf("%s: %s", what, err)
		}
		for _, record := range records {
			if _, ok := record[Mongodb.ObjectIdField]; ok {
				t.Fatalf("%s: expect no [%s] in %v", what, Mongodb.ObjectIdField, record)
			}
		}
		return records
	}
	name := func(record map[string]interface{}) interface{} {
		return record["data"].(map[string]interface{})["name"]
	}

	if err := db.Create(table, map[string]interface{}{"__type": "rack", "__id": "r1", "__ver": "0.0.1", "data": map[string]interface{}{"name": "one"}}); err != nil {
		t.Fatalf("create r1: %s", err)
	}
	if records := get("get r1", byId); len(records) != 1 || records[0]["__id"] != "r1" || name(records[0]) != "one" {
		t.Fatalf("unexpected r1 %v", records)
	}
	scanned := 0
	err = DbIface.Scan(db, byType, func(record map[string]interface{}) error {
		if _, ok := record[Mongodb.ObjectIdField]; ok {
			return fmt.Errorf("expect no [%s] from Scan, got %v", Mongodb.ObjectIdField, record)
		}
		scanned++
		return nil
	})
	if err != nil || scanned != 1 {
		t.Fatalf("scan: %d records, err %v", scanned, err)
	}

	// 客户端把读到（旧版本返回过）的 _id 带回来：替换照常成功，_id 不被写入
	err = db.Replace(table, keys, map[string]interface{}{"__type": "rack", "__id": "r1", "__ver": "0.0.1", "_id": "6af28c14f8d1d4a3dd6b1d7d", "data": map[string]interface{}{"name": "two"}})
	if err != nil {
		t.Fatalf("replace with a stale _id: %s", err)
	}
	if records := get("get r1 after replace", byType); len(records) != 1 || name(records[0]) != "two" {
		t.Fatalf("expect one r1 named two, got %v", records)
	}
	if err := db.Create(table, map[string]interface{}{"__type": "rack", "__id": "r2", "__ver": "0.0.1", "_id": "r2-client-id", "data": map[string]interface{}{"name": "r2"}}); err != nil {
		t.Fatalf("create with a client _id: %s", err)
	}

	// Update 按主键替换，不留第二份文档
	patched, err := db.Update(table, map[string]interface{}{"__type": "rack", "__id": "r1", DbIface.PatchPath: "data/name"}, "three")
	if err != nil {
		t.Fatalf("update r1: %s", err)
	}
	if patched == nil || name(patched) != "three" {
		t.Fatalf("expect the patched record returned, got %v", patched)
	}
	records := get("get after update", byType)
	if len(records) != 2 {
		t.Fatalf("expect r1 and r2 only, got %v", records)
	}
	for _, record := range records {
		if record["__id"] == "r1" && name(record) != "three" {
			t.Fatalf("expect r1 named three, got %v", record)
		}
	}

	if err := db.Delete(table, keys); err != nil {
		t.Fatalf("delete r1: %s", err)
	}
	if records := get("get r1 after delete", byId); len(records) != 0 {
		t.Fatalf("expect r1 deleted, got %v", records)
	}
}
