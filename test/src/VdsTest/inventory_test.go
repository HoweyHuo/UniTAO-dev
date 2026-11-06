package VdsTest

import (
	"net/http"
	"testing"
)

// TestReregisterReplacesUrl：同一个 DS（instanceId 相同）换了 URL 重新注册时，Inventory 替换原记录，
// 而不是新增一条同 id 的记录。升级到 vDS 后 local 的 URL 多了 /local，在 MongoDB 上曾因
// PutData 的过滤条件带了 table 字段而每次都插入重复记录。
func TestReregisterReplacesUrl(t *testing.T) {
	inv := startInventory(t, nil)
	register := func(url string) {
		t.Helper()
		resp := call(t, http.MethodPut, inv.Url+"/", map[string]interface{}{
			"__type": "inventory", "__id": "DataService01", "__ver": "0.0.1",
			"data": map[string]interface{}{"dsId": "DataService01", "instanceId": "uuid-ds01", "url": []string{url}},
		})
		if resp.Status >= 300 {
			t.Fatalf("register %s failed: %d %s", url, resp.Status, resp.Body)
		}
		if id := string(resp.Body); id != "DataService01" {
			t.Fatalf("expect DataService01 kept for re-registration, got %s", id)
		}
	}
	register("http://127.0.0.1:9001")
	register("http://127.0.0.1:9001/local")

	expectList(t, call(t, http.MethodGet, inv.Url+"/inventory", nil), []string{"DataService01"}, "inventory list")
	data, ok := getInvRecord(t, inv, "DataService01")
	if !ok {
		t.Fatalf("expect record DataService01")
	}
	if urls := urlsOf(data); len(urls) != 1 || urls[0] != "http://127.0.0.1:9001/local" {
		t.Fatalf("expect url replaced by the new one, got %v", urls)
	}
}
