package VdsTest

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"InventoryService/DataHandler"
)

// TestMirroredAliasUnifiesInventoryId：镜像因撞名在 Inventory 中被改名为 {name}-{指纹前 8 位} 后，
// 在 config 中给它配 alias 并重启，DS 上的路径和 Inventory 的 dsId 都变成 alias，旧记录被删除。
//
// Inventory 的 DELETE 被人为延迟：注册必须等旧 URL 注销完成后再进行，否则新名字的注册会因
// 指纹相同被合并进旧记录，拿回旧名字。
func TestMirroredAliasUnifiesInventoryId(t *testing.T) {
	inv := startInventory(t, nil)
	channelDir := filepath.Join(t.TempDir(), "channel")
	keyFile, fingerprint := newSigningKey(t)
	keyB := newSubKey(t)

	// 来源在另一个网络，不注册到这个 Inventory
	dsA := newDsNode(t, "dsA", "")
	dsA.start(t, map[string]interface{}{
		"shared": []interface{}{map[string]interface{}{
			"name": "prod", "signingKeyFile": keyFile, "publishIntervalSec": 3600,
			"channel": withEncrypt(dirChannel(channelDir), recipientsFile(t, keyB)),
		}},
	})
	expectStatus(t, call(t, http.MethodPost, dsA.Url+"/shared/prod", schemaRecord("rack", "0.0.1", rackSchema)), http.StatusCreated, "post rack schema")
	waitJournal(t, dsA, "shared/prod", 1)
	pub := dsA.srv.GetVds("shared/prod").Publisher

	// 本网已有另一个同名来源占着 prod
	otherFp := "SHA256:ffffffff99999999"
	expectStatus(t, call(t, http.MethodPut, inv.Url+"/", map[string]interface{}{
		"__type": "inventory", "__id": "prod", "__ver": "0.0.1",
		"data": map[string]interface{}{"dsId": "prod", "instanceId": otherFp, "kind": "mirrored", "url": []string{"http://127.0.0.1:9009/mirrored/prod"}},
	}), http.StatusAccepted, "register other prod")

	mirrorCfg := map[string]interface{}{
		"name": "prod", "trustedFingerprint": fingerprint, "pullIntervalSec": 3600,
		"channel": withDecrypt(dirChannel(channelDir), keyB),
	}
	dsB := newDsNode(t, "dsB", inv.Url)
	dsB.start(t, map[string]interface{}{"local": map[string]interface{}{}, "mirrored": []interface{}{mirrorCfg}})
	syncAll(t, pub, dsB.srv.GetVds("mirrored/prod").Mirror)
	derived := DataHandler.DerivedDsId("prod", fingerprint)
	waitFor(t, "mirror registered under derived id", func() bool {
		return dsB.srv.GetVds("mirrored/prod").InvId() == derived
	})

	// 配 alias 重启，DELETE 延迟到达 Inventory
	inv.setHook(func(r *http.Request) {
		if r.Method == http.MethodDelete {
			time.Sleep(300 * time.Millisecond)
		}
	})
	defer inv.setHook(nil)
	mirrorCfg["alias"] = "prod-b"
	dsB.start(t, map[string]interface{}{"local": map[string]interface{}{}, "mirrored": []interface{}{mirrorCfg}})

	vds := dsB.srv.GetVds("mirrored/prod-b")
	if vds == nil {
		t.Fatalf("expect vDS mirrored/prod-b after alias change")
	}
	waitFor(t, "alias registered", func() bool { _, ok := getInvRecord(t, inv, "prod-b"); return ok })
	if id := vds.InvId(); id != "prod-b" {
		t.Fatalf("expect inventory id prod-b for mirrored/prod-b, got %s", id)
	}
	data, _ := getInvRecord(t, inv, "prod-b")
	if urls := urlsOf(data); len(urls) != 1 || urls[0] != dsB.Url+"/mirrored/prod-b" {
		t.Fatalf("expect prod-b to hold only the new url, got %v", urls)
	}
	if _, ok := getInvRecord(t, inv, derived); ok {
		t.Fatalf("expect old record %s removed after alias change", derived)
	}
	other, ok := getInvRecord(t, inv, "prod")
	if !ok || other["instanceId"] != otherFp {
		t.Fatalf("expect the other prod untouched, got %v", other)
	}

	// 新路径重新同步（新 vDS 的 state 是空的，重新订阅），旧路径不存在
	syncAll(t, pub, vds.Mirror)
	expectStatus(t, call(t, http.MethodGet, dsB.Url+"/mirrored/prod-b/schema/rack", nil), http.StatusOK, "rack schema on new path")
	expectStatus(t, call(t, http.MethodGet, dsB.Url+"/mirrored/prod/schema/rack", nil), http.StatusNotFound, "old path")

	// 注销完成后 state.json 中不再有待注销的 URL
	raw, err := os.ReadFile(filepath.Join(dsB.dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]interface{}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if pending, ok := state["unregister"]; ok {
		t.Fatalf("expect no pending unregister in state.json, got %v", pending)
	}
}
