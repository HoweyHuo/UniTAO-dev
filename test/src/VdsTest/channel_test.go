package VdsTest

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"encoding/base64"
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

func signer(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	path, fingerprint := newSigningKey(t)
	key, err := Channel.LoadSigningKey(path)
	if err != nil {
		t.Fatal(err)
	}
	return key, fingerprint
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

// subKey 是一个 subscriber 的加密身份：私钥文件、公钥（recipient）和 subscriber id。
type subKey struct {
	IdentityFile string
	Recipient    string
	Id           string
}

func newSubKey(t *testing.T) subKey {
	t.Helper()
	identity, recipient, err := Channel.GenerateEncryptionKey()
	mustNoErr(t, err, "generate encryption key")
	path := filepath.Join(t.TempDir(), "identity")
	mustNoErr(t, os.WriteFile(path, []byte(identity+"\n"), 0o600), "write identity")
	id, err := Channel.RecipientSubscriberId(recipient)
	mustNoErr(t, err, "subscriber id")
	return subKey{IdentityFile: path, Recipient: recipient, Id: id}
}

func (k subKey) private(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	data, err := os.ReadFile(k.IdentityFile)
	mustNoErr(t, err, "read identity")
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	mustNoErr(t, err, "decode identity")
	priv, err := ecdh.X25519().NewPrivateKey(raw)
	mustNoErr(t, err, "parse identity")
	return priv
}

func recipientsFile(t *testing.T, keys ...subKey) string {
	t.Helper()
	lines := []string{"# subscriber whitelist"}
	for _, k := range keys {
		lines = append(lines, k.Recipient)
	}
	path := filepath.Join(t.TempDir(), "recipients")
	mustNoErr(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644), "write recipients")
	return path
}

func pubConfig(base Channel.Config, recipients string) Channel.Config {
	base.Encrypt = &Channel.EncryptConfig{RecipientsFile: recipients}
	return base
}

func subConfig(base Channel.Config, key subKey) Channel.Config {
	base.Decrypt = &Channel.DecryptConfig{IdentityFile: key.IdentityFile}
	return base
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

// TestQueueChannel 覆盖队列通道（dir）：订阅登记、history 只给请求者、queue 消费、清理、Reset、
// 旧格式、folder 隔离与签名 / 摘要校验。
func TestQueueChannel(t *testing.T) {
	key, fingerprint := signer(t)
	keyA, keyB, outsider := newSubKey(t), newSubKey(t), newSubKey(t)
	root := t.TempDir()
	base := Channel.Config{Type: Channel.TypeDir, Path: root, Folder: "apps/prod"}

	if _, err := Channel.NewPublisher(base, "prod", key, nil); err == nil {
		t.Fatal("expect publisher without recipients rejected")
	}
	if _, err := Channel.NewSubscriber(base, "prod", fingerprint, nil); err == nil {
		t.Fatal("expect subscriber without identity rejected")
	}
	pub, err := Channel.NewPublisher(pubConfig(base, recipientsFile(t, keyA, keyB)), "prod", key, nil)
	mustNoErr(t, err, "new publisher")
	subA, err := Channel.NewSubscriber(subConfig(base, keyA), "prod", fingerprint, nil)
	mustNoErr(t, err, "new subscriber A")
	subB, err := Channel.NewSubscriber(subConfig(base, keyB), "prod", fingerprint, nil)
	mustNoErr(t, err, "new subscriber B")
	if subA.Id() != keyA.Id || !pub.Trusted(keyA.Id) || pub.Trusted(outsider.Id) {
		t.Fatalf("unexpected subscriber identity / whitelist")
	}

	// 来源还没发布时也能登记订阅
	if err := subA.Refresh(); !errors.Is(err, Channel.ErrNotPublished) {
		t.Fatalf("expect ErrNotPublished, got %v", err)
	}
	mustNoErr(t, subA.WriteProgress(Channel.Progress{Request: "reqA", State: Channel.StateSubscribe}), "subscribe A")
	view, err := pub.View()
	mustNoErr(t, err, "view")
	if view.Published || len(view.Subscribers) != 1 || view.Subscribers[0].Id != keyA.Id || view.Subscribers[0].Request != "reqA" {
		t.Fatalf("unexpected view before publish: %+v", view)
	}

	mustNoErr(t, pub.Publish(changes("prod", 1, 3)), "publish 1-3")
	if err := pub.Publish(changes("prod", 5, 5)); err == nil {
		t.Fatal("expect non-continuous publish rejected")
	}
	history := &Channel.History{Seq: 3, Source: "prod", Records: []map[string]interface{}{dataRecord("rack", "r1", map[string]interface{}{"name": "r1"})}}
	mustNoErr(t, pub.PublishHistory(keyA.Id, "reqA", history), "history for A")
	if err := pub.PublishHistory(outsider.Id, "x", history); err == nil {
		t.Fatal("expect history for a subscriber outside the whitelist rejected")
	}
	if _, err := os.Stat(filepath.Join(root, "apps", "prod", "manifest.json")); err != nil {
		t.Fatalf("expect manifest under folder apps/prod: %s", err)
	}

	mustNoErr(t, subA.Refresh(), "refresh A")
	if subA.LastSeq() != 3 || subA.SourceFingerprint() != fingerprint {
		t.Fatalf("unexpected lastSeq %d / fingerprint %s", subA.LastSeq(), subA.SourceFingerprint())
	}
	if p := subA.Progress(); p == nil || p.State != Channel.StateSubscribe {
		t.Fatalf("expect own progress, got %+v", p)
	}
	got, err := subA.History("reqA")
	mustNoErr(t, err, "history A")
	if got.Seq != 3 || len(got.Records) != 1 {
		t.Fatalf("unexpected history %+v", got)
	}
	if _, err := subA.History("other"); !errors.Is(err, Channel.ErrNoHistory) {
		t.Fatalf("expect ErrNoHistory for another request, got %v", err)
	}
	// history 只对 A 加密：B 看不到，也解不开
	mustNoErr(t, subB.Refresh(), "refresh B")
	if _, err := subB.History("reqA"); !errors.Is(err, Channel.ErrNoHistory) {
		t.Fatalf("expect B has no history, got %v", err)
	}
	historyFiles := channelFiles(t, filepath.Join(root, "apps", "prod", "history"))
	if len(historyFiles) != 1 {
		t.Fatalf("expect one history file, got %v", historyFiles)
	}
	raw, err := os.ReadFile(filepath.Join(root, "apps", "prod", "history", historyFiles[0]))
	mustNoErr(t, err, "read history file")
	if _, err := Channel.Decrypt(raw, keyB.private(t)); err == nil {
		t.Fatal("expect B unable to decrypt A's history")
	}
	if _, err := Channel.Decrypt(raw, keyA.private(t)); err != nil {
		t.Fatalf("expect A able to decrypt its history: %s", err)
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
	mustNoErr(t, subA.WriteProgress(Channel.Progress{Request: "reqA", State: Channel.StateActive, Cursor: 5}), "progress A")
	view, err = pub.View()
	mustNoErr(t, err, "view")
	if !view.Published || view.LastSeq != 5 || len(view.Queue) != 2 || view.Subscribers[0].Cursor != 5 {
		t.Fatalf("unexpected view: %+v", view)
	}

	// 清理：batch 1-3、A 的 history；删除 A 的进度文件
	mustNoErr(t, pub.Prune(Channel.Prune{QueueUpTo: 3, Histories: []string{view.Histories[0].File}}), "prune")
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
	mustNoErr(t, pub.Prune(Channel.Prune{Subscribers: []string{keyA.Id}}), "remove subscriber A")
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
	// 不可信的来源
	_, otherFp := signer(t)
	untrusted, err := Channel.NewSubscriber(subConfig(base, keyA), "prod", otherFp, nil)
	mustNoErr(t, err, "new untrusted subscriber")
	if err := untrusted.Refresh(); err == nil || !strings.Contains(err.Error(), "trusted fingerprint") {
		t.Fatalf("expect untrusted source rejected, got %v", err)
	}
	// 另一把签名密钥不能接管同一个 folder
	otherKey, _ := signer(t)
	intruder, err := Channel.NewPublisher(pubConfig(base, recipientsFile(t, keyA)), "prod", otherKey, nil)
	mustNoErr(t, err, "new intruder")
	if err := intruder.Publish(changes("prod", 6, 6)); err == nil || !strings.Contains(err.Error(), "another key") {
		t.Fatalf("expect another key refused, got %v", err)
	}

	// Reset：通道从 seq 9 重新开始
	mustNoErr(t, pub.Reset(9), "reset")
	mustNoErr(t, subA.Refresh(), "refresh A")
	if subA.LastSeq() != 9 {
		t.Fatalf("expect lastSeq 9 after reset, got %d", subA.LastSeq())
	}
	if _, err := subA.Fetch(5, 100); !errors.Is(err, Channel.ErrGap) {
		t.Fatalf("expect ErrGap after reset, got %v", err)
	}
	mustNoErr(t, pub.Publish(changes("prod", 10, 10)), "publish after reset")

	// 同一个根目录下的另一个 folder 互不影响；旧格式的 manifest 被识别并可 Reset
	legacy := filepath.Join(root, "apps", "lab")
	mustNoErr(t, os.MkdirAll(filepath.Join(legacy, "segments"), 0o755), "mkdir legacy")
	mustNoErr(t, os.WriteFile(filepath.Join(legacy, "manifest.json"), []byte(`{"version":1,"source":"lab"}`), 0o644), "legacy manifest")
	lab, err := Channel.NewPublisher(pubConfig(Channel.Config{Type: Channel.TypeDir, Path: root, Folder: "apps/lab"}, recipientsFile(t, keyA)), "lab", key, nil)
	mustNoErr(t, err, "new lab publisher")
	labView, err := lab.View()
	mustNoErr(t, err, "lab view")
	if !labView.OldFormat {
		t.Fatalf("expect old format detected, got %+v", labView)
	}
	if err := lab.Publish(changes("lab", 1, 1)); err == nil {
		t.Fatal("expect publish on old format refused before reset")
	}
	mustNoErr(t, lab.Reset(0), "reset legacy")
	if _, err := os.Stat(filepath.Join(legacy, "segments")); !os.IsNotExist(err) {
		t.Fatalf("expect old-format directories removed, got %v", err)
	}
	mustNoErr(t, lab.Publish(changes("lab", 1, 1)), "publish lab")
	mustNoErr(t, subA.Refresh(), "refresh prod after lab publish")
	if subA.LastSeq() != 10 {
		t.Fatalf("expect prod untouched by lab, got lastSeq %d", subA.LastSeq())
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
	key, fingerprint := signer(t)
	keyA, keyB := newSubKey(t), newSubKey(t)
	recipients := recipientsFile(t, keyA, keyB)
	remote := filepath.Join(t.TempDir(), "remote.git")
	mustNoErr(t, os.MkdirAll(remote, 0o755), "mkdir remote")
	gitCmd(t, remote, "init", "-q", "--bare", "--initial-branch=main")
	gitBase := func(workDir string, folder string) Channel.Config {
		return Channel.Config{Type: Channel.TypeGit, Repo: remote, WorkDir: workDir, Folder: folder}
	}

	pubDir := filepath.Join(t.TempDir(), "pub")
	pub, err := Channel.NewPublisher(pubConfig(gitBase(pubDir, "infra/prod"), recipients), "prod", key, nil)
	mustNoErr(t, err, "new git publisher")
	subA, err := Channel.NewSubscriber(subConfig(gitBase(filepath.Join(t.TempDir(), "a"), "infra/prod"), keyA), "prod", fingerprint, nil)
	mustNoErr(t, err, "new git subscriber A")
	subB, err := Channel.NewSubscriber(subConfig(gitBase(filepath.Join(t.TempDir(), "b"), "infra/prod"), keyB), "prod", fingerprint, nil)
	mustNoErr(t, err, "new git subscriber B")

	// 远端还是空仓库时 subscriber 就能登记
	if err := subA.Refresh(); !errors.Is(err, Channel.ErrNotPublished) {
		t.Fatalf("expect ErrNotPublished on empty remote, got %v", err)
	}
	mustNoErr(t, subA.WriteProgress(Channel.Progress{Request: "reqA", State: Channel.StateSubscribe}), "subscribe A on empty remote")
	mustNoErr(t, pub.Publish(changes("prod", 1, 2)), "git publish 1-2")
	mustNoErr(t, pub.PublishHistory(keyA.Id, "reqA", &Channel.History{Seq: 2, Source: "prod", Records: []map[string]interface{}{}}), "git history")
	mustNoErr(t, subA.Refresh(), "git refresh A")
	if _, err := subA.History("reqA"); err != nil {
		t.Fatalf("git history A: %s", err)
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
	lab, err := Channel.NewPublisher(pubConfig(gitBase(pubDir, "infra/lab"), recipients), "lab", key, nil)
	mustNoErr(t, err, "publisher sharing the work dir")
	mustNoErr(t, lab.Publish(changes("lab", 1, 1)), "publish lab on shared work dir")
	mustNoErr(t, pub.Publish(changes("prod", 8, 8)), "publish prod on shared work dir")
	other := gitBase(pubDir, "x")
	other.Branch = "other"
	if _, err := Channel.NewPublisher(pubConfig(other, recipients), "x", key, nil); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("expect work dir reused for another branch rejected, got %v", err)
	}
	files := gitCmd(t, remote, "ls-tree", "-r", "--name-only", "main")
	for _, expect := range []string{"infra/prod/manifest.json", "infra/lab/manifest.json", "infra/prod/subscribers/" + keyA.Id + ".json"} {
		if !strings.Contains(files, expect) {
			t.Fatalf("expect %s in remote, got:\n%s", expect, files)
		}
	}
}
