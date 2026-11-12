package Channel

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"path"
	"strings"
)

// SourceId 是一个来源的身份：它在通道上的地址（仓库 + 分支 + folder，dir 通道为路径 + folder）的 sha256，
// 写成 SHA256:<hex>。shared 与它的各个 mirror 配置的是同一个地址，各自算出的 SourceId 相同，Inventory
// 据此把它们合并成一条记录；重建的 publisher 配置不变，身份也不变。
// 仓库地址先规范化，同一个仓库的 ssh / https 写法（git@host:a/b.git、ssh://git@host/a/b、https://user@host/a/b.git）得到同一个地址。
func SourceId(cfg Config, folder string) string {
	var address string
	switch cfg.Type {
	case TypeGit:
		address = "git:" + NormalizeRepo(cfg.Repo) + "#" + cfg.ResolvedBranch()
	default:
		address = cfg.Type + ":" + path.Clean(cfg.Path)
	}
	address += ":" + strings.Trim(path.Clean(folder), "/")
	sum := sha256.Sum256([]byte(address))
	return "SHA256:" + hex.EncodeToString(sum[:])
}

// NormalizeRepo 把 git 仓库地址化成 host/path（主机名小写，去掉协议、用户、凭据、端口、结尾的 / 和 .git）；
// file:// 与本地路径保留路径。
func NormalizeRepo(repo string) string {
	repo = strings.TrimSpace(repo)
	var host, p string
	switch {
	case strings.HasPrefix(repo, "file://"):
		p = strings.TrimPrefix(repo, "file://")
	case strings.Contains(repo, "://"):
		u, err := url.Parse(repo)
		if err != nil {
			return repo
		}
		host, p = u.Hostname(), u.Path
	case strings.Contains(repo, ":") && !strings.HasPrefix(repo, "/"):
		// scp 写法：[user@]host:path
		hostPart, rest, _ := strings.Cut(repo, ":")
		if at := strings.LastIndex(hostPart, "@"); at >= 0 {
			hostPart = hostPart[at+1:]
		}
		host, p = hostPart, rest
	default:
		p = repo
	}
	p = strings.TrimSuffix(strings.TrimRight(p, "/"), ".git")
	if host == "" {
		return path.Clean(p)
	}
	return strings.ToLower(host) + "/" + strings.TrimLeft(p, "/")
}
