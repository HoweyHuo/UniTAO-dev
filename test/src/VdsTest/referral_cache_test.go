package VdsTest

import (
	"net/http"
	"testing"
)

var siteSchema = map[string]interface{}{
	"name": map[string]interface{}{"type": "string"},
}

var networkSchema = map[string]interface{}{
	"sites": map[string]interface{}{
		"type":  "array",
		"items": map[string]interface{}{"type": "string", "contentMediaType": "inventory/site"},
	},
}

// referralHas 返回 Inventory 中 dataType 的 referral 是否包含 dsId。
func referralHas(t *testing.T, inv *invNode, dataType string, dsId string) bool {
	resp := call(t, http.MethodGet, inv.Url+"/referral/"+dataType, nil)
	if resp.Status != http.StatusOK {
		return false
	}
	data, ok := resp.Map(t)["data"].(map[string]interface{})
	if !ok {
		return false
	}
	list, _ := data["DataServices"].([]interface{})
	for _, item := range list {
		if item == dsId {
			return true
		}
	}
	return false
}

// TestReferralCacheRefresh：DS 缓存了某类型只在本 DS 上的 referral 之后，别的 DS 也登记了该类型，
// 引用别的 DS 上的记录时不必重启 DS（各 DS 都 404 时强制刷新一次 referral）。
func TestReferralCacheRefresh(t *testing.T) {
	inv := startInventory(t, nil)
	dsA := newDsNode(t, "dsA", inv.Url)
	dsA.start(t, map[string]interface{}{"local": map[string]interface{}{}})
	dsB := newDsNode(t, "dsB", inv.Url)
	dsB.start(t, map[string]interface{}{"local": map[string]interface{}{}})
	urlA := dsA.Url + "/local"
	urlB := dsB.Url + "/local"
	waitFor(t, "registrations", func() bool {
		_, okA := getInvRecord(t, inv, "dsA")
		_, okB := getInvRecord(t, inv, "dsB")
		return okA && okB
	})

	// 只有 dsA 有 site：dsA 缓存 site 的 referral = [dsA]
	expectStatus(t, call(t, http.MethodPost, urlA, schemaRecord("site", "0.0.1", siteSchema)), http.StatusCreated, "post site schema on dsA")
	expectStatus(t, call(t, http.MethodPost, urlA, schemaRecord("network", "0.0.1", networkSchema)), http.StatusCreated, "post network schema on dsA")
	expectStatus(t, call(t, http.MethodPost, urlA, dataRecord("site", "site-main", map[string]interface{}{"name": "main"})), http.StatusCreated, "post site-main")
	if err := inv.srv.SyncNow(); err != nil {
		t.Fatalf("inventory sync failed: %s", err)
	}
	waitFor(t, "site referral has dsA", func() bool { return referralHas(t, inv, "site", "dsA") })
	// 引用一条不存在的记录：本地 404 → 按 referral 查各 DS，缓存下 [dsA]
	resp := call(t, http.MethodPost, urlA, dataRecord("network", "net-0", map[string]interface{}{"sites": []interface{}{"site-main", "site-b"}}))
	expectStatus(t, resp, http.StatusBadRequest, "reference to missing site-b")

	// dsB 也登记 site 并写入 site-b
	expectStatus(t, call(t, http.MethodPost, urlB, schemaRecord("site", "0.0.1", siteSchema)), http.StatusCreated, "post site schema on dsB")
	expectStatus(t, call(t, http.MethodPost, urlB, dataRecord("site", "site-b", map[string]interface{}{"name": "b"})), http.StatusCreated, "post site-b")
	if err := inv.srv.SyncNow(); err != nil {
		t.Fatalf("inventory sync failed: %s", err)
	}
	waitFor(t, "site referral has dsB", func() bool { return referralHas(t, inv, "site", "dsB") })

	// 缓存未过期（30 秒），仍要能找到 dsB 上的 site-b
	resp = call(t, http.MethodPost, urlA, dataRecord("network", "net-1", map[string]interface{}{"sites": []interface{}{"site-main", "site-b"}}))
	expectStatus(t, resp, http.StatusCreated, "reference to site-b on dsB without restart")

	// 真正不存在的记录仍然 400
	resp = call(t, http.MethodPost, urlA, dataRecord("network", "net-2", map[string]interface{}{"sites": []interface{}{"site-x"}}))
	expectStatus(t, resp, http.StatusBadRequest, "reference to missing site-x")
}
