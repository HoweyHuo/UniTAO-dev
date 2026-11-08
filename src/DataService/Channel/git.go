package Channel

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	DefaultBranch = "main"
	gitRetry      = 3
	gitUserName   = "UniTAO"
	gitUserEmail  = "unitao@localhost"
)

// gitRepo 用 git 命令行维护通道的本地工作目录。
// 发布方：完整克隆，写入后 commit + push，推送冲突时 rebase 重试（不同来源写不同目录，不会真正冲突）。
// 订阅方：浅克隆，只 fetch --depth 1 + reset --hard，从不写入。
type gitRepo struct {
	repo    string
	branch  string
	workDir string
	shallow bool
	log     *log.Logger
}

func newGitRepo(cfg Config, shallow bool, logger *log.Logger) (*gitRepo, error) {
	if cfg.Repo == "" || cfg.WorkDir == "" {
		return nil, fmt.Errorf("channel type [%s] requires [repo] and [workDir]", TypeGit)
	}
	branch := cfg.Branch
	if branch == "" {
		branch = DefaultBranch
	}
	return &gitRepo{repo: cfg.Repo, branch: branch, workDir: cfg.WorkDir, shallow: shallow, log: logger}, nil
}

func (g *gitRepo) run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		// 参数（remote add origin <repo>）和 git 的输出都可能带远端地址，写进错误（日志）前隐去凭据
		msg := fmt.Sprintf("git %s failed: %s, output: %s", strings.Join(args, " "), err, strings.TrimSpace(out.String()))
		if safe := RedactRepo(g.repo); safe != g.repo {
			msg = strings.ReplaceAll(msg, g.repo, safe)
		}
		return out.String(), fmt.Errorf("%s", msg)
	}
	return out.String(), nil
}

func (g *gitRepo) remoteHasBranch() (bool, error) {
	out, err := g.run(g.workDir, "ls-remote", "--heads", "origin", g.branch)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// ensure 保证工作目录是该仓库的克隆。远端还是空仓库时，在本地建好分支，首次 push 时创建远端分支。
func (g *gitRepo) ensure() error {
	if _, err := os.Stat(filepath.Join(g.workDir, ".git")); err == nil {
		return nil
	}
	if err := os.MkdirAll(g.workDir, 0755); err != nil {
		return err
	}
	if _, err := g.run(g.workDir, "init", "-q"); err != nil {
		return err
	}
	if _, err := g.run(g.workDir, "remote", "add", "origin", g.repo); err != nil {
		return err
	}
	if _, err := g.run(g.workDir, "checkout", "-q", "-b", g.branch); err != nil {
		return err
	}
	return nil
}

// sync 把工作目录对齐到远端分支。
func (g *gitRepo) sync() error {
	hasBranch, err := g.remoteHasBranch()
	if err != nil {
		return err
	}
	if !hasBranch {
		return nil
	}
	fetchArgs := []string{"fetch", "-q", "origin", g.branch}
	if g.shallow {
		fetchArgs = []string{"fetch", "-q", "--depth", "1", "origin", g.branch}
	}
	if _, err := g.run(g.workDir, fetchArgs...); err != nil {
		return err
	}
	if g.shallow {
		_, err := g.run(g.workDir, "reset", "-q", "--hard", "FETCH_HEAD")
		return err
	}
	// 发布方：本地可能有未推送的提交，rebase 到远端之上
	if _, err := g.run(g.workDir, "rev-parse", "--verify", "-q", "HEAD"); err != nil {
		_, err := g.run(g.workDir, "reset", "-q", "--hard", "FETCH_HEAD")
		return err
	}
	_, err = g.run(g.workDir, "rebase", "-q", "FETCH_HEAD")
	return err
}

func (g *gitRepo) commitAndPush(paths []string, message string) error {
	addArgs := append([]string{"add", "-A", "--"}, paths...)
	if _, err := g.run(g.workDir, addArgs...); err != nil {
		return err
	}
	status, err := g.run(g.workDir, "status", "--porcelain", "--")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		if _, err := g.run(g.workDir, "-c", "user.name="+gitUserName, "-c", "user.email="+gitUserEmail, "commit", "-q", "-m", message); err != nil {
			return err
		}
	}
	var lastErr error
	for attempt := 0; attempt < gitRetry; attempt++ {
		_, err := g.run(g.workDir, "push", "-q", "origin", "HEAD:refs/heads/"+g.branch)
		if err == nil {
			return nil
		}
		lastErr = err
		g.log.Printf("GitChannel: push failed, rebase and retry. Error:%s", err)
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
		if err := g.sync(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

func newGitPublisher(cfg Config, source string, signer ed25519.PrivateKey, recipients []*ecdh.PublicKey, logger *log.Logger) (Publisher, error) {
	repo, err := newGitRepo(cfg, false, logger)
	if err != nil {
		return nil, err
	}
	store := newFileStore(cfg.WorkDir, source, signer, recipients, nil, "", logger)
	prepare := func() error {
		if err := repo.ensure(); err != nil {
			return err
		}
		return repo.sync()
	}
	store.beforeRead = prepare
	store.beforeWrite = prepare
	store.afterWrite = func(message string) error {
		return repo.commitAndPush([]string{source}, message)
	}
	return store, nil
}

func newGitSubscriber(cfg Config, source string, identity *ecdh.PrivateKey, trusted string, logger *log.Logger) (Subscriber, error) {
	repo, err := newGitRepo(cfg, true, logger)
	if err != nil {
		return nil, err
	}
	store := newFileStore(cfg.WorkDir, source, nil, nil, identity, trusted, logger)
	store.beforeRead = func() error {
		if err := repo.ensure(); err != nil {
			return err
		}
		return repo.sync()
	}
	return store, nil
}
