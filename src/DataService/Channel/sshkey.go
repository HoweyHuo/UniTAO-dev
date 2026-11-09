package Channel

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// git 通道走 SSH 远端时使用的身份。
//
// Data Service 启动时在 config 目录下的 ssh/ 里生成（已有则沿用）一把 ed25519 私钥，
// 公钥写进 state.json，由人把它加到 git 服务器上。私钥只在 DS 内部产生，不需要从外部挂入。
// git 命令经 GIT_SSH_COMMAND 使用这把私钥；进程环境里已经设置了 GIT_SSH_COMMAND 时以环境为准。
const (
	SSHDirName        = "ssh"
	SSHKeyFile        = "id_ed25519"
	SSHKnownHostsFile = "known_hosts"
	sshKeyType        = "ssh-ed25519"
	opensshMagic      = "openssh-key-v1\x00"
	opensshPemType    = "OPENSSH PRIVATE KEY"
)

// SSHIdentity 是 DS 用于 git 通道 SSH 远端的身份。
type SSHIdentity struct {
	KeyFile        string // 私钥（OpenSSH 格式，0600）
	KnownHostsFile string // 首次连接时记录服务器的 host key（StrictHostKeyChecking=accept-new）
	PublicKey      string // authorized_keys 格式：ssh-ed25519 <base64> <comment>
	Fingerprint    string // 与 ssh-keygen -l 相同：SHA256:<base64>
}

var (
	sshIdentityLock sync.Mutex
	sshIdentity     *SSHIdentity
)

// SetSSHIdentity 设置 git 命令使用的 SSH 身份（进程级，nil 表示不设置）。
func SetSSHIdentity(id *SSHIdentity) {
	sshIdentityLock.Lock()
	defer sshIdentityLock.Unlock()
	sshIdentity = id
}

// gitSSHEnv 返回要追加给 git 的 GIT_SSH_COMMAND；未设置身份或环境里已有 GIT_SSH_COMMAND 时返回空。
func gitSSHEnv() []string {
	if _, ok := os.LookupEnv("GIT_SSH_COMMAND"); ok {
		return nil
	}
	sshIdentityLock.Lock()
	id := sshIdentity
	sshIdentityLock.Unlock()
	if id == nil {
		return nil
	}
	return []string{"GIT_SSH_COMMAND=" + id.SSHCommand()}
}

// SSHCommand 返回使用该身份的 ssh 命令行（经 shell 执行，路径加单引号）。
// ConnectTimeout 限制建立连接的时间；ServerAlive* 每 15 秒探测一次服务器，连续 4 次无响应（约 1 分钟）就断开，
// 连接停在半路时 ssh 能自己退出，而不是等 git 命令的总超时。
func (id *SSHIdentity) SSHCommand() string {
	return fmt.Sprintf("ssh -i %s -o IdentitiesOnly=yes -o UserKnownHostsFile=%s -o StrictHostKeyChecking=accept-new -o BatchMode=yes"+
		" -o ConnectTimeout=30 -o ServerAliveInterval=15 -o ServerAliveCountMax=4",
		shellQuote(id.KeyFile), shellQuote(id.KnownHostsFile))
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// EnsureSSHIdentity 读取 dir 下的 SSH 私钥，不存在时生成。comment 写进新生成的私钥与公钥行。
func EnsureSSHIdentity(dir string, comment string) (*SSHIdentity, bool, error) {
	id := &SSHIdentity{
		KeyFile:        filepath.Join(dir, SSHKeyFile),
		KnownHostsFile: filepath.Join(dir, SSHKnownHostsFile),
	}
	created := false
	data, err := os.ReadFile(id.KeyFile)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, false, fmt.Errorf("failed to create ssh dir [%s], err:%s", dir, err)
		}
		data, err = generateOpenSSHKey(comment)
		if err != nil {
			return nil, false, err
		}
		// O_EXCL：不覆盖并发或之前留下的私钥
		f, err := os.OpenFile(id.KeyFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, false, fmt.Errorf("failed to create ssh key [%s], err:%s", id.KeyFile, err)
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			os.Remove(id.KeyFile)
			return nil, false, fmt.Errorf("failed to write ssh key [%s], err:%s", id.KeyFile, err)
		}
		if err := f.Close(); err != nil {
			return nil, false, fmt.Errorf("failed to write ssh key [%s], err:%s", id.KeyFile, err)
		}
		created = true
	} else if err != nil {
		return nil, false, fmt.Errorf("failed to read ssh key [%s], err:%s", id.KeyFile, err)
	}
	blob, keyComment, err := parseOpenSSHPublic(data)
	if err != nil {
		return nil, false, fmt.Errorf("invalid ssh key [%s], err:%s", id.KeyFile, err)
	}
	id.PublicKey = sshKeyType + " " + base64.StdEncoding.EncodeToString(blob)
	if keyComment != "" {
		id.PublicKey += " " + keyComment
	}
	sum := sha256.Sum256(blob)
	id.Fingerprint = "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
	return id, created, nil
}

func sshString(buf *bytes.Buffer, data []byte) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(data)))
	buf.Write(n[:])
	buf.Write(data)
}

func sshPublicBlob(pub ed25519.PublicKey) []byte {
	var blob bytes.Buffer
	sshString(&blob, []byte(sshKeyType))
	sshString(&blob, pub)
	return blob.Bytes()
}

// generateOpenSSHKey 生成未加密的 ed25519 私钥，格式与 ssh-keygen 相同（openssh-key-v1）。
func generateOpenSSHKey(comment string) ([]byte, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	var check [4]byte
	if _, err := rand.Read(check[:]); err != nil {
		return nil, err
	}
	var secret bytes.Buffer
	secret.Write(check[:])
	secret.Write(check[:])
	sshString(&secret, []byte(sshKeyType))
	sshString(&secret, pub)
	sshString(&secret, priv) // 64 字节：seed + 公钥
	sshString(&secret, []byte(comment))
	for i := byte(1); secret.Len()%8 != 0; i++ {
		secret.WriteByte(i)
	}
	var out bytes.Buffer
	out.WriteString(opensshMagic)
	sshString(&out, []byte("none")) // cipher
	sshString(&out, []byte("none")) // kdf
	sshString(&out, nil)            // kdf options
	binary.Write(&out, binary.BigEndian, uint32(1))
	sshString(&out, sshPublicBlob(pub))
	sshString(&out, secret.Bytes())
	return pem.EncodeToMemory(&pem.Block{Type: opensshPemType, Bytes: out.Bytes()}), nil
}

type sshReader struct {
	data []byte
	err  error
}

func (r *sshReader) uint32() uint32 {
	if r.err != nil || len(r.data) < 4 {
		r.err = errors.New("truncated key")
		return 0
	}
	v := binary.BigEndian.Uint32(r.data)
	r.data = r.data[4:]
	return v
}

func (r *sshReader) string() []byte {
	n := r.uint32()
	if r.err != nil || uint32(len(r.data)) < n {
		r.err = errors.New("truncated key")
		return nil
	}
	v := r.data[:n]
	r.data = r.data[n:]
	return v
}

// parseOpenSSHPublic 从 OpenSSH 私钥中取出公钥 blob 与注释（注释在私有段里，只有未加密时能读到）。
func parseOpenSSHPublic(data []byte) ([]byte, string, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != opensshPemType {
		return nil, "", fmt.Errorf("not an OpenSSH private key")
	}
	if !bytes.HasPrefix(block.Bytes, []byte(opensshMagic)) {
		return nil, "", fmt.Errorf("not an openssh-key-v1 key")
	}
	r := &sshReader{data: block.Bytes[len(opensshMagic):]}
	cipher := string(r.string())
	r.string() // kdf
	r.string() // kdf options
	if n := r.uint32(); r.err == nil && n != 1 {
		return nil, "", fmt.Errorf("expect 1 key, got %d", n)
	}
	blob := r.string()
	secret := r.string()
	if r.err != nil {
		return nil, "", r.err
	}
	pubReader := &sshReader{data: blob}
	if keyType := string(pubReader.string()); pubReader.err != nil || keyType != sshKeyType {
		return nil, "", fmt.Errorf("unsupported key type [%s], expect [%s]", keyType, sshKeyType)
	}
	if cipher != "none" {
		// 加了密码的私钥 git 无法非交互使用
		return nil, "", fmt.Errorf("ssh key is passphrase-protected (cipher %s), git cannot use it non-interactively", cipher)
	}
	sr := &sshReader{data: secret}
	sr.uint32()
	sr.uint32()
	sr.string() // type
	sr.string() // pub
	sr.string() // priv
	comment := string(sr.string())
	if sr.err != nil {
		return nil, "", sr.err
	}
	return blob, comment, nil
}
