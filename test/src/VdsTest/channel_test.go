package VdsTest

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestDirChannelPublishFetch(t *testing.T) {
	key, fingerprint := signer(t)
	cfg := Channel.Config{Type: Channel.TypeDir, Path: t.TempDir()}
	pub, err := Channel.NewPublisher(cfg, "prod", key, nil)
	mustNoErr(t, err, "new publisher")
	sub, err := Channel.NewSubscriber(cfg, "prod", fingerprint, nil)
	mustNoErr(t, err, "new subscriber")

	if err := sub.Refresh(); !errors.Is(err, Channel.ErrNotPublished) {
		t.Fatalf("expect ErrNotPublished before publishing, got %v", err)
	}
	mustNoErr(t, pub.Publish(changes("prod", 1, 3)), "publish 1-3")
	mustNoErr(t, pub.Publish(changes("prod", 4, 5)), "publish 4-5")
	if _, err := pub.LastPublishedSeq(); err != nil {
		t.Fatal(err)
	}
	if err := pub.Publish(changes("prod", 7, 7)); err == nil {
		t.Fatalf("expect publishing non-contiguous seq rejected")
	}
	mustNoErr(t, sub.Refresh(), "refresh")
	if sub.SourceFingerprint() != fingerprint {
		t.Fatalf("unexpected fingerprint %s", sub.SourceFingerprint())
	}
	got, err := sub.Fetch(0, 100)
	mustNoErr(t, err, "fetch from 0")
	if len(got) != 5 || got[0].Seq != 1 || got[4].Seq != 5 {
		t.Fatalf("unexpected changes %v", got)
	}
	got, err = sub.Fetch(3, 100)
	mustNoErr(t, err, "fetch from 3")
	if len(got) != 2 || got[0].Seq != 4 {
		t.Fatalf("expect seq 4-5 after cursor 3, got %v", got)
	}
	got, err = sub.Fetch(5, 100)
	mustNoErr(t, err, "fetch at head")
	if len(got) != 0 {
		t.Fatalf("expect no change at head, got %v", got)
	}
	if _, err := sub.LatestSnapshot(); !errors.Is(err, Channel.ErrNoSnapshot) {
		t.Fatalf("expect ErrNoSnapshot, got %v", err)
	}

	// snapshot 覆盖 1-5 后，旧 segment 被清理：cursor 0 无法连续，要求重建
	snapshot := &Channel.Snapshot{Seq: 5, Source: "prod", Records: []map[string]interface{}{dataRecord("rack", "r1", map[string]interface{}{"name": "r1"})}}
	mustNoErr(t, pub.PublishSnapshot(snapshot), "publish snapshot")
	mustNoErr(t, pub.Publish(changes("prod", 6, 6)), "publish 6")
	mustNoErr(t, sub.Refresh(), "refresh after snapshot")
	if _, err := sub.Fetch(2, 100); !errors.Is(err, Channel.ErrGap) {
		t.Fatalf("expect ErrGap for cursor before snapshot, got %v", err)
	}
	snap, err := sub.LatestSnapshot()
	mustNoErr(t, err, "latest snapshot")
	if snap.Seq != 5 || len(snap.Records) != 1 {
		t.Fatalf("unexpected snapshot seq=%d records=%d", snap.Seq, len(snap.Records))
	}
	got, err = sub.Fetch(5, 100)
	mustNoErr(t, err, "fetch after snapshot")
	if len(got) != 1 || got[0].Seq != 6 {
		t.Fatalf("expect seq 6 after snapshot, got %v", got)
	}
}

func TestDirChannelRejectsUntrustedAndTampered(t *testing.T) {
	key, fingerprint := signer(t)
	dir := t.TempDir()
	cfg := Channel.Config{Type: Channel.TypeDir, Path: dir}
	pub, err := Channel.NewPublisher(cfg, "prod", key, nil)
	mustNoErr(t, err, "new publisher")
	mustNoErr(t, pub.Publish(changes("prod", 1, 2)), "publish")

	// 指纹不匹配
	_, otherFingerprint := signer(t)
	untrusted, err := Channel.NewSubscriber(cfg, "prod", otherFingerprint, nil)
	mustNoErr(t, err, "new subscriber")
	if err := untrusted.Refresh(); err == nil {
		t.Fatalf("expect untrusted fingerprint rejected")
	}

	// 另一把密钥不能接管同一个来源
	otherKey, _ := signer(t)
	intruder, err := Channel.NewPublisher(cfg, "prod", otherKey, nil)
	if err == nil {
		err = intruder.Publish(changes("prod", 3, 3))
	}
	if err == nil {
		t.Fatalf("expect publisher with another key rejected")
	}

	// 篡改 segment 内容
	segments, _ := filepath.Glob(filepath.Join(dir, "prod", "segments", "*"))
	if len(segments) == 0 {
		t.Fatalf("no segment file found")
	}
	mustNoErr(t, os.WriteFile(segments[0], []byte("tampered"), 0o644), "tamper")
	sub, err := Channel.NewSubscriber(cfg, "prod", fingerprint, nil)
	mustNoErr(t, err, "new subscriber")
	err = sub.Refresh()
	if err == nil {
		_, err = sub.Fetch(0, 100)
	}
	if err == nil {
		t.Fatalf("expect tampered segment rejected")
	}

	// 篡改 manifest
	manifest := filepath.Join(dir, "prod", "manifest.json")
	raw, err := os.ReadFile(manifest)
	mustNoErr(t, err, "read manifest")
	mustNoErr(t, os.WriteFile(manifest, []byte(strings.Replace(string(raw), "\"prod\"", "\"prod2\"", 1)), 0o644), "tamper manifest")
	if err := sub.Refresh(); err == nil {
		t.Fatalf("expect tampered manifest rejected")
	}
}

func TestDirChannelEncryption(t *testing.T) {
	key, fingerprint := signer(t)
	keyDir := t.TempDir()
	identity, recipient, err := Channel.GenerateEncryptionKey()
	mustNoErr(t, err, "generate encryption key")
	otherIdentity, _, err := Channel.GenerateEncryptionKey()
	mustNoErr(t, err, "generate other key")
	recipientsFile := filepath.Join(keyDir, "recipients")
	identityFile := filepath.Join(keyDir, "identity")
	otherIdentityFile := filepath.Join(keyDir, "other")
	mustNoErr(t, os.WriteFile(recipientsFile, []byte("# receiver\n"+recipient+"\n"), 0o644), "write recipients")
	mustNoErr(t, os.WriteFile(identityFile, []byte(identity+"\n"), 0o600), "write identity")
	mustNoErr(t, os.WriteFile(otherIdentityFile, []byte(otherIdentity+"\n"), 0o600), "write other identity")

	dir := t.TempDir()
	pubCfg := Channel.Config{Type: Channel.TypeDir, Path: dir, Encrypt: &Channel.EncryptConfig{RecipientsFile: recipientsFile}}
	pub, err := Channel.NewPublisher(pubCfg, "prod", key, nil)
	mustNoErr(t, err, "new publisher")
	mustNoErr(t, pub.Publish(changes("prod", 1, 2)), "publish")

	segments, _ := filepath.Glob(filepath.Join(dir, "prod", "segments", "*"))
	if len(segments) == 0 || !strings.HasSuffix(segments[0], ".enc") {
		t.Fatalf("expect encrypted segment file, got %v", segments)
	}
	raw, _ := os.ReadFile(segments[0])
	if strings.Contains(string(raw), "rack") {
		t.Fatalf("encrypted segment must not contain plain text")
	}

	subCfg := Channel.Config{Type: Channel.TypeDir, Path: dir, Decrypt: &Channel.DecryptConfig{IdentityFile: identityFile}}
	sub, err := Channel.NewSubscriber(subCfg, "prod", fingerprint, nil)
	mustNoErr(t, err, "new subscriber")
	mustNoErr(t, sub.Refresh(), "refresh")
	got, err := sub.Fetch(0, 100)
	mustNoErr(t, err, "fetch encrypted")
	if len(got) != 2 || got[1].Record["__id"] != "r2" {
		t.Fatalf("unexpected decrypted changes %v", got)
	}

	wrongCfg := Channel.Config{Type: Channel.TypeDir, Path: dir, Decrypt: &Channel.DecryptConfig{IdentityFile: otherIdentityFile}}
	wrong, err := Channel.NewSubscriber(wrongCfg, "prod", fingerprint, nil)
	mustNoErr(t, err, "new subscriber")
	err = wrong.Refresh()
	if err == nil {
		_, err = wrong.Fetch(0, 100)
	}
	if err == nil {
		t.Fatalf("expect decrypt with a key that is not a recipient to fail")
	}
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %s, %s", args, err, out)
	}
}

func TestGitChannel(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	key, fingerprint := signer(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	mustNoErr(t, os.MkdirAll(remote, 0o755), "mkdir remote")
	gitCmd(t, remote, "init", "--bare", "--initial-branch=main")

	pubCfg := Channel.Config{Type: Channel.TypeGit, Repo: remote, WorkDir: filepath.Join(t.TempDir(), "pub")}
	pub, err := Channel.NewPublisher(pubCfg, "prod", key, nil)
	mustNoErr(t, err, "new git publisher")
	mustNoErr(t, pub.Publish(changes("prod", 1, 3)), "git publish 1-3")
	mustNoErr(t, pub.PublishSnapshot(&Channel.Snapshot{Seq: 3, Source: "prod", Records: []map[string]interface{}{dataRecord("rack", "r1", map[string]interface{}{"name": "r1"})}}), "git snapshot")
	mustNoErr(t, pub.Publish(changes("prod", 4, 4)), "git publish 4")

	// 另一个共享 vDS 往同一个仓库发布，互不影响（各自的目录）
	key2, _ := signer(t)
	pub2, err := Channel.NewPublisher(Channel.Config{Type: Channel.TypeGit, Repo: remote, WorkDir: filepath.Join(t.TempDir(), "pub2")}, "lab", key2, nil)
	mustNoErr(t, err, "new second git publisher")
	mustNoErr(t, pub2.Publish(changes("lab", 1, 1)), "git publish lab")

	subCfg := Channel.Config{Type: Channel.TypeGit, Repo: remote, WorkDir: filepath.Join(t.TempDir(), "sub")}
	sub, err := Channel.NewSubscriber(subCfg, "prod", fingerprint, nil)
	mustNoErr(t, err, "new git subscriber")
	mustNoErr(t, sub.Refresh(), "git refresh")
	snap, err := sub.LatestSnapshot()
	mustNoErr(t, err, "git snapshot")
	if snap.Seq != 3 {
		t.Fatalf("expect snapshot seq 3, got %d", snap.Seq)
	}
	got, err := sub.Fetch(3, 100)
	mustNoErr(t, err, "git fetch")
	if len(got) != 1 || got[0].Seq != 4 {
		t.Fatalf("expect seq 4 after snapshot, got %v", got)
	}

	// 增量：新发布后再次 Refresh 能看到
	mustNoErr(t, pub.Publish(changes("prod", 5, 6)), "git publish 5-6")
	mustNoErr(t, sub.Refresh(), "git refresh again")
	got, err = sub.Fetch(4, 100)
	mustNoErr(t, err, "git fetch again")
	if len(got) != 2 || got[1].Seq != 6 {
		t.Fatalf("expect seq 5-6, got %v", got)
	}

	// 发布端重启（新的工作目录）后继续发布
	pubAgain, err := Channel.NewPublisher(Channel.Config{Type: Channel.TypeGit, Repo: remote, WorkDir: filepath.Join(t.TempDir(), "pub3")}, "prod", key, nil)
	mustNoErr(t, err, "restart git publisher")
	seq, err := pubAgain.LastPublishedSeq()
	mustNoErr(t, err, "last published seq after restart")
	if seq != 6 {
		t.Fatalf("expect last published 6 after restart, got %d", seq)
	}
	mustNoErr(t, pubAgain.Publish(changes("prod", 7, 7)), "git publish after restart")
}
