package InvReportTest

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

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
		w.Write([]byte(`{"accepted":1,"head":1}`))
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

// mustReport 执行一次 ProcessEntry 并返回捕获到的 body。
func mustReport(t *testing.T, c *captureServer, r *InvReport.Reporter, entry *ProcessIface.JournalEntry) map[string]interface{} {
	t.Helper()
	if hx := r.ProcessEntry("server", "s1", entry); hx != nil {
		t.Fatalf("ProcessEntry failed: %s", hx.Message)
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

func TestProcessEntryConversion(t *testing.T) {
	c := newCaptureServer(http.StatusOK)
	defer c.srv.Close()
	r := newReporter(c.srv.URL)

	// create：before=nil → upsert，ver 取 after
	body := mustReport(t, c, r, &ProcessIface.JournalEntry{
		Time:  "2026-01-01T00:00:00Z",
		After: recordMap("server", "s1", "0.0.1"),
	})
	if body["dsId"] != "ds1" || body["vds"] != "local" {
		t.Fatalf("unexpected envelope: %v", body)
	}
	change := firstChange(t, body)
	if change["type"] != "server" || change["id"] != "s1" || change["op"] != "upsert" || change["ver"] != "0.0.1" || change["ts"] != "2026-01-01T00:00:00Z" {
		t.Fatalf("unexpected change: %v", change)
	}
	if _, has := change["data"]; has {
		t.Fatalf("change must not carry record content: %v", change)
	}

	// update：before/after 都在 → upsert，ver 取 after
	body = mustReport(t, c, r, &ProcessIface.JournalEntry{
		Time:   "2026-01-01T00:00:01Z",
		Before: recordMap("server", "s1", "0.0.1"),
		After:  recordMap("server", "s1", "0.0.2"),
	})
	change = firstChange(t, body)
	if change["op"] != "upsert" || change["ver"] != "0.0.2" {
		t.Fatalf("unexpected update change: %v", change)
	}

	// delete：after=nil → delete，ver 取 before
	body = mustReport(t, c, r, &ProcessIface.JournalEntry{
		Time:   "2026-01-01T00:00:02Z",
		Before: recordMap("server", "s1", "0.0.2"),
	})
	change = firstChange(t, body)
	if change["op"] != "delete" || change["ver"] != "0.0.2" {
		t.Fatalf("unexpected delete change: %v", change)
	}
	if c.count() != 3 {
		t.Fatalf("expected 3 reports, got %d", c.count())
	}
}

// TestNormalizeTs 验证 journal 的时间格式（Go 默认 String()）被归一化为 RFC3339。
func TestNormalizeTs(t *testing.T) {
	c := newCaptureServer(http.StatusOK)
	defer c.srv.Close()
	r := newReporter(c.srv.URL)

	// journal 的 Go 默认格式（含单调时钟读数）→ RFC3339 UTC
	body := mustReport(t, c, r, &ProcessIface.JournalEntry{
		Time:  "2026-01-01 08:09:10.123456789 +0000 UTC m=+131.568673789",
		After: recordMap("server", "s1", "0.0.1"),
	})
	if change := firstChange(t, body); change["ts"] != "2026-01-01T08:09:10Z" {
		t.Fatalf("go-format time not normalized: %v", change["ts"])
	}

	// 已是 RFC3339 → 换算为 UTC
	body = mustReport(t, c, r, &ProcessIface.JournalEntry{
		Time:  "2026-01-02T10:11:12+02:00",
		After: recordMap("server", "s1", "0.0.1"),
	})
	if change := firstChange(t, body); change["ts"] != "2026-01-02T08:11:12Z" {
		t.Fatalf("rfc3339 time not converted to UTC: %v", change["ts"])
	}

	// 空时间 → 回退为当前时刻（仍是合法 RFC3339）
	body = mustReport(t, c, r, &ProcessIface.JournalEntry{
		After: recordMap("server", "s1", "0.0.1"),
	})
	change := firstChange(t, body)
	ts, _ := change["ts"].(string)
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Fatalf("fallback ts is not RFC3339: %q (%s)", ts, err)
	}
}

func TestProcessEntryErrors(t *testing.T) {
	// dsId 未分配（self registration 未完成）→ 503，journal 会重试
	r := InvReport.NewReporter("local", func() string { return "" }, "http://localhost:9999", log.New(io.Discard, "", 0))
	if hx := r.ProcessEntry("server", "s1", &ProcessIface.JournalEntry{After: recordMap("server", "s1", "0.0.1")}); hx.Status != 503 {
		t.Fatalf("missing dsId: expected 503, got %d", hx.Status)
	}

	// Inventory 返回 500 → 503（可重试）；恢复正常 → nil
	c := newCaptureServer(http.StatusInternalServerError)
	defer c.srv.Close()
	r = newReporter(c.srv.URL)
	if hx := r.ProcessEntry("server", "s1", &ProcessIface.JournalEntry{After: recordMap("server", "s1", "0.0.1")}); hx.Status != 503 {
		t.Fatalf("inventory 500: expected 503, got %d", hx.Status)
	}
	c.setStatus(http.StatusOK)
	if hx := r.ProcessEntry("server", "s1", &ProcessIface.JournalEntry{After: recordMap("server", "s1", "0.0.1")}); hx != nil {
		t.Fatalf("inventory 200: expected nil, got %s", hx.Message)
	}
}

// TestJournalIntegration 验证 reporter 与 journal 重试语义的配合：
// 上报成功条目归档，失败条目保留、重跑重投。
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

	c := newCaptureServer(http.StatusOK)
	defer c.srv.Close()
	reporter := newReporter(c.srv.URL)
	worker := DataJournal.NewJournalWorker(journal, "server", "s1", nil, []ProcessIface.JournalProcess{reporter})

	// create 一条记录 → journal entry → 处理成功 → 条目归档
	if hx := handler.AddJournal("server", "s1", nil, recordMap("server", "s1", "0.0.1")); hx != nil {
		t.Fatalf("failed to add journal entry: %s", hx.Message)
	}
	if hx := worker.ProcessNextEntry(); hx != nil {
		t.Fatalf("expected entry processed, got: %s", hx.Message)
	}
	if journal.NextJournalEntry("server", "s1") != nil {
		t.Fatalf("entry should be archived after successful report")
	}
	if c.count() != 1 {
		t.Fatalf("expected 1 report, got %d", c.count())
	}

	// Inventory 故障 → 处理失败，条目保留；恢复后重跑投递成功
	c.setStatus(http.StatusInternalServerError)
	if hx := handler.AddJournal("server", "s1", recordMap("server", "s1", "0.0.1"), recordMap("server", "s1", "0.0.2")); hx != nil {
		t.Fatalf("failed to add journal entry: %s", hx.Message)
	}
	if hx := worker.ProcessNextEntry(); hx == nil {
		t.Fatalf("expected failure while inventory is down")
	}
	if journal.NextJournalEntry("server", "s1") == nil {
		t.Fatalf("entry must be retained after failed report")
	}
	c.setStatus(http.StatusOK)
	if hx := worker.ProcessNextEntry(); hx != nil {
		t.Fatalf("expected retry to succeed after recovery, got: %s", hx.Message)
	}
	if journal.NextJournalEntry("server", "s1") != nil {
		t.Fatalf("entry should be archived after successful retry")
	}
	if c.count() != 3 { // 500 那次也发出了 POST
		t.Fatalf("expected 3 reports total, got %d", c.count())
	}
}
