// Package Channel 定义 shared vDS 与 mirrored vDS 之间的通信通道（Communication Channel）。
//
// 通道是一个单 publisher、多 subscriber 的队列（见 plan/feature-channel-queue/plan.md）：
//   - publisher（shared vDS）把变更按 seq 连续打包成 queue batch；新 subscriber 订阅时给它一份当前数据
//     （history，按大小切成多个分片）。同一份 history 可以授权给多个 subscriber：复用期内新来的订阅方
//     直接拿到已有 history 的授权，不再重新打包。所有存活 subscriber 都处理过的 batch / history 被删除。
//   - subscriber（mirrored vDS）在 subscribers/<id>.json 汇报自己的进度。
//
// 通道内容由 publisher 的 ed25519 私钥签名，subscriber 只接受公钥指纹等于可信指纹的来源。
// queue batch 对白名单里所有 subscriber 的 X25519 公钥加密；history 用自己的内容密钥加密，
// 内容密钥在 manifest 里为每个被授权的 subscriber 各包装一份。subscriber 的 id 就是它公钥的 sha256，
// publisher 的接收方列表（recipientsFile）同时是 subscriber 白名单。
//
// 具体实现：
//   - "dir"：本地目录（测试、同机或共享文件系统场景）
//   - "git"：git 仓库（跨隔离网络）
package Channel

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"path"
	"regexp"
	"strings"
)

const (
	OpUpsert = "upsert"
	OpDelete = "delete"

	TypeDir = "dir"
	TypeGit = "git"

	// subscriber 状态
	StateSubscribe = "subscribe" // 已请求订阅，等待 / 正在加载 history
	StateActive    = "active"    // 已加载 history，按 queue 消费
)

var (
	// ErrGap 表示通道已无法从给定 cursor 连续提供变更（中间的 batch 已删除），subscriber 需要重新订阅。
	ErrGap = errors.New("channel cannot serve changes continuously from cursor, re-subscribe required")
	// ErrNotPublished 表示通道上还没有该来源的内容（manifest 不存在或还是旧格式）。
	ErrNotPublished = errors.New("source has not published anything on channel yet")
	// ErrNoHistory 表示 publisher 还没有为这个订阅请求打包 history。
	ErrNoHistory = errors.New("no history published for this subscription request yet")
	// ErrHistoryKeyUnknown 表示本进程没有这份 history 的内容密钥（publisher 重启过），不能再给它增加授权。
	ErrHistoryKeyUnknown = errors.New("content key of this history is not known to this process")
)

// Change 是一条变更：完整的 after 记录（upsert）或删除。
type Change struct {
	Seq    uint64                 `json:"seq"`
	Source string                 `json:"source"`
	Op     string                 `json:"op"`
	Type   string                 `json:"type"`
	Id     string                 `json:"id"`
	Record map[string]interface{} `json:"record,omitempty"`
	Ts     string                 `json:"ts"`
}

// History 是 publisher 打包的全量数据：seq 时刻 vDS 中所有需要同步的记录，schema 在前。
type History struct {
	Seq     uint64
	Source  string
	Ts      string
	Records []map[string]interface{}
}

// HistoryGrant 是一次授权：把 history 交给 subscriber 的某个订阅请求。
type HistoryGrant struct {
	Subscriber string
	Request    string
}

// HistoryReader 是 subscriber 拿到的、授权给自己的 history：元数据在 manifest 里，记录按分片逐个读取。
type HistoryReader struct {
	Id        string
	Seq       uint64
	Source    string
	Published string
	Records   int // manifest 记录的总记录数
	Parts     int
	each      func(fn func(record map[string]interface{}) error) error
}

// Each 按顺序逐条读取 history 的记录，一次只解密、解压一个分片。
func (h *HistoryReader) Each(fn func(record map[string]interface{}) error) error {
	return h.each(fn)
}

// Progress 是 subscriber 写在 subscribers/<id>.json 里的进度。
type Progress struct {
	Id      string `json:"id"`
	Request string `json:"request"`
	State   string `json:"state"`
	Cursor  uint64 `json:"cursor"`
	Updated string `json:"updated"`
}

// View 是 publisher 读到的通道状态。
type View struct {
	// Published 为 false 表示通道上还没有本来源的 manifest。
	Published bool
	// OldFormat 为 true 表示通道上是旧格式（snapshot + segment）的 manifest，需要 Reset。
	OldFormat   bool
	LastSeq     uint64
	Queue       []FileRef // 按 From 升序
	Histories   []HistoryRef
	Subscribers []Progress // subscribers/ 下所有文件，含白名单之外的
}

// Prune 描述一次清理。
type Prune struct {
	QueueUpTo   uint64   // 删除 To <= QueueUpTo 的 queue batch
	Histories   []string // 要删除的 history（HistoryRef.Id），连同它的全部分片
	Subscribers []string // 要删除进度文件的 subscriber id
}

// Config 是 vDS 配置中的 channel 段。vDS 在通道上的地址是 repo（dir 为 path）+ branch + folder。
type Config struct {
	Type    string         `json:"type"`
	Path    string         `json:"path,omitempty"`    // dir：通道根目录
	Repo    string         `json:"repo,omitempty"`    // git：远端仓库地址
	Branch  string         `json:"branch,omitempty"`  // git：分支，默认 main
	Folder  string         `json:"folder,omitempty"`  // 通道内的目录，可以多级；默认等于 vDS 名字
	WorkDir string         `json:"workDir,omitempty"` // git：本地工作目录，同一进程里 repo + branch 相同的 vDS 可以共用
	Encrypt *EncryptConfig `json:"encrypt,omitempty"` // shared 侧（必填）：subscriber 的公钥，同时是 subscriber 白名单
	Decrypt *DecryptConfig `json:"decrypt,omitempty"` // mirrored 侧（必填）：自己的私钥
}

type EncryptConfig struct {
	RecipientsFile string `json:"recipientsFile"`
}

type DecryptConfig struct {
	IdentityFile string `json:"identityFile"`
}

// ResolvedFolder 返回 vDS 在通道上的目录：配置了 folder 用 folder，否则用 vDS（来源）名字。
func (c Config) ResolvedFolder(name string) string {
	if c.Folder != "" {
		return path.Clean(c.Folder)
	}
	return name
}

// ResolvedBranch 返回 git 分支，默认 main。
func (c Config) ResolvedBranch() string {
	if c.Branch != "" {
		return c.Branch
	}
	return DefaultBranch
}

// Publisher 是 shared vDS 侧的通道接口。
type Publisher interface {
	// View 读取通道当前状态（manifest 与所有 subscriber 进度）。
	View() (*View, error)
	// Publish 发布一批 seq 连续、紧接 LastSeq 的变更，作为一个 queue batch。
	// 白名单为空时没有人能解密，不写 queue batch，只把 manifest 的 LastSeq 推进到这批的末尾：
	// 新订阅方总是先拿 history（订阅时的全量），不需要它加入之前的 queue。
	Publish(changes []Change) error
	// PublishHistory 打包一份新的 history：按大小切片，用新的内容密钥加密，并授权给 grants；返回它在 manifest 里的条目。
	// 内容密钥只留在本进程内存里，供之后 GrantHistory 使用。
	PublishHistory(h *History, grants []HistoryGrant) (*HistoryRef, error)
	// GrantHistory 把已有的 history 授权给更多订阅请求（只改 manifest，不重写分片），返回更新后的条目。
	// 同一 subscriber 的旧授权被替换。本进程没有它的内容密钥时返回 ErrHistoryKeyUnknown。
	GrantHistory(id string, grants []HistoryGrant) (*HistoryRef, error)
	// CanGrant 判断本进程是否还有这份 history 的内容密钥。
	CanGrant(id string) bool
	// Prune 删除 queue batch、history 与死亡 subscriber 的进度文件。
	Prune(p Prune) error
	// Reset 让通道从 lastSeq 重新开始：清空 queue、history 和旧格式文件（subscriber 进度保留）。
	// 用于通道落后于本地（仓库被重置或更换）或是旧格式。
	Reset(lastSeq uint64) error
	// Compact 压缩通道历史（git：用当前文件树替换整个分支历史）；没有可压缩的历史或 dir 通道返回 false。
	Compact() (bool, error)
	// Trusted 判断 subscriber id 是否在白名单（接收方列表）中。
	Trusted(subscriber string) bool
	// RecipientCount 返回白名单中的接收方数量；为 0 时 Publish 不写 queue batch。
	RecipientCount() int
}

// Subscriber 是 mirrored vDS 侧的通道接口。
type Subscriber interface {
	// Refresh 拉取通道最新内容，读取自己的进度文件，校验 manifest 签名与来源可信指纹。
	// 来源还没有发布时返回 ErrNotPublished，此时 Progress 仍然可用。
	Refresh() error
	// Id 返回本 subscriber 的 id（自己 X25519 公钥的 sha256）。
	Id() string
	// SourceFingerprint 返回最近一次 Refresh 校验通过的来源公钥指纹。
	SourceFingerprint() string
	// LastSeq 返回最近一次 Refresh 时通道上已发布的最大 seq。
	LastSeq() uint64
	// Progress 返回最近一次 Refresh 时读到的自己的进度文件；不存在时返回 nil。
	Progress() *Progress
	// WriteProgress 写自己的进度文件（Id 与 Updated 由通道填写）。
	WriteProgress(p Progress) error
	// History 返回授权给这个订阅请求的 history；还没有时返回 ErrNoHistory。
	History(request string) (*HistoryReader, error)
	// Fetch 返回 seq > cursor 的变更（最多约 limit 条，按 batch 粒度）；没有新数据返回空切片。
	// 无法从 cursor+1 连续提供时返回 ErrGap。
	Fetch(cursor uint64, limit int) ([]Change, error)
}

// NewRequestId 生成一个订阅请求 id。
func NewRequestId() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}

func newBackend(cfg Config, logger *log.Logger) (backend, string, error) {
	switch cfg.Type {
	case TypeDir:
		if cfg.Path == "" {
			return nil, "", fmt.Errorf("channel type [%s] requires [path]", TypeDir)
		}
		return newDirBackend(), cfg.Path, nil
	case TypeGit:
		repo, err := sharedGitRepo(cfg, logger)
		if err != nil {
			return nil, "", err
		}
		return repo, repo.workDir, nil
	}
	return nil, "", fmt.Errorf("unknown channel type [%s]", cfg.Type)
}

// NewPublisher 创建 Publisher。source 为共享名，signer 为签名私钥。接收方列表（encrypt.recipientsFile）必须配置；
// 文件不存在时创建一个空白名单（目录必须存在）。
func NewPublisher(cfg Config, source string, signer ed25519.PrivateKey, logger *log.Logger) (Publisher, error) {
	if logger == nil {
		logger = log.Default()
	}
	if cfg.Encrypt == nil || cfg.Encrypt.RecipientsFile == "" {
		return nil, fmt.Errorf("channel of shared vDS [%s] requires [encrypt.recipientsFile] (the subscriber whitelist)", source)
	}
	created, err := EnsureRecipientsFile(cfg.Encrypt.RecipientsFile)
	if err != nil {
		return nil, err
	}
	if created {
		logger.Printf("created empty recipients file [%s] for shared vDS [%s]; add subscribers' recipients there and restart", cfg.Encrypt.RecipientsFile, source)
	}
	recipients, err := loadRecipients(cfg.Encrypt)
	if err != nil {
		return nil, err
	}
	be, root, err := newBackend(cfg, logger)
	if err != nil {
		return nil, err
	}
	store := newFileStore(be, root, cfg.ResolvedFolder(source), source, logger)
	store.signer = signer
	store.recipients = map[string]*ecdh.PublicKey{}
	for _, pub := range recipients {
		store.recipients[SubscriberId(pub)] = pub
	}
	return store, nil
}

// NewSubscriber 创建 Subscriber。只接受公钥指纹等于 trustedFingerprint 的来源。自己的私钥（decrypt.identityFile）必填。
func NewSubscriber(cfg Config, source string, trustedFingerprint string, logger *log.Logger) (Subscriber, error) {
	if logger == nil {
		logger = log.Default()
	}
	if trustedFingerprint == "" {
		return nil, fmt.Errorf("trusted fingerprint of source [%s] is required", source)
	}
	if cfg.Decrypt == nil || cfg.Decrypt.IdentityFile == "" {
		return nil, fmt.Errorf("channel of mirrored vDS [%s] requires [decrypt.identityFile] (the subscriber identity)", source)
	}
	identity, err := loadIdentity(cfg.Decrypt)
	if err != nil {
		return nil, err
	}
	be, root, err := newBackend(cfg, logger)
	if err != nil {
		return nil, err
	}
	store := newFileStore(be, root, cfg.ResolvedFolder(source), source, logger)
	store.identity = identity
	store.selfId = SubscriberId(identity.PublicKey())
	store.trusted = trustedFingerprint
	return store, nil
}

var folderSegment = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// validFolder 检查 folder：相对路径，各级只允许 [A-Za-z0-9_.-]，不能是 . 或 ..。
func validFolder(folder string) bool {
	if folder == "" || strings.HasPrefix(folder, "/") {
		return false
	}
	for _, seg := range strings.Split(folder, "/") {
		if seg == "." || seg == ".." || !folderSegment.MatchString(seg) {
			return false
		}
	}
	return true
}

// Validate 检查 channel 配置是否完整。加密配置按角色在 vDS 配置校验中检查。
func (c Config) Validate() error {
	switch c.Type {
	case TypeDir:
		if c.Path == "" {
			return fmt.Errorf("channel type [%s] requires [path]", TypeDir)
		}
	case TypeGit:
		if c.Repo == "" || c.WorkDir == "" {
			return fmt.Errorf("channel type [%s] requires [repo] and [workDir]", TypeGit)
		}
	default:
		return fmt.Errorf("unknown channel type [%s], expect [%s] or [%s]", c.Type, TypeDir, TypeGit)
	}
	if c.Folder != "" && !validFolder(c.Folder) {
		return fmt.Errorf("invalid channel folder [%s]: relative path, segments of [A-Za-z0-9_.-] only", c.Folder)
	}
	return nil
}
