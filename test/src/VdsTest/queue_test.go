package VdsTest

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"DataService/Share"
)

// queueSetup 起一个带 shared/prod 的 dsA，以及每把 key 一个带 mirrored/prod 的 DS。
type queueSetup struct {
	dsA     *dsNode
	pub     *Share.Publisher
	mirrors []*dsNode
	keys    []subKey
}

func (q *queueSetup) mirror(idx int) *Share.Mirror {
	return q.mirrors[idx].srv.GetVds("mirrored/prod").Mirror
}

func (q *queueSetup) sharedUrl() string {
	return q.dsA.Url + "/shared/prod"
}

func (q *queueSetup) mirroredUrl(idx int) string {
	return q.mirrors[idx].Url + "/mirrored/prod"
}

// newQueueSetup：whitelist 中的 key 能订阅；extra 中的 key 也起 mirror，但不在白名单里。
func newQueueSetup(t *testing.T, channel map[string]interface{}, whitelist int, extra int) *queueSetup {
	t.Helper()
	keyFile, fingerprint := newSigningKey(t)
	q := &queueSetup{}
	for i := 0; i < whitelist+extra; i++ {
		q.keys = append(q.keys, newSubKey(t))
	}
	q.dsA = newDsNode(t, "dsA", "")
	q.dsA.start(t, map[string]interface{}{
		"shared": []interface{}{map[string]interface{}{
			"name": "prod", "signingKeyFile": keyFile, "publishIntervalSec": 3600,
			"channel": withEncrypt(channel, recipientsFile(t, q.keys[:whitelist]...)),
		}},
	})
	q.pub = q.dsA.srv.GetVds("shared/prod").Publisher
	for i, key := range q.keys {
		ds := newDsNode(t, "ds"+string(rune('B'+i)), "")
		ds.start(t, map[string]interface{}{
			"mirrored": []interface{}{map[string]interface{}{
				"name": "prod", "trustedFingerprint": fingerprint, "pullIntervalSec": 3600,
				"channel": withDecrypt(channel, key),
			}},
		})
		q.mirrors = append(q.mirrors, ds)
	}
	return q
}

func (q *queueSetup) post(t *testing.T, record map[string]interface{}, outbox uint64) {
	t.Helper()
	expectStatus(t, call(t, http.MethodPost, q.sharedUrl(), record), http.StatusCreated, "post to shared")
	waitJournal(t, q.dsA, "shared/prod", outbox)
}

func subscriberStates(t *testing.T, pub *Share.Publisher) map[string]Share.SubscriberStatus {
	t.Helper()
	result := map[string]Share.SubscriberStatus{}
	list, _ := pub.Status()["subscribers"].([]Share.SubscriberStatus)
	for _, s := range list {
		result[s.Id] = s
	}
	return result
}

// TestQueueTwoSubscribersPrune：两个 subscriber 各自进度；batch 在两者都处理过之后才删除。
func TestQueueTwoSubscribersPrune(t *testing.T) {
	channelDir := filepath.Join(t.TempDir(), "channel")
	q := newQueueSetup(t, dirChannel(channelDir), 2, 0)
	q.post(t, schemaRecord("rack", "0.0.1", rackSchema), 1)
	syncAll(t, q.pub, q.mirror(0), q.mirror(1))
	queueDir := filepath.Join(channelDir, "prod", "queue")

	// 只有 B 消费了新变更：batch 要留给 C
	q.post(t, dataRecord("rack", "r1", map[string]interface{}{"name": "one"}), 2)
	syncAll(t, q.pub, q.mirror(0))
	if files := channelFiles(t, queueDir); len(files) != 1 {
		t.Fatalf("expect the batch kept for the lagging subscriber, got %v", files)
	}
	states := subscriberStates(t, q.pub)
	if states[q.keys[0].Id].Cursor != 2 || states[q.keys[1].Id].Cursor != 1 {
		t.Fatalf("unexpected cursors: %+v", states)
	}
	expectStatus(t, call(t, http.MethodGet, q.mirroredUrl(0)+"/rack/r1", nil), http.StatusOK, "r1 on B")
	expectStatus(t, call(t, http.MethodGet, q.mirroredUrl(1)+"/rack/r1", nil), http.StatusNotFound, "r1 not yet on C")

	// C 跟上后 batch 被删除
	syncAll(t, q.pub, q.mirror(1))
	expectStatus(t, call(t, http.MethodGet, q.mirroredUrl(1)+"/rack/r1", nil), http.StatusOK, "r1 on C")
	if files := channelFiles(t, queueDir); len(files) != 0 {
		t.Fatalf("expect queue emptied after both consumed, got %v", files)
	}
}

// TestDeadSubscriberResubscribe：subscriber 太久没处理 batch 被判死；它回来后重新订阅，
// 加载 history 时整体替换本地数据（期间被删除的记录也随之消失）。
func TestDeadSubscriberResubscribe(t *testing.T) {
	channelDir := filepath.Join(t.TempDir(), "channel")
	q := newQueueSetup(t, dirChannel(channelDir), 2, 0)
	clock := time.Now()
	q.pub.SetClock(func() time.Time { return clock })
	q.pub.SetSubscriberTimeout(time.Hour)

	q.post(t, schemaRecord("rack", "0.0.1", rackSchema), 1)
	q.post(t, dataRecord("rack", "r1", map[string]interface{}{"name": "one"}), 2)
	syncAll(t, q.pub, q.mirror(0), q.mirror(1))
	expectStatus(t, call(t, http.MethodGet, q.mirroredUrl(0)+"/rack/r1", nil), http.StatusOK, "r1 on B")
	firstRequest := q.mirror(0).Status()["request"]

	// B 下线期间：删除 r1、新增 r2；C 照常消费
	expectStatus(t, call(t, http.MethodDelete, q.sharedUrl()+"/rack/r1", nil), http.StatusAccepted, "delete r1")
	q.post(t, dataRecord("rack", "r2", map[string]interface{}{"name": "two"}), 4)
	syncAll(t, q.pub, q.mirror(1))
	if files := channelFiles(t, filepath.Join(channelDir, "prod", "queue")); len(files) == 0 {
		t.Fatalf("expect the batch kept for the offline subscriber")
	}

	// 超时：B 被判死，进度文件删除，它不再拖住 queue
	clock = clock.Add(2 * time.Hour)
	mustNoErr(t, q.pub.RunOnce(), "publisher run after timeout")
	states := subscriberStates(t, q.pub)
	if states[q.keys[0].Id].State != Share.SubscriberDead || states[q.keys[1].Id].State != "active" {
		t.Fatalf("expect B dead and C active, got %+v", states)
	}
	if _, err := os.Stat(filepath.Join(channelDir, "prod", "subscribers", q.keys[0].Id+".json")); !os.IsNotExist(err) {
		t.Fatalf("expect B's progress file removed, got %v", err)
	}
	if files := channelFiles(t, filepath.Join(channelDir, "prod", "queue")); len(files) != 0 {
		t.Fatalf("expect queue emptied once the dead subscriber no longer holds it, got %v", files)
	}

	// B 回来：发现进度文件没了，重新订阅；history 到来之前继续提供旧数据
	mustNoErr(t, q.mirror(0).SyncOnce(), "B sync after coming back")
	status := q.mirror(0).Status()
	if status["subscription"] != Share.PhaseWaiting || status["request"] == firstRequest {
		t.Fatalf("expect B re-subscribed with a new request, got %v", status)
	}
	expectStatus(t, call(t, http.MethodGet, q.mirroredUrl(0)+"/rack/r1", nil), http.StatusOK, "old r1 still served while waiting")
	syncAll(t, q.pub, q.mirror(0))
	expectStatus(t, call(t, http.MethodGet, q.mirroredUrl(0)+"/rack/r1", nil), http.StatusNotFound, "r1 gone after full replacement")
	expectStatus(t, call(t, http.MethodGet, q.mirroredUrl(0)+"/rack/r2", nil), http.StatusOK, "r2 after full replacement")
	if q.mirror(0).Status()["subscription"] != Share.PhaseActive {
		t.Fatalf("expect B active again, got %v", q.mirror(0).Status())
	}
}

// TestSubscribeBeforePublishAndUntrusted：publisher 还没发布任何内容时 subscriber 就能订阅；
// 不在白名单里的 subscriber 被忽略，一直等待。
func TestSubscribeBeforePublishAndUntrusted(t *testing.T) {
	channelDir := filepath.Join(t.TempDir(), "channel")
	q := newQueueSetup(t, dirChannel(channelDir), 1, 1)
	for i := range q.mirrors {
		mustNoErr(t, q.mirror(i).SyncOnce(), "subscribe before anything is published")
	}
	if _, err := os.Stat(filepath.Join(channelDir, "prod", "manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("expect nothing published yet, got %v", err)
	}
	syncAll(t, q.pub, q.mirror(0), q.mirror(1))
	if q.mirror(0).Status()["subscription"] != Share.PhaseActive {
		t.Fatalf("expect trusted subscriber active on an empty vDS, got %v", q.mirror(0).Status())
	}
	if q.mirror(1).Status()["subscription"] != Share.PhaseWaiting {
		t.Fatalf("expect untrusted subscriber still waiting, got %v", q.mirror(1).Status())
	}
	states := subscriberStates(t, q.pub)
	if states[q.keys[1].Id].State != Share.SubscriberUntrusted {
		t.Fatalf("expect untrusted subscriber reported, got %+v", states)
	}
	q.post(t, schemaRecord("rack", "0.0.1", rackSchema), 1)
	syncAll(t, q.pub, q.mirror(0), q.mirror(1))
	expectStatus(t, call(t, http.MethodGet, q.mirroredUrl(0)+"/schema/rack", nil), http.StatusOK, "rack schema on trusted mirror")
	expectStatus(t, call(t, http.MethodGet, q.mirroredUrl(1)+"/schema/rack", nil), http.StatusNotFound, "nothing on untrusted mirror")
}

// TestGitQueueEndToEnd：shared → git → mirrored，folder 放在多级目录里；publisher 第一轮就压缩历史。
func TestGitQueueEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	mustNoErr(t, os.MkdirAll(remote, 0o755), "mkdir remote")
	gitCmd(t, remote, "init", "-q", "--bare", "--initial-branch=main")
	keyFile, fingerprint := newSigningKey(t)
	keyB := newSubKey(t)
	channel := func(workDir string) map[string]interface{} {
		return map[string]interface{}{"type": "git", "repo": remote, "branch": "infra", "folder": "dc/catalog", "workDir": workDir}
	}
	dsA := newDsNode(t, "dsA", "")
	dsA.start(t, map[string]interface{}{
		"shared": []interface{}{map[string]interface{}{
			"name": "prod", "signingKeyFile": keyFile, "publishIntervalSec": 3600,
			"channel": withEncrypt(channel(filepath.Join(t.TempDir(), "a")), recipientsFile(t, keyB)),
		}},
	})
	dsB := newDsNode(t, "dsB", "")
	dsB.start(t, map[string]interface{}{
		"mirrored": []interface{}{map[string]interface{}{
			"name": "prod", "trustedFingerprint": fingerprint, "pullIntervalSec": 3600,
			"channel": withDecrypt(channel(filepath.Join(t.TempDir(), "b")), keyB),
		}},
	})
	pub := dsA.srv.GetVds("shared/prod").Publisher
	mirror := dsB.srv.GetVds("mirrored/prod").Mirror
	expectStatus(t, call(t, http.MethodPost, dsA.Url+"/shared/prod", schemaRecord("rack", "0.0.1", rackSchema)), http.StatusCreated, "post rack schema")
	expectStatus(t, call(t, http.MethodPost, dsA.Url+"/shared/prod", dataRecord("rack", "r1", map[string]interface{}{"name": "one"})), http.StatusCreated, "post r1")
	waitJournal(t, dsA, "shared/prod", 2)
	syncAll(t, pub, mirror)
	expectStatus(t, call(t, http.MethodGet, dsB.Url+"/mirrored/prod/rack/r1", nil), http.StatusOK, "r1 over git")
	// 第一轮 RunOnce 已压缩过一次；之后的写入在新历史上继续
	if n, err := countCommits(remote, "infra"); err != nil || n > 6 {
		t.Fatalf("expect a short history after compaction, got %d %v", n, err)
	}
	files := gitCmd(t, remote, "ls-tree", "-r", "--name-only", "infra")
	for _, expect := range []string{"dc/catalog/manifest.json", "dc/catalog/subscribers/" + keyB.Id + ".json"} {
		if !contains(splitLines(files), expect) {
			t.Fatalf("expect %s on branch infra, got:\n%s", expect, files)
		}
	}
	cfg := call(t, http.MethodGet, dsA.Url+"/shared/prod/config", nil).Map(t)
	if ch, _ := cfg["channel"].(map[string]interface{}); ch["folder"] != "dc/catalog" || ch["branch"] != "infra" {
		t.Fatalf("unexpected channel info in config: %v", cfg["channel"])
	}
}

func countCommits(remote string, branch string) (int, error) {
	out, err := exec.Command("git", "-C", remote, "rev-list", "--count", branch).Output()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range string(out) {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n, nil
}

func splitLines(s string) []string {
	result := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			result = append(result, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		result = append(result, s[start:])
	}
	return result
}
