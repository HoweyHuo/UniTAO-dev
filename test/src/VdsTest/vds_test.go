package VdsTest

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"DataService/Channel"
	"DataService/Share"
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

// dirSourceId 是 dir 通道上一个来源的 SourceId（shared 与 mirror 注册到 Inventory 的身份）。
func dirSourceId(path string, folder string) string {
	return Channel.SourceId(Channel.Config{Type: Channel.TypeDir, Path: path}, folder)
}

// syncAll 走一轮完整的订阅 / 消费：mirror 登记或汇报进度 → publisher 打包 history、清理 →
// mirror 加载 history 并消费 queue、汇报进度 → publisher 按新进度清理。
func syncAll(t *testing.T, pub *Share.Publisher, mirrors ...*Share.Mirror) {
	t.Helper()
	for round := 0; round < 2; round++ {
		for _, m := range mirrors {
			if err := m.SyncOnce(); err != nil {
				t.Fatalf("mirror %s sync failed: %s", m.LocalName, err)
			}
		}
		if err := pub.RunOnce(); err != nil {
			t.Fatalf("publisher %s run failed: %s", pub.Name, err)
		}
	}
	for _, m := range mirrors {
		if err := m.SyncOnce(); err != nil {
			t.Fatalf("mirror %s sync failed: %s", m.LocalName, err)
		}
	}
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
	sourceId := dirSourceId(channelDir, "prod")

	dsA := newDsNode(t, "dsA", inv.Url)
	dsA.start(t, map[string]interface{}{
		"local": map[string]interface{}{},
		"shared": []interface{}{map[string]interface{}{
			"name": "prod", "publishIntervalSec": 3600, "channel": dirChannel(channelDir),
		}},
	})
	dsB := newDsNode(t, "dsB", inv.Url)
	dsB.start(t, map[string]interface{}{
		"local": map[string]interface{}{},
		"mirrored": []interface{}{map[string]interface{}{
			"name": "prod", "pullIntervalSec": 3600, "channel": dirChannel(channelDir),
		}},
	})
	pub := dsA.srv.GetVds("shared/prod").Publisher
	mirror := dsB.srv.GetVds("mirrored/prod").Mirror

	// 路由
	expectList(t, call(t, http.MethodGet, dsA.Url+"/", nil), []string{"local", "shared/prod"}, "GET / on dsA")
	expectList(t, call(t, http.MethodGet, dsA.Url+"/shared", nil), []string{"prod"}, "GET /shared on dsA")
	expectList(t, call(t, http.MethodGet, dsB.Url+"/mirrored", nil), []string{"prod"}, "GET /mirrored on dsB")
	cfg := call(t, http.MethodGet, dsA.Url+"/shared/prod/config", nil)
	expectStatus(t, cfg, http.StatusOK, "GET shared config")
	if kind := cfg.Map(t)["kind"]; kind != "shared" {
		t.Fatalf("expect kind shared, got %v", kind)
	}
	if id := cfg.Map(t)["instanceId"]; id != sourceId {
		t.Fatalf("expect shared instanceId = source id %s, got %v", sourceId, id)
	}
	if _, ok := cfg.Map(t)["ignoredConfig"]; ok {
		t.Fatalf("expect no ignoredConfig without old fields, got %v", cfg.Map(t)["ignoredConfig"])
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

	// 发布并镜像：mirror 订阅 → publisher 打包 history → mirror 加载
	waitJournal(t, dsA, "shared/prod", 4)
	syncAll(t, pub, mirror)
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
	syncAll(t, pub, mirror)
	got = call(t, http.MethodGet, mirroredUrl+"/server/s1", nil)
	if hostname := got.Map(t)["data"].(map[string]interface{})["hostname"]; hostname != "web-01b" {
		t.Fatalf("expect incremental update web-01b, got %v", hostname)
	}
	expectStatus(t, call(t, http.MethodGet, mirroredUrl+"/rack/r2", nil), http.StatusNotFound, "deleted r2 on mirror")
	// 全部消费后 queue 被清空，通道上只剩 manifest 和进度文件
	if queue := channelFiles(t, filepath.Join(channelDir, "prod", "queue")); len(queue) != 0 {
		t.Fatalf("expect queue emptied after the only subscriber consumed it, got %v", queue)
	}
	if history := channelFiles(t, filepath.Join(channelDir, "prod", "history")); len(history) != 0 {
		t.Fatalf("expect history removed after it was loaded, got %v", history)
	}
	pcfg := call(t, http.MethodGet, sharedUrl+"/config", nil).Map(t)
	subscriberId := call(t, http.MethodGet, mirroredUrl+"/config", nil).Map(t)["subscriberId"]
	subs, _ := pcfg["subscribers"].([]interface{})
	if len(subs) != 1 || subs[0].(map[string]interface{})["state"] != "active" || subs[0].(map[string]interface{})["id"] != subscriberId {
		t.Fatalf("unexpected subscribers in shared config: %v", pcfg["subscribers"])
	}
	// 通道被清空（仓库重置）：publisher 让通道从当前 seq 重新开始，mirror 发现进度文件没了，重新订阅
	if err := os.RemoveAll(channelDir); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, call(t, http.MethodPost, sharedUrl, dataRecord("rack", "r3", map[string]interface{}{"name": "rack three"})), http.StatusCreated, "post rack r3")
	waitJournal(t, dsA, "shared/prod", 8)
	syncAll(t, pub, mirror)
	expectStatus(t, call(t, http.MethodGet, mirroredUrl+"/rack/r3", nil), http.StatusOK, "r3 on mirror after channel reset")
	mcfg := call(t, http.MethodGet, mirroredUrl+"/config", nil).Map(t)
	if mcfg["status"] != "ok" || mcfg["sourceId"] != sourceId || mcfg["instanceId"] != sourceId || mcfg["subscription"] != "active" || mcfg["subscriberId"] != subscriberId {
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
	if prod["kind"] != "shared" || prod["instanceId"] != sourceId {
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
	dsA := newDsNode(t, "dsA", "")
	dsA.start(t, map[string]interface{}{
		"shared": []interface{}{map[string]interface{}{
			"name": "prod", "publishIntervalSec": 3600, "channel": dirChannel(channelDir),
		}},
	})
	expectStatus(t, call(t, http.MethodPost, dsA.Url+"/shared/prod", schemaRecord("rack", "0.0.1", rackSchema)), http.StatusCreated, "post rack schema")
	waitJournal(t, dsA, "shared/prod", 1)

	mirrorCfg := map[string]interface{}{
		"name": "prod", "pullIntervalSec": 3600, "channel": dirChannel(channelDir),
	}
	dsB := newDsNode(t, "dsB", inv.Url)
	dsB.start(t, map[string]interface{}{"local": map[string]interface{}{}, "mirrored": []interface{}{mirrorCfg}})
	syncAll(t, dsA.srv.GetVds("shared/prod").Publisher, dsB.srv.GetVds("mirrored/prod").Mirror)
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

// TestSourceRegistration 覆盖 Inventory 对 shared / mirrored vDS 的名字分配（身份是 SourceId）：副本合并、撞名派生、
// 同来源不同名、pinnedSources、按 URL 注销。
func TestSourceRegistration(t *testing.T) {
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
	if id := register("prod", fpA, "mirrored", "http://127.0.0.1:9001/mirrored/prod"); id != "prod" {
		t.Fatalf("expect prod, got %s", id)
	}
	// 同名同来源：副本，URL 合并
	if id := register("prod", fpA, "mirrored", "http://127.0.0.1:9002/mirrored/prod"); id != "prod" {
		t.Fatalf("expect replica merged into prod, got %s", id)
	}
	data, _ := getInvRecord(t, inv, "prod")
	if urls := urlsOf(data); len(urls) != 2 {
		t.Fatalf("expect 2 replica urls, got %v", urls)
	}
	// 同名不同来源：派生名字
	derived := DataHandler.DerivedDsId("prod", fpB)
	if id := register("prod", fpB, "mirrored", "http://127.0.0.1:9003/mirrored/prod"); id != derived {
		t.Fatalf("expect derived id %s, got %s", derived, id)
	}
	// 同来源不同名（alias 不一致）：合并进已有记录
	if id := register("prod-alias", fpA, "mirrored", "http://127.0.0.1:9004/mirrored/prod-alias"); id != "prod" {
		t.Fatalf("expect same-source merged into prod, got %s", id)
	}
	// local 占用了名字：shared / mirrored 得到派生名字
	register("plain", "uuid-1", "", "http://127.0.0.1:9005/local")
	if id := register("plain", fpD, "shared", "http://127.0.0.1:9006/shared/plain"); id != DataHandler.DerivedDsId("plain", fpD) {
		t.Fatalf("expect derived id for name taken by local, got %s", id)
	}
	// pinnedSources：名字被固定给 fpC，其它来源得到派生名字；fpC 来时拿到名字
	if id := register("pinned", fpE, "mirrored", "http://127.0.0.1:9007/mirrored/pinned"); id != DataHandler.DerivedDsId("pinned", fpE) {
		t.Fatalf("expect non-pinned source get derived id, got %s", id)
	}
	if id := register("pinned", fpC, "mirrored", "http://127.0.0.1:9008/mirrored/pinned"); id != "pinned" {
		t.Fatalf("expect pinned source get the name, got %s", id)
	}
	// 按 URL 注销副本
	expectStatus(t, call(t, http.MethodDelete, inv.Url+"/inventory/prod?url=http://127.0.0.1:9001/mirrored/prod", nil), http.StatusAccepted, "remove one url")
	data, _ = getInvRecord(t, inv, "prod")
	if urls := urlsOf(data); contains(urls, "http://127.0.0.1:9001/mirrored/prod") || len(urls) != 2 {
		t.Fatalf("expect url removed, got %v", urls)
	}
	call(t, http.MethodDelete, inv.Url+"/inventory/prod?url=http://127.0.0.1:9002/mirrored/prod", nil)
	call(t, http.MethodDelete, inv.Url+"/inventory/prod?url=http://127.0.0.1:9004/mirrored/prod-alias", nil)
	if _, ok := getInvRecord(t, inv, "prod"); ok {
		t.Fatalf("expect record deleted when no url left")
	}
}

// TestSharedSchemaClosure shared 的 schema 不能引用本 vDS 之外的类型。
func TestSharedSchemaClosure(t *testing.T) {
	ds := newDsNode(t, "dsA", "")
	ds.start(t, map[string]interface{}{
		"local": map[string]interface{}{},
		"shared": []interface{}{map[string]interface{}{
			"name": "prod", "publishIntervalSec": 3600, "channel": dirChannel(filepath.Join(t.TempDir(), "channel")),
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
