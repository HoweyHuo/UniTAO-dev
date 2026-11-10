package VdsTest

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"DataService/Channel"
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

// TestSubscribeBeforePublishAndUntrusted：publisher 启动后立即在通道上建出自己的 manifest（还没有数据，lastSeq 0），
// subscriber 在第一条数据之前就能订阅；不在白名单里的 subscriber 被忽略，一直等待。
// （来源完全没有 manifest 时订阅的情形见 TestGitQueueChannel 的 ErrNotPublished。）
func TestSubscribeBeforePublishAndUntrusted(t *testing.T) {
	channelDir := filepath.Join(t.TempDir(), "channel")
	q := newQueueSetup(t, dirChannel(channelDir), 1, 1)
	manifestFile := filepath.Join(channelDir, "prod", "manifest.json")
	waitFor(t, "publisher to initialize its manifest", func() bool {
		_, err := os.Stat(manifestFile)
		return err == nil
	})
	raw, err := os.ReadFile(manifestFile)
	mustNoErr(t, err, "read initial manifest")
	var manifest map[string]interface{}
	mustNoErr(t, json.Unmarshal(raw, &manifest), "parse initial manifest")
	if manifest["lastSeq"] != float64(0) || manifest["fingerprint"] == "" {
		t.Fatalf("expect an empty manifest at seq 0, got %s", raw)
	}
	if _, err := os.Stat(filepath.Join(channelDir, "prod", "manifest.sig")); err != nil {
		t.Fatalf("expect the initial manifest to be signed: %s", err)
	}
	for i := range q.mirrors {
		mustNoErr(t, q.mirror(i).SyncOnce(), "subscribe before any data is published")
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

// TestEmptyRecipients：白名单为空时 shared vDS 照常启动并写出 manifest；变更只推进 lastSeq，不写 queue batch。
// 订阅方在此期间是 untrusted；把它的 recipient 加进白名单、重启发布方后，它拿到包含之前全部数据的 history，
// 之后的变更正常走 queue。
func TestEmptyRecipients(t *testing.T) {
	channelDir := filepath.Join(t.TempDir(), "channel")
	channel := dirChannel(channelDir)
	keyFile, fingerprint := newSigningKey(t)
	key := newSubKey(t)
	sharedCfg := func(recipients string) map[string]interface{} {
		return map[string]interface{}{
			"shared": []interface{}{map[string]interface{}{
				"name": "prod", "signingKeyFile": keyFile, "publishIntervalSec": 3600,
				"channel": withEncrypt(channel, recipients),
			}},
		}
	}
	dsA := newDsNode(t, "dsA", "")
	dsA.start(t, sharedCfg(recipientsFile(t))) // 只有注释
	pub := dsA.srv.GetVds("shared/prod").Publisher
	if pub.Status()["recipients"] != 0 {
		t.Fatalf("expect 0 recipients, got %v", pub.Status()["recipients"])
	}
	manifestFile := filepath.Join(channelDir, "prod", "manifest.json")
	waitFor(t, "publisher to initialize its manifest", func() bool {
		_, err := os.Stat(manifestFile)
		return err == nil
	})

	dsB := newDsNode(t, "dsB", "")
	dsB.start(t, map[string]interface{}{
		"mirrored": []interface{}{map[string]interface{}{
			"name": "prod", "trustedFingerprint": fingerprint, "pullIntervalSec": 3600,
			"channel": withDecrypt(channel, key),
		}},
	})
	mirror := dsB.srv.GetVds("mirrored/prod").Mirror
	sharedUrl, mirroredUrl := dsA.Url+"/shared/prod", dsB.Url+"/mirrored/prod"

	expectStatus(t, call(t, http.MethodPost, sharedUrl, schemaRecord("rack", "0.0.1", rackSchema)), http.StatusCreated, "post schema")
	expectStatus(t, call(t, http.MethodPost, sharedUrl, dataRecord("rack", "rack-1", map[string]interface{}{"name": "r1"})), http.StatusCreated, "post rack-1")
	waitJournal(t, dsA, "shared/prod", 2)
	syncAll(t, pub, mirror)

	if seq := pub.Status()["lastPublishedSeq"]; seq != uint64(2) {
		t.Fatalf("expect lastPublishedSeq 2 without recipients, got %v", seq)
	}
	raw, err := os.ReadFile(manifestFile)
	mustNoErr(t, err, "read manifest")
	var manifest map[string]interface{}
	mustNoErr(t, json.Unmarshal(raw, &manifest), "parse manifest")
	if manifest["lastSeq"] != float64(2) || len(manifest["queue"].([]interface{})) != 0 {
		t.Fatalf("expect lastSeq 2 and an empty queue, got %s", raw)
	}
	if entries, _ := os.ReadDir(filepath.Join(channelDir, "prod", "queue")); len(entries) != 0 {
		t.Fatalf("expect no queue batch without recipients, got %d files", len(entries))
	}
	if states := subscriberStates(t, pub); states[key.Id].State != Share.SubscriberUntrusted {
		t.Fatalf("expect subscriber untrusted while the whitelist is empty, got %+v", states)
	}
	if mirror.Status()["subscription"] != Share.PhaseWaiting {
		t.Fatalf("expect mirror waiting, got %v", mirror.Status())
	}

	// 加入 recipient 并重启发布方：订阅方拿到 seq 2 的 history（含之前的数据），之后的变更走 queue
	dsA.start(t, sharedCfg(recipientsFile(t, key)))
	pub = dsA.srv.GetVds("shared/prod").Publisher
	syncAll(t, pub, mirror)
	if mirror.Status()["subscription"] != Share.PhaseActive {
		t.Fatalf("expect mirror active after its recipient is added, got %v", mirror.Status())
	}
	expectStatus(t, call(t, http.MethodGet, mirroredUrl+"/rack/rack-1", nil), http.StatusOK, "rack-1 from history")

	expectStatus(t, call(t, http.MethodPost, sharedUrl, dataRecord("rack", "rack-2", map[string]interface{}{"name": "r2"})), http.StatusCreated, "post rack-2")
	waitJournal(t, dsA, "shared/prod", 3)
	syncAll(t, pub, mirror)
	expectStatus(t, call(t, http.MethodGet, mirroredUrl+"/rack/rack-2", nil), http.StatusOK, "rack-2 through the queue")
	if c := mirror.Status()["cursor"]; c != uint64(3) {
		t.Fatalf("expect mirror cursor 3, got %v", c)
	}
}

// 白名单文件不存在时，DS 在配置路径上建一个空白名单并照常启动；已有文件不被改动；目录不存在则启动失败。
func TestCreateMissingRecipientsFile(t *testing.T) {
	keyFile, _ := newSigningKey(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "recipients")
	dsA := newDsNode(t, "dsA", "")
	dsA.start(t, map[string]interface{}{
		"shared": []interface{}{map[string]interface{}{
			"name": "prod", "signingKeyFile": keyFile, "publishIntervalSec": 3600,
			"channel": withEncrypt(dirChannel(filepath.Join(dir, "channel")), missing),
		}},
	})
	content, err := os.ReadFile(missing)
	if err != nil {
		t.Fatalf("recipients file not created: %s", err)
	}
	if pub := dsA.srv.GetVds("shared/prod").Publisher; pub.Status()["recipients"] != 0 {
		t.Fatalf("expect 0 recipients, got %v", pub.Status()["recipients"])
	}
	var cfg map[string]interface{}
	resp := call(t, http.MethodGet, dsA.Url+"/shared/prod/config", nil)
	expectStatus(t, resp, http.StatusOK, "get config")
	if err := json.Unmarshal(resp.Body, &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg["channel"].(map[string]interface{})["recipientsFile"]; got != missing {
		t.Fatalf("expect channel.recipientsFile=%s, got %v", missing, got)
	}

	// 已有文件不改动
	if created, err := Channel.EnsureRecipientsFile(missing); err != nil || created {
		t.Fatalf("existing file: created=%v err=%v", created, err)
	}
	if again, _ := os.ReadFile(missing); string(again) != string(content) {
		t.Fatalf("existing recipients file modified")
	}
	// 目录不存在
	if _, err := Channel.EnsureRecipientsFile(filepath.Join(dir, "no-such-dir", "recipients")); err == nil {
		t.Fatalf("expect error for missing directory")
	}
}

func historyStatuses(t *testing.T, pub *Share.Publisher) []Share.HistoryStatus {
	t.Helper()
	list, _ := pub.Status()["histories"].([]Share.HistoryStatus)
	return list
}

// TestSharedHistory：同一轮的多个订阅请求共用一份 history；复用期内后来的订阅方只拿到授权，
// 不重新打包，加载后从 history 的 seq 继续消费 queue；过了复用期的新请求得到一份新的 history。
// 一份 history 的所有授权方都加载完以后它被删除。
func TestSharedHistory(t *testing.T) {
	channelDir := filepath.Join(t.TempDir(), "channel")
	q := newQueueSetup(t, dirChannel(channelDir), 3, 0)
	historyDir := filepath.Join(channelDir, "prod", "history")
	clock := time.Now()
	q.pub.SetClock(func() time.Time { return clock })
	q.pub.SetHistoryReuseWindow(time.Hour)

	// mirror 启动时就会订阅：先让三个都进入 active、history 清理干净，之后的订阅请求由测试触发
	q.post(t, schemaRecord("rack", "0.0.1", rackSchema), 1)
	q.post(t, dataRecord("rack", "r1", map[string]interface{}{"name": "one"}), 2)
	syncAll(t, q.pub, q.mirror(0), q.mirror(1), q.mirror(2))
	mustNoErr(t, q.pub.RunOnce(), "prune after initial sync")
	if dirs := channelFiles(t, historyDir); len(dirs) != 0 {
		t.Fatalf("expect no history after the initial sync, got %v", dirs)
	}
	// 删除进度文件，mirror 下一次同步时用新请求重新订阅
	resubscribe := func(idx int) {
		mustNoErr(t, os.Remove(filepath.Join(channelDir, "prod", "subscribers", q.keys[idx].Id+".json")), "remove progress file")
		mustNoErr(t, q.mirror(idx).SyncOnce(), "re-subscribe")
	}

	// B、C 同一轮订阅：一份 history，两条授权；打包时流式读取数据库
	scans := q.dsA.db.Scans()
	resubscribe(0)
	resubscribe(1)
	mustNoErr(t, q.pub.RunOnce(), "history for B and C")
	if q.dsA.db.Scans() == scans {
		t.Fatal("expect the history packaged by scanning the database")
	}
	scans = q.dsA.db.Scans()
	if dirs := channelFiles(t, historyDir); len(dirs) != 1 {
		t.Fatalf("expect one history for both subscribers, got %v", dirs)
	}
	histories := historyStatuses(t, q.pub)
	if len(histories) != 1 || histories[0].Seq != 2 || histories[0].Grants != 2 || histories[0].Pending != 2 || !histories[0].Reusable {
		t.Fatalf("unexpected histories %+v", histories)
	}
	first := histories[0].Id

	// 新变更之后 D 订阅：仍授权同一份 history（seq 2），不重新打包
	q.post(t, dataRecord("rack", "r2", map[string]interface{}{"name": "two"}), 3)
	mustNoErr(t, q.pub.RunOnce(), "publish 3")
	resubscribe(2)
	mustNoErr(t, q.pub.RunOnce(), "grant D")
	if dirs := channelFiles(t, historyDir); len(dirs) != 1 || dirs[0] != first {
		t.Fatalf("expect D granted the existing history, got %v", dirs)
	}
	if q.dsA.db.Scans() != scans {
		t.Fatal("expect a grant without reading the database")
	}
	histories = historyStatuses(t, q.pub)
	if len(histories) != 1 || histories[0].Grants != 3 || histories[0].Pending != 3 {
		t.Fatalf("expect three pending grants, got %+v", histories)
	}

	// 都加载、追上 queue：D 从 history 拿到 r1，从 queue 拿到 r2；history 被删除
	syncAll(t, q.pub, q.mirror(0), q.mirror(1), q.mirror(2))
	for i := 0; i < 3; i++ {
		expectStatus(t, call(t, http.MethodGet, q.mirroredUrl(i)+"/rack/r1", nil), http.StatusOK, "r1 from history")
		expectStatus(t, call(t, http.MethodGet, q.mirroredUrl(i)+"/rack/r2", nil), http.StatusOK, "r2 from queue")
	}
	mustNoErr(t, q.pub.RunOnce(), "prune after loading")
	if dirs := channelFiles(t, historyDir); len(dirs) != 0 {
		t.Fatalf("expect the history removed once every grantee loaded it, got %v", dirs)
	}

	// 复用期：B 重新订阅得到新 history；过了复用期 C 重新订阅，不再复用 B 那份
	resubscribe(0)
	mustNoErr(t, q.pub.RunOnce(), "history for B")
	clock = clock.Add(2 * time.Hour)
	resubscribe(1)
	mustNoErr(t, q.pub.RunOnce(), "history for C")
	if dirs := channelFiles(t, historyDir); len(dirs) != 2 {
		t.Fatalf("expect a new history after the reuse window, got %v", dirs)
	}
	// （通道用真实时间记录 published，测试拨快的是 publisher 的时钟，所以这里不看 Reusable）
	histories = historyStatuses(t, q.pub)
	if len(histories) != 2 || histories[0].Grants != 1 || histories[1].Grants != 1 {
		t.Fatalf("expect B and C on separate histories, got %+v", histories)
	}
	syncAll(t, q.pub, q.mirror(0), q.mirror(1))
	mustNoErr(t, q.pub.RunOnce(), "prune after loading")
	if dirs := channelFiles(t, historyDir); len(dirs) != 0 {
		t.Fatalf("expect both histories removed, got %v", dirs)
	}
	for i := 0; i < 2; i++ {
		if q.mirror(i).Status()["subscription"] != Share.PhaseActive {
			t.Fatalf("expect mirror %d active, got %v", i, q.mirror(i).Status())
		}
	}
}
