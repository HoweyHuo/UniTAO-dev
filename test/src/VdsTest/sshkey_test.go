package VdsTest

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"DataService/Channel"
	"DataService/Config"
)

// firstTwo 取公钥行的类型与 base64 两段（忽略注释）。
func firstTwo(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return line
	}
	return fields[0] + " " + fields[1]
}

func TestSSHIdentityGenerate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ssh")
	id, created, err := Channel.EnsureSSHIdentity(dir, "unitao-test")
	mustNoErr(t, err, "generate ssh identity")
	if !created {
		t.Fatal("expect a new key to be created")
	}
	info, err := os.Stat(id.KeyFile)
	mustNoErr(t, err, "stat key")
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expect key mode 0600, got %o", info.Mode().Perm())
	}
	if !strings.HasPrefix(id.PublicKey, "ssh-ed25519 ") || !strings.HasSuffix(id.PublicKey, " unitao-test") {
		t.Fatalf("unexpected public key line: %s", id.PublicKey)
	}
	if !strings.HasPrefix(id.Fingerprint, "SHA256:") {
		t.Fatalf("unexpected fingerprint: %s", id.Fingerprint)
	}

	again, created, err := Channel.EnsureSSHIdentity(dir, "other-comment")
	mustNoErr(t, err, "reload ssh identity")
	if created || again.PublicKey != id.PublicKey || again.Fingerprint != id.Fingerprint {
		t.Fatalf("expect the existing key to be reused, got created=%v %s", created, again.PublicKey)
	}

	// 与 OpenSSH 互通：ssh-keygen 能读出同样的公钥与指纹
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	out, err := exec.Command("ssh-keygen", "-y", "-f", id.KeyFile).CombinedOutput()
	mustNoErr(t, err, "ssh-keygen -y: "+string(out))
	if firstTwo(string(out)) != firstTwo(id.PublicKey) {
		t.Fatalf("ssh-keygen public key [%s] != ours [%s]", strings.TrimSpace(string(out)), id.PublicKey)
	}
	out, err = exec.Command("ssh-keygen", "-l", "-f", id.KeyFile).CombinedOutput()
	mustNoErr(t, err, "ssh-keygen -l: "+string(out))
	if !strings.Contains(string(out), id.Fingerprint) {
		t.Fatalf("ssh-keygen fingerprint [%s] does not contain ours [%s]", strings.TrimSpace(string(out)), id.Fingerprint)
	}
}

func TestSSHIdentityFromSSHKeygen(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	// ssh-keygen 生成的私钥也能直接用
	dir := t.TempDir()
	key := filepath.Join(dir, Channel.SSHKeyFile)
	out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "from-keygen", "-f", key).CombinedOutput()
	mustNoErr(t, err, "ssh-keygen: "+string(out))
	pub, err := os.ReadFile(key + ".pub")
	mustNoErr(t, err, "read .pub")
	id, created, err := Channel.EnsureSSHIdentity(dir, "ignored")
	mustNoErr(t, err, "load ssh-keygen key")
	if created || strings.TrimSpace(string(pub)) != id.PublicKey {
		t.Fatalf("expect [%s], got created=%v [%s]", strings.TrimSpace(string(pub)), created, id.PublicKey)
	}

	// 有密码的私钥 git 不能非交互使用，直接报错
	locked := t.TempDir()
	out, err = exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "secret", "-f", filepath.Join(locked, Channel.SSHKeyFile)).CombinedOutput()
	mustNoErr(t, err, "ssh-keygen with passphrase: "+string(out))
	if _, _, err := Channel.EnsureSSHIdentity(locked, "x"); err == nil || !strings.Contains(err.Error(), "passphrase") {
		t.Fatalf("expect passphrase error, got %v", err)
	}
}

// unsetEnv 在测试期间删除环境变量，结束后恢复。
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	old, had := os.LookupEnv(key)
	os.Unsetenv(key)
	t.Cleanup(func() {
		if had {
			os.Setenv(key, old)
		} else {
			os.Unsetenv(key)
		}
	})
}

// fakeSSH 在 dir 下放一个 ssh 脚本：把参数写进 argsFile 后失败退出。
func fakeSSH(t *testing.T, dir string, name string, argsFile string) string {
	t.Helper()
	script := filepath.Join(dir, name)
	body := "#!/bin/sh\necho \"$@\" >> '" + argsFile + "'\necho 'fake ssh: permission denied' >&2\nexit 255\n"
	mustNoErr(t, os.WriteFile(script, []byte(body), 0o755), "write fake ssh")
	return script
}

func TestGitChannelUsesSSHIdentity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	id, _, err := Channel.EnsureSSHIdentity(filepath.Join(t.TempDir(), "my ssh"), "unitao-test")
	mustNoErr(t, err, "ssh identity")
	Channel.SetSSHIdentity(id)
	t.Cleanup(func() { Channel.SetSSHIdentity(nil) })

	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	fakeSSH(t, bin, "ssh", argsFile)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	unsetEnv(t, "GIT_SSH_COMMAND")
	unsetEnv(t, "GIT_SSH")

	_, fingerprint := signer(t)
	subKey := newSubKey(t)
	refresh := func() {
		cfg := Channel.Config{Type: Channel.TypeGit, Repo: "ssh://git@unitao.invalid/channel.git", WorkDir: filepath.Join(t.TempDir(), "work"), Folder: "infra"}
		sub, err := Channel.NewSubscriber(subConfig(cfg, subKey), "prod", fingerprint, nil)
		mustNoErr(t, err, "new subscriber")
		if err := sub.Refresh(); err == nil || errors.Is(err, Channel.ErrNotPublished) {
			t.Fatalf("expect fetch through fake ssh to fail, got %v", err)
		}
	}

	// git 经 GIT_SSH_COMMAND 用 DS 的私钥连接远端（路径带空格也能正确传给 ssh）
	refresh()
	args, err := os.ReadFile(argsFile)
	mustNoErr(t, err, "fake ssh was not called")
	for _, want := range []string{"-i " + id.KeyFile, "IdentitiesOnly=yes", "UserKnownHostsFile=" + id.KnownHostsFile, "StrictHostKeyChecking=accept-new", "ConnectTimeout=30", "ServerAliveInterval=15", "ServerAliveCountMax=4", "unitao.invalid"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("ssh args [%s] missing [%s]", strings.TrimSpace(string(args)), want)
		}
	}

	// 环境里已设置 GIT_SSH_COMMAND 时以环境为准
	otherArgs := filepath.Join(bin, "other-args")
	other := fakeSSH(t, bin, "other-ssh", otherArgs)
	t.Setenv("GIT_SSH_COMMAND", other+" -o FromEnv=yes")
	refresh()
	args, err = os.ReadFile(otherArgs)
	mustNoErr(t, err, "GIT_SSH_COMMAND from env was not used")
	if !strings.Contains(string(args), "FromEnv=yes") || strings.Contains(string(args), id.KeyFile) {
		t.Fatalf("expect env GIT_SSH_COMMAND to win, got args [%s]", strings.TrimSpace(string(args)))
	}
}

func TestDataServiceGeneratesSSHKey(t *testing.T) {
	ds := newDsNode(t, "SshDS", "")
	ds.start(t, map[string]interface{}{"local": map[string]interface{}{}})
	t.Cleanup(func() { Channel.SetSSHIdentity(nil) })

	keyFile := filepath.Join(ds.dir, Channel.SSHDirName, Channel.SSHKeyFile)
	if _, err := os.Stat(keyFile); err != nil {
		t.Fatalf("expect ssh key at [%s]: %s", keyFile, err)
	}
	readState := func() Config.SSHState {
		raw, err := os.ReadFile(filepath.Join(ds.dir, Config.StateFile))
		mustNoErr(t, err, "read state.json")
		var st struct {
			SSH *Config.SSHState `json:"ssh"`
		}
		mustNoErr(t, json.Unmarshal(raw, &st), "parse state.json")
		if st.SSH == nil {
			t.Fatalf("state.json has no ssh section: %s", raw)
		}
		return *st.SSH
	}
	first := readState()
	if !strings.HasPrefix(first.PublicKey, "ssh-ed25519 ") || !strings.HasSuffix(first.PublicKey, " unitao-SshDS") || first.KeyFile != keyFile {
		t.Fatalf("unexpected ssh state: %+v", first)
	}

	resp := call(t, http.MethodGet, ds.Url+"/local/config", nil)
	expectStatus(t, resp, http.StatusOK, "get local config")
	instance := resp.Map(t)["instance"].(map[string]interface{})
	ssh, ok := instance["ssh"].(map[string]interface{})
	if !ok || ssh["publicKey"] != first.PublicKey || ssh["fingerprint"] != first.Fingerprint {
		t.Fatalf("expect ssh public key in /local/config, got %v", instance["ssh"])
	}
	if strings.Contains(string(resp.Body), "PRIVATE KEY") {
		t.Fatal("private key leaked in /local/config")
	}

	// 重启沿用同一把私钥
	ds.start(t, map[string]interface{}{"local": map[string]interface{}{}})
	if again := readState(); again.PublicKey != first.PublicKey {
		t.Fatalf("expect the same key after restart, got %s", again.PublicKey)
	}
}

func TestDataServiceSSHDirConfig(t *testing.T) {
	ds := newDsNode(t, "SshDirDS", "")
	keyDir := filepath.Join(t.TempDir(), "volume")
	ds.extra = map[string]interface{}{"ssh": map[string]interface{}{"dir": keyDir}}
	ds.start(t, map[string]interface{}{"local": map[string]interface{}{}})
	t.Cleanup(func() { Channel.SetSSHIdentity(nil) })

	// 私钥在 ssh.dir，不在 config 目录
	keyFile := filepath.Join(keyDir, Channel.SSHKeyFile)
	if _, err := os.Stat(keyFile); err != nil {
		t.Fatalf("expect ssh key at [%s]: %s", keyFile, err)
	}
	if _, err := os.Stat(filepath.Join(ds.dir, Channel.SSHDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expect no ssh dir next to config, got err=%v", err)
	}
	raw, err := os.ReadFile(filepath.Join(ds.dir, Config.StateFile))
	mustNoErr(t, err, "read state.json")
	if strings.Contains(string(raw), "PRIVATE KEY") || !strings.Contains(string(raw), keyFile) {
		t.Fatalf("expect state.json to point at [%s] without the private key: %s", keyFile, raw)
	}

	// 相对路径相对 config 所在目录
	cfg := &Config.Confuguration{SSH: Config.SSHConfig{Dir: "keys/ssh"}}
	if got := Config.SSHDir(filepath.Join(ds.dir, "config.json"), cfg); got != filepath.Join(ds.dir, "keys", "ssh") {
		t.Fatalf("relative ssh.dir resolved to [%s]", got)
	}
}

// TestGitCommandTimeout：远端连接停在半路（ssh 不退出）时，git 命令按超时结束，连同 ssh 一起被杀掉，
// 不会一直占着工作目录。
func TestGitCommandTimeout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	bin := t.TempDir()
	pidFile := filepath.Join(bin, "pid")
	script := filepath.Join(bin, "hang-ssh")
	// exec：sleep 直接替换脚本进程，pid 文件里记的就是要确认被杀掉的进程
	body := "#!/bin/sh\necho $$ > '" + pidFile + "'\nexec sleep 3600\n"
	mustNoErr(t, os.WriteFile(script, []byte(body), 0o755), "write hanging ssh")
	t.Setenv("GIT_SSH_COMMAND", script)
	restore := Channel.SetGitTimeout(2 * time.Second)
	defer restore()

	_, fingerprint := signer(t)
	cfg := Channel.Config{Type: Channel.TypeGit, Repo: "ssh://git@unitao.invalid/channel.git", WorkDir: filepath.Join(t.TempDir(), "work"), Folder: "infra"}
	sub, err := Channel.NewSubscriber(subConfig(cfg, newSubKey(t)), "prod", fingerprint, nil)
	mustNoErr(t, err, "new subscriber")

	start := time.Now()
	err = sub.Refresh()
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timed out after 2s") {
		t.Fatalf("expect a timeout error, got %v", err)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("expect the command to stop soon after the timeout, took %s", elapsed)
	}
	raw, err := os.ReadFile(pidFile)
	mustNoErr(t, err, "hanging ssh did not start")
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	mustNoErr(t, err, "parse pid")
	waitFor(t, "hanging ssh to be killed", func() bool {
		return syscall.Kill(pid, 0) != nil
	})
}
