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

// queueSetup 起一个带 shared/prod 的 dsA，以及若干个带 mirrored/prod 的 DS。
type queueSetup struct {
	dsA     *dsNode
	pub     *Share.Publisher
	mirrors []*dsNode
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

// subscriberId 返回第 idx 个 mirror 在通道上的 subscriber id。
func (q *queueSetup) subscriberId(idx int) string {
	return q.mirror(idx).Status()["subscriberId"].(string)
}

func sharedVds(channel map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"shared": []interface{}{map[string]interface{}{"name": "prod", "publishIntervalSec": 3600, "channel": channel}},
	}
}

func mirroredVds(channel map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"mirrored": []interface{}{map[string]interface{}{"name": "prod", "pullIntervalSec": 3600, "channel": channel}},
	}
}

// newQueueSetup 起 shared 和 mirrors 个 mirror。
func newQueueSetup(t *testing.T, channel map[string]interface{}, mirrors int) *queueSetup {
	t.Helper()
	q := &queueSetup{}
	q.dsA = newDsNode(t, "dsA", "")
	q.dsA.start(t, sharedVds(channel))
	q.pub = q.dsA.srv.GetVds("shared/prod").Publisher
	for i := 0; i < mirrors; i++ {
		ds := newDsNode(t, "ds"+string(rune('B'+i)), "")
		ds.start(t, mirroredVds(channel))
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
	q := newQueueSetup(t, dirChannel(channelDir), 2)
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
	if states[q.subscriberId(0)].Cursor != 2 || states[q.subscriberId(1)].Cursor != 1 {
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
	q := newQueueSetup(t, dirChannel(channelDir), 2)
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
	if states[q.subscriberId(0)].State != Share.SubscriberDead || states[q.subscriberId(1)].State != "active" {
		t.Fatalf("expect B dead and C active, got %+v", states)
	}
	if _, err := os.Stat(filepath.Join(channelDir, "prod", "subscribers", q.subscriberId(0)+".json")); !os.IsNotExist(err) {
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

// TestSubscribeBeforePublish：publisher 启动后立即在通道上建出自己的 manifest（还没有数据，lastSeq 0），
// subscriber 在第一条数据之前就能订阅；没有白名单，所有 subscriber 都会被服务。
// （来源完全没有 manifest 时订阅的情形见 TestGitQueueChannel 的 ErrNotPublished。）
func TestSubscribeBeforePublish(t *testing.T) {
	channelDir := filepath.Join(t.TempDir(), "channel")
	q := newQueueSetup(t, dirChannel(channelDir), 2)
	manifestFile := filepath.Join(channelDir, "prod", "manifest.json")
	waitFor(t, "publisher to initialize its manifest", func() bool {
		_, err := os.Stat(manifestFile)
		return err == nil
	})
	raw, err := os.ReadFile(manifestFile)
	mustNoErr(t, err, "read initial manifest")
	var manifest map[string]interface{}
	mustNoErr(t, json.Unmarshal(raw, &manifest), "parse initial manifest")
	if manifest["lastSeq"] != float64(0) || manifest["instance"] == "" || manifest["epoch"] == "" {
		t.Fatalf("expect an empty manifest at seq 0 with instance and epoch, got %s", raw)
	}
	if _, err := os.Stat(filepath.Join(channelDir, "prod", "manifest.sig")); !os.IsNotExist(err) {
		t.Fatalf("expect no signature next to the manifest, got %v", err)
	}
	for i := range q.mirrors {
		mustNoErr(t, q.mirror(i).SyncOnce(), "subscribe before any data is published")
	}
	syncAll(t, q.pub, q.mirror(0), q.mirror(1))
	for i := range q.mirrors {
		if q.mirror(i).Status()["subscription"] != Share.PhaseActive {
			t.Fatalf("expect subscriber %d active on an empty vDS, got %v", i, q.mirror(i).Status())
		}
	}
	q.post(t, schemaRecord("rack", "0.0.1", rackSchema), 1)
	syncAll(t, q.pub, q.mirror(0), q.mirror(1))
	for i := range q.mirrors {
		expectStatus(t, call(t, http.MethodGet, q.mirroredUrl(i)+"/schema/rack", nil), http.StatusOK, "rack schema on mirror")
	}
}

// TestGitQueueEndToEnd：shared → git → mirrored，folder 放在多级目录里；publisher 第一轮就压缩历史。
func TestGitQueueEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	mustNoErr(t, os.MkdirAll(remote, 0o755), "mkdir remote")
	gitCmd(t, remote, "init", "-q", "--bare", "--initial-branch=main")
	channel := func(workDir string) map[string]interface{} {
		return map[string]interface{}{"type": "git", "repo": remote, "branch": "infra", "folder": "dc/catalog", "workDir": workDir}
	}
	dsA := newDsNode(t, "dsA", "")
	dsA.start(t, sharedVds(channel(filepath.Join(t.TempDir(), "a"))))
	dsB := newDsNode(t, "dsB", "")
	dsB.start(t, mirroredVds(channel(filepath.Join(t.TempDir(), "b"))))
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
	for _, expect := range []string{"dc/catalog/manifest.json", "dc/catalog/subscribers/" + mirror.Status()["subscriberId"].(string) + ".json"} {
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

// TestRebuiltPublisherTakesOver：发布方的实例整个丢失后用同样的配置重建（新的数据库、新的实例 id），
// 它接管通道上原来的 folder；mirror 配置不变，发现通道重新开始（epoch 变了）后重新订阅，
// 用重建后的数据整体替换本地数据。
func TestRebuiltPublisherTakesOver(t *testing.T) {
	channelDir := filepath.Join(t.TempDir(), "channel")
	channel := dirChannel(channelDir)
	dsA := newDsNode(t, "dsA", "")
	dsA.start(t, sharedVds(channel))
	dsB := newDsNode(t, "dsB", "")
	dsB.start(t, mirroredVds(channel))
	mirror := dsB.srv.GetVds("mirrored/prod").Mirror
	mirroredUrl := dsB.Url + "/mirrored/prod"

	pub := dsA.srv.GetVds("shared/prod").Publisher
	for i, record := range []map[string]interface{}{
		schemaRecord("rack", "0.0.1", rackSchema),
		dataRecord("rack", "old-1", map[string]interface{}{"name": "old one"}),
		dataRecord("rack", "old-2", map[string]interface{}{"name": "old two"}),
	} {
		expectStatus(t, call(t, http.MethodPost, dsA.Url+"/shared/prod", record), http.StatusCreated, "post to the first instance")
		waitJournal(t, dsA, "shared/prod", uint64(i+1))
	}
	syncAll(t, pub, mirror)
	expectStatus(t, call(t, http.MethodGet, mirroredUrl+"/rack/old-2", nil), http.StatusOK, "old-2 on mirror")
	firstEpoch := mirror.Status()["epoch"]

	// 重建：新的 DS 节点（空数据库、新的 state.json），同样的 shared 配置；旧实例不再运行
	rebuilt := newDsNode(t, "dsA", "")
	rebuilt.start(t, sharedVds(channel))
	pub = rebuilt.srv.GetVds("shared/prod").Publisher
	// 重建后的 seq 超过 mirror 原来的 cursor（3）：只看 seq 的话 mirror 会接着 cursor 往下读，漏掉新 epoch 的
	// seq 1-3 而且留着旧数据；必须靠 epoch 发现通道已重新开始
	for i, record := range []map[string]interface{}{
		schemaRecord("rack", "0.0.1", rackSchema),
		dataRecord("rack", "new-1", map[string]interface{}{"name": "new one"}),
		dataRecord("rack", "new-2", map[string]interface{}{"name": "new two"}),
		dataRecord("rack", "new-3", map[string]interface{}{"name": "new three"}),
	} {
		expectStatus(t, call(t, http.MethodPost, rebuilt.Url+"/shared/prod", record), http.StatusCreated, "post to the rebuilt instance")
		waitJournal(t, rebuilt, "shared/prod", uint64(i+1))
	}
	mustNoErr(t, pub.RunOnce(), "rebuilt publisher takes over and publishes past the old cursor")
	syncAll(t, pub, mirror)

	if status := pub.Status(); status["lastError"] != nil {
		t.Fatalf("expect the rebuilt publisher to take over the folder, got %v", status["lastError"])
	}
	raw, err := os.ReadFile(filepath.Join(channelDir, "prod", "manifest.json"))
	mustNoErr(t, err, "read manifest")
	var manifest map[string]interface{}
	mustNoErr(t, json.Unmarshal(raw, &manifest), "parse manifest")
	instance := call(t, http.MethodGet, rebuilt.Url+"/shared/prod/config", nil).Map(t)["instance"].(map[string]interface{})["instanceId"]
	firstInstance := call(t, http.MethodGet, dsA.Url+"/shared/prod/config", nil).Map(t)["instance"].(map[string]interface{})["instanceId"]
	if manifest["instance"] != instance || instance == firstInstance || manifest["lastSeq"] != float64(4) {
		t.Fatalf("expect the manifest of the rebuilt instance %v (not %v) at seq 4, got %s", instance, firstInstance, raw)
	}
	status := mirror.Status()
	if status["subscription"] != Share.PhaseActive || status["epoch"] == firstEpoch || status["cursor"] != uint64(4) {
		t.Fatalf("expect the mirror re-subscribed to the new epoch at seq 4, got %v", status)
	}
	for _, id := range []string{"new-1", "new-2", "new-3"} {
		expectStatus(t, call(t, http.MethodGet, mirroredUrl+"/rack/"+id, nil), http.StatusOK, id+" on mirror")
	}
	expectStatus(t, call(t, http.MethodGet, mirroredUrl+"/rack/old-1", nil), http.StatusNotFound, "old-1 gone after full replacement")
	expectStatus(t, call(t, http.MethodGet, mirroredUrl+"/rack/old-2", nil), http.StatusNotFound, "old-2 gone after full replacement")
}

// TestPublisherReconcile：发布前与通道对账。上次发布后、保存进度前进程退出时按通道前移进度（epoch 不变）；
// 本地状态丢失（通道上的 seq 超过 outbox 已提交的 seq，如从旧备份恢复）时 Reset，epoch 更换；
// folder 被另一个实例写过时接管并 Reset。
func TestPublisherReconcile(t *testing.T) {
	root := t.TempDir()
	cfg := Channel.Config{Type: Channel.TypeDir, Path: root}
	newPublisher := func(instance string) (*Share.Publisher, *Share.Outbox) {
		db := NewMemDb()
		mustNoErr(t, db.CreateTable("t", nil), "create table")
		outbox := Share.NewOutbox(db, "t")
		return Share.NewPublisher("prod", nil, outbox, newPub(t, cfg, "prod", instance), "sid", Share.PublisherOptions{}, testLogger()), outbox
	}
	sub := newSub(t, cfg, "prod", "watcher")
	head := func() (uint64, string) {
		mustNoErr(t, sub.Refresh(), "refresh")
		return sub.LastSeq(), sub.Epoch()
	}

	pub, outbox := newPublisher("i1")
	mustNoErr(t, outbox.Add(changes("prod", 1, 3)), "outbox 1-3")
	mustNoErr(t, pub.PublishPending(), "publish 1-3")
	seq, epoch := head()
	if seq != 3 || epoch == "" {
		t.Fatalf("expect seq 3, got %d %q", seq, epoch)
	}

	// 写进通道后、保存进度前退出：通道在 5，本地进度还是 3，outbox 已提交到 5
	mustNoErr(t, outbox.Add(changes("prod", 4, 5)), "outbox 4-5")
	direct := newPub(t, cfg, "prod", "i1")
	mustNoErr(t, direct.Publish(changes("prod", 4, 5)), "publish 4-5 without saving progress")
	mustNoErr(t, pub.PublishPending(), "catch up")
	if seq, e := head(); seq != 5 || e != epoch {
		t.Fatalf("expect catch up at seq 5 in the same epoch, got %d %s", seq, e)
	}
	if got := pub.Status()["lastPublishedSeq"]; got != uint64(5) {
		t.Fatalf("expect local progress moved to 5, got %v", got)
	}

	// 同一个实例、数据库从旧备份恢复（outbox 只到 2）：通道在 5，超过本地已提交的 seq，Reset
	restored, restoredOutbox := newPublisher("i1")
	mustNoErr(t, restoredOutbox.Add(changes("prod", 1, 2)), "outbox of the restored database")
	mustNoErr(t, restored.PublishPending(), "publish after restore")
	seq, restoredEpoch := head()
	if seq != 2 || restoredEpoch == epoch {
		t.Fatalf("expect a reset to a new epoch, then seq 2, got %d %s", seq, restoredEpoch)
	}

	// 另一个实例接管
	other, otherOutbox := newPublisher("i2")
	mustNoErr(t, otherOutbox.Add(changes("prod", 1, 1)), "outbox of another instance")
	mustNoErr(t, other.PublishPending(), "publish by another instance")
	if seq, e := head(); seq != 1 || e == restoredEpoch {
		t.Fatalf("expect another instance to take over in a new epoch at seq 1, got %d %s", seq, e)
	}
}

// TestOldConfigFieldsIgnored：旧版本的签名 / 加密配置（signingKeyFile、channel.encrypt、trustedFingerprint、
// channel.decrypt）仍能启动，字段被忽略，/config 的 ignoredConfig 列出它们；文件不必存在。
func TestOldConfigFieldsIgnored(t *testing.T) {
	channelDir := filepath.Join(t.TempDir(), "channel")
	withOld := func(extra map[string]interface{}) map[string]interface{} {
		result := dirChannel(channelDir)
		for k, v := range extra {
			result[k] = v
		}
		return result
	}
	dsA := newDsNode(t, "dsA", "")
	dsA.start(t, map[string]interface{}{
		"shared": []interface{}{map[string]interface{}{
			"name": "prod", "publishIntervalSec": 3600, "signingKeyFile": "/no/such/signing.pem",
			"channel": withOld(map[string]interface{}{"encrypt": map[string]interface{}{"recipientsFile": "/no/such/recipients"}}),
		}},
	})
	dsB := newDsNode(t, "dsB", "")
	dsB.start(t, map[string]interface{}{
		"mirrored": []interface{}{map[string]interface{}{
			"name": "prod", "pullIntervalSec": 3600, "trustedFingerprint": "SHA256:00",
			"channel": withOld(map[string]interface{}{"decrypt": map[string]interface{}{"identityFile": "/no/such/identity"}}),
		}},
	})
	check := func(url string, expect []string) {
		cfg := call(t, http.MethodGet, url+"/config", nil).Map(t)
		got := []string{}
		for _, item := range cfg["ignoredConfig"].([]interface{}) {
			got = append(got, item.(string))
		}
		if len(got) != len(expect) || got[0] != expect[0] || got[1] != expect[1] {
			t.Fatalf("expect ignoredConfig %v, got %v", expect, got)
		}
		ch := cfg["channel"].(map[string]interface{})
		for _, key := range []string{"encrypt", "decrypt", "recipientsFile"} {
			if _, ok := ch[key]; ok {
				t.Fatalf("expect no %s in channel info, got %v", key, ch)
			}
		}
	}
	check(dsA.Url+"/shared/prod", []string{"signingKeyFile", "channel.encrypt"})
	check(dsB.Url+"/mirrored/prod", []string{"trustedFingerprint", "channel.decrypt"})
	expectStatus(t, call(t, http.MethodPost, dsA.Url+"/shared/prod", schemaRecord("rack", "0.0.1", rackSchema)), http.StatusCreated, "post schema")
	waitJournal(t, dsA, "shared/prod", 1)
	syncAll(t, dsA.srv.GetVds("shared/prod").Publisher, dsB.srv.GetVds("mirrored/prod").Mirror)
	expectStatus(t, call(t, http.MethodGet, dsB.Url+"/mirrored/prod/schema/rack", nil), http.StatusOK, "schema mirrored with old config fields")
}

func historyStatuses(t *testing.T, pub *Share.Publisher) []Share.HistoryStatus {
	t.Helper()
	list, _ := pub.Status()["histories"].([]Share.HistoryStatus)
	return list
}

// TestSharedHistory：同一轮的多个订阅请求共用一份 history；复用期内后来的订阅方直接分到它，
// 不重新打包，加载后从 history 的 seq 继续消费 queue；过了复用期的新请求得到一份新的 history。
// 一份 history 分到的订阅方都加载完以后它被删除。
func TestSharedHistory(t *testing.T) {
	channelDir := filepath.Join(t.TempDir(), "channel")
	q := newQueueSetup(t, dirChannel(channelDir), 3)
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
		mustNoErr(t, os.Remove(filepath.Join(channelDir, "prod", "subscribers", q.subscriberId(idx)+".json")), "remove progress file")
		mustNoErr(t, q.mirror(idx).SyncOnce(), "re-subscribe")
	}

	// B、C 同一轮订阅：一份 history，两条分配；打包时流式读取数据库
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

	// 新变更之后 D 订阅：仍分到同一份 history（seq 2），不重新打包
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
