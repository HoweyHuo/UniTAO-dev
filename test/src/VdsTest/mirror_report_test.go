package VdsTest

// mirrored vDS 向本网 Inventory 上报记录变化：mirrored 没有 Journal，由 Share.Mirror 自己上报——
// 用 history 整体替换本表后发一个重置信号（推送体的 resets），之后每批增量照常推 changes。
// shared 与 mirrored 各连一个 Inventory：共用一个的话 shared 源头的上报会混进来，分不清是谁报的。

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// pushBodies 解析收到的全部推送 body（保留 resets）。
func (s *pushSink) pushBodies(t *testing.T) []map[string]interface{} {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	result := []map[string]interface{}{}
	for _, body := range s.bodies {
		push := map[string]interface{}{}
		if err := json.Unmarshal([]byte(body), &push); err != nil {
			t.Fatalf("failed to parse push body [%s]: %s", body, err)
		}
		result = append(result, push)
	}
	return result
}

func (s *pushSink) rawBodies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.bodies...)
}

func TestMirrorReportsResetAndChanges(t *testing.T) {
	invA := startInventory(t, nil)
	invB := startInventory(t, nil)
	channelDir := filepath.Join(t.TempDir(), "channel")

	dsA := newDsNode(t, "dsA", invA.Url)
	dsA.start(t, map[string]interface{}{
		"local": map[string]interface{}{},
		"shared": []interface{}{map[string]interface{}{
			"name": "prod", "publishIntervalSec": 3600, "channel": dirChannel(channelDir),
		}},
	})
	dsB := newDsNode(t, "dsB", invB.Url)
	dsB.start(t, map[string]interface{}{
		"local": map[string]interface{}{},
		"mirrored": []interface{}{map[string]interface{}{
			"name": "prod", "pullIntervalSec": 3600, "channel": dirChannel(channelDir),
		}},
	})
	pub := dsA.srv.GetVds("shared/prod").Publisher
	mirroredVds := dsB.srv.GetVds("mirrored/prod")
	mirror := mirroredVds.Mirror
	dsId := mirroredVds.InvId()

	sink := newPushSink(http.StatusOK)
	defer sink.srv.Close()
	subscribe(t, invB, map[string]interface{}{"endpoint": sink.srv.URL, "name": "modeling"})

	// shared 写入（上报给 invA，与 invB 的订阅者无关）
	sharedUrl := dsA.Url + "/shared/prod"
	expectStatus(t, call(t, http.MethodPost, sharedUrl, appSchema()), http.StatusCreated, "post app schema")
	expectStatus(t, call(t, http.MethodPost, sharedUrl, dataRecord("app", "app1", map[string]interface{}{"name": "one"})), http.StatusCreated, "post app1")
	waitJournal(t, dsA, "shared/prod", 2)

	// 首次加载 history：只推一个重置信号，changes 为 []，不逐条推
	syncAll(t, pub, mirror)
	waitFor(t, "reset pushed", func() bool { return sink.count() >= 1 })
	time.Sleep(200 * time.Millisecond)
	bodies := sink.rawBodies()
	if len(bodies) != 1 || bodies[0] != `{"changes":[],"resets":["`+dsId+`"]}` {
		t.Fatalf("expect exactly one reset push for ds [%s], got %v", dsId, bodies)
	}
	expectStatus(t, call(t, http.MethodGet, invB.Url+"/app/"+dsId+"/app1", nil), http.StatusOK, "app1 via mirror's inventory")

	// 之后的增量：按批推 changes（去重、无内部类型），不带 resets
	expectStatus(t, call(t, http.MethodPost, sharedUrl, dataRecord("app", "app2", map[string]interface{}{"name": "two"})), http.StatusCreated, "post app2")
	expectStatus(t, call(t, http.MethodPut, sharedUrl+"/app/app1", dataRecord("app", "app1", map[string]interface{}{"name": "one-v2"})), http.StatusCreated, "update app1")
	expectStatus(t, call(t, http.MethodPut, sharedUrl+"/app/app1", dataRecord("app", "app1", map[string]interface{}{"name": "one-v3"})), http.StatusCreated, "update app1 again")
	waitJournal(t, dsA, "shared/prod", 5)
	syncAll(t, pub, mirror)
	waitFor(t, "changes pushed", func() bool { return sink.count() >= 2 })
	time.Sleep(200 * time.Millisecond)
	pushes := sink.pushBodies(t)
	if len(pushes) != 2 {
		t.Fatalf("expect one change push after the reset, got %v", sink.rawBodies())
	}
	if _, has := pushes[1]["resets"]; has {
		t.Fatalf("change push must not carry resets: %v", pushes[1])
	}
	got := map[string]bool{}
	for _, item := range pushes[1]["changes"].([]interface{}) {
		change := item.(map[string]interface{})
		if change["ds"] != dsId || change["type"] != "app" {
			t.Fatalf("unexpected change: %v", change)
		}
		key := change["id"].(string)
		if got[key] {
			t.Fatalf("duplicate change in one batch: %v", pushes[1])
		}
		got[key] = true
	}
	if !got["app1"] || !got["app2"] || len(got) != 2 {
		t.Fatalf("expect changes of app1 and app2, got %v", pushes[1])
	}
}
