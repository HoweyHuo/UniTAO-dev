package VdsTest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Data/DbConfig"
	"Data/DbIface"
	"DataService/DataServer"
	"InventoryService/InventoryServer"
)

func init() {
	// 测试里注册失败后很快重试；注册成功后不再周期复查，避免旧实例在重启测试中重新注册
	DataServer.SelfRegRetrySec = 1
	DataServer.SelfRegCheckSec = 3600
}

func testLogger() *log.Logger {
	if os.Getenv("VDS_TEST_LOG") != "" {
		return log.New(os.Stderr, "", log.Lmicroseconds)
	}
	return log.New(io.Discard, "", 0)
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

// swapHandler 是可替换内部 handler 的 http.Handler，用来在同一个 URL 上“重启”服务。
type swapHandler struct {
	lock    sync.RWMutex
	handler http.Handler
}

func (s *swapHandler) set(h http.Handler) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.handler = h
}

func (s *swapHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.lock.RLock()
	h := s.handler
	s.lock.RUnlock()
	if h == nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	h.ServeHTTP(w, r)
}

type invNode struct {
	srv  *InventoryServer.Server
	http *httptest.Server
	db   *MemDb
	Url  string
	hook atomic.Value // func(*http.Request)：每个请求处理前调用，测试用来制造延迟
}

// setHook 设置请求处理前的回调，传 nil 清除。
func (n *invNode) setHook(hook func(r *http.Request)) {
	n.hook.Store(hook)
}

func startInventory(t *testing.T, pinned map[string]string) *invNode {
	t.Helper()
	dir := t.TempDir()
	cfg := map[string]interface{}{
		"database":      map[string]interface{}{"type": "memdb"},
		"http":          map[string]interface{}{"type": "http", "dns": "localhost", "port": "0", "headers": map[string]interface{}{}},
		"sync":          map[string]interface{}{"intervalSec": 3600},
		"pinnedSources": pinned,
	}
	path := writeJson(t, filepath.Join(dir, "config.json"), cfg)
	srv, err := InventoryServer.NewServer(path, "0")
	if err != nil {
		t.Fatalf("failed to create inventory server: %s", err)
	}
	db := NewMemDb()
	srv.ConnectDb = func(config DbConfig.DatabaseConfig, logger *log.Logger) (DbIface.Database, error) {
		return db, nil
	}
	if err := srv.Start(testLogger()); err != nil {
		t.Fatalf("failed to start inventory server: %s", err)
	}
	node := &invNode{srv: srv, db: db}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hook, ok := node.hook.Load().(func(*http.Request)); ok && hook != nil {
			hook(r)
		}
		srv.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	node.http = server
	node.Url = server.URL
	return node
}

type dsNode struct {
	Id     string
	srv    *DataServer.Server
	http   *httptest.Server
	swap   *swapHandler
	db     *MemDb
	dir    string
	Url    string
	invUrl string
	extra  map[string]interface{} // 额外的 config 顶层字段
}

func newDsNode(t *testing.T, id string, invUrl string) *dsNode {
	t.Helper()
	swap := &swapHandler{}
	server := httptest.NewServer(swap)
	t.Cleanup(server.Close)
	return &dsNode{Id: id, swap: swap, http: server, db: NewMemDb(), dir: t.TempDir(), Url: server.URL, invUrl: invUrl}
}

// start 用给定的 vds 配置（重新）启动该节点；数据库与 URL 在重启之间保持不变。
func (n *dsNode) start(t *testing.T, vds map[string]interface{}) *DataServer.Server {
	t.Helper()
	cfg := map[string]interface{}{
		"database": map[string]interface{}{"type": "memdb"},
		"table":    map[string]interface{}{"data": n.Id},
		"http":     map[string]interface{}{"type": "http", "dns": "localhost", "port": "0", "headers": map[string]interface{}{}},
		"ds":       map[string]interface{}{"id": n.Id, "urls": []string{n.Url}},
		"init":     map[string]interface{}{"schemaFile": filepath.Join(repoRoot(), "lib", "Schema", "data", "schema.json")},
		"vds":      vds,
	}
	if n.invUrl != "" {
		cfg["inventory"] = map[string]interface{}{"url": n.invUrl}
	}
	for key, value := range n.extra {
		cfg[key] = value
	}
	path := writeJson(t, filepath.Join(n.dir, "config.json"), cfg)
	srv, err := DataServer.NewServer(n.Id, path, "0")
	if err != nil {
		t.Fatalf("failed to create DS [%s]: %s", n.Id, err)
	}
	db := n.db
	srv.ConnectDb = func(config DbConfig.DatabaseConfig, logger *log.Logger) (DbIface.Database, error) {
		return db, nil
	}
	if err := srv.Start(testLogger(), nil); err != nil {
		t.Fatalf("failed to start DS [%s]: %s", n.Id, err)
	}
	n.srv = srv
	n.swap.set(srv)
	return srv
}

func writeJson(t *testing.T, path string, data interface{}) string {
	t.Helper()
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

type response struct {
	Status int
	Body   []byte
}

func (r response) Json(t *testing.T) interface{} {
	t.Helper()
	var data interface{}
	if err := json.Unmarshal(r.Body, &data); err != nil {
		t.Fatalf("response is not JSON: %s, body=%s", err, r.Body)
	}
	return data
}

func (r response) Map(t *testing.T) map[string]interface{} {
	t.Helper()
	data, ok := r.Json(t).(map[string]interface{})
	if !ok {
		t.Fatalf("response is not a JSON object: %s", r.Body)
	}
	return data
}

func (r response) Strings(t *testing.T) []string {
	t.Helper()
	list, ok := r.Json(t).([]interface{})
	if !ok {
		t.Fatalf("response is not a JSON array: %s", r.Body)
	}
	result := []string{}
	for _, item := range list {
		result = append(result, fmt.Sprintf("%v", item))
	}
	return result
}

func call(t *testing.T, method string, url string, payload interface{}) response {
	t.Helper()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s failed: %s", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return response{Status: resp.StatusCode, Body: raw}
}

func expectStatus(t *testing.T, resp response, status int, what string) {
	t.Helper()
	if resp.Status != status {
		t.Fatalf("%s: expect status %d, got %d, body=%s", what, status, resp.Status, resp.Body)
	}
}

// waitFor 轮询直到 check 返回 true，超时则失败。
func waitFor(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func schemaRecord(name string, version string, properties map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"__type": "schema",
		"__id":   name,
		"__ver":  "0.0.1",
		"data": map[string]interface{}{
			"name":        name,
			"version":     version,
			"description": fmt.Sprintf("schema of %s", name),
			"properties":  properties,
		},
	}
}

func dataRecord(dataType string, id string, data map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"__type": dataType,
		"__id":   id,
		"__ver":  "0.0.1",
		"data":   data,
	}
}

func contains(list []string, item string) bool {
	for _, value := range list {
		if value == item {
			return true
		}
	}
	return false
}
