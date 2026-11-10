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
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"Data/DbConfig"
	"Data/DbDynamoDb"
	"Data/DbIface"
)

// DynamoDbEndpointEnv 给出 DynamoDB（一般是 DynamoDB Local）的地址；不设置时跳过需要真实 DynamoDB 的测试。
// scripts/test-dynamodb.sh 起一个内存模式的 DynamoDB Local 并设置它。
const DynamoDbEndpointEnv = "UNITAO_TEST_DYNAMODB_ENDPOINT"

// TestDynamoDbPaging：DynamoDB 一次 Query 最多返回 1MB。一个类型的数据超过 1MB 时，
// Get 和 Scan 都要按 LastEvaluatedKey 翻页读完（以前 Get 只读第一页，会悄悄截断）。
func TestDynamoDbPaging(t *testing.T) {
	endpoint := os.Getenv(DynamoDbEndpointEnv)
	if endpoint == "" {
		t.Skipf("%s not set, skip test against a real DynamoDB", DynamoDbEndpointEnv)
	}
	db, err := DbDynamoDb.Connect(DbConfig.DatabaseConfig{
		DbType:   DbDynamoDb.Name,
		Dynamodb: DbConfig.DynmoDbConfig{Region: "us-west-2", EndPoint: endpoint},
	}, nil)
	if err != nil {
		t.Fatalf("connect to DynamoDB at [%s]: %s", endpoint, err)
	}
	// 每次用新表名，指向已有数据的 DynamoDB Local 时也不碰别的表
	table := fmt.Sprintf("paging_test_%d", time.Now().UnixNano())
	err = db.CreateTable(table, map[string]interface{}{
		"ProvisionedThroughput": map[string]interface{}{"ReadCapacityUnits": 3, "WriteCapacityUnits": 3},
		"AttributeDefinitions": []interface{}{
			map[string]interface{}{"AttributeName": "__type", "AttributeType": "S"},
			map[string]interface{}{"AttributeName": "__id", "AttributeType": "S"},
		},
		"KeySchema": []interface{}{
			map[string]interface{}{"AttributeName": "__type", "KeyType": "HASH"},
			map[string]interface{}{"AttributeName": "__id", "KeyType": "RANGE"},
		},
	})
	if err != nil {
		t.Fatalf("create table [%s]: %s", table, err)
	}
	defer db.DeleteTable(table)

	// 3000 条 × 约 1.2KB ≈ 3.6MB，同一个分区键下至少 4 页
	const bigCount = 3000
	pad := strings.Repeat("x", 1200)
	for i := 0; i < bigCount; i++ {
		record := map[string]interface{}{"__type": "big", "__id": fmt.Sprintf("b%05d", i), "__ver": "0.0.1", "data": map[string]interface{}{"pad": pad, "n": i}}
		if err := db.Create(table, record); err != nil {
			t.Fatalf("create record %d: %s", i, err)
		}
	}
	for i := 0; i < 10; i++ {
		record := map[string]interface{}{"__type": "small", "__id": fmt.Sprintf("s%02d", i), "__ver": "0.0.1", "data": map[string]interface{}{}}
		if err := db.Create(table, record); err != nil {
			t.Fatalf("create small record %d: %s", i, err)
		}
	}
	query := func(dataType string) map[string]interface{} {
		return map[string]interface{}{DbIface.Table: table, "__type": dataType}
	}

	// Get 读完所有页，没有重复
	records, err := db.Get(query("big"))
	if err != nil {
		t.Fatalf("get big: %s", err)
	}
	ids := map[string]bool{}
	for _, record := range records {
		ids[record["__id"].(string)] = true
	}
	if len(records) != bigCount || len(ids) != bigCount {
		t.Fatalf("expect Get to return %d distinct records, got %d (%d distinct)", bigCount, len(records), len(ids))
	}

	// Scan 走 Scanner 接口，逐条按主键顺序回调
	if _, ok := db.(DbIface.Scanner); !ok {
		t.Fatal("expect DynamoDB to implement DbIface.Scanner")
	}
	count, last := 0, ""
	err = DbIface.Scan(db, query("big"), func(record map[string]interface{}) error {
		id := record["__id"].(string)
		if id <= last {
			return fmt.Errorf("record [%s] after [%s], expect key order", id, last)
		}
		last = id
		count++
		return nil
	})
	if err != nil || count != bigCount {
		t.Fatalf("expect Scan to return %d records, got %d, err %v", bigCount, count, err)
	}

	// 回调返回错误时停止（第 1500 条已经跨过第一页）并返回该错误
	stop := errors.New("stop")
	count = 0
	err = DbIface.Scan(db, query("big"), func(record map[string]interface{}) error {
		count++
		if count == 1500 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || count != 1500 {
		t.Fatalf("expect Scan to stop at the callback error, got count %d, err %v", count, err)
	}

	// 其它类型、按 id 查询不受影响
	small, err := db.Get(query("small"))
	if err != nil || len(small) != 10 {
		t.Fatalf("expect 10 small records, got %d, err %v", len(small), err)
	}
	one, err := db.Get(map[string]interface{}{DbIface.Table: table, "__type": "big", "__id": "b02999"})
	if err != nil || len(one) != 1 {
		t.Fatalf("expect one record by id, got %d, err %v", len(one), err)
	}
}
