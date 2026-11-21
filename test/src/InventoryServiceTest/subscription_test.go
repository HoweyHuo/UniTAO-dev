package InventoryServiceTest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"Data/DbConfig"
	"Data/DbIface"
	"InventoryService/Subscription"
	"UniTao/Test/DataServiceTest"

	"github.com/salesforce/UniTAO/lib/Util/Http"
)

// testClock 可手拨的时钟（同 Share.Publisher 的 SetClock 测试手法）。
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }

// reg 是注册载荷（订阅者声明的 data），与 POST /subscription 的 body 同形。
type reg = map[string]interface{}

// newMgr 建一个空库上的 Manager，时钟从当前时间开始（调用方接续 clock.apply(mgr)）。
func newMgr(t *testing.T) (*Subscription.Manager, *testClock) {
	t.Helper()
	mgr, clock, _ := newMgrDb(t)
	return mgr, clock
}

// newMgrDb 同 newMgr，另外返回底层库（测试直接写入旧版本的记录）。
func newMgrDb(t *testing.T) (*Subscription.Manager, *testClock, DbIface.Database) {
	t.Helper()
	db, err := DataServiceTest.NewMockDb(DbConfig.DatabaseConfig{}, "{}", nil)
	if err != nil {
		t.Fatalf("failed to create mock db: %s", err)
	}
	clock := &testClock{now: time.Now()}
	mgr, err := Subscription.NewManager(db, Subscription.Options{}, nil)
	if err != nil {
		t.Fatalf("failed to create subscription manager: %s", err)
	}
	return mgr, clock, db
}

func (c *testClock) apply(mgr *Subscription.Manager) {
	mgr.SetClock(c.Now)
}

func hxStatus(t *testing.T, hx *Http.HttpError) int {
	t.Helper()
	if hx == nil {
		t.Fatalf("expected http error, got nil")
	}
	return hx.Status
}

// sinkServer 模拟订阅者 endpoint：收推送、可切换状态码（模拟不可达/恢复）。
type sinkServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
	status int
}

func newSinkServer(status int) *sinkServer {
	s := &sinkServer{status: status}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, string(body))
		status := s.status
		s.mu.Unlock()
		w.WriteHeader(status)
	})
	s.srv = httptest.NewServer(mux)
	return s
}

func (s *sinkServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

func (s *sinkServer) setStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

// pushes 解析收到的全部推送 body。
func (s *sinkServer) pushes(t *testing.T) []Subscription.PushRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	result := []Subscription.PushRequest{}
	for _, body := range s.bodies {
		push := Subscription.PushRequest{}
		if err := json.Unmarshal([]byte(body), &push); err != nil {
			t.Fatalf("failed to parse push body [%s]: %s", body, err)
		}
		result = append(result, push)
	}
	return result
}

// register 按 HTTP 的形状调用 Register：载荷经一次 JSON 往返（[]string → []interface{}，数字 → float64）。
func register(t *testing.T, mgr *Subscription.Manager, payload reg) (map[string]interface{}, *Http.HttpError) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to marshal payload: %s", err)
	}
	generic := map[string]interface{}{}
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("failed to unmarshal payload: %s", err)
	}
	return mgr.Register(generic)
}

func mustRegister(t *testing.T, mgr *Subscription.Manager, payload reg) *Subscription.Subscription {
	t.Helper()
	saved, hx := register(t, mgr, payload)
	if hx != nil {
		t.Fatalf("register failed: %s", hx.Message)
	}
	return envelopeSub(t, saved)
}

// envelopeSub 把记录信封的 data 解析为运行时视图。
func envelopeSub(t *testing.T, envelope map[string]interface{}) *Subscription.Subscription {
	t.Helper()
	raw, err := json.Marshal(envelope["data"])
	if err != nil {
		t.Fatalf("failed to marshal envelope data: %s", err)
	}
	sub := &Subscription.Subscription{}
	if err := json.Unmarshal(raw, sub); err != nil {
		t.Fatalf("failed to parse envelope data: %s", err)
	}
	return sub
}

func listSubs(t *testing.T, mgr *Subscription.Manager) []Subscription.Subscription {
	t.Helper()
	envelopes, hx := mgr.List()
	if hx != nil {
		t.Fatalf("list failed: %s", hx.Message)
	}
	subs := []Subscription.Subscription{}
	for _, envelope := range envelopes {
		subs = append(subs, *envelopeSub(t, envelope))
	}
	return subs
}

func findSub(subs []Subscription.Subscription, endpoint string) *Subscription.Subscription {
	for idx := range subs {
		if subs[idx].Endpoint == endpoint {
			return &subs[idx]
		}
	}
	return nil
}

func dispatch(t *testing.T, mgr *Subscription.Manager, ds string, changes ...Subscription.Change) {
	t.Helper()
	// 单测里直接同步执行分发；Ingest 的异步路径由 TestIngestAsyncDelivery 覆盖
	mgr.Dispatch(&Subscription.ReportRequest{DsId: ds, Changes: changes})
}

// TestIngestAsyncDelivery 验证 Ingest → 分发队列 → 推送 的异步链路。
func TestIngestAsyncDelivery(t *testing.T) {
	mgr, _ := newMgr(t)
	sink := newSinkServer(http.StatusOK)
	defer sink.srv.Close()
	mustRegister(t, mgr, reg{"endpoint": sink.srv.URL})

	resp, hx := mgr.Ingest(&Subscription.ReportRequest{DsId: "ds1", Changes: []Subscription.Change{{Type: "app", Id: "app1"}}})
	if hx != nil {
		t.Fatalf("ingest failed: %s", hx.Message)
	}
	if resp.Accepted != 1 {
		t.Fatalf("expected accepted=1, got %d", resp.Accepted)
	}
	waitFor("push delivered via dispatch loop", func() bool { return sink.count() == 1 })
	if pushes := sink.pushes(t); len(pushes) != 1 || pushes[0].Changes[0].Id != "app1" {
		t.Fatalf("unexpected push: %+v", pushes)
	}
}

func waitFor(what string, check func() bool) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRegisterValidationAndUpsert(t *testing.T) {
	mgr, clock := newMgr(t)
	clock.apply(mgr)

	// endpoint 校验：空 / 非 URL / 非 http(s) → 400
	for _, bad := range []string{"", "not-a-url", "ftp://host/path"} {
		if hx := hxStatus(t, func() *Http.HttpError {
			_, hx := register(t, mgr, reg{"endpoint": bad})
			return hx
		}()); hx != 400 {
			t.Fatalf("endpoint [%s]: expected 400, got %d", bad, hx)
		}
	}
	// types 含空串 → 400
	if hx := hxStatus(t, func() *Http.HttpError {
		_, hx := register(t, mgr, reg{"endpoint": "http://host/p", "types": []string{""}})
		return hx
	}()); hx != 400 {
		t.Fatalf("empty type: expected 400, got %d", hx)
	}

	// 注册：默认 TTL、字段持久化
	first := mustRegister(t, mgr, reg{"endpoint": "http://host/p", "name": "ui", "types": []string{"app"}, "ds": "ds1"})
	if first.TtlSec != Subscription.DefaultTtlSec || first.Created == "" || first.Registered == "" {
		t.Fatalf("unexpected registration: %+v", first)
	}
	// ttl 声明过小 → 钳到下限
	tiny := mustRegister(t, mgr, reg{"endpoint": "http://host/tiny", "ttlSec": 1})
	if tiny.TtlSec != Subscription.MinTtlSec {
		t.Fatalf("expected ttl clamped to %d, got %d", Subscription.MinTtlSec, tiny.TtlSec)
	}

	// 同一 endpoint 心跳 = 幂等 upsert：过滤条件更新、Created 保留、注册时间刷新
	clock.now = clock.now.Add(time.Hour)
	second := mustRegister(t, mgr, reg{"endpoint": "http://host/p", "types": []string{"other"}})
	if second.Created != first.Created || second.Registered == first.Registered {
		t.Fatalf("heartbeat must keep created but refresh registered: first=%+v second=%+v", first, second)
	}
	subs := listSubs(t, mgr)
	if len(subs) != 2 {
		t.Fatalf("expected 2 subscriptions, got %d", len(subs))
	}
	if sub := findSub(subs, "http://host/p"); sub == nil || len(sub.Types) != 1 || sub.Types[0] != "other" {
		t.Fatalf("filters not updated on heartbeat: %+v", sub)
	}

	// 注销幂等
	if hx := mgr.Delete("http://host/p"); hx != nil {
		t.Fatalf("delete failed: %s", hx.Message)
	}
	if hx := mgr.Delete("http://host/p"); hx != nil {
		t.Fatalf("delete must be idempotent, got %s", hx.Message)
	}
	if hx := mgr.Delete(""); hx.Status != 400 {
		t.Fatalf("empty endpoint: expected 400, got %d", hx.Status)
	}
	if len(listSubs(t, mgr)) != 1 {
		t.Fatalf("expected 1 subscription after delete")
	}
}

func TestIngestValidation(t *testing.T) {
	mgr, _ := newMgr(t)

	// 校验失败一律 400
	if hx := hxStatus(t, func() *Http.HttpError {
		_, hx := mgr.Ingest(&Subscription.ReportRequest{Changes: []Subscription.Change{{Type: "a", Id: "1"}}})
		return hx
	}()); hx != 400 {
		t.Fatalf("missing dsId: expected 400, got %d", hx)
	}
	if hx := hxStatus(t, func() *Http.HttpError {
		_, hx := mgr.Ingest(&Subscription.ReportRequest{DsId: "ds1"})
		return hx
	}()); hx != 400 {
		t.Fatalf("no changes: expected 400, got %d", hx)
	}
	if hx := hxStatus(t, func() *Http.HttpError {
		_, hx := mgr.Ingest(&Subscription.ReportRequest{DsId: "ds1", Changes: []Subscription.Change{{Type: "", Id: "1"}}})
		return hx
	}()); hx != 400 {
		t.Fatalf("missing type: expected 400, got %d", hx)
	}
	if hx := hxStatus(t, func() *Http.HttpError {
		_, hx := mgr.Ingest(&Subscription.ReportRequest{DsId: "ds1", Changes: []Subscription.Change{{Type: "a", Id: ""}}})
		return hx
	}()); hx != 400 {
		t.Fatalf("missing id: expected 400, got %d", hx)
	}
}

func TestDispatchPushFanout(t *testing.T) {
	mgr, clock := newMgr(t)
	clock.apply(mgr)

	all := newSinkServer(http.StatusOK)
	defer all.srv.Close()
	filtered := newSinkServer(http.StatusOK)
	defer filtered.srv.Close()

	mustRegister(t, mgr, reg{"endpoint": all.srv.URL})
	mustRegister(t, mgr, reg{"endpoint": filtered.srv.URL, "types": []string{"app"}})

	dispatch(t, mgr, "ds1",
		Subscription.Change{Type: "app", Id: "app1"},
		Subscription.Change{Type: "other", Id: "other1"},
	)

	waitFor("all subscriber got 1 push", func() bool { return all.count() == 1 })
	waitFor("filtered subscriber got 1 push", func() bool { return filtered.count() == 1 })

	// 无过滤订阅者收到全部；载荷只有位置（ds/type/id），没有 op/ver/ts/data
	pushes := all.pushes(t)
	if len(pushes) != 1 || len(pushes[0].Changes) != 2 {
		t.Fatalf("expected 1 push with 2 changes, got %+v", pushes)
	}
	first := pushes[0].Changes[0]
	if first.Ds != "ds1" || first.Type != "app" || first.Id != "app1" {
		t.Fatalf("unexpected change: %+v", first)
	}
	// 过滤订阅者只收到 app
	fPushes := filtered.pushes(t)
	if len(fPushes) != 1 || len(fPushes[0].Changes) != 1 || fPushes[0].Changes[0].Type != "app" {
		t.Fatalf("filtered subscriber must only get app changes: %+v", fPushes)
	}

	// ds 过滤：不匹配的 DS 一条也不推
	mustRegister(t, mgr, reg{"endpoint": "http://dead.example/push", "ds": "dsX"})
	dispatch(t, mgr, "ds1", Subscription.Change{Type: "app", Id: "app2"})
	// 只验证不崩溃即可（dead endpoint 的失败标注走异步 goroutine）
}

func TestPushFailureAnnotationNoRetryAndRecovery(t *testing.T) {
	mgr, clock := newMgr(t)
	clock.apply(mgr)

	sink := newSinkServer(http.StatusInternalServerError)
	defer sink.srv.Close()
	mustRegister(t, mgr, reg{"endpoint": sink.srv.URL})

	dispatch(t, mgr, "ds1", Subscription.Change{Type: "app", Id: "app1"})
	waitFor("failure annotated", func() bool {
		sub := findSub(listSubs(t, mgr), sink.srv.URL)
		return sub != nil && sub.Unreachable && sub.LastError != "" && sub.LastAttempt != ""
	})

	// 不重试：等待一段时间后推送次数仍是 1
	time.Sleep(200 * time.Millisecond)
	if sink.count() != 1 {
		t.Fatalf("push must not be retried, got %d attempts", sink.count())
	}

	// 恢复：下一次事件照推（失败不导致停用），成功后清除标注
	sink.setStatus(http.StatusOK)
	dispatch(t, mgr, "ds1", Subscription.Change{Type: "app", Id: "app2"})
	waitFor("second push arrives", func() bool { return sink.count() == 2 })
	waitFor("unreachable cleared", func() bool {
		sub := findSub(listSubs(t, mgr), sink.srv.URL)
		return sub != nil && !sub.Unreachable
	})
}

func TestSweepExpiredLease(t *testing.T) {
	mgr, clock := newMgr(t)
	clock.apply(mgr)

	alive := mustRegister(t, mgr, reg{"endpoint": "http://host/alive"})
	mustRegister(t, mgr, reg{"endpoint": "http://host/gone"})

	// 快进到两个租约都过期，中间给 alive 心跳一次：只有没心跳的被清
	clock.now = clock.now.Add(time.Duration(Subscription.DefaultTtlSec+60) * time.Second)
	mustRegister(t, mgr, reg{"endpoint": alive.Endpoint})
	clock.now = clock.now.Add(time.Second)
	if err := mgr.Sweep(); err != nil {
		t.Fatalf("sweep failed: %s", err)
	}
	subs := listSubs(t, mgr)
	if findSub(subs, "http://host/gone") != nil {
		t.Fatalf("expired subscription must be swept")
	}
	if findSub(subs, alive.Endpoint) == nil {
		t.Fatalf("heartbeat subscription must survive sweep")
	}

	// 订阅者声明自己的租约：短租约过期更快
	short := mustRegister(t, mgr, reg{"endpoint": "http://host/short", "ttlSec": Subscription.MinTtlSec})
	clock.now = clock.now.Add(time.Duration(short.TtlSec+1) * time.Second)
	if err := mgr.Sweep(); err != nil {
		t.Fatalf("sweep failed: %s", err)
	}
	if findSub(listSubs(t, mgr), short.Endpoint) != nil {
		t.Fatalf("short lease must expire after its ttl")
	}
}

// seedLegacy 直接写入一条旧版本的订阅记录（绕过 Register）。
func seedLegacy(t *testing.T, db DbIface.Database, id string, data map[string]interface{}) {
	t.Helper()
	record := map[string]interface{}{
		"__type": Subscription.KeySubscription,
		"__id":   id,
		"__ver":  "0.0.1",
		"data":   data,
	}
	if err := db.Replace(Subscription.KeySubscription, map[string]interface{}{
		"__type": Subscription.KeySubscription,
		"__id":   id,
	}, record); err != nil {
		t.Fatalf("failed to seed legacy record [%s]: %s", id, err)
	}
}

// listIds 返回所有订阅记录的 __id → endpoint。
func listIds(t *testing.T, mgr *Subscription.Manager) map[string]string {
	t.Helper()
	envelopes, hx := mgr.List()
	if hx != nil {
		t.Fatalf("list failed: %s", hx.Message)
	}
	ids := map[string]string{}
	for _, envelope := range envelopes {
		id, _ := envelope["__id"].(string)
		data, _ := envelope["data"].(map[string]interface{})
		endpoint, _ := data["endpoint"].(string)
		ids[id] = endpoint
	}
	return ids
}

// TestSweepMigratesLegacyRecords 验证 Sweep 迁移旧版本记录：
// 拉取模式留下的无 endpoint 记录被删除；以 endpoint 原文为 __id 的记录改存到哈希 id（推送随之恢复）；
// 同一 endpoint 新旧两条并存时保留新的；迁移后租约到期按真实 __id 删除。
func TestSweepMigratesLegacyRecords(t *testing.T) {
	mgr, clock, db := newMgrDb(t)
	clock.apply(mgr)
	sink := newSinkServer(http.StatusOK)
	defer sink.srv.Close()
	nowStr := clock.now.UTC().Format(time.RFC3339)

	// 拉取模式（PR #70）：没有 endpoint、没有 registered
	seedLegacy(t, db, "sub-pullmodel01", map[string]interface{}{"name": "old", "created": nowStr})
	seedLegacy(t, db, "sub-pullmodel02", map[string]interface{}{"name": "old", "created": nowStr})
	// 推送模式初版（PR #75）：__id = endpoint 原文
	seedLegacy(t, db, sink.srv.URL, map[string]interface{}{
		"endpoint": sink.srv.URL, "name": "raw", "ttlSec": 3600, "created": nowStr, "registered": nowStr,
	})
	// 同一 endpoint：订阅者升级后已重新注册（新记录），旧的原文 id 记录还在
	mustRegister(t, mgr, reg{"endpoint": "http://host/both", "name": "new"})
	seedLegacy(t, db, "http://host/both", map[string]interface{}{
		"endpoint": "http://host/both", "name": "stale", "ttlSec": 60, "created": nowStr, "registered": nowStr,
	})

	// 迁移前：旧记录不参与推送（避免同一 endpoint 重复推送）
	dispatch(t, mgr, "ds1", Subscription.Change{Type: "app", Id: "app1"})
	time.Sleep(100 * time.Millisecond)
	if sink.count() != 0 {
		t.Fatalf("legacy record must not be pushed before migration, got %d", sink.count())
	}

	if err := mgr.Sweep(); err != nil {
		t.Fatalf("sweep failed: %s", err)
	}
	ids := listIds(t, mgr)
	if len(ids) != 2 {
		t.Fatalf("expected 2 records after migration, got %v", ids)
	}
	for id, endpoint := range ids {
		if endpoint == "" || id == endpoint || len(id) != len("sub-")+16 {
			t.Fatalf("record [%s] -> [%s] is not migrated to the hashed id", id, endpoint)
		}
	}
	if sub := findSub(listSubs(t, mgr), "http://host/both"); sub == nil || sub.Name != "new" {
		t.Fatalf("the re-registered record must be kept over the stale one: %+v", sub)
	}
	if sub := findSub(listSubs(t, mgr), sink.srv.URL); sub == nil || sub.Name != "raw" || sub.Registered != nowStr {
		t.Fatalf("legacy record data must be carried over: %+v", sub)
	}

	// 迁移后推送恢复
	dispatch(t, mgr, "ds1", Subscription.Change{Type: "app", Id: "app2"})
	waitFor("push after migration", func() bool { return sink.count() == 1 })
	if sink.count() != 1 {
		t.Fatalf("expected 1 push after migration, got %d", sink.count())
	}

	// 迁移是幂等的：再扫一次不变
	if err := mgr.Sweep(); err != nil {
		t.Fatalf("second sweep failed: %s", err)
	}
	if again := listIds(t, mgr); len(again) != 2 {
		t.Fatalf("sweep must be idempotent, got %v", again)
	}

	// 迁移后的记录租约到期按真实 __id 删除；新注册的（默认 24h 租约）保留
	clock.now = clock.now.Add(2 * time.Hour)
	if err := mgr.Sweep(); err != nil {
		t.Fatalf("sweep failed: %s", err)
	}
	subs := listSubs(t, mgr)
	if findSub(subs, sink.srv.URL) != nil {
		t.Fatalf("migrated record must expire after its ttl")
	}
	if findSub(subs, "http://host/both") == nil {
		t.Fatalf("re-registered record must survive")
	}
}
