package VdsTest

// 记录变化订阅系统端到端测试：DS 上报（POST /report）→ Inventory 订阅登记 →
// 拉取（cursor + ack）→ 清理（判死/保留期）。全部经 HTTP 走真实路由。

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

func appSchema() map[string]interface{} {
	return schemaRecord("app", "0.0.1", map[string]interface{}{
		"name": map[string]interface{}{"type": "string"},
	})
}

// subscribe 在 Inventory 上登记订阅，返回 subscriberId。
func subscribe(t *testing.T, inv *invNode, filter map[string]interface{}) string {
	t.Helper()
	resp := call(t, http.MethodPost, inv.Url+"/subscription", filter)
	expectStatus(t, resp, http.StatusOK, "subscribe")
	body := resp.Map(t)
	subId, _ := body["subscriberId"].(string)
	if subId == "" {
		t.Fatalf("no subscriberId in response: %s", resp.Body)
	}
	return subId
}

// pullChanges 拉取变更，返回 PullResponse map。
func pullChanges(t *testing.T, inv *invNode, subId string, query string) map[string]interface{} {
	t.Helper()
	resp := call(t, http.MethodGet, fmt.Sprintf("%s/subscription/%s/changes%s", inv.Url, subId, query), nil)
	expectStatus(t, resp, http.StatusOK, "pull changes")
	return resp.Map(t)
}

func changeSeqs(t *testing.T, resp map[string]interface{}) []float64 {
	t.Helper()
	list, ok := resp["changes"].([]interface{})
	if !ok {
		t.Fatalf("changes is not an array: %v", resp)
	}
	seqs := []float64{}
	for _, item := range list {
		change, ok := item.(map[string]interface{})
		if !ok {
			t.Fatalf("change is not an object: %v", item)
		}
		seq, _ := change["seq"].(float64)
		seqs = append(seqs, seq)
	}
	return seqs
}

func nextOf(t *testing.T, resp map[string]interface{}) uint64 {
	t.Helper()
	next, _ := resp["next"].(float64)
	return uint64(next)
}

func TestReportSubscribePullAck(t *testing.T) {
	inv := startInventory(t, nil)
	ds := newDsNode(t, "dsA", inv.Url)
	ds.start(t, map[string]interface{}{"local": map[string]interface{}{}})

	// 注册 schema 不产生变更（schema 是内部类型，不上报）
	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", appSchema()), http.StatusCreated, "add schema")

	// /report 校验：缺 dsId → 400
	expectStatus(t, call(t, http.MethodPost, inv.Url+"/report", map[string]interface{}{
		"changes": []map[string]interface{}{{"type": "app", "id": "app1", "op": "upsert"}},
	}), http.StatusBadRequest, "report without dsId")

	// 先订阅（订阅默认从"现在"开始），再写入
	subId := subscribe(t, inv, map[string]interface{}{"name": "watcher"})
	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", dataRecord("app", "app1", map[string]interface{}{"name": "one"})), http.StatusCreated, "create app1")

	// create → upsert 变更
	waitFor(t, "create change reported", func() bool {
		resp := pullChanges(t, inv, subId, "?limit=100")
		return len(changeSeqs(t, resp)) == 1
	})
	resp := pullChanges(t, inv, subId, "?limit=100")
	changes, _ := resp["changes"].([]interface{})
	first, _ := changes[0].(map[string]interface{})
	if first["op"] != "upsert" || first["type"] != "app" || first["id"] != "app1" || first["ds"] != "dsA" || first["vds"] != "local" {
		t.Fatalf("unexpected change: %v", first)
	}
	if _, has := first["data"]; has {
		t.Fatalf("change must not carry record content: %v", first)
	}

	// update → upsert（新版本）
	expectStatus(t, call(t, http.MethodPut, ds.Url+"/local/app/app1", dataRecord("app", "app1", map[string]interface{}{"name": "one-v2"})), http.StatusCreated, "update app1")
	waitFor(t, "update change reported", func() bool {
		return len(changeSeqs(t, pullChanges(t, inv, subId, "?after=1"))) == 1
	})

	// delete → delete 变更
	expectStatus(t, call(t, http.MethodDelete, ds.Url+"/local/app/app1", nil), http.StatusAccepted, "delete app1")
	waitFor(t, "delete change reported", func() bool {
		resp := pullChanges(t, inv, subId, "?after=2")
		seqs := changeSeqs(t, resp)
		if len(seqs) != 1 {
			return false
		}
		changes, _ := resp["changes"].([]interface{})
		change, _ := changes[0].(map[string]interface{})
		return change["op"] == "delete"
	})

	// ack 推进游标，状态可查
	resp = pullChanges(t, inv, subId, "?limit=100")
	if next := nextOf(t, resp); next != 3 {
		t.Fatalf("expected next=3 (head), got %d", next)
	}
	ack := call(t, http.MethodPost, fmt.Sprintf("%s/subscription/%s/ack", inv.Url, subId), map[string]interface{}{"cursor": 3})
	expectStatus(t, ack, http.StatusOK, "ack")
	status := call(t, http.MethodGet, fmt.Sprintf("%s/subscription/%s", inv.Url, subId), nil)
	expectStatus(t, status, http.StatusOK, "subscription status")
	body := status.Map(t)
	subscription, _ := body["subscription"].(map[string]interface{})
	if cursor, _ := subscription["cursor"].(float64); cursor != 3 {
		t.Fatalf("expected cursor=3, got %v", subscription["cursor"])
	}

	// 订阅列表包含该订阅
	list := call(t, http.MethodGet, inv.Url+"/subscription", nil)
	expectStatus(t, list, http.StatusOK, "list subscriptions")
	ids := []string{}
	for _, item := range list.Json(t).([]interface{}) {
		sub, _ := item.(map[string]interface{})
		id, _ := sub["id"].(string)
		ids = append(ids, id)
	}
	if !contains(ids, subId) {
		t.Fatalf("subscription [%s] not in list: %v", subId, ids)
	}

	// 注销后拉取 → 404
	expectStatus(t, call(t, http.MethodDelete, fmt.Sprintf("%s/subscription/%s", inv.Url, subId), nil), http.StatusOK, "unsubscribe")
	expectStatus(t, call(t, http.MethodGet, fmt.Sprintf("%s/subscription/%s", inv.Url, subId), nil), http.StatusNotFound, "pull after unsubscribe")
}

func TestSubscriptionFilteredWatermark(t *testing.T) {
	inv := startInventory(t, nil)
	ds := newDsNode(t, "dsA", inv.Url)
	ds.start(t, map[string]interface{}{"local": map[string]interface{}{}})

	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", appSchema()), http.StatusCreated, "add app schema")
	otherSchema := schemaRecord("other", "0.0.1", map[string]interface{}{
		"name": map[string]interface{}{"type": "string"},
	})
	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", otherSchema), http.StatusCreated, "add other schema")

	subId := subscribe(t, inv, map[string]interface{}{"types": []string{"app"}})

	// 交替写两种类型（id 各不相同），过滤订阅用 limit=1 逐条推进
	for idx := 0; idx < 3; idx++ {
		expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", dataRecord("app", fmt.Sprintf("appA%d", idx), map[string]interface{}{"name": fmt.Sprintf("a%d", idx)})), http.StatusCreated, "create appA")
		expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", dataRecord("other", fmt.Sprintf("otherB%d", idx), map[string]interface{}{"name": fmt.Sprintf("b%d", idx)})), http.StatusCreated, "create otherB")
	}

	// 等 head 到 6（3 个 app + 3 个 other）
	var head uint64
	waitFor(t, "all changes reported", func() bool {
		resp := pullChanges(t, inv, subId, "?limit=1000")
		head = nextOf(t, resp)
		return head == 6
	})

	// 从头回放，limit=1 逐条推进：next 必须推进过 other 的行
	cursor := uint64(0)
	matched := []float64{}
	for cursor < head {
		resp := pullChanges(t, inv, subId, fmt.Sprintf("?after=%d&limit=1", cursor))
		seqs := changeSeqs(t, resp)
		if len(seqs) == 1 {
			matched = append(matched, seqs[0])
		}
		next := nextOf(t, resp)
		if next <= cursor {
			t.Fatalf("next must advance: cursor=%d next=%d", cursor, next)
		}
		cursor = next
	}
	// 只匹配到 3 条 app 变更
	if len(matched) != 3 {
		t.Fatalf("expected 3 matched changes, got %v", matched)
	}
}

func TestReportRetryWhenInventoryDownAndRestart(t *testing.T) {
	inv := startInventory(t, nil)
	// 先指向一个不存在的地址：上报失败，journal 条目滞留
	ds := newDsNode(t, "dsA", "http://127.0.0.1:1")
	ds.start(t, map[string]interface{}{"local": map[string]interface{}{}})
	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", appSchema()), http.StatusCreated, "add schema")
	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", dataRecord("app", "app1", map[string]interface{}{"name": "one"})), http.StatusCreated, "create app1")

	// 指向真实 Inventory 重启（MemDb 持久，journal 条目保留），自注册完成后补报
	ds.invUrl = inv.Url
	ds.start(t, map[string]interface{}{"local": map[string]interface{}{}})

	subId := subscribe(t, inv, map[string]interface{}{})
	waitFor(t, "pre-outage change delivered after restart", func() bool {
		resp := call(t, http.MethodGet, fmt.Sprintf("%s/subscription/%s/changes", inv.Url, subId), nil)
		if resp.Status != http.StatusOK {
			return false
		}
		body := resp.Map(t)
		head, _ := body["head"].(float64)
		return head >= 1 && len(changeSeqs(t, body)) == 1
	})
}

func TestStaleSubscriberReaping(t *testing.T) {
	inv := startInventory(t, nil)
	ds := newDsNode(t, "dsA", inv.Url)
	ds.start(t, map[string]interface{}{"local": map[string]interface{}{}})

	subId := subscribe(t, inv, map[string]interface{}{})
	status := call(t, http.MethodGet, fmt.Sprintf("%s/subscription/%s", inv.Url, subId), nil)
	expectStatus(t, status, http.StatusOK, "subscription status")

	// 快进超过判死时长（默认 24h）后手动触发清理
	mgr := inv.srv.SubscriptionManager()
	if mgr == nil {
		t.Fatalf("subscription manager is nil")
	}
	future := time.Now().Add(25 * time.Hour)
	mgr.SetClock(func() time.Time { return future })
	if err := mgr.Prune(); err != nil {
		t.Fatalf("prune failed: %s", err)
	}
	expectStatus(t, call(t, http.MethodGet, fmt.Sprintf("%s/subscription/%s", inv.Url, subId), nil), http.StatusNotFound, "reaped subscription")
}
