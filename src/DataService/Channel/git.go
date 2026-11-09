package Channel

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	DefaultBranch = "main"
	gitUserName   = "UniTAO"
	gitUserEmail  = "unitao@localhost"
)

// gitRepo 用 git 命令行维护通道的本地工作目录（浅克隆）。
//
// 同一个 branch 上可能有多个写入方（各 vDS 的 publisher、各 subscriber 写自己的进度），
// publisher 还会强推压缩后的历史，所以不做 rebase：每次写入都先 fetch 并 reset 到远端，
// 只写自己负责的文件，提交后普通推送；被拒就从头重做（见 fileStore.write）。
// 每个写入方只碰自己的文件，重做不会冲掉别人的修改。
//
// 同一进程里 workDir 相同的 vDS 共用一个 gitRepo（必须是同一个 repo + branch），由 mu 串行化所有文件访问。
// 它们共用一份克隆，任何一个 vDS 的拉取都会更新整个 branch 的内容。
type gitRepo struct {
	repo       string
	branch     string
	workDir    string
	log        *log.Logger
	mu         sync.Mutex
	remoteHead string // 最近一次对齐时远端分支的提交；分支不存在时为空
}

var (
	gitReposLock sync.Mutex
	gitRepos     = map[string]*gitRepo{}
)

// sharedGitRepo 返回 workDir 对应的 gitRepo，同一个 workDir 只创建一个。
func sharedGitRepo(cfg Config, logger *log.Logger) (*gitRepo, error) {
	if cfg.Repo == "" || cfg.WorkDir == "" {
		return nil, fmt.Errorf("channel type [%s] requires [repo] and [workDir]", TypeGit)
	}
	key, err := filepath.Abs(cfg.WorkDir)
	if err != nil {
		return nil, err
	}
	branch := cfg.ResolvedBranch()
	gitReposLock.Lock()
	defer gitReposLock.Unlock()
	if repo, ok := gitRepos[key]; ok {
		if repo.repo != cfg.Repo || repo.branch != branch {
			return nil, fmt.Errorf("git workDir [%s] is already used for repo [%s] branch [%s], cannot reuse it for repo [%s] branch [%s]",
				cfg.WorkDir, RedactRepo(repo.repo), repo.branch, RedactRepo(cfg.Repo), branch)
		}
		return repo, nil
	}
	repo := &gitRepo{repo: cfg.Repo, branch: branch, workDir: key, log: logger}
	gitRepos[key] = repo
	return repo, nil
}

func (g *gitRepo) Lock()   { g.mu.Lock() }
func (g *gitRepo) Unlock() { g.mu.Unlock() }

// DefaultGitTimeout 是单条 git 命令的最长运行时间。网络连接可能停在半路（两端都不再收发、也不断开），
// 没有超时的话 git 会一直挂着，并一直占着这个工作目录的锁。
const DefaultGitTimeout = 10 * time.Minute

var (
	gitTimeoutLock sync.Mutex
	gitTimeout     = DefaultGitTimeout
)

// SetGitTimeout 设置单条 git 命令的超时（测试使用），返回恢复原值的函数。
func SetGitTimeout(d time.Duration) func() {
	gitTimeoutLock.Lock()
	defer gitTimeoutLock.Unlock()
	old := gitTimeout
	gitTimeout = d
	return func() {
		gitTimeoutLock.Lock()
		defer gitTimeoutLock.Unlock()
		gitTimeout = old
	}
}

func currentGitTimeout() time.Duration {
	gitTimeoutLock.Lock()
	defer gitTimeoutLock.Unlock()
	return gitTimeout
}

func (g *gitRepo) run(args ...string) (string, error) {
	timeout := currentGitTimeout()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.workDir
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), gitSSHEnv()...)
	// 超时时连同 git 启动的 ssh / git-remote-https 一起结束：它们拿着输出管道，只杀 git 的话 Wait 会等它们
	killProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("timed out after %s (%v)", timeout, err)
	}
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

func (g *gitRepo) withIdentity(args ...string) []string {
	return append([]string{"-c", "user.name=" + gitUserName, "-c", "user.email=" + gitUserEmail}, args...)
}

// ensure 保证工作目录是一个指向远端的 git 仓库。
func (g *gitRepo) ensure() error {
	if _, err := os.Stat(filepath.Join(g.workDir, ".git")); err == nil {
		return nil
	}
	if err := os.MkdirAll(g.workDir, 0755); err != nil {
		return err
	}
	if _, err := g.run("init", "-q"); err != nil {
		return err
	}
	if _, err := g.run("remote", "add", "origin", g.repo); err != nil {
		return err
	}
	_, err := g.run("symbolic-ref", "HEAD", "refs/heads/"+g.branch)
	return err
}

// sync 把工作目录对齐到远端分支最新提交，丢弃本地未推送的提交和修改。
// 远端还没有这个分支时，工作目录变成空的未出生分支，第一次推送时创建远端分支。
func (g *gitRepo) sync() error {
	if err := g.ensure(); err != nil {
		return err
	}
	if _, err := g.run("fetch", "-q", "--depth", "1", "origin", g.branch); err != nil {
		if !strings.Contains(err.Error(), "couldn't find remote ref") {
			return err
		}
		if err := g.makeEmpty(); err != nil {
			return err
		}
		g.remoteHead = ""
		return nil
	}
	head, err := g.run("rev-parse", "FETCH_HEAD")
	if err != nil {
		return err
	}
	g.remoteHead = strings.TrimSpace(head)
	if _, err := g.run("checkout", "-q", "-f", "-B", g.branch, g.remoteHead); err != nil {
		return err
	}
	_, err = g.run("clean", "-q", "-f", "-d")
	return err
}

func (g *gitRepo) makeEmpty() error {
	if _, err := g.run("symbolic-ref", "HEAD", "refs/heads/"+g.branch); err != nil {
		return err
	}
	// 删掉本地分支（如果有），回到未出生状态
	g.run("update-ref", "-d", "refs/heads/"+g.branch)
	if _, err := g.run("read-tree", "--empty"); err != nil {
		return err
	}
	_, err := g.run("clean", "-q", "-f", "-d", "-x")
	return err
}

func (g *gitRepo) Pull() error {
	return g.sync()
}

func (g *gitRepo) Prepare() error {
	return g.sync()
}

func (g *gitRepo) Commit(folder string, message string) error {
	if _, err := g.run("add", "-A", "--", filepath.FromSlash(folder)); err != nil {
		return err
	}
	if _, err := g.run("diff", "--cached", "--quiet"); err == nil {
		return nil // 没有修改
	}
	if _, err := g.run(g.withIdentity("commit", "-q", "-m", message)...); err != nil {
		return err
	}
	if _, err := g.run("push", "-q", "origin", "HEAD:refs/heads/"+g.branch); err != nil {
		return fmt.Errorf("%w: %s", errRetry, err)
	}
	head, err := g.run("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	g.remoteHead = strings.TrimSpace(head)
	return nil
}

// Compact 用当前文件树建一个没有父提交的新提交，强推替换整个分支历史。
// --force-with-lease 保证只有远端在这次对齐之后没有变化时才替换，不会冲掉别人刚推上去的提交；
// 失败时返回错误，下一轮再试。成功后回收本地旧对象。
func (g *gitRepo) Compact(message string) (bool, error) {
	if err := g.sync(); err != nil {
		return false, err
	}
	if g.remoteHead == "" {
		return false, nil
	}
	// 浅克隆里看不到父提交本身，但提交对象里仍有 parent 行
	commit, err := g.run("cat-file", "-p", g.remoteHead)
	if err != nil {
		return false, err
	}
	if !strings.Contains(commit, "\nparent ") {
		return false, nil
	}
	out, err := g.run(g.withIdentity("commit-tree", g.remoteHead+"^{tree}", "-m", message)...)
	if err != nil {
		return false, err
	}
	squashed := strings.TrimSpace(out)
	lease := fmt.Sprintf("--force-with-lease=refs/heads/%s:%s", g.branch, g.remoteHead)
	if _, err := g.run("push", "-q", lease, "origin", squashed+":refs/heads/"+g.branch); err != nil {
		return false, fmt.Errorf("compact rejected (remote changed or force push not allowed): %s", err)
	}
	if _, err := g.run("checkout", "-q", "-f", "-B", g.branch, squashed); err != nil {
		return true, err
	}
	g.remoteHead = squashed
	g.run("reflog", "expire", "--expire=now", "--all")
	if _, err := g.run("gc", "-q", "--prune=now"); err != nil {
		g.log.Printf("GitChannel: local gc after compact failed: %s", err)
	}
	return true, nil
}
