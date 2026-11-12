package VdsTest

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"DataService/Channel"
)

const testInstance = "instance-1"

func newPub(t *testing.T, cfg Channel.Config, source string, instance string) Channel.Publisher {
	t.Helper()
	pub, err := Channel.NewPublisher(cfg, source, instance, nil)
	mustNoErr(t, err, "new publisher "+source)
	return pub
}

func newSub(t *testing.T, cfg Channel.Config, source string, id string) Channel.Subscriber {
	t.Helper()
	sub, err := Channel.NewSubscriber(cfg, source, id, nil)
	mustNoErr(t, err, "new subscriber "+id)
	return sub
}

func changes(source string, from uint64, to uint64) []Channel.Change {
	result := []Channel.Change{}
	for seq := from; seq <= to; seq++ {
		id := fmt.Sprintf("r%d", seq)
		result = append(result, Channel.Change{
			Seq: seq, Source: source, Op: Channel.OpUpsert, Type: "rack", Id: id,
			Record: dataRecord("rack", id, map[string]interface{}{"name": id}),
		})
	}
	return result
}

func mustNoErr(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %s", what, err)
	}
}

func channelFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []string{}
	}
	mustNoErr(t, err, "read dir "+dir)
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestQueueChannel 覆盖队列通道（dir）：订阅登记、history 只给分到的订阅请求、queue 消费、清理、Reset 与 epoch、
// 别的实例接管 folder、旧格式、folder 隔离与摘要校验。
func TestQueueChannel(t *testing.T) {
	root := t.TempDir()
	base := Channel.Config{Type: Channel.TypeDir, Path: root, Folder: "apps/prod"}

	if _, err := Channel.NewPublisher(base, "prod", "", nil); err == nil {
		t.Fatal("expect publisher without instance id rejected")
	}
	if _, err := Channel.NewSubscriber(base, "prod", "../x", nil); err == nil {
		t.Fatal("expect invalid subscriber id rejected")
	}
	pub := newPub(t, base, "prod", testInstance)
	subA := newSub(t, base, "prod", "subA")
	subB := newSub(t, base, "prod", "subB")
	if subA.Id() != "subA" {
		t.Fatalf("unexpected subscriber id %s", subA.Id())
	}

	// 来源还没发布时也能登记订阅
	if err := subA.Refresh(); !errors.Is(err, Channel.ErrNotPublished) {
		t.Fatalf("expect ErrNotPublished, got %v", err)
	}
	mustNoErr(t, subA.WriteProgress(Channel.Progress{Request: "reqA", State: Channel.StateSubscribe}), "subscribe A")
	view, err := pub.View()
	mustNoErr(t, err, "view")
	if view.Published || len(view.Subscribers) != 1 || view.Subscribers[0].Id != "subA" || view.Subscribers[0].Request != "reqA" {
		t.Fatalf("unexpected view before publish: %+v", view)
	}
	if err := pub.Publish(changes("prod", 1, 3)); err == nil {
		t.Fatal("expect publish before the manifest exists refused (reset first)")
	}
	mustNoErr(t, pub.Reset(0), "init manifest")

	mustNoErr(t, pub.Publish(changes("prod", 1, 3)), "publish 1-3")
	if err := pub.Publish(changes("prod", 5, 5)); err == nil {
		t.Fatal("expect non-continuous publish rejected")
	}
	history := &Channel.History{Seq: 3, Source: "prod", Records: Channel.RecordList([]map[string]interface{}{dataRecord("rack", "r1", map[string]interface{}{"name": "r1"})})}
	published, err := pub.PublishHistory(history, []Channel.HistoryGrant{{Subscriber: "subA", Request: "reqA"}})
	mustNoErr(t, err, "history for A")
	historyId := published.Id
	if published.Seq != 3 || published.Records != 1 || len(published.Parts) != 1 || len(published.Grants) != 1 {
		t.Fatalf("unexpected history entry %+v", published)
	}
	if _, err := pub.PublishHistory(history, []Channel.HistoryGrant{{Subscriber: "subB"}}); err == nil {
		t.Fatal("expect a grant without request id rejected")
	}
	manifestPath := filepath.Join(root, "apps", "prod", "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	mustNoErr(t, err, "manifest under folder apps/prod")
	if !strings.Contains(string(raw), `"instance": "`+testInstance+`"`) || !strings.Contains(string(raw), `"version": 4`) {
		t.Fatalf("expect manifest v4 owned by %s, got %s", testInstance, raw)
	}

	mustNoErr(t, subA.Refresh(), "refresh A")
	epoch := subA.Epoch()
	if subA.LastSeq() != 3 || epoch == "" {
		t.Fatalf("unexpected lastSeq %d / epoch %q", subA.LastSeq(), epoch)
	}
	if p := subA.Progress(); p == nil || p.State != Channel.StateSubscribe {
		t.Fatalf("expect own progress, got %+v", p)
	}
	got, err := subA.History("reqA")
	mustNoErr(t, err, "history A")
	if got.Id != historyId || got.Seq != 3 || got.Records != 1 || len(readHistory(t, got)) != 1 {
		t.Fatalf("unexpected history %+v", got)
	}
	if _, err := subA.History("other"); !errors.Is(err, Channel.ErrNoHistory) {
		t.Fatalf("expect ErrNoHistory for another request, got %v", err)
	}
	// 没有分给 B：B 看不到
	mustNoErr(t, subB.Refresh(), "refresh B")
	if _, err := subB.History("reqB"); !errors.Is(err, Channel.ErrNoHistory) {
		t.Fatalf("expect B has no history, got %v", err)
	}
	historyDir := filepath.Join(root, "apps", "prod", "history", historyId)
	partsBefore := channelFiles(t, historyDir)
	if len(partsBefore) != 1 || !strings.HasSuffix(partsBefore[0], ".jsonl.gz") {
		t.Fatalf("expect one plain gzip history part, got %v", partsBefore)
	}
	partBefore, _ := os.ReadFile(filepath.Join(historyDir, partsBefore[0]))

	// 重启后的 publisher（同一个实例）把同一份 history 分给 B：只改 manifest，分片不变；B 能读到同样的记录
	restarted := newPub(t, base, "prod", testInstance)
	granted, err := restarted.GrantHistory(historyId, []Channel.HistoryGrant{{Subscriber: "subB", Request: "reqB"}})
	mustNoErr(t, err, "grant B")
	if len(granted.Grants) != 2 {
		t.Fatalf("expect two grants after granting B, got %+v", granted.Grants)
	}
	mustNoErr(t, subB.Refresh(), "refresh B")
	gotB, err := subB.History("reqB")
	mustNoErr(t, err, "history B")
	if gotB.Id != historyId || len(readHistory(t, gotB)) != 1 {
		t.Fatalf("unexpected history for B %+v", gotB)
	}
	partAfter, _ := os.ReadFile(filepath.Join(historyDir, partsBefore[0]))
	if files := channelFiles(t, historyDir); len(files) != 1 || string(partAfter) != string(partBefore) {
		t.Fatalf("expect the parts untouched by a grant, got %v", files)
	}
	// A 的分配仍在
	mustNoErr(t, subA.Refresh(), "refresh A")
	if _, err := subA.History("reqA"); err != nil {
		t.Fatalf("expect A's grant kept after granting B: %s", err)
	}

	// 消费 queue
	none, err := subA.Fetch(3, 100)
	mustNoErr(t, err, "fetch at head")
	if len(none) != 0 {
		t.Fatalf("expect nothing new, got %v", none)
	}
	mustNoErr(t, pub.Publish(changes("prod", 4, 5)), "publish 4-5")
	mustNoErr(t, subA.Refresh(), "refresh A")
	batch, err := subA.Fetch(3, 100)
	mustNoErr(t, err, "fetch 4-5")
	if len(batch) != 2 || batch[0].Seq != 4 || batch[1].Seq != 5 {
		t.Fatalf("expect seq 4-5, got %v", batch)
	}
	all, err := subB.Fetch(0, 100) // B 的缓存还是旧 manifest
	mustNoErr(t, err, "fetch from 0")
	if len(all) != 3 {
		t.Fatalf("expect seq 1-3 from B's cached manifest, got %d", len(all))
	}
	mustNoErr(t, subA.WriteProgress(Channel.Progress{Request: "reqA", State: Channel.StateActive, Cursor: 5, Epoch: epoch}), "progress A")
	view, err = pub.View()
	mustNoErr(t, err, "view")
	if !view.Published || view.Foreign || view.Epoch != epoch || view.LastSeq != 5 || len(view.Queue) != 2 || view.Subscribers[0].Cursor != 5 || view.Subscribers[0].Epoch != epoch {
		t.Fatalf("unexpected view: %+v", view)
	}

	// 清理：batch 1-3、history；删除 A 的进度文件
	if len(view.Histories) != 1 || view.Histories[0].Id != historyId || len(view.Histories[0].Grants) != 2 {
		t.Fatalf("expect one history with two grants, got %+v", view.Histories)
	}
	mustNoErr(t, pub.Prune(Channel.Prune{QueueUpTo: 3, Histories: []string{historyId}}), "prune")
	if _, err := os.Stat(historyDir); !os.IsNotExist(err) {
		t.Fatalf("expect the history directory removed, got %v", err)
	}
	mustNoErr(t, subA.Refresh(), "refresh A")
	if _, err := subA.Fetch(0, 100); !errors.Is(err, Channel.ErrGap) {
		t.Fatalf("expect ErrGap after pruning, got %v", err)
	}
	if _, err := subA.History("reqA"); !errors.Is(err, Channel.ErrNoHistory) {
		t.Fatalf("expect history removed, got %v", err)
	}
	if files := channelFiles(t, filepath.Join(root, "apps", "prod", "queue")); len(files) != 1 {
		t.Fatalf("expect one queue file left, got %v", files)
	}
	mustNoErr(t, pub.Prune(Channel.Prune{Subscribers: []string{"subA"}}), "remove subscriber A")
	mustNoErr(t, subA.Refresh(), "refresh A")
	if p := subA.Progress(); p != nil {
		t.Fatalf("expect A's progress file removed, got %+v", p)
	}

	// 篡改 batch：摘要不符
	queueFile := filepath.Join(root, "apps", "prod", "queue", channelFiles(t, filepath.Join(root, "apps", "prod", "queue"))[0])
	original, _ := os.ReadFile(queueFile)
	mustNoErr(t, os.WriteFile(queueFile, append([]byte("x"), original...), 0o644), "tamper")
	if _, err := subA.Fetch(3, 100); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("expect digest mismatch, got %v", err)
	}
	mustNoErr(t, os.WriteFile(queueFile, original, 0o644), "restore")

	// Reset：通道从 seq 9 重新开始，epoch 更换
	mustNoErr(t, pub.Reset(9), "reset")
	mustNoErr(t, subA.Refresh(), "refresh A")
	if subA.LastSeq() != 9 || subA.Epoch() == epoch || subA.Epoch() == "" {
		t.Fatalf("expect lastSeq 9 and a new epoch after reset, got %d / %s", subA.LastSeq(), subA.Epoch())
	}
	if _, err := subA.Fetch(5, 100); !errors.Is(err, Channel.ErrGap) {
		t.Fatalf("expect ErrGap after reset, got %v", err)
	}
	mustNoErr(t, pub.Publish(changes("prod", 10, 10)), "publish after reset")

	// 另一个实例（重建的 DS）：看到 folder 属于别的实例，Reset 前不能写；Reset 后接管，原来的实例反过来看到 Foreign
	other := newPub(t, base, "prod", "instance-2")
	otherView, err := other.View()
	mustNoErr(t, err, "view of another instance")
	if !otherView.Published || !otherView.Foreign || otherView.Owner != testInstance {
		t.Fatalf("expect the folder seen as foreign (owner %s), got %+v", testInstance, otherView)
	}
	if err := other.Publish(changes("prod", 11, 11)); err == nil || !strings.Contains(err.Error(), "reset it first") {
		t.Fatalf("expect publish on another instance's manifest refused, got %v", err)
	}
	beforeTakeover := subA.Epoch()
	mustNoErr(t, other.Reset(0), "take over")
	mustNoErr(t, other.Publish(changes("prod", 1, 1)), "publish after take over")
	mustNoErr(t, subA.Refresh(), "refresh A after take over")
	if subA.Epoch() == beforeTakeover || subA.LastSeq() != 1 {
		t.Fatalf("expect a new epoch at seq 1 after take over, got %s / %d", subA.Epoch(), subA.LastSeq())
	}
	if v, err := pub.View(); err != nil || !v.Foreign || v.Owner != "instance-2" {
		t.Fatalf("expect the first instance to see the folder taken over, got %+v %v", v, err)
	}
	if err := pub.Publish(changes("prod", 2, 2)); err == nil {
		t.Fatal("expect the first instance refused after take over")
	}

	// 同一个根目录下的另一个 folder 互不影响；旧格式（version 1 的 segments、version 3 的签名）被识别，Reset 时清掉
	legacy := filepath.Join(root, "apps", "lab")
	mustNoErr(t, os.MkdirAll(filepath.Join(legacy, "segments"), 0o755), "mkdir legacy")
	mustNoErr(t, os.WriteFile(filepath.Join(legacy, "manifest.json"), []byte(`{"version":3,"source":"lab","publicKey":"x","lastSeq":7}`), 0o644), "legacy manifest")
	mustNoErr(t, os.WriteFile(filepath.Join(legacy, "manifest.sig"), []byte("sig"), 0o644), "legacy signature")
	labCfg := Channel.Config{Type: Channel.TypeDir, Path: root, Folder: "apps/lab"}
	lab := newPub(t, labCfg, "lab", testInstance)
	labView, err := lab.View()
	mustNoErr(t, err, "lab view")
	if !labView.OldFormat {
		t.Fatalf("expect old format detected, got %+v", labView)
	}
	labSub := newSub(t, labCfg, "lab", "subA")
	if err := labSub.Refresh(); !errors.Is(err, Channel.ErrNotPublished) {
		t.Fatalf("expect an old-format folder seen as not published by a subscriber, got %v", err)
	}
	if err := lab.Publish(changes("lab", 1, 1)); err == nil {
		t.Fatal("expect publish on old format refused before reset")
	}
	mustNoErr(t, lab.Reset(0), "reset legacy")
	for _, name := range []string{"segments", "manifest.sig"} {
		if _, err := os.Stat(filepath.Join(legacy, name)); !os.IsNotExist(err) {
			t.Fatalf("expect old-format %s removed, got %v", name, err)
		}
	}
	mustNoErr(t, lab.Publish(changes("lab", 1, 1)), "publish lab")
	mustNoErr(t, subA.Refresh(), "refresh prod after lab publish")
	if subA.LastSeq() != 1 {
		t.Fatalf("expect prod untouched by lab, got lastSeq %d", subA.LastSeq())
	}
}

// TestSourceId：同一个仓库的不同写法得到同一个身份；分支、folder、仓库不同则不同。
func TestSourceId(t *testing.T) {
	git := func(repo string, branch string) Channel.Config {
		return Channel.Config{Type: Channel.TypeGit, Repo: repo, Branch: branch, WorkDir: "/w"}
	}
	same := []string{
		"git@gitee.com:howey/exchange.git",
		"ssh://git@gitee.com/howey/exchange",
		"ssh://git@Gitee.com:22/howey/exchange.git/",
		"https://user:token@gitee.com/howey/exchange.git",
	}
	want := Channel.SourceId(git(same[0], "master"), "net/modeling")
	if !strings.HasPrefix(want, "SHA256:") || len(want) != len("SHA256:")+64 {
		t.Fatalf("unexpected source id format %s", want)
	}
	for _, repo := range same[1:] {
		if got := Channel.SourceId(git(repo, "master"), "net/modeling/"); got != want {
			t.Fatalf("expect %s to give the same source id as %s", repo, same[0])
		}
	}
	for what, got := range map[string]string{
		"branch": Channel.SourceId(git(same[0], "main"), "net/modeling"),
		"folder": Channel.SourceId(git(same[0], "master"), "net/site"),
		"repo":   Channel.SourceId(git("git@gitee.com:howey/other.git", "master"), "net/modeling"),
		"dir":    Channel.SourceId(Channel.Config{Type: Channel.TypeDir, Path: "/opt/UniTAO/channel"}, "net/modeling"),
	} {
		if got == want {
			t.Fatalf("expect a different %s to give another source id", what)
		}
	}
	if Channel.SourceId(git("file:///opt/UniTAO/channel/x.git", ""), "a") != Channel.SourceId(git("/opt/UniTAO/channel/x.git/", "main"), "a") {
		t.Fatal("expect file:// and a plain path to the same repository, and the default branch main, to give the same source id")
	}
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %s, %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitCount(t *testing.T, remote string) int {
	t.Helper()
	n, err := strconv.Atoi(gitCmd(t, remote, "rev-list", "--count", "main"))
	mustNoErr(t, err, "parse commit count")
	return n
}

// TestGitQueueChannel 覆盖 git 通道：subscriber 也写仓库、多个写入方并发推送、压缩历史后各方继续工作、
// 同一个工作目录被多个 vDS 共用。
func TestGitQueueChannel(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	mustNoErr(t, os.MkdirAll(remote, 0o755), "mkdir remote")
	gitCmd(t, remote, "init", "-q", "--bare", "--initial-branch=main")
	gitBase := func(workDir string, folder string) Channel.Config {
		return Channel.Config{Type: Channel.TypeGit, Repo: remote, WorkDir: workDir, Folder: folder}
	}

	pubDir := filepath.Join(t.TempDir(), "pub")
	pub := newPub(t, gitBase(pubDir, "infra/prod"), "prod", testInstance)
	subA := newSub(t, gitBase(filepath.Join(t.TempDir(), "a"), "infra/prod"), "prod", "subA")
	subB := newSub(t, gitBase(filepath.Join(t.TempDir(), "b"), "infra/prod"), "prod", "subB")

	// 远端还是空仓库时 subscriber 就能登记
	if err := subA.Refresh(); !errors.Is(err, Channel.ErrNotPublished) {
		t.Fatalf("expect ErrNotPublished on empty remote, got %v", err)
	}
	mustNoErr(t, subA.WriteProgress(Channel.Progress{Request: "reqA", State: Channel.StateSubscribe}), "subscribe A on empty remote")
	mustNoErr(t, pub.Reset(0), "git init manifest")
	mustNoErr(t, pub.Publish(changes("prod", 1, 2)), "git publish 1-2")
	_, err := pub.PublishHistory(&Channel.History{Seq: 2, Source: "prod", Records: Channel.RecordList(nil)}, []Channel.HistoryGrant{{Subscriber: "subA", Request: "reqA"}})
	mustNoErr(t, err, "git history")
	mustNoErr(t, subA.Refresh(), "git refresh A")
	if got, err := subA.History("reqA"); err != nil || got.Parts != 1 || len(readHistory(t, got)) != 0 {
		t.Fatalf("git history A: %+v %v", got, err)
	}

	// 并发：publisher 发布的同时两个 subscriber 写进度，推送冲突靠重做解决，谁的修改都不丢
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	wg.Add(3)
	go func() {
		defer wg.Done()
		for seq := uint64(3); seq <= 6; seq++ {
			if err := pub.Publish(changes("prod", seq, seq)); err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 4; i++ {
			if err := subA.WriteProgress(Channel.Progress{Request: "reqA", State: Channel.StateActive, Cursor: 2}); err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 4; i++ {
			if err := subB.WriteProgress(Channel.Progress{Request: "reqB", State: Channel.StateSubscribe}); err != nil {
				errs <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent git writes failed: %s", err)
	}
	view, err := pub.View()
	mustNoErr(t, err, "git view")
	if view.LastSeq != 6 || len(view.Subscribers) != 2 {
		t.Fatalf("expect lastSeq 6 and 2 subscribers after concurrent writes, got %d / %+v", view.LastSeq, view.Subscribers)
	}

	// 压缩历史：整个分支只剩一个提交，内容不变；各方之后照常读写
	if n := commitCount(t, remote); n < 5 {
		t.Fatalf("expect several commits before compact, got %d", n)
	}
	compacted, err := pub.Compact()
	mustNoErr(t, err, "compact")
	if !compacted || commitCount(t, remote) != 1 {
		t.Fatalf("expect one commit after compact, got compacted=%v count=%d", compacted, commitCount(t, remote))
	}
	if again, err := pub.Compact(); err != nil || again {
		t.Fatalf("expect nothing to compact the second time, got %v %v", again, err)
	}
	mustNoErr(t, subA.Refresh(), "refresh A after compact")
	batch, err := subA.Fetch(2, 100)
	mustNoErr(t, err, "fetch after compact")
	if len(batch) != 4 || batch[3].Seq != 6 {
		t.Fatalf("expect seq 3-6 after compact, got %v", batch)
	}
	// 进度内容没变（同一秒内写同样的内容）时不产生提交，所以只检查发布一定多一个提交
	mustNoErr(t, subB.WriteProgress(Channel.Progress{Request: "reqB", State: Channel.StateSubscribe}), "subscriber write after compact")
	before := commitCount(t, remote)
	mustNoErr(t, pub.Publish(changes("prod", 7, 7)), "publish after compact")
	if n := commitCount(t, remote); n != before+1 {
		t.Fatalf("expect publish after compact to add one commit on top of %d, got %d", before, n)
	}

	// 同一个工作目录上的另一个 vDS（同 repo + branch，不同 folder）共用克隆；换 branch 则拒绝
	lab := newPub(t, gitBase(pubDir, "infra/lab"), "lab", testInstance)
	mustNoErr(t, lab.Reset(0), "init lab on shared work dir")
	mustNoErr(t, lab.Publish(changes("lab", 1, 1)), "publish lab on shared work dir")
	mustNoErr(t, pub.Publish(changes("prod", 8, 8)), "publish prod on shared work dir")
	other := gitBase(pubDir, "x")
	other.Branch = "other"
	if _, err := Channel.NewPublisher(other, "x", testInstance, nil); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("expect work dir reused for another branch rejected, got %v", err)
	}
	files := gitCmd(t, remote, "ls-tree", "-r", "--name-only", "main")
	for _, expect := range []string{"infra/prod/manifest.json", "infra/lab/manifest.json", "infra/prod/subscribers/subA.json"} {
		if !strings.Contains(files, expect) {
			t.Fatalf("expect %s in remote, got:\n%s", expect, files)
		}
	}
}

func readHistory(t *testing.T, h *Channel.HistoryReader) []map[string]interface{} {
	t.Helper()
	records := []map[string]interface{}{}
	mustNoErr(t, h.Each(func(record map[string]interface{}) error {
		records = append(records, record)
		return nil
	}), "read history "+h.Id)
	return records
}

// TestHistoryParts：history 按压缩后的大小切片，按顺序读回；分片被篡改时摘要校验失败；
// 只分给 A 时 B 没有 history。
func TestHistoryParts(t *testing.T) {
	defer Channel.SetHistoryPartBytes(1)() // 每条记录一个分片
	spoolDir := t.TempDir()
	t.Setenv("TMPDIR", spoolDir) // 打包时的临时目录
	root := t.TempDir()
	base := Channel.Config{Type: Channel.TypeDir, Path: root}
	pub := newPub(t, base, "prod", testInstance)
	mustNoErr(t, pub.Reset(0), "init manifest")
	mustNoErr(t, pub.Publish(changes("prod", 1, 5)), "publish 1-5")
	records := []map[string]interface{}{}
	for _, change := range changes("prod", 1, 5) {
		records = append(records, change.Record)
	}
	ref, err := pub.PublishHistory(&Channel.History{Seq: 5, Source: "prod", Records: Channel.RecordList(records)}, []Channel.HistoryGrant{{Subscriber: "subA", Request: "reqA"}})
	mustNoErr(t, err, "publish history")
	partDir := filepath.Join(root, "prod", "history", ref.Id)
	parts := channelFiles(t, partDir)
	if len(parts) != 5 {
		t.Fatalf("expect 5 parts, got %v", parts)
	}

	subA := newSub(t, base, "prod", "subA")
	mustNoErr(t, subA.Refresh(), "refresh A")
	h, err := subA.History("reqA")
	mustNoErr(t, err, "history A")
	got := readHistory(t, h)
	if h.Parts != 5 || h.Records != 5 || len(got) != 5 {
		t.Fatalf("unexpected history %+v with %d records", h, len(got))
	}
	for idx, record := range got {
		if record["__id"] != fmt.Sprintf("r%d", idx+1) {
			t.Fatalf("expect records in order, got %v at %d", record["__id"], idx)
		}
	}

	subB := newSub(t, base, "prod", "subB")
	mustNoErr(t, subB.Refresh(), "refresh B")
	if _, err := subB.History("reqA"); !errors.Is(err, Channel.ErrNoHistory) {
		t.Fatalf("expect B without a grant to get ErrNoHistory, got %v", err)
	}

	// 读记录出错：不发布，manifest 里没有新 history，通道上没有它的文件
	failing := func(fn func(record map[string]interface{}) error) error {
		if err := fn(records[0]); err != nil {
			return err
		}
		return errors.New("db read failed")
	}
	if _, err := pub.PublishHistory(&Channel.History{Seq: 5, Source: "prod", Records: failing}, []Channel.HistoryGrant{{Subscriber: "subA", Request: "reqA2"}}); err == nil || !strings.Contains(err.Error(), "db read failed") {
		t.Fatalf("expect the read error returned, got %v", err)
	}
	view, err := pub.View()
	mustNoErr(t, err, "view")
	if len(view.Histories) != 1 || len(channelFiles(t, filepath.Join(root, "prod", "history"))) != 1 {
		t.Fatalf("expect no history from a failed read, got %+v", view.Histories)
	}
	if left := channelFiles(t, spoolDir); len(left) != 0 {
		t.Fatalf("expect spool directories removed, got %v", left)
	}

	// 篡改第 3 个分片：读到它时报错
	third := filepath.Join(partDir, parts[2])
	original, _ := os.ReadFile(third)
	mustNoErr(t, os.WriteFile(third, append([]byte("x"), original...), 0o644), "tamper part")
	err = h.Each(func(record map[string]interface{}) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("expect digest mismatch on the tampered part, got %v", err)
	}
}
