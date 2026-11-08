package VdsTest

import (
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"DataService/Channel"
)

func TestRedactRepo(t *testing.T) {
	cases := []struct {
		repo   string
		expect string
	}{
		// http(s)：整个 userinfo 视为凭据
		{"https://unitao-bot:s3cret@git.internal/unitao/exchange.git", "https://***@git.internal/unitao/exchange.git"},
		{"https://ghp_token123@github.com/org/exchange.git", "https://***@github.com/org/exchange.git"},
		{"HTTP://user:pw@git.internal:8080/x.git", "http://***@git.internal:8080/x.git"},
		{"https://git.internal/unitao/exchange.git", "https://git.internal/unitao/exchange.git"},
		// 其它 scheme：保留用户名，只隐去密码
		{"ssh://git:pw@git.internal:2222/unitao/exchange.git", "ssh://git:***@git.internal:2222/unitao/exchange.git"},
		{"ssh://git@git.internal/unitao/exchange.git", "ssh://git@git.internal/unitao/exchange.git"},
		// 不含凭据
		{"git@git.internal:unitao/exchange.git", "git@git.internal:unitao/exchange.git"},
		{"file:///opt/UniTAO/remote/exchange.git", "file:///opt/UniTAO/remote/exchange.git"},
		{"/srv/git/exchange.git", "/srv/git/exchange.git"},
		{"", ""},
		// 解析失败时保守地隐去 "://" 与 "@" 之间的全部内容
		{"https://user:p%zz@git.internal/x.git", "https://***@git.internal/x.git"},
	}
	for _, c := range cases {
		if got := Channel.RedactRepo(c.repo); got != c.expect {
			t.Errorf("RedactRepo(%q) = %q, expect %q", c.repo, got, c.expect)
		}
	}
}

// git 命令失败时，错误信息（会写进日志）不能带出远端地址里的凭据。
// 新版 git 自己会在输出里隐去 URL 中的凭据；gitRepo.run 再按 RedactRepo 替换一遍，
// 兜底老版本 git 以及参数里直接带地址的命令（remote add origin <repo>）。这里检查的是整体结果。
func TestGitChannelErrorRedactsCredentials(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	key, _ := signer(t)
	// 端口 1 上没有服务，git 会立即连接失败
	repo := "https://unitao-bot:s3cret-token@127.0.0.1:1/exchange.git"
	pub, err := Channel.NewPublisher(pubConfig(Channel.Config{Type: Channel.TypeGit, Repo: repo, WorkDir: filepath.Join(t.TempDir(), "pub")}, recipientsFile(t, newSubKey(t))), "prod", key, nil)
	mustNoErr(t, err, "new git publisher")
	err = pub.Publish(changes("prod", 1, 1))
	if err == nil {
		t.Fatal("expect publish to an unreachable remote to fail")
	}
	if strings.Contains(err.Error(), "s3cret-token") {
		t.Fatalf("git error leaks the credential: %s", err)
	}
}

// GET /{vDS}/config 不需要认证，返回的 channel.repo 不能带凭据。
func TestVdsConfigRedactsRepoCredentials(t *testing.T) {
	keyFile, _ := newSigningKey(t)
	ds := newDsNode(t, "dsA", "")
	ds.start(t, map[string]interface{}{
		"local": map[string]interface{}{},
		"shared": []interface{}{map[string]interface{}{
			"name": "prod", "signingKeyFile": keyFile,
			"publishIntervalSec": 3600,
			"channel": withEncrypt(map[string]interface{}{
				"type": "git", "repo": "https://unitao-bot:s3cret-token@127.0.0.1:1/exchange.git",
				"workDir": filepath.Join(t.TempDir(), "work"),
			}, recipientsFile(t, newSubKey(t))),
		}},
	})
	resp := call(t, http.MethodGet, ds.Url+"/shared/prod/config", nil)
	expectStatus(t, resp, http.StatusOK, "GET shared config")
	if strings.Contains(string(resp.Body), "s3cret-token") {
		t.Fatalf("/config leaks the credential: %s", resp.Body)
	}
	channel, _ := resp.Map(t)["channel"].(map[string]interface{})
	if repo := channel["repo"]; repo != "https://***@127.0.0.1:1/exchange.git" {
		t.Fatalf("expect redacted repo in /config, got %v", repo)
	}
}
