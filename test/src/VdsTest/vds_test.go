package VdsTest

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"InventoryService/DataHandler"
)

var rackSchema = map[string]interface{}{
	"name": map[string]interface{}{"type": "string"},
}

var serverSchema = map[string]interface{}{
	"hostname": map[string]interface{}{"type": "string"},
	"rack":     map[string]interface{}{"type": "string", "contentMediaType": "inventory/rack"},
}

func dirChannel(path string) map[string]interface{} {
	return map[string]interface{}{"type": "dir", "path": path}
}

func getInvRecord(t *testing.T, inv *invNode, id string) (map[string]interface{}, bool) {
	resp := call(t, http.MethodGet, inv.Url+"/inventory/"+id, nil)
	if resp.Status != http.StatusOK {
		return nil, false
	}
	return resp.Map(t)["data"].(map[string]interface{}), true
}

func urlsOf(data map[string]interface{}) []string {
	result := []string{}
	for _, item := range data["url"].([]interface{}) {
		result = append(result, item.(string))
	}
	return result
}

// TestSharedMirroredEndToEnd 覆盖 shared -> Channel(dir) -> mirrored 的完整链路，
// 以及 vDS 路由、只读、local 与 shared 的 schema 隔离、Inventory 的 {ds}/{id} 查询与副本合并。
func TestSharedMirroredEndToEnd(t *testing.T) {
	inv := startInventory(t, nil)
	channelDir := filepath.Join(t.TempDir(), "channel")
	keyFile, fingerprint := newSigningKey(t)

	dsA := newDsNode(t, "dsA", inv.Url)
	dsA.start(t, map[string]interface{}{
		"local": map[string]interface{}{},
		"shared": []interface{}{map[string]interface{}{
			"name": "prod", "signingKeyFile": keyFile,
			"publishIntervalSec": 3600, "snapshotIntervalSec": 3600,
			"channel": dirChannel(channelDir),
		}},
	})
	dsB := newDsNode(t, "dsB", inv.Url)
	dsB.start(t, map[string]interface{}{
		"local": map[string]interface{}{},
		"mirrored": []interface{}{map[string]interface{}{
			"name": "prod", "trustedFingerprint": fingerprint, "pullIntervalSec": 3600,
			"channel": dirChannel(channelDir),
		}},
	})

	// 路由
	expectList(t, call(t, http.MethodGet, dsA.Url+"/", nil), []string{"local", "shared/prod"}, "GET / on dsA")
	expectList(t, call(t, http.MethodGet, dsA.Url+"/shared", nil), []string{"prod"}, "GET /shared on dsA")
	expectList(t, call(t, http.MethodGet, dsB.Url+"/mirrored", nil), []string{"prod"}, "GET /mirrored on dsB")
	cfg := call(t, http.MethodGet, dsA.Url+"/shared/prod/config", nil)
	expectStatus(t, cfg, http.StatusOK, "GET shared config")
	if kind := cfg.Map(t)["kind"]; kind != "shared" {
		t.Fatalf("expect kind shared, got %v", kind)
	}
	if fp := cfg.Map(t)["instanceId"]; fp != fingerprint {
		t.Fatalf("expect shared instanceId = fingerprint %s, got %v", fingerprint, fp)
	}
	expectStatus(t, call(t, http.MethodGet, dsA.Url+"/nope/server", nil), http.StatusNotFound, "unknown vDS")

	// shared 中写入 schema 与数据
	sharedUrl := dsA.Url + "/shared/prod"
	expectStatus(t, call(t, http.MethodPost, sharedUrl, schemaRecord("rack", "0.0.1", rackSchema)), http.StatusCreated, "post rack schema")
	expectStatus(t, call(t, http.MethodPost, sharedUrl, schemaRecord("server", "0.0.1", serverSchema)), http.StatusCreated, "post server schema")
	expectStatus(t, call(t, http.MethodPost, sharedUrl, dataRecord("rack", "r1", map[string]interface{}{"name": "rack one"})), http.StatusCreated, "post rack r1")
	expectStatus(t, call(t, http.MethodPost, sharedUrl, dataRecord("server", "s1", map[string]interface{}{"hostname": "web-01", "rack": "r1"})), http.StatusCreated, "post server s1")
	// shared 只能引用本 vDS 内的记录
	resp := call(t, http.MethodPost, sharedUrl, dataRecord("server", "s2", map[string]interface{}{"hostname": "web-02", "rack": "missing"}))
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("expect dangling reference rejected in shared, got %d %s", resp.Status, resp.Body)
	}
	expectList(t, call(t, http.MethodGet, sharedUrl, nil), []string{"config", "schema", "rack", "server"}, "GET /shared/prod")

	// local 有自己的 schema 副本：同内容的 rack 正常合并，同 type 同 id 的记录互不覆盖
	localUrl := dsA.Url + "/local"
	expectStatus(t, call(t, http.MethodPost, localUrl, schemaRecord("rack", "0.0.1", rackSchema)), http.StatusCreated, "post local rack schema")
	expectStatus(t, call(t, http.MethodPost, localUrl, dataRecord("rack", "r1", map[string]interface{}{"name": "local rack"})), http.StatusCreated, "post local r1")
	// 同名同版本但内容不同的 server schema：D6 冲突，dsA 不进入 server 的 referral
	expectStatus(t, call(t, http.MethodPost, localUrl, schemaRecord("server", "0.0.1", map[string]interface{}{"hostname": map[string]interface{}{"type": "string"}})), http.StatusCreated, "post conflicting local server schema")

	// 发布并镜像
	waitJournal(t, dsA, "shared/prod", 4)
	dsA.srv.GetVds("shared/prod").Publisher.RunOnce()
	if err := dsB.srv.GetVds("mirrored/prod").Mirror.SyncOnce(); err != nil {
		t.Fatalf("mirror sync failed: %s", err)
	}
	mirroredUrl := dsB.Url + "/mirrored/prod"
	got := call(t, http.MethodGet, mirroredUrl+"/server/s1", nil)
	expectStatus(t, got, http.StatusOK, "get mirrored s1")
	if hostname := got.Map(t)["data"].(map[string]interface{})["hostname"]; hostname != "web-01" {
		t.Fatalf("expect mirrored hostname web-01, got %v", hostname)
	}
	// mirrored 只读
	expectStatus(t, call(t, http.MethodPost, mirroredUrl, dataRecord("rack", "r9", map[string]interface{}{"name": "x"})), http.StatusMethodNotAllowed, "post to mirrored")
	expectStatus(t, call(t, http.MethodDelete, mirroredUrl+"/rack/r1", nil), http.StatusMethodNotAllowed, "delete on mirrored")

	// 增量：修改与删除
	expectStatus(t, call(t, http.MethodPut, sharedUrl+"/server/s1", dataRecord("server", "s1", map[string]interface{}{"hostname": "web-01b", "rack": "r1"})), http.StatusCreated, "put s1")
	expectStatus(t, call(t, http.MethodPost, sharedUrl, dataRecord("rack", "r2", map[string]interface{}{"name": "rack two"})), http.StatusCreated, "post rack r2")
	expectStatus(t, call(t, http.MethodDelete, sharedUrl+"/rack/r2", nil), http.StatusAccepted, "delete r2")
	waitJournal(t, dsA, "shared/prod", 7)
	dsA.srv.GetVds("shared/prod").Publisher.RunOnce()
	if err := dsB.srv.GetVds("mirrored/prod").Mirror.SyncOnce(); err != nil {
		t.Fatalf("mirror incremental sync failed: %s", err)
	}
	got = call(t, http.MethodGet, mirroredUrl+"/server/s1", nil)
	if hostname := got.Map(t)["data"].(map[string]interface{})["hostname"]; hostname != "web-01b" {
		t.Fatalf("expect incremental update web-01b, got %v", hostname)
	}
	expectStatus(t, call(t, http.MethodGet, mirroredUrl+"/rack/r2", nil), http.StatusNotFound, "deleted r2 on mirror")
	// 通道被清空（仓库重置）：发布端先补一个当前 seq 的 snapshot，之后的增量照常
	if err := os.RemoveAll(channelDir); err != nil {
		t.Fatal(err)
	}
	dsA.srv.GetVds("shared/prod").Publisher.RunOnce()
	expectStatus(t, call(t, http.MethodPost, sharedUrl, dataRecord("rack", "r3", map[string]interface{}{"name": "rack three"})), http.StatusCreated, "post rack r3")
	waitJournal(t, dsA, "shared/prod", 8)
	dsA.srv.GetVds("shared/prod").Publisher.RunOnce()
	if err := dsB.srv.GetVds("mirrored/prod").Mirror.SyncOnce(); err != nil {
		t.Fatalf("mirror sync after channel reset failed: %s", err)
	}
	expectStatus(t, call(t, http.MethodGet, mirroredUrl+"/rack/r3", nil), http.StatusOK, "r3 on mirror after channel reset")
	mcfg := call(t, http.MethodGet, mirroredUrl+"/config", nil).Map(t)
	if mcfg["status"] != "ok" || mcfg["sourceFingerprint"] != fingerprint {
		t.Fatalf("unexpected mirror status: %v", mcfg)
	}

	// Inventory：shared 与它的镜像合并成同一条记录，源头排在前
	waitFor(t, "registrations", func() bool {
		data, ok := getInvRecord(t, inv, "prod")
		if !ok || len(urlsOf(data)) != 2 {
			return false
		}
		_, okA := getInvRecord(t, inv, "dsA")
		_, okB := getInvRecord(t, inv, "dsB")
		return okA && okB
	})
	if err := inv.srv.SyncNow(); err != nil {
		t.Fatalf("inventory sync failed: %s", err)
	}
	prod, _ := getInvRecord(t, inv, "prod")
	if prod["kind"] != "shared" || prod["instanceId"] != fingerprint {
		t.Fatalf("unexpected merged record: %v", prod)
	}
	if urls := urlsOf(prod); urls[0] != sharedUrl {
		t.Fatalf("expect shared source first, got %v", urls)
	}
	expectList(t, call(t, http.MethodGet, inv.Url+"/rack", nil), []string{"dsA/r1", "prod/r1", "prod/r3"}, "inventory list rack")
	local := call(t, http.MethodGet, inv.Url+"/rack/dsA/r1", nil)
	expectStatus(t, local, http.StatusOK, "inventory get dsA/r1")
	// Inventory 的路径查询返回 data 的内容
	if name := local.Map(t)["name"]; name != "local rack" {
		t.Fatalf("expect local record, got %v", name)
	}
	expectList(t, call(t, http.MethodGet, inv.Url+"/server", nil), []string{"prod/s1"}, "inventory list server (dsA excluded by schema conflict)")
	shared := call(t, http.MethodGet, inv.Url+"/server/prod/s1", nil)
	expectStatus(t, shared, http.StatusOK, "inventory get prod/s1")
	if hostname := shared.Map(t)["hostname"]; hostname != "web-01b" {
		t.Fatalf("expect shared record, got %v", hostname)
	}
	// 引用按 vDS 优先解析：prod 的 s1 引用的 r1 在 prod 里
	rackName := call(t, http.MethodGet, inv.Url+"/server/prod/s1/rack/name", nil)
	expectStatus(t, rackName, http.StatusOK, "follow reference within vDS")
	if rackName.Json(t) != "rack one" {
		t.Fatalf("expect reference resolved to rack one, got %s", rackName.Body)
	}
	expectStatus(t, call(t, http.MethodGet, inv.Url+"/server/s1", nil), http.StatusBadRequest, "inventory get without ds")
}

// TestRemoveMirroredVds 从 config 中删除 mirrored 后重启：数据表被删除，URL 从 Inventory 注销。
func TestRemoveMirroredVds(t *testing.T) {
	inv := startInventory(t, nil)
	channelDir := filepath.Join(t.TempDir(), "channel")
	keyFile, fingerprint := newSigningKey(t)
	dsA := newDsNode(t, "dsA", "")
	dsA.start(t, map[string]interface{}{
		"shared": []interface{}{map[string]interface{}{
			"name": "prod", "signingKeyFile": keyFile, "publishIntervalSec": 3600, "snapshotIntervalSec": 3600,
			"channel": dirChannel(channelDir),
		}},
	})
	expectStatus(t, call(t, http.MethodPost, dsA.Url+"/shared/prod", schemaRecord("rack", "0.0.1", rackSchema)), http.StatusCreated, "post rack schema")
	waitJournal(t, dsA, "shared/prod", 1)
	dsA.srv.GetVds("shared/prod").Publisher.RunOnce()

	mirrorCfg := map[string]interface{}{
		"name": "prod", "trustedFingerprint": fingerprint, "pullIntervalSec": 3600,
		"channel": dirChannel(channelDir),
	}
	dsB := newDsNode(t, "dsB", inv.Url)
	dsB.start(t, map[string]interface{}{"local": map[string]interface{}{}, "mirrored": []interface{}{mirrorCfg}})
	if err := dsB.srv.GetVds("mirrored/prod").Mirror.SyncOnce(); err != nil {
		t.Fatalf("mirror sync failed: %s", err)
	}
	table := "dsB_mirrored_prod"
	if !dsB.db.HasTable(table) {
		t.Fatalf("expect table %s", table)
	}
	waitFor(t, "mirror registration", func() bool { _, ok := getInvRecord(t, inv, "prod"); return ok })

	dsB.start(t, map[string]interface{}{"local": map[string]interface{}{}})
	if dsB.db.HasTable(table) {
		t.Fatalf("expect table %s dropped after mirrored vDS removed", table)
	}
	waitFor(t, "mirror unregistered", func() bool { _, ok := getInvRecord(t, inv, "prod"); return !ok })
	expectStatus(t, call(t, http.MethodGet, dsB.Url+"/mirrored/prod/rack", nil), http.StatusNotFound, "removed vDS")
}

// TestSignedRegistration 覆盖 Inventory 对签名 vDS 的名字分配：副本合并、撞名派生、同指纹不同名、pinnedSources、按 URL 注销。
func TestSignedRegistration(t *testing.T) {
	fpA := "SHA256:aaaaaaaa11111111"
	fpB := "SHA256:bbbbbbbb22222222"
	fpC := "SHA256:cccccccc33333333"
	fpD := "SHA256:dddddddd44444444"
	fpE := "SHA256:eeeeeeee55555555"
	inv := startInventory(t, map[string]string{"pinned": fpC})
	register := func(id string, fp string, kind string, url string) string {
		record := map[string]interface{}{
			"__type": "inventory", "__id": id, "__ver": "0.0.1",
			"data": map[string]interface{}{"dsId": id, "instanceId": fp, "kind": kind, "url": []string{url}},
		}
		resp := call(t, http.MethodPut, inv.Url+"/", record)
		if resp.Status >= 300 {
			t.Fatalf("register %s failed: %d %s", id, resp.Status, resp.Body)
		}
		return string(resp.Body)
	}
	if id := register("prod", fpA, "mirrored", "http://x1/mirrored/prod"); id != "prod" {
		t.Fatalf("expect prod, got %s", id)
	}
	// 同名同指纹：副本，URL 合并
	if id := register("prod", fpA, "mirrored", "http://x2/mirrored/prod"); id != "prod" {
		t.Fatalf("expect replica merged into prod, got %s", id)
	}
	data, _ := getInvRecord(t, inv, "prod")
	if urls := urlsOf(data); len(urls) != 2 {
		t.Fatalf("expect 2 replica urls, got %v", urls)
	}
	// 同名不同指纹：派生名字
	derived := DataHandler.DerivedDsId("prod", fpB)
	if id := register("prod", fpB, "mirrored", "http://x3/mirrored/prod"); id != derived {
		t.Fatalf("expect derived id %s, got %s", derived, id)
	}
	// 同指纹不同名（alias 不一致）：合并进已有记录
	if id := register("prod-alias", fpA, "mirrored", "http://x4/mirrored/prod-alias"); id != "prod" {
		t.Fatalf("expect same-fingerprint merged into prod, got %s", id)
	}
	// local 占用了名字：签名 vDS 得到派生名字
	register("plain", "uuid-1", "", "http://x5/local")
	if id := register("plain", fpD, "shared", "http://x6/shared/plain"); id != DataHandler.DerivedDsId("plain", fpD) {
		t.Fatalf("expect derived id for name taken by local, got %s", id)
	}
	// pinnedSources：名字被固定给 fpC，其它指纹得到派生名字；fpC 来时拿到名字
	if id := register("pinned", fpE, "mirrored", "http://x7/mirrored/pinned"); id != DataHandler.DerivedDsId("pinned", fpE) {
		t.Fatalf("expect non-pinned source get derived id, got %s", id)
	}
	if id := register("pinned", fpC, "mirrored", "http://x8/mirrored/pinned"); id != "pinned" {
		t.Fatalf("expect pinned source get the name, got %s", id)
	}
	// 按 URL 注销副本
	expectStatus(t, call(t, http.MethodDelete, inv.Url+"/inventory/prod?url=http://x1/mirrored/prod", nil), http.StatusAccepted, "remove one url")
	data, _ = getInvRecord(t, inv, "prod")
	if urls := urlsOf(data); contains(urls, "http://x1/mirrored/prod") || len(urls) != 2 {
		t.Fatalf("expect url removed, got %v", urls)
	}
	call(t, http.MethodDelete, inv.Url+"/inventory/prod?url=http://x2/mirrored/prod", nil)
	call(t, http.MethodDelete, inv.Url+"/inventory/prod?url=http://x4/mirrored/prod-alias", nil)
	if _, ok := getInvRecord(t, inv, "prod"); ok {
		t.Fatalf("expect record deleted when no url left")
	}
}

// TestSharedSchemaClosure shared 的 schema 不能引用本 vDS 之外的类型。
func TestSharedSchemaClosure(t *testing.T) {
	keyFile, _ := newSigningKey(t)
	ds := newDsNode(t, "dsA", "")
	ds.start(t, map[string]interface{}{
		"local": map[string]interface{}{},
		"shared": []interface{}{map[string]interface{}{
			"name": "prod", "signingKeyFile": keyFile, "publishIntervalSec": 3600, "snapshotIntervalSec": 3600,
			"channel": dirChannel(filepath.Join(t.TempDir(), "channel")),
		}},
	})
	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", schemaRecord("rack", "0.0.1", rackSchema)), http.StatusCreated, "post local rack schema")
	resp := call(t, http.MethodPost, ds.Url+"/shared/prod", schemaRecord("server", "0.0.1", serverSchema))
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("expect shared schema referencing type outside the vDS rejected, got %d %s", resp.Status, resp.Body)
	}
	// 保留名
	resp = call(t, http.MethodPost, ds.Url+"/local", schemaRecord("config", "0.0.1", rackSchema))
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("expect reserved schema name rejected, got %d %s", resp.Status, resp.Body)
	}
}

func expectList(t *testing.T, resp response, expect []string, what string) {
	t.Helper()
	expectStatus(t, resp, http.StatusOK, what)
	got := resp.Strings(t)
	if len(got) != len(expect) {
		t.Fatalf("%s: expect %v, got %v", what, expect, got)
	}
	for _, item := range expect {
		if !contains(got, item) {
			t.Fatalf("%s: expect %v, got %v", what, expect, got)
		}
	}
}

// waitJournal 等待 shared vDS 的 Journal 把至少 n 条变更写入 outbox。
func waitJournal(t *testing.T, node *dsNode, path string, n uint64) {
	t.Helper()
	waitFor(t, "journal to fill outbox", func() bool {
		status := node.srv.GetVds(path).Publisher.Status()
		seq, _ := status["outboxSeq"].(uint64)
		return seq >= n
	})
}
