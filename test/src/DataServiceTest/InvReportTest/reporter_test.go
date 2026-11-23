package InvReportTest

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"DataService/DataJournal"
	"DataService/DataJournal/ProcessIface"
	"DataService/InvReport"
	"UniTao/Test/DataServiceTest"
)

// captureServer 收 POST body 并返回预设状态码（可运行中切换，模拟 Inventory 故障/恢复）。
type captureServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
	status int
}

func newCaptureServer(status int) *captureServer {
	c := &captureServer{status: status}
	mux := http.NewServeMux()
	mux.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, string(body))
		status := c.status
		c.mu.Unlock()
		w.WriteHeader(status)
		w.Write([]byte(`{"accepted":1}`))
	})
	c.srv = httptest.NewServer(mux)
	return c
}

func (c *captureServer) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func (c *captureServer) setStatus(status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = status
}

// lastBody 解析最近一次捕获的请求体。
func (c *captureServer) lastBody(t *testing.T) map[string]interface{} {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		t.Fatalf("no request captured")
	}
	body := map[string]interface{}{}
	if err := json.Unmarshal([]byte(c.bodies[len(c.bodies)-1]), &body); err != nil {
		t.Fatalf("failed to parse captured body: %s", err)
	}
	return body
}

func recordMap(dataType string, id string, ver string) map[string]interface{} {
	return map[string]interface{}{
		"__type": dataType,
		"__id":   id,
		"__ver":  ver,
		"data":   map[string]interface{}{"value": "v"},
	}
}

// mustReport 执行一次 ProcessEntry 并断言它不向 journal 报错（best-effort：失败也返回 nil）。
func mustReport(t *testing.T, c *captureServer, r *InvReport.Reporter, entry *ProcessIface.JournalEntry) map[string]interface{} {
	t.Helper()
	if hx := r.ProcessEntry("server", "s1", entry); hx != nil {
		t.Fatalf("ProcessEntry must not fail the journal, got: %s", hx.Message)
	}
	return c.lastBody(t)
}

func firstChange(t *testing.T, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	changes, ok := body["changes"].([]interface{})
	if !ok || len(changes) != 1 {
		t.Fatalf("expected exactly 1 change in report: %v", body)
	}
	change, ok := changes[0].(map[string]interface{})
	if !ok {
		t.Fatalf("change is not an object: %v", changes[0])
	}
	return change
}

func newReporter(invUrl string) *InvReport.Reporter {
	return InvReport.NewReporter("local", func() string { return "ds1" }, invUrl, log.New(io.Discard, "", 0))
}

func TestHandleTypeMatrix(t *testing.T) {
	r := newReporter("http://localhost:9999")
	for _, dataType := range []string{"journal", "outbox", "channelState", "schema", "record", "cmtIdx", "cmtSubscriber"} {
		can, err := r.HandleType(dataType, "0.0.1")
		if err != nil {
			t.Fatalf("HandleType(%s) returned error: %s", dataType, err)
		}
		if can {
			t.Fatalf("internal type [%s] must not be reported", dataType)
		}
	}
	if can, _ := r.HandleType("server", "0.0.1"); !can {
		t.Fatalf("data type [server] must be reported")
	}
	// 未链接 Inventory（invUrl 为空）时对任何类型都不生效
	off := newReporter("")
	if can, _ := off.HandleType("server", "0.0.1"); can {
		t.Fatalf("reporter without inventory url must handle nothing")
	}
}

// TestProcessEntryPayload 载荷只有记录位置 {type, id}：创建/更新/删除产出完全相同的形状，
// op/ver/ts 都不由 DS 决定——订阅者回查 Inventory 推导。
func TestProcessEntryPayload(t *testing.T) {
	c := newCaptureServer(http.StatusOK)
	defer c.srv.Close()
	r := newReporter(c.srv.URL)

	entry := &ProcessIface.JournalEntry{
		Time:   "2026-01-01 08:09:10.123456789 +0000 UTC m=+131.568673789",
		Before: recordMap("server", "s1", "0.0.1"),
		After:  recordMap("server", "s1", "0.0.2"),
	}
	for name, e := range map[string]*ProcessIface.JournalEntry{
		"create": {Time: entry.Time, After: recordMap("server", "s1", "0.0.1")},
		"update": entry,
		"delete": {Time: entry.Time, Before: recordMap("server", "s1", "0.0.2")},
	} {
		body := mustReport(t, c, r, e)
		if body["dsId"] != "ds1" {
			t.Fatalf("%s: unexpected envelope: %v", name, body)
		}
		change := firstChange(t, body)
		if change["type"] != "server" || change["id"] != "s1" {
			t.Fatalf("%s: unexpected position: %v", name, change)
		}
		for _, absent := range []string{"op", "ver", "ts", "data", "vds"} {
			if _, has := change[absent]; has {
				t.Fatalf("%s: change must not carry [%s]: %v", name, absent, change)
			}
		}
		if _, has := body["vds"]; has {
			t.Fatalf("%s: report must not carry vds: %v", name, body)
		}
	}
	if c.count() != 3 {
		t.Fatalf("expected 3 reports, got %d", c.count())
	}
}

// TestFailuresNeverBlockJournal 上报的任何失败都只记日志、返回 nil：
// journal 条目照常归档，不重试、不滞留 head。
func TestFailuresNeverBlockJournal(t *testing.T) {
	entry := &ProcessIface.JournalEntry{After: recordMap("server", "s1", "0.0.1")}

	// dsId 未分配（self registration 未完成）→ 丢弃，不报错、不发请求
	noId := InvReport.NewReporter("local", func() string { return "" }, "http://localhost:9999", log.New(io.Discard, "", 0))
	if hx := noId.ProcessEntry("server", "s1", entry); hx != nil {
		t.Fatalf("missing dsId must not fail the journal, got: %s", hx.Message)
	}

	// Inventory 返回 500 → 记日志、返回 nil
	c := newCaptureServer(http.StatusInternalServerError)
	defer c.srv.Close()
	r := newReporter(c.srv.URL)
	if hx := r.ProcessEntry("server", "s1", entry); hx != nil {
		t.Fatalf("inventory 500 must not fail the journal, got: %s", hx.Message)
	}
	if c.count() != 1 {
		t.Fatalf("the failed report must still have been sent once, got %d", c.count())
	}

	// Inventory 不可达（连接拒绝）→ 记日志、返回 nil
	dead := newCaptureServer(http.StatusOK)
	url := dead.srv.URL
	dead.srv.Close()
	r2 := InvReport.NewReporter("local", func() string { return "ds1" }, url, log.New(io.Discard, "", 0))
	if hx := r2.ProcessEntry("server", "s1", entry); hx != nil {
		t.Fatalf("unreachable inventory must not fail the journal, got: %s", hx.Message)
	}
}

// TestJournalIntegration 验证 reporter 与 journal 的配合：上报成败都不影响条目归档——
// 失败的条目同样被归档（没有重试），后续条目继续处理。
func TestJournalIntegration(t *testing.T) {
	handler, err := DataServiceTest.MockHandler()
	if err != nil {
		t.Fatalf("failed to load MockHandler: %s", err)
	}
	journal, ex := DataJournal.NewJournalLib(handler.DB, handler.Config.DataTable.Data, nil)
	if ex != nil {
		t.Fatalf("failed to create Journal Library. Error: %s", ex)
	}
	handler.AddJournal = journal.AddJournal

	c := newCaptureServer(http.StatusInternalServerError)
	defer c.srv.Close()
	reporter := newReporter(c.srv.URL)
	worker := DataJournal.NewJournalWorker(journal, "server", "s1", nil, []ProcessIface.JournalProcess{reporter})

	// Inventory 故障期间写入：上报失败，但条目照常归档、不滞留 head
	if hx := handler.AddJournal("server", "s1", nil, recordMap("server", "s1", "0.0.1")); hx != nil {
		t.Fatalf("failed to add journal entry: %s", hx.Message)
	}
	if hx := worker.ProcessNextEntry(); hx != nil {
		t.Fatalf("failed report must not block the journal, got: %s", hx.Message)
	}
	if journal.NextJournalEntry("server", "s1") != nil {
		t.Fatalf("entry must be archived even when the report failed")
	}
	if c.count() != 1 {
		t.Fatalf("expected 1 report attempt, got %d", c.count())
	}

	// 恢复后写入：上报成功，同样归档
	c.setStatus(http.StatusOK)
	if hx := handler.AddJournal("server", "s1", recordMap("server", "s1", "0.0.1"), recordMap("server", "s1", "0.0.2")); hx != nil {
		t.Fatalf("failed to add journal entry: %s", hx.Message)
	}
	if hx := worker.ProcessNextEntry(); hx != nil {
		t.Fatalf("expected entry processed, got: %s", hx.Message)
	}
	if journal.NextJournalEntry("server", "s1") != nil {
		t.Fatalf("entry should be archived after successful report")
	}
	if c.count() != 2 {
		t.Fatalf("expected 2 reports total, got %d", c.count())
	}
}

// TestProcessResetPayload 重置信号：{"dsId", "changes": [], "reset": true}，changes 是空数组而非 null；
// 失败同样即弃（不 panic、不重试）。
func TestProcessResetPayload(t *testing.T) {
	c := newCaptureServer(http.StatusAccepted)
	defer c.srv.Close()
	r := newReporter(c.srv.URL)
	r.ProcessReset()
	c.mu.Lock()
	raw := c.bodies[len(c.bodies)-1]
	c.mu.Unlock()
	if raw != `{"dsId":"ds1","changes":[],"reset":true}` {
		t.Fatalf("unexpected reset payload: %s", raw)
	}
	// 普通增量不带 reset 字段
	r.ProcessChanges([]InvReport.Change{{Type: "server", Id: "s1"}})
	if body := c.lastBody(t); body["reset"] != nil {
		t.Fatalf("change report must not carry reset: %v", body)
	}
	// Inventory 拒绝：只记日志，不重试
	c.setStatus(http.StatusInternalServerError)
	r.ProcessReset()
	if c.count() != 3 {
		t.Fatalf("expected exactly 3 requests (no retry), got %d", c.count())
	}
	// dsId 未分配 / 未链接 Inventory：不发
	InvReport.NewReporter("mirrored/x", func() string { return "" }, c.srv.URL, log.New(io.Discard, "", 0)).ProcessReset()
	newReporter("").ProcessReset()
	if c.count() != 3 {
		t.Fatalf("reset must be dropped without dsId or inventory url, got %d requests", c.count())
	}
}

// TestFilterChanges 去掉 DS 内部类型、按 (type, id) 去重、保持首次出现的顺序；全被过滤时返回空切片。
func TestFilterChanges(t *testing.T) {
	got := InvReport.FilterChanges([]InvReport.Change{
		{Type: "server", Id: "s1"},
		{Type: "schema", Id: "server"},
		{Type: "rack", Id: "r1"},
		{Type: "server", Id: "s1"},
		{Type: "cmtIdx", Id: "x"},
		{Type: "server", Id: "s2"},
	})
	want := []InvReport.Change{{Type: "server", Id: "s1"}, {Type: "rack", Id: "r1"}, {Type: "server", Id: "s2"}}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for idx := range want {
		if got[idx] != want[idx] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
	if empty := InvReport.FilterChanges([]InvReport.Change{{Type: "schema", Id: "a"}}); empty == nil || len(empty) != 0 {
		t.Fatalf("expected empty non-nil slice, got %#v", empty)
	}
}
