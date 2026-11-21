package VdsTest

// 记录变化订阅系统端到端测试：DS 上报（POST /report）→ Inventory 推送到订阅者 endpoint →
// 失败标注 / 租约清扫 / 注册注销。全部经 HTTP 走真实路由。
// 事件是 best-effort 的脏标记：只携带位置（ds/type/id），不保证送达，不重试。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func appSchema() map[string]interface{} {
	return schemaRecord("app", "0.0.1", map[string]interface{}{
		"name": map[string]interface{}{"type": "string"},
	})
}

func otherSchema() map[string]interface{} {
	return schemaRecord("other", "0.0.1", map[string]interface{}{
		"name": map[string]interface{}{"type": "string"},
	})
}

// pushSink 模拟订阅者 endpoint：收推送、可切换状态码（模拟不可达/恢复）。
type pushSink struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
	status int
}

func newPushSink(status int) *pushSink {
	s := &pushSink{status: status}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, string(body))
		status := s.status
		s.mu.Unlock()
		w.WriteHeader(status)
		w.Write([]byte(`{}`))
	}))
	return s
}

func (s *pushSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

func (s *pushSink) setStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

// changesOf 拍平收到的全部推送，返回 change 列表。
func (s *pushSink) changesOf(t *testing.T) []map[string]interface{} {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	changes := []map[string]interface{}{}
	for _, body := range s.bodies {
		push := map[string]interface{}{}
		if err := json.Unmarshal([]byte(body), &push); err != nil {
			t.Fatalf("failed to parse push body [%s]: %s", body, err)
		}
		list, ok := push["changes"].([]interface{})
		if !ok {
			t.Fatalf("push body has no changes array: %s", body)
		}
		for _, item := range list {
			change, ok := item.(map[string]interface{})
			if !ok {
				t.Fatalf("change is not an object: %v", item)
			}
			changes = append(changes, change)
		}
	}
	return changes
}

// subscribe 在 Inventory 上登记（或心跳）订阅；返回 sink 收到的推送。
func subscribe(t *testing.T, inv *invNode, body map[string]interface{}) {
	t.Helper()
	expectStatus(t, call(t, http.MethodPost, inv.Url+"/subscription", body), http.StatusOK, "subscribe")
}

func subscriptionList(t *testing.T, inv *invNode) []map[string]interface{} {
	t.Helper()
	resp := call(t, http.MethodGet, inv.Url+"/subscription", nil)
	expectStatus(t, resp, http.StatusOK, "list subscriptions")
	list, ok := resp.Json(t).([]interface{})
	if !ok {
		t.Fatalf("subscription list is not an array: %s", resp.Body)
	}
	// 每项是记录信封 {__id, __type, __ver, data}，返回其 data（订阅字段）
	result := []map[string]interface{}{}
	for _, item := range list {
		envelope, _ := item.(map[string]interface{})
		if envelope["__type"] != "subscription" {
			t.Fatalf("subscription list item is not a subscription record envelope: %v", item)
		}
		sub, _ := envelope["data"].(map[string]interface{})
		result = append(result, sub)
	}
	return result
}

func findSubscription(list []map[string]interface{}, endpoint string) map[string]interface{} {
	for _, sub := range list {
		if sub["endpoint"] == endpoint {
			return sub
		}
	}
	return nil
}

func waitForPush(t *testing.T, sink *pushSink, what string, check func(changes []map[string]interface{}) bool) {
	t.Helper()
	waitFor(t, what, func() bool { return check(sink.changesOf(t)) })
}

func TestReportPushFanout(t *testing.T) {
	inv := startInventory(t, nil)
	ds := newDsNode(t, "dsA", inv.Url)
	ds.start(t, map[string]interface{}{"local": map[string]interface{}{}})

	sink := newPushSink(http.StatusOK)
	defer sink.srv.Close()
	subscribe(t, inv, map[string]interface{}{"endpoint": sink.srv.URL, "name": "watcher"})

	// 注册 schema 不产生通知（schema 是内部类型，不上报）
	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", appSchema()), http.StatusCreated, "add schema")
	time.Sleep(200 * time.Millisecond)
	if sink.count() != 0 {
		t.Fatalf("schema registration must not be reported, got %d pushes", sink.count())
	}

	// create → 推送位置 {ds, type, id}；没有 op/ver/ts/data
	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", dataRecord("app", "app1", map[string]interface{}{"name": "one"})), http.StatusCreated, "create app1")
	waitForPush(t, sink, "create pushed", func(changes []map[string]interface{}) bool { return len(changes) == 1 })
	first := sink.changesOf(t)[0]
	if first["ds"] != "dsA" || first["type"] != "app" || first["id"] != "app1" {
		t.Fatalf("unexpected change: %v", first)
	}
	for _, absent := range []string{"op", "ver", "ts", "data"} {
		if _, has := first[absent]; has {
			t.Fatalf("push must not carry [%s]: %v", absent, first)
		}
	}

	// update / delete → 同样只推位置；订阅者回查推导（404 = 删除）
	expectStatus(t, call(t, http.MethodPut, ds.Url+"/local/app/app1", dataRecord("app", "app1", map[string]interface{}{"name": "one-v2"})), http.StatusCreated, "update app1")
	waitForPush(t, sink, "update pushed", func(changes []map[string]interface{}) bool { return len(changes) == 2 })
	expectStatus(t, call(t, http.MethodDelete, ds.Url+"/local/app/app1", nil), http.StatusAccepted, "delete app1")
	waitForPush(t, sink, "delete pushed", func(changes []map[string]interface{}) bool { return len(changes) == 3 })
	if last := sink.changesOf(t)[2]; last["id"] != "app1" {
		t.Fatalf("unexpected delete push: %v", last)
	}
}

func TestSubscriptionFilters(t *testing.T) {
	inv := startInventory(t, nil)
	ds := newDsNode(t, "dsA", inv.Url)
	ds.start(t, map[string]interface{}{"local": map[string]interface{}{}})

	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", appSchema()), http.StatusCreated, "add app schema")
	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", otherSchema()), http.StatusCreated, "add other schema")

	appOnly := newPushSink(http.StatusOK)
	defer appOnly.srv.Close()
	everything := newPushSink(http.StatusOK)
	defer everything.srv.Close()
	subscribe(t, inv, map[string]interface{}{"endpoint": appOnly.srv.URL, "types": []string{"app"}})
	subscribe(t, inv, map[string]interface{}{"endpoint": everything.srv.URL})

	// 各写一条：app 过滤订阅只收 app，无过滤收全部
	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", dataRecord("app", "app1", map[string]interface{}{"name": "a"})), http.StatusCreated, "create app1")
	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", dataRecord("other", "other1", map[string]interface{}{"name": "b"})), http.StatusCreated, "create other1")

	waitForPush(t, everything, "all changes pushed", func(changes []map[string]interface{}) bool { return len(changes) == 2 })
	waitForPush(t, appOnly, "filtered changes pushed", func(changes []map[string]interface{}) bool { return len(changes) == 1 })
	if got := appOnly.changesOf(t)[0]; got["type"] != "app" {
		t.Fatalf("filtered subscription must only receive app, got %v", got)
	}
}

func TestPushFailureAnnotationAndRecovery(t *testing.T) {
	inv := startInventory(t, nil)
	ds := newDsNode(t, "dsA", inv.Url)
	ds.start(t, map[string]interface{}{"local": map[string]interface{}{}})
	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", appSchema()), http.StatusCreated, "add schema")

	sink := newPushSink(http.StatusInternalServerError)
	defer sink.srv.Close()
	subscribe(t, inv, map[string]interface{}{"endpoint": sink.srv.URL})

	// 推送失败 → 订阅记录标注不可达 + 最新错误
	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", dataRecord("app", "app1", map[string]interface{}{"name": "one"})), http.StatusCreated, "create app1")
	waitFor(t, "failure annotated", func() bool {
		sub := findSubscription(subscriptionList(t, inv), sink.srv.URL)
		return sub != nil && sub["unreachable"] == true && sub["lastError"] != ""
	})

	// 不重试：等待后推送次数不变
	time.Sleep(300 * time.Millisecond)
	if sink.count() != 1 {
		t.Fatalf("push must not be retried, got %d attempts", sink.count())
	}

	// 恢复：失败不停用，下一次变更照推，成功后清除标注
	sink.setStatus(http.StatusOK)
	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", dataRecord("app", "app2", map[string]interface{}{"name": "two"})), http.StatusCreated, "create app2")
	waitForPush(t, sink, "second push after recovery", func(changes []map[string]interface{}) bool { return len(changes) == 2 })
	waitFor(t, "unreachable cleared", func() bool {
		sub := findSubscription(subscriptionList(t, inv), sink.srv.URL)
		return sub != nil && sub["unreachable"] != true
	})
}

func TestRegisterHeartbeatUnregister(t *testing.T) {
	inv := startInventory(t, nil)

	sink := newPushSink(http.StatusOK)
	defer sink.srv.Close()

	// 无 endpoint → 400；非法 endpoint → 400
	expectStatus(t, call(t, http.MethodPost, inv.Url+"/subscription", map[string]interface{}{"name": "x"}), http.StatusBadRequest, "register without endpoint")
	expectStatus(t, call(t, http.MethodPost, inv.Url+"/subscription", map[string]interface{}{"endpoint": "not-a-url"}), http.StatusBadRequest, "register with bad endpoint")

	// 注册 → 列表可见；心跳（同 endpoint 重复注册）→ 仍是一条
	subscribe(t, inv, map[string]interface{}{"endpoint": sink.srv.URL, "name": "ui"})
	subscribe(t, inv, map[string]interface{}{"endpoint": sink.srv.URL, "name": "ui-v2"})
	list := subscriptionList(t, inv)
	if len(list) != 1 {
		t.Fatalf("heartbeat must be idempotent upsert, got %d subscriptions", len(list))
	}
	if list[0]["name"] != "ui-v2" {
		t.Fatalf("heartbeat must update filters, got %v", list[0])
	}

	// 注销（?url=）→ 列表为空；再注销幂等
	expectStatus(t, call(t, http.MethodDelete, fmt.Sprintf("%s/subscription?url=%s", inv.Url, sink.srv.URL), nil), http.StatusOK, "unsubscribe")
	if len(subscriptionList(t, inv)) != 0 {
		t.Fatalf("subscription must be gone after unsubscribe")
	}
	expectStatus(t, call(t, http.MethodDelete, fmt.Sprintf("%s/subscription?url=%s", inv.Url, sink.srv.URL), nil), http.StatusOK, "unsubscribe again")

	// 不认识的订阅子路由 → 404（不落回通用 GET /{type} 分发）
	expectStatus(t, call(t, http.MethodGet, inv.Url+"/subscription/whatever", nil), http.StatusNotFound, "unknown subscription route")
}

func TestSubscriberLeaseSweep(t *testing.T) {
	inv := startInventory(t, nil)
	sink := newPushSink(http.StatusOK)
	defer sink.srv.Close()
	subscribe(t, inv, map[string]interface{}{"endpoint": sink.srv.URL, "name": "gone"})

	// 快进超过租约（默认 24h）后手动清扫 → 订阅被清
	mgr := inv.srv.SubscriptionManager()
	if mgr == nil {
		t.Fatalf("subscription manager is nil")
	}
	future := time.Now().Add(25 * time.Hour)
	mgr.SetClock(func() time.Time { return future })
	if err := mgr.Sweep(); err != nil {
		t.Fatalf("sweep failed: %s", err)
	}
	if len(subscriptionList(t, inv)) != 0 {
		t.Fatalf("expired subscription must be swept")
	}
}

// TestNoBackfillWhenInventoryComesBack Inventory 宕机期间的通知被丢弃、不回填：
// DS 重启指向真实 Inventory 后，只有新写入产生推送——漏掉的通知由订阅者对账弥补。
func TestNoBackfillWhenInventoryComesBack(t *testing.T) {
	inv := startInventory(t, nil)
	// 先指向一个不存在的地址：通知发送失败即弃
	ds := newDsNode(t, "dsA", "http://127.0.0.1:1")
	ds.start(t, map[string]interface{}{"local": map[string]interface{}{}})
	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", appSchema()), http.StatusCreated, "add schema")
	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", dataRecord("app", "app1", map[string]interface{}{"name": "one"})), http.StatusCreated, "create app1 during outage")
	time.Sleep(200 * time.Millisecond) // 等 journal 处理完宕机期间的条目（失败即弃）

	// 指向真实 Inventory 重启，订阅后写入新记录：只收到新通知
	ds.invUrl = inv.Url
	ds.start(t, map[string]interface{}{"local": map[string]interface{}{}})
	sink := newPushSink(http.StatusOK)
	defer sink.srv.Close()
	subscribe(t, inv, map[string]interface{}{"endpoint": sink.srv.URL})
	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", dataRecord("app", "app2", map[string]interface{}{"name": "two"})), http.StatusCreated, "create app2 after recovery")

	waitForPush(t, sink, "new change pushed", func(changes []map[string]interface{}) bool { return len(changes) == 1 })
	changes := sink.changesOf(t)
	if len(changes) != 1 || changes[0]["id"] != "app2" {
		t.Fatalf("only the post-recovery change must arrive (no backfill), got %v", changes)
	}
}

// TestListWithVer 对账列表：Inventory GET /{type}?ver=1 返回 {ds,id,ver}，
// 订阅者 diff id 发现新增/删除、diff ver 发现更新。
func TestListWithVer(t *testing.T) {
	inv := startInventory(t, nil)
	ds := newDsNode(t, "dsA", inv.Url)
	ds.start(t, map[string]interface{}{"local": map[string]interface{}{}})
	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", appSchema()), http.StatusCreated, "add schema")
	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", dataRecord("app", "app1", map[string]interface{}{"name": "one"})), http.StatusCreated, "create app1")
	waitFor(t, "referral synced", func() bool {
		resp := call(t, http.MethodGet, inv.Url+"/app", nil)
		return resp.Status == http.StatusOK
	})

	resp := call(t, http.MethodGet, inv.Url+"/app?ver=1", nil)
	expectStatus(t, resp, http.StatusOK, "ver list")
	list, ok := resp.Json(t).([]interface{})
	if !ok || len(list) != 1 {
		t.Fatalf("expected 1 ver entry, got: %s", resp.Body)
	}
	entry, _ := list[0].(map[string]interface{})
	if entry["ds"] != "dsA" || entry["id"] != "app1" || entry["ver"] != "0.0.1" {
		t.Fatalf("unexpected ver entry: %v", entry)
	}

	// 不带 ver 的列表保持原形状（ds/id 字符串数组）
	plain := call(t, http.MethodGet, inv.Url+"/app", nil)
	expectStatus(t, plain, http.StatusOK, "plain list")
	plainList, _ := plain.Json(t).([]interface{})
	if len(plainList) != 1 || plainList[0] != "dsA/app1" {
		t.Fatalf("plain list must stay [{ds}/{id}] strings, got: %s", plain.Body)
	}
}
