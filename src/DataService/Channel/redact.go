package Channel

import (
	"net/url"
	"strings"
)

const redacted = "***"

// RedactRepo 隐去 git 远端地址中的凭据，用于 GET /{vDS}/config 和日志。
//
//   - http(s)：整个 userinfo 都可能是凭据（如 https://<token>@host/...），整体换成 ***
//   - 其它带 scheme 的地址（ssh://user:pass@host/...）：保留用户名，只隐去密码
//   - scp 风格（git@host:path）和本地路径：不含凭据，原样返回
//
// 推荐的做法仍是不把凭据写进地址，而是用 credential helper 或 SSH 密钥。
func RedactRepo(repo string) string {
	if !strings.Contains(repo, "://") {
		return repo
	}
	u, err := url.Parse(repo)
	if err != nil {
		// 解析失败就无法准确定位凭据，保守处理："://" 与最后一个 "@" 之间的内容全部隐去
		start := strings.Index(repo, "://") + len("://")
		if at := strings.LastIndex(repo, "@"); at > start {
			return repo[:start] + redacted + repo[at:]
		}
		return repo
	}
	if u.User == nil {
		return repo
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "http" || scheme == "https" {
		u.User = url.User(redacted)
	} else if _, hasPassword := u.User.Password(); hasPassword {
		u.User = url.UserPassword(u.User.Username(), redacted)
	} else {
		return repo
	}
	// url 包会把 userinfo 里的 * 转义成 %2A，换回来便于阅读
	return strings.Replace(u.String(), "%2A%2A%2A", redacted, 1)
}
