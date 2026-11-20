package InventoryServiceTest

import (
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

func u64(v uint64) *uint64 {
	return &v
}

// ingest 上报一条变更的便捷封装，返回 head。
func ingest(t *testing.T, mgr *Subscription.Manager, ds string, change Subscription.Change) uint64 {
	t.Helper()
	resp, hx := mgr.Ingest(&Subscription.ReportRequest{DsId: ds, Changes: []Subscription.Change{change}})
	if hx != nil {
		t.Fatalf("ingest failed: %s", hx.Message)
	}
	return resp.Head
}

func tsAt(clock *testClock, offset time.Duration) string {
	return clock.now.Add(offset).UTC().Format(time.RFC3339)
}

func hxStatus(t *testing.T, hx *Http.HttpError) int {
	t.Helper()
	if hx == nil {
		t.Fatalf("expected http error, got nil")
	}
	return hx.Status
}

func mustRegister(t *testing.T, mgr *Subscription.Manager, req *Subscription.RegisterRequest) *Subscription.RegisterResponse {
	t.Helper()
	resp, hx := mgr.Register(req)
	if hx != nil {
		t.Fatalf("register failed: %s", hx.Message)
	}
	return resp
}

func mustPull(t *testing.T, mgr *Subscription.Manager, id string, after *uint64, limit int) *Subscription.PullResponse {
	t.Helper()
	resp, hx := mgr.Pull(id, after, limit)
	if hx != nil {
		t.Fatalf("pull failed: %s", hx.Message)
	}
	return resp
}

func TestIngestSeqAndValidation(t *testing.T) {
	mgr, clock := newMgr(t)
	clock.apply(mgr)

	head := ingest(t, mgr, "ds1", Subscription.Change{Type: "server", Id: "s1", Op: Subscription.OpUpsert, Ver: "0.0.1", Ts: tsAt(clock, 0)})
	if head != 1 {
		t.Fatalf("expected head=1, got %d", head)
	}
	head = ingest(t, mgr, "ds1", Subscription.Change{Type: "server", Id: "s1", Op: Subscription.OpDelete, Ts: tsAt(clock, time.Second)})
	if head != 2 {
		t.Fatalf("expected head=2, got %d", head)
	}

	// 校验失败一律 400
	if hx := hxStatus(t, func() *Http.HttpError {
		_, hx := mgr.Ingest(&Subscription.ReportRequest{Changes: []Subscription.Change{{Type: "a", Id: "1", Op: Subscription.OpUpsert}}})
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
		_, hx := mgr.Ingest(&Subscription.ReportRequest{DsId: "ds1", Changes: []Subscription.Change{{Type: "a", Id: "1", Op: "patch"}}})
		return hx
	}()); hx != 400 {
		t.Fatalf("bad op: expected 400, got %d", hx)
	}
	if hx := hxStatus(t, func() *Http.HttpError {
		_, hx := mgr.Ingest(&Subscription.ReportRequest{DsId: "ds1", Changes: []Subscription.Change{{Type: "", Id: "1", Op: Subscription.OpUpsert}}})
		return hx
	}()); hx != 400 {
		t.Fatalf("missing type: expected 400, got %d", hx)
	}
}

func TestPullScanAndFilters(t *testing.T) {
	mgr, clock := newMgr(t)
	clock.apply(mgr)

	// 5 条变更：a/a/b/b/a
	for idx, dataType := range []string{"a", "a", "b", "b", "a"} {
		ingest(t, mgr, "ds1", Subscription.Change{
			Type: dataType, Id: string(rune('1' + idx)), Op: Subscription.OpUpsert, Ts: tsAt(clock, time.Duration(idx)*time.Second),
		})
	}

	// 无过滤订阅，from=0 回放全部
	var from uint64
	all := mustRegister(t, mgr, &Subscription.RegisterRequest{From: &from})
	resp := mustPull(t, mgr, all.SubscriberId, nil, 2)
	if len(resp.Changes) != 2 || resp.Changes[0].Seq != 1 || resp.Changes[1].Seq != 2 {
		t.Fatalf("expected changes [1,2], got %+v", resp.Changes)
	}
	if resp.Next != 2 || resp.Head != 5 || resp.Floor != 1 {
		t.Fatalf("expected next=2 head=5 floor=1, got next=%d head=%d floor=%d", resp.Next, resp.Head, resp.Floor)
	}
	resp = mustPull(t, mgr, all.SubscriberId, nil, 0) // after 缺省 = cursor(0)；limit<=0 → 默认 100
	if len(resp.Changes) != 5 {
		t.Fatalf("expected all 5 changes from cursor, got %d", len(resp.Changes))
	}
	after := uint64(2)
	resp = mustPull(t, mgr, all.SubscriberId, &after, 100)
	if len(resp.Changes) != 3 || resp.Next != 5 {
		t.Fatalf("expected changes [3,4,5] next=5, got %d changes next=%d", len(resp.Changes), resp.Next)
	}

	// 过滤订阅：limit 计扫描数而非匹配数，next 推进过不匹配的行
	filtered := mustRegister(t, mgr, &Subscription.RegisterRequest{Types: []string{"a"}, From: &from})
	resp = mustPull(t, mgr, filtered.SubscriberId, nil, 1)
	if len(resp.Changes) != 1 || resp.Changes[0].Seq != 1 || resp.Next != 1 {
		t.Fatalf("expected change seq=1 next=1, got %+v next=%d", resp.Changes, resp.Next)
	}
	after = 1
	resp = mustPull(t, mgr, filtered.SubscriberId, &after, 1) // seq2 也是 a，命中
	if len(resp.Changes) != 1 || resp.Changes[0].Seq != 2 || resp.Next != 2 {
		t.Fatalf("expected change seq=2 next=2, got %+v next=%d", resp.Changes, resp.Next)
	}
	after = 2
	resp = mustPull(t, mgr, filtered.SubscriberId, &after, 1) // 扫过 seq3(b) 不匹配
	if len(resp.Changes) != 0 || resp.Next != 3 {
		t.Fatalf("expected no match but next=3, got %+v next=%d", resp.Changes, resp.Next)
	}
	after = 3
	resp = mustPull(t, mgr, filtered.SubscriberId, &after, 2) // seq4(b) 跳过、seq5(a) 命中
	if len(resp.Changes) != 1 || resp.Changes[0].Seq != 5 || resp.Next != 5 {
		t.Fatalf("expected change seq=5 next=5, got %+v next=%d", resp.Changes, resp.Next)
	}

	// ds 过滤：没有命中时也推进到 head
	otherDs := mustRegister(t, mgr, &Subscription.RegisterRequest{Ds: "dsX", From: &from})
	resp = mustPull(t, mgr, otherDs.SubscriberId, nil, 100)
	if len(resp.Changes) != 0 || resp.Next != 5 {
		t.Fatalf("expected no changes but next=head=5, got %+v next=%d", resp.Changes, resp.Next)
	}
}

func TestPullErrorsAndGap(t *testing.T) {
	mgr, clock := newMgr(t)
	clock.apply(mgr)

	ingest(t, mgr, "ds1", Subscription.Change{Type: "a", Id: "1", Op: Subscription.OpUpsert, Ts: tsAt(clock, 0)})
	sub := mustRegister(t, mgr, &Subscription.RegisterRequest{})
	ingest(t, mgr, "ds1", Subscription.Change{Type: "a", Id: "2", Op: Subscription.OpUpsert, Ts: tsAt(clock, time.Second)})
	ingest(t, mgr, "ds1", Subscription.Change{Type: "a", Id: "3", Op: Subscription.OpUpsert, Ts: tsAt(clock, 2*time.Second)})

	if _, hx := mgr.Pull(sub.SubscriberId, u64(9), 0); hx.Status != 400 {
		t.Fatalf("after>head: expected 400, got %d", hx.Status)
	}
	if _, hx := mgr.Pull("sub-none", nil, 0); hx.Status != 404 {
		t.Fatalf("unknown id: expected 404, got %d", hx.Status)
	}

	// 订阅者停在 cursor=1，把保留期缩到 1 小时并快进 200 小时 → 行被 retention 清理 → 410 gap。
	// ack 同时是活跃信号：订阅者仍然存活，只是落后到了被清理区间。
	clock.now = clock.now.Add(200 * time.Hour)
	mgr.SetRetention(time.Hour)
	if _, hx := mgr.Ack(sub.SubscriberId, 1); hx != nil {
		t.Fatalf("ack failed: %s", hx.Message)
	}
	if err := mgr.Prune(); err != nil {
		t.Fatalf("prune failed: %s", err)
	}
	_, hx := mgr.Pull(sub.SubscriberId, nil, 0)
	if hx == nil || hx.Status != 410 {
		t.Fatalf("expected 410 gap, got %+v", hx)
	}
	if hx.Payload == nil {
		t.Fatalf("expected gap payload with floor/head")
	}
}

func TestAckSemantics(t *testing.T) {
	mgr, clock := newMgr(t)
	clock.apply(mgr)

	sub := mustRegister(t, mgr, &Subscription.RegisterRequest{})
	for idx := 0; idx < 3; idx++ {
		ingest(t, mgr, "ds1", Subscription.Change{Type: "a", Id: string(rune('1' + idx)), Op: Subscription.OpUpsert, Ts: tsAt(clock, time.Duration(idx)*time.Second)})
	}

	if _, hx := mgr.Ack(sub.SubscriberId, 5); hx.Status != 400 {
		t.Fatalf("cursor>head: expected 400, got %d", hx.Status)
	}
	ack, hx := mgr.Ack(sub.SubscriberId, 2)
	if hx != nil || ack.Cursor != 2 {
		t.Fatalf("expected cursor=2, got %+v (hx=%v)", ack, hx)
	}
	ack, hx = mgr.Ack(sub.SubscriberId, 1) // 回退是 no-op
	if hx != nil || ack.Cursor != 2 {
		t.Fatalf("regression must not move cursor, got %+v", ack)
	}
	if _, hx := mgr.Ack("sub-none", 1); hx.Status != 404 {
		t.Fatalf("unknown id: expected 404, got %d", hx.Status)
	}

	// ack 刷新活跃时间：+2h 后 ack 仍活；再过 25h 不 ack → 判死
	clock.now = clock.now.Add(2 * time.Hour)
	if _, hx := mgr.Ack(sub.SubscriberId, 3); hx != nil {
		t.Fatalf("ack failed: %s", hx.Message)
	}
	clock.now = clock.now.Add(25 * time.Hour)
	if err := mgr.Prune(); err != nil {
		t.Fatalf("prune failed: %s", err)
	}
	if _, hx := mgr.Get(sub.SubscriberId); hx.Status != 404 {
		t.Fatalf("stale subscriber expected 404, got %d", hx.Status)
	}
}

func TestRegisterDefaults(t *testing.T) {
	mgr, clock := newMgr(t)
	clock.apply(mgr)

	// 空 log：from=0 钳到 floor-1=head=0，注册即拉到空
	var from uint64
	empty := mustRegister(t, mgr, &Subscription.RegisterRequest{From: &from})
	resp := mustPull(t, mgr, empty.SubscriberId, nil, 10)
	if len(resp.Changes) != 0 || resp.Next != 0 || resp.Head != 0 {
		t.Fatalf("empty log: expected no changes next=0, got %+v next=%d", resp.Changes, resp.Next)
	}

	for idx := 0; idx < 3; idx++ {
		ingest(t, mgr, "ds1", Subscription.Change{Type: "a", Id: string(rune('1' + idx)), Op: Subscription.OpUpsert, Ts: tsAt(clock, time.Duration(idx)*time.Second)})
	}

	// 缺省从 head 开始（只收今后的）
	nowSub := mustRegister(t, mgr, &Subscription.RegisterRequest{})
	resp = mustPull(t, mgr, nowSub.SubscriberId, nil, 10)
	if len(resp.Changes) != 0 || resp.Next != 3 {
		t.Fatalf("subscribe-from-now: expected no changes next=3, got %+v next=%d", resp.Changes, resp.Next)
	}
	// from=0 回放保留期内全部
	reply := mustRegister(t, mgr, &Subscription.RegisterRequest{From: &from})
	resp = mustPull(t, mgr, reply.SubscriberId, nil, 10)
	if len(resp.Changes) != 3 || resp.Next != 3 {
		t.Fatalf("replay: expected 3 changes, got %d next=%d", len(resp.Changes), resp.Next)
	}
	// from=N 从 N+1 开始
	fromN := uint64(2)
	mid := mustRegister(t, mgr, &Subscription.RegisterRequest{From: &fromN})
	resp = mustPull(t, mgr, mid.SubscriberId, nil, 10)
	if len(resp.Changes) != 1 || resp.Changes[0].Seq != 3 {
		t.Fatalf("from=2: expected change seq=3, got %+v", resp.Changes)
	}
	// from 超过 head 钳到 head
	fromBig := uint64(99)
	big := mustRegister(t, mgr, &Subscription.RegisterRequest{From: &fromBig})
	if big.Cursor != 3 {
		t.Fatalf("from>head must clamp to head=3, got %d", big.Cursor)
	}
	// filters 持久化
	typed := mustRegister(t, mgr, &Subscription.RegisterRequest{Name: "ui", Types: []string{"a"}, Ds: "ds1"})
	status, hx := mgr.Get(typed.SubscriberId)
	if hx != nil || status.Subscription.Name != "ui" || len(status.Subscription.Types) != 1 || status.Subscription.Ds != "ds1" {
		t.Fatalf("filters not persisted: %+v (hx=%v)", status, hx)
	}
	// types 含空串 → 400
	if _, hx := mgr.Register(&Subscription.RegisterRequest{Types: []string{""}}); hx.Status != 400 {
		t.Fatalf("empty type: expected 400, got %d", hx.Status)
	}
	// 未知 id Get → 404
	if _, hx := mgr.Get("sub-none"); hx.Status != 404 {
		t.Fatalf("unknown id: expected 404, got %d", hx.Status)
	}
}

func TestPruneCursorVsRetention(t *testing.T) {
	mgr, clock := newMgr(t)
	clock.apply(mgr)

	for idx := 0; idx < 5; idx++ {
		ingest(t, mgr, "ds1", Subscription.Change{Type: "a", Id: string(rune('1' + idx)), Op: Subscription.OpUpsert, Ts: tsAt(clock, time.Duration(idx)*time.Second)})
	}
	// 活跃订阅者 cursor=2：seq<=2 被清，3..5 保留
	slow := mustRegister(t, mgr, &Subscription.RegisterRequest{From: u64(0)})
	if _, hx := mgr.Ack(slow.SubscriberId, 2); hx != nil {
		t.Fatalf("ack failed: %s", hx.Message)
	}
	if err := mgr.Prune(); err != nil {
		t.Fatalf("prune failed: %s", err)
	}
	after := uint64(2)
	resp := mustPull(t, mgr, slow.SubscriberId, &after, 10)
	if len(resp.Changes) != 3 || resp.Floor != 3 {
		t.Fatalf("expected rows 3..5 (floor=3), got %d changes floor=%d", len(resp.Changes), resp.Floor)
	}

	// 无活跃订阅者（快进超时判死）→ minCursor 回落到 head，全清
	clock.now = clock.now.Add(25 * time.Hour)
	if err := mgr.Prune(); err != nil {
		t.Fatalf("prune failed: %s", err)
	}
	if _, hx := mgr.Get(slow.SubscriberId); hx.Status != 404 {
		t.Fatalf("stale subscriber expected 404, got %d", hx.Status)
	}
	resp = mustPull(t, mgr, mustRegister(t, mgr, &Subscription.RegisterRequest{}).SubscriberId, nil, 10)
	if len(resp.Changes) != 0 {
		t.Fatalf("log should be empty after reaping, got %d", len(resp.Changes))
	}

	// 幂等删除：不存在的 id 也成功
	if hx := mgr.Delete("sub-none"); hx != nil {
		t.Fatalf("delete must be idempotent, got %s", hx.Message)
	}
}
