// Package Channel 定义 shared vDS 与 mirrored vDS 之间的通信通道（Communication Channel）插件接口。
//
// shared vDS 通过 Publisher 把变更（Change）和全量快照（Snapshot）发布到通道；
// mirrored vDS 通过 Subscriber 从通道读取。通道内容由 shared vDS 的 ed25519 私钥签名，
// Subscriber 只接受公钥指纹等于配置中可信指纹的来源。
//
// 具体实现：
//   - "dir"：本地目录（测试、同机或共享文件系统场景）
//   - "git"：git 仓库（跨隔离网络）
package Channel

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"log"
)

const (
	OpUpsert = "upsert"
	OpDelete = "delete"

	TypeDir = "dir"
	TypeGit = "git"
)

var (
	// ErrGap 表示通道已无法从给定 cursor 连续提供变更（中间的 segment 已被 snapshot 取代或丢失），
	// 订阅方需要从最新 snapshot 重建。
	ErrGap = errors.New("channel cannot serve changes continuously from cursor, rebuild from snapshot required")
	// ErrNoSnapshot 表示通道上还没有任何 snapshot。
	ErrNoSnapshot = errors.New("no snapshot published on channel yet")
	// ErrNotPublished 表示通道上还没有该来源的内容（manifest 不存在）。
	ErrNotPublished = errors.New("source has not published anything on channel yet")
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

// Snapshot 是某个 seq 时刻 shared vDS 的全量数据。Records 中 schema 记录排在数据记录之前。
type Snapshot struct {
	Seq     uint64                   `json:"seq"`
	Source  string                   `json:"source"`
	Ts      string                   `json:"ts"`
	Records []map[string]interface{} `json:"-"`
}

// Config 是 vDS 配置中的 channel 段。
type Config struct {
	Type    string         `json:"type"`
	Path    string         `json:"path,omitempty"`    // dir：通道根目录
	Repo    string         `json:"repo,omitempty"`    // git：远端仓库地址
	Branch  string         `json:"branch,omitempty"`  // git：分支，默认 main
	WorkDir string         `json:"workDir,omitempty"` // git：本地工作目录
	Encrypt *EncryptConfig `json:"encrypt,omitempty"` // shared 侧：按接收方公钥加密
	Decrypt *DecryptConfig `json:"decrypt,omitempty"` // mirrored 侧：用自己的私钥解密
}

type EncryptConfig struct {
	RecipientsFile string `json:"recipientsFile"`
}

type DecryptConfig struct {
	IdentityFile string `json:"identityFile"`
}

// Publisher 是 shared vDS 侧的通道接口。
type Publisher interface {
	// Publish 发布一批 seq 连续递增的变更；返回 nil 即视为已持久化到通道。
	Publish(changes []Change) error
	// PublishSnapshot 发布全量快照，并清理被它覆盖的旧 segment 与旧 snapshot。
	PublishSnapshot(s *Snapshot) error
	// LastPublishedSeq 返回通道上已发布的最大 seq（含 snapshot）。
	LastPublishedSeq() (uint64, error)
	// LatestSnapshotSeq 返回最新 snapshot 的 seq；没有 snapshot 时 ok=false。
	LatestSnapshotSeq() (seq uint64, ok bool, err error)
}

// Subscriber 是 mirrored vDS 侧的通道接口。
type Subscriber interface {
	// Refresh 拉取通道最新内容并校验 manifest 签名与来源可信指纹。
	Refresh() error
	// SourceFingerprint 返回最近一次 Refresh 校验通过的来源公钥指纹。
	SourceFingerprint() string
	// Fetch 返回 seq > cursor 的变更（最多约 limit 条，按 segment 粒度）；无新数据返回空切片。
	// 无法从 cursor+1 连续提供时返回 ErrGap。
	Fetch(cursor uint64, limit int) ([]Change, error)
	// LatestSnapshot 返回最新 snapshot；没有时返回 ErrNoSnapshot。
	LatestSnapshot() (*Snapshot, error)
}

// NewPublisher 按 cfg.Type 创建 Publisher。source 为共享名，signer 为签名私钥。
func NewPublisher(cfg Config, source string, signer ed25519.PrivateKey, logger *log.Logger) (Publisher, error) {
	if logger == nil {
		logger = log.Default()
	}
	recipients, err := loadRecipients(cfg.Encrypt)
	if err != nil {
		return nil, err
	}
	switch cfg.Type {
	case TypeDir:
		if cfg.Path == "" {
			return nil, fmt.Errorf("channel type [%s] requires [path]", TypeDir)
		}
		return newFileStore(cfg.Path, source, signer, recipients, nil, "", logger), nil
	case TypeGit:
		return newGitPublisher(cfg, source, signer, recipients, logger)
	}
	return nil, fmt.Errorf("unknown channel type [%s]", cfg.Type)
}

// NewSubscriber 按 cfg.Type 创建 Subscriber。只接受公钥指纹等于 trustedFingerprint 的来源。
func NewSubscriber(cfg Config, source string, trustedFingerprint string, logger *log.Logger) (Subscriber, error) {
	if logger == nil {
		logger = log.Default()
	}
	if trustedFingerprint == "" {
		return nil, fmt.Errorf("trusted fingerprint of source [%s] is required", source)
	}
	identity, err := loadIdentity(cfg.Decrypt)
	if err != nil {
		return nil, err
	}
	switch cfg.Type {
	case TypeDir:
		if cfg.Path == "" {
			return nil, fmt.Errorf("channel type [%s] requires [path]", TypeDir)
		}
		return newFileStore(cfg.Path, source, nil, nil, identity, trustedFingerprint, logger), nil
	case TypeGit:
		return newGitSubscriber(cfg, source, identity, trustedFingerprint, logger)
	}
	return nil, fmt.Errorf("unknown channel type [%s]", cfg.Type)
}

// Validate 检查 channel 配置是否完整。
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
	return nil
}
