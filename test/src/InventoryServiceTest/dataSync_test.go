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
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"UniTao/Test/DataServiceTest"

	"InventoryService/DataHandler"
	"InventoryService/DataSync"
)

// fakeDs 启动一个假 DataService /schema 端点，返回指定的类型数组。
func fakeDs(types ...string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/schema" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		resp := "["
		for i, ty := range types {
			if i > 0 {
				resp += ","
			}
			resp += fmt.Sprintf("%q", ty)
		}
		resp += "]"
		fmt.Fprint(w, resp)
	}))
}

// seedInventory 向 mock 写入一个 inventory 记录（url 指向假 DS）。
func seedInventory(db *DataServiceTest.MockDatabase, id string, url string) {
	typeMap, ok := db.Data["inventory"].(map[string]interface{})
	if !ok {
		typeMap = map[string]interface{}{}
		db.Data["inventory"] = typeMap
	}
	typeMap[id] = map[string]interface{}{
		"__type": "inventory",
		"__id":   id,
		"__ver":  "0.0.1",
		"data": map[string]interface{}{
			"dsId":       id,
			"instanceId": "inst-" + id,
			"url":        []interface{}{url},
		},
	}
}

// seedReferral 直接写入一条 referral 记录。
func seedReferral(db *DataServiceTest.MockDatabase, dataType string, services []string) {
	typeMap, ok := db.Data["referral"].(map[string]interface{})
	if !ok {
		typeMap = map[string]interface{}{}
		db.Data["referral"] = typeMap
	}
	raw := make([]interface{}, 0, len(services))
	for _, s := range services {
		raw = append(raw, s)
	}
	typeMap[dataType] = map[string]interface{}{
		"__type": "referral",
		"__id":   dataType,
		"__ver":  "0.0.1",
		"data": map[string]interface{}{
			"DataType":     dataType,
			"DataServices": raw,
		},
	}
}

// referralServices 从 mock 读取某 type 的 referral 的 DataServices 列表。
func referralServices(t *testing.T, db *DataServiceTest.MockDatabase, dataType string) []string {
	t.Helper()
	typeMap, ok := db.Data["referral"].(map[string]interface{})
	if !ok {
		return nil
	}
	rec, ok := typeMap[dataType].(map[string]interface{})
	if !ok {
		return nil
	}
	inner, ok := rec["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("referral[%s] data is not a map: %v", dataType, rec)
	}
	raw, ok := inner["DataServices"].([]interface{})
	if !ok {
		t.Fatalf("referral[%s] DataServices is not []interface{}: %v", dataType, inner["DataServices"])
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		out = append(out, v.(string))
	}
	return out
}

func TestSyncDsCreatesReferral(t *testing.T) {
	ds := fakeDs("Server", "Rack", "schema")
	defer ds.Close()
	db := &DataServiceTest.MockDatabase{Data: map[string]interface{}{}}
	seedInventory(db, "ds1", ds.URL)
	handler := &DataHandler.Handler{Db: db}
	syncer := DataSync.New(handler, log.Default())

	if err := syncer.SyncDs("ds1"); err != nil {
		t.Fatalf("SyncDs failed: %s", err)
	}
	if got := referralServices(t, db, "Server"); len(got) != 1 || got[0] != "ds1" {
		t.Fatalf("referral[Server] = %v, want [ds1]", got)
	}
	if got := referralServices(t, db, "Rack"); len(got) != 1 || got[0] != "ds1" {
		t.Fatalf("referral[Rack] = %v, want [ds1]", got)
	}
	// 内部类型 "schema" 应被跳过，不生成 referral
	if _, err := handler.GetData("referral", "schema"); err == nil {
		t.Fatalf("referral[schema] should not be created (internal type)")
	}
}

func TestSyncDsAppendsExisting(t *testing.T) {
	ds := fakeDs("Server")
	defer ds.Close()
	db := &DataServiceTest.MockDatabase{Data: map[string]interface{}{}}
	seedReferral(db, "Server", []string{"dsOld"})
	seedInventory(db, "ds1", ds.URL)
	handler := &DataHandler.Handler{Db: db}
	syncer := DataSync.New(handler, log.Default())

	if err := syncer.SyncDs("ds1"); err != nil {
		t.Fatalf("SyncDs failed: %s", err)
	}
	got := referralServices(t, db, "Server")
	if len(got) != 2 || got[0] != "dsOld" || got[1] != "ds1" {
		t.Fatalf("referral[Server] = %v, want [dsOld ds1]", got)
	}
}

func TestSyncFullReconcile(t *testing.T) {
	ds1 := fakeDs("Server", "schema")
	defer ds1.Close()
	ds2 := fakeDs("Rack")
	defer ds2.Close()
	db := &DataServiceTest.MockDatabase{Data: map[string]interface{}{}}
	seedInventory(db, "ds1", ds1.URL)
	seedInventory(db, "ds2", ds2.URL)
	handler := &DataHandler.Handler{Db: db}
	syncer := DataSync.New(handler, log.Default())

	if err := syncer.Sync(); err != nil {
		t.Fatalf("Sync failed: %s", err)
	}
	if got := referralServices(t, db, "Server"); len(got) != 1 || got[0] != "ds1" {
		t.Fatalf("referral[Server] = %v, want [ds1]", got)
	}
	if got := referralServices(t, db, "Rack"); len(got) != 1 || got[0] != "ds2" {
		t.Fatalf("referral[Rack] = %v, want [ds2]", got)
	}
}

func TestSyncRemovesStaleReferral(t *testing.T) {
	ds1 := fakeDs("Server")
	defer ds1.Close()
	db := &DataServiceTest.MockDatabase{Data: map[string]interface{}{}}
	seedReferral(db, "Server", []string{"ds1"})
	seedReferral(db, "Gone", []string{"ds1"}) // 任一 DS 都不再提供该类型
	seedInventory(db, "ds1", ds1.URL)
	handler := &DataHandler.Handler{Db: db}
	syncer := DataSync.New(handler, log.Default())

	if err := syncer.Sync(); err != nil {
		t.Fatalf("Sync failed: %s", err)
	}
	if _, err := handler.GetData("referral", "Gone"); err == nil {
		t.Fatalf("referral[Gone] should be removed")
	}
	if got := referralServices(t, db, "Server"); len(got) != 1 || got[0] != "ds1" {
		t.Fatalf("referral[Server] = %v, want [ds1]", got)
	}
}

func TestSyncAbortsOnUnreachableDs(t *testing.T) {
	ds1 := fakeDs("Server")
	defer ds1.Close()
	unreachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[]`)
	}))
	unreachableURL := unreachable.URL
	unreachable.Close() // 关闭后 URL 不可达

	db := &DataServiceTest.MockDatabase{Data: map[string]interface{}{}}
	seedReferral(db, "Server", []string{"ds1"})
	seedInventory(db, "ds1", ds1.URL)
	seedInventory(db, "ds2", unreachableURL)
	handler := &DataHandler.Handler{Db: db}
	syncer := DataSync.New(handler, log.Default())

	if err := syncer.Sync(); err == nil {
		t.Fatalf("Sync should fail when a DS is unreachable")
	}
	// 严格中止语义：现有 referral 保持不变
	if got := referralServices(t, db, "Server"); len(got) != 1 || got[0] != "ds1" {
		t.Fatalf("referral[Server] should remain [ds1], got %v", got)
	}
}
