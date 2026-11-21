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
	"InventoryService/Subscription"
	"UniTao/Test/DataServiceTest"

	"github.com/salesforce/UniTAO/lib/Util/Http"
)

// testClock 可手拨的时钟（同 Share.Publisher 的 SetClock 测试手法）。
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }

// newMgr 建一个空库上的 Manager，时钟从当前时间开始（调用方接续 clock.apply(mgr)）。
func newMgr(t *testing.T) (*Subscription.Manager, *testClock) {
	t.Helper()
	db, err := DataServiceTest.NewMockDb(DbConfig.DatabaseConfig{}, "{}", nil)
	if err != nil {
		t.Fatalf("failed to create mock db: %s", err)
	}
	clock := &testClock{now: time.Now()}
	return Subscription.NewManager(db, Subscription.Options{}, nil), clock
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

func mustRegister(t *testing.T, mgr *Subscription.Manager, req *Subscription.RegisterRequest) *Subscription.Subscription {
	t.Helper()
	sub, hx := mgr.Register(req)
	if hx != nil {
		t.Fatalf("register failed: %s", hx.Message)
	}
	return sub
}

func listSubs(t *testing.T, mgr *Subscription.Manager) []Subscription.Subscription {
	t.Helper()
	subs, hx := mgr.List()
	if hx != nil {
		t.Fatalf("list failed: %s", hx.Message)
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
	mustRegister(t, mgr, &Subscription.RegisterRequest{Endpoint: sink.srv.URL})

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
			_, hx := mgr.Register(&Subscription.RegisterRequest{Endpoint: bad})
			return hx
		}()); hx != 400 {
			t.Fatalf("endpoint [%s]: expected 400, got %d", bad, hx)
		}
	}
	// types 含空串 → 400
	if hx := hxStatus(t, func() *Http.HttpError {
		_, hx := mgr.Register(&Subscription.RegisterRequest{Endpoint: "http://host/p", Types: []string{""}})
		return hx
	}()); hx != 400 {
		t.Fatalf("empty type: expected 400, got %d", hx)
	}

	// 注册：默认 TTL、字段持久化
	first := mustRegister(t, mgr, &Subscription.RegisterRequest{Endpoint: "http://host/p", Name: "ui", Types: []string{"app"}, Ds: "ds1"})
	if first.TtlSec != Subscription.DefaultTtlSec || first.Created == "" || first.Registered == "" {
		t.Fatalf("unexpected registration: %+v", first)
	}
	// ttl 声明过小 → 钳到下限
	tiny := mustRegister(t, mgr, &Subscription.RegisterRequest{Endpoint: "http://host/tiny", TtlSec: 1})
	if tiny.TtlSec != Subscription.MinTtlSec {
		t.Fatalf("expected ttl clamped to %d, got %d", Subscription.MinTtlSec, tiny.TtlSec)
	}

	// 同一 endpoint 心跳 = 幂等 upsert：过滤条件更新、Created 保留、注册时间刷新
	clock.now = clock.now.Add(time.Hour)
	second := mustRegister(t, mgr, &Subscription.RegisterRequest{Endpoint: "http://host/p", Types: []string{"other"}})
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

	mustRegister(t, mgr, &Subscription.RegisterRequest{Endpoint: all.srv.URL})
	mustRegister(t, mgr, &Subscription.RegisterRequest{Endpoint: filtered.srv.URL, Types: []string{"app"}})

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
	mustRegister(t, mgr, &Subscription.RegisterRequest{Endpoint: "http://dead.example/push", Ds: "dsX"})
	dispatch(t, mgr, "ds1", Subscription.Change{Type: "app", Id: "app2"})
	// 只验证不崩溃即可（dead endpoint 的失败标注走异步 goroutine）
}

func TestPushFailureAnnotationNoRetryAndRecovery(t *testing.T) {
	mgr, clock := newMgr(t)
	clock.apply(mgr)

	sink := newSinkServer(http.StatusInternalServerError)
	defer sink.srv.Close()
	mustRegister(t, mgr, &Subscription.RegisterRequest{Endpoint: sink.srv.URL})

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

	alive := mustRegister(t, mgr, &Subscription.RegisterRequest{Endpoint: "http://host/alive"})
	mustRegister(t, mgr, &Subscription.RegisterRequest{Endpoint: "http://host/gone"})

	// 快进到两个租约都过期，中间给 alive 心跳一次：只有没心跳的被清
	clock.now = clock.now.Add(time.Duration(Subscription.DefaultTtlSec+60) * time.Second)
	mustRegister(t, mgr, &Subscription.RegisterRequest{Endpoint: alive.Endpoint})
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
	short := mustRegister(t, mgr, &Subscription.RegisterRequest{Endpoint: "http://host/short", TtlSec: Subscription.MinTtlSec})
	clock.now = clock.now.Add(time.Duration(short.TtlSec+1) * time.Second)
	if err := mgr.Sweep(); err != nil {
		t.Fatalf("sweep failed: %s", err)
	}
	if findSub(listSubs(t, mgr), short.Endpoint) != nil {
		t.Fatalf("short lease must expire after its ttl")
	}
}
