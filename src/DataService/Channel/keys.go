package Channel

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	FingerprintPrefix = "SHA256:"
	encryptInfo       = "UniTAO-channel-v1"
	encryptVersion    = 1
)

// Fingerprint 返回 ed25519 公钥指纹："SHA256:" + 公钥原始字节 sha256 的小写十六进制。
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return FingerprintPrefix + hex.EncodeToString(sum[:])
}

// SubscriberId 返回 subscriber 的 id：X25519 公钥原始字节 sha256 的小写十六进制。
// 它用作通道上 subscribers/<id>.json 的文件名，publisher 用接收方列表判断 id 是否可信。
func SubscriberId(pub *ecdh.PublicKey) string {
	sum := sha256.Sum256(pub.Bytes())
	return hex.EncodeToString(sum[:])
}

// RecipientSubscriberId 返回 base64 编码的接收方公钥（enckey 生成的 recipient）对应的 subscriber id。
func RecipientSubscriberId(recipient string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(recipient))
	if err != nil {
		return "", err
	}
	pub, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return "", err
	}
	return SubscriberId(pub), nil
}

// FingerprintShort 返回指纹的前 8 位十六进制，用于撞名时派生名字。
func FingerprintShort(fingerprint string) string {
	hexPart := strings.TrimPrefix(fingerprint, FingerprintPrefix)
	if len(hexPart) > 8 {
		return hexPart[:8]
	}
	return hexPart
}

// GenerateSigningKey 生成 ed25519 私钥，返回 PKCS#8 PEM。
func GenerateSigningKey() ([]byte, ed25519.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), pub, nil
}

// LoadSigningKey 读取 PKCS#8 PEM 格式的 ed25519 私钥（与 `openssl genpkey -algorithm ed25519` 兼容）。
func LoadSigningKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read signing key [%s], Error:%s", path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("signing key [%s] is not PEM", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse signing key [%s], Error:%s", path, err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("signing key [%s] is not an ed25519 key", path)
	}
	return priv, nil
}

// GenerateEncryptionKey 生成 X25519 密钥对，返回 base64 编码的私钥（identity）和公钥（recipient）。
func GenerateEncryptionKey() (identity string, recipient string, err error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(priv.Bytes()), base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes()), nil
}

// readKeyLines 读取文件中非空、非 # 注释的行。
func readKeyLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := []string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
	}
	return lines, nil
}

const recipientsFileHeader = "# subscriber whitelist of this shared vDS: one recipient (base64 X25519 public key from\n" +
	"# `DataServiceAdmin enckey`) per line, # starts a comment. restart the Data Service after editing.\n"

// EnsureRecipientsFile 在白名单文件不存在时创建它（只有注释，即空白名单），返回是否新建。
// 只建文件、不建目录：目录不存在多半是路径写错，直接报错。已有的文件不会被改动。
func EnsureRecipientsFile(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to create recipients file [%s], Error:%s", path, err)
	}
	if _, err := f.WriteString(recipientsFileHeader); err != nil {
		f.Close()
		return false, fmt.Errorf("failed to write recipients file [%s], Error:%s", path, err)
	}
	if err := f.Close(); err != nil {
		return false, fmt.Errorf("failed to write recipients file [%s], Error:%s", path, err)
	}
	return true, nil
}

func loadRecipients(cfg *EncryptConfig) ([]*ecdh.PublicKey, error) {
	if cfg == nil || cfg.RecipientsFile == "" {
		return nil, nil
	}
	lines, err := readKeyLines(cfg.RecipientsFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read recipients file [%s], Error:%s", cfg.RecipientsFile, err)
	}
	// 白名单可以为空（还没有订阅方）：publisher 照常启动、写 manifest，只是不写 queue batch，见 fileStore.Publish。
	// 文件不存在时由 EnsureRecipientsFile 在配置的路径上建出来，路径写错时能看到多出来的文件。
	result := []*ecdh.PublicKey{}
	for _, line := range lines {
		raw, err := base64.StdEncoding.DecodeString(line)
		if err != nil {
			return nil, fmt.Errorf("invalid recipient key in [%s], Error:%s", cfg.RecipientsFile, err)
		}
		pub, err := ecdh.X25519().NewPublicKey(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid recipient key in [%s], Error:%s", cfg.RecipientsFile, err)
		}
		result = append(result, pub)
	}
	return result, nil
}

func loadIdentity(cfg *DecryptConfig) (*ecdh.PrivateKey, error) {
	if cfg == nil || cfg.IdentityFile == "" {
		return nil, nil
	}
	lines, err := readKeyLines(cfg.IdentityFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read identity file [%s], Error:%s", cfg.IdentityFile, err)
	}
	if len(lines) != 1 {
		return nil, fmt.Errorf("identity file [%s] must contain exactly one key", cfg.IdentityFile)
	}
	raw, err := base64.StdEncoding.DecodeString(lines[0])
	if err != nil {
		return nil, fmt.Errorf("invalid identity key in [%s], Error:%s", cfg.IdentityFile, err)
	}
	priv, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid identity key in [%s], Error:%s", cfg.IdentityFile, err)
	}
	return priv, nil
}

// encryptedFile 是加密文件的格式：随机文件密钥用每个接收方的 X25519 公钥各包装一份。
type encryptedFile struct {
	Version    int                  `json:"v"`
	Recipients []encryptedRecipient `json:"recipients"`
	Nonce      string               `json:"nonce"`
	Data       string               `json:"data"`
}

type encryptedRecipient struct {
	EphemeralKey string `json:"epk"`
	WrappedKey   string `json:"key"`
}

func wrapKey(shared []byte, ephPub []byte, recipientPub []byte) ([]byte, error) {
	salt := append(append([]byte{}, ephPub...), recipientPub...)
	return hkdf.Key(sha256.New, shared, salt, encryptInfo, 32)
}

func gcmSeal(key []byte, nonce []byte, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, nonce, plain, nil), nil
}

func gcmOpen(key []byte, nonce []byte, sealed []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, nonce, sealed, nil)
}

// newFileKey 生成一把随机的 AES-256 内容密钥。
func newFileKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}

// wrapFileKey 用 X25519 ECDH（临时密钥）+ HKDF-SHA256 派生的密钥为 recipient 包装内容密钥。
func wrapFileKey(fileKey []byte, recipient *ecdh.PublicKey) (encryptedRecipient, error) {
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return encryptedRecipient{}, err
	}
	shared, err := eph.ECDH(recipient)
	if err != nil {
		return encryptedRecipient{}, err
	}
	key, err := wrapKey(shared, eph.PublicKey().Bytes(), recipient.Bytes())
	if err != nil {
		return encryptedRecipient{}, err
	}
	// 每个包装密钥只用一次（临时密钥随机），固定 nonce 是安全的
	wrapped, err := gcmSeal(key, make([]byte, 12), fileKey)
	if err != nil {
		return encryptedRecipient{}, err
	}
	return encryptedRecipient{
		EphemeralKey: base64.StdEncoding.EncodeToString(eph.PublicKey().Bytes()),
		WrappedKey:   base64.StdEncoding.EncodeToString(wrapped),
	}, nil
}

// unwrapFileKey 用 identity 解开 wrapFileKey 包装的内容密钥；不是给这个 identity 的返回错误。
func unwrapFileKey(recipient encryptedRecipient, identity *ecdh.PrivateKey) ([]byte, error) {
	ephRaw, err := base64.StdEncoding.DecodeString(recipient.EphemeralKey)
	if err != nil {
		return nil, err
	}
	ephPub, err := ecdh.X25519().NewPublicKey(ephRaw)
	if err != nil {
		return nil, err
	}
	shared, err := identity.ECDH(ephPub)
	if err != nil {
		return nil, err
	}
	key, err := wrapKey(shared, ephRaw, identity.PublicKey().Bytes())
	if err != nil {
		return nil, err
	}
	wrapped, err := base64.StdEncoding.DecodeString(recipient.WrappedKey)
	if err != nil {
		return nil, err
	}
	return gcmOpen(key, make([]byte, 12), wrapped)
}

// Encrypt 用随机文件密钥（AES-256-GCM）加密数据，文件密钥对每个接收方用
// X25519 ECDH + HKDF-SHA256 派生的密钥包装。
func Encrypt(plain []byte, recipients []*ecdh.PublicKey) ([]byte, error) {
	fileKey, err := newFileKey()
	if err != nil {
		return nil, err
	}
	out := encryptedFile{Version: encryptVersion}
	for _, recipient := range recipients {
		wrapped, err := wrapFileKey(fileKey, recipient)
		if err != nil {
			return nil, err
		}
		out.Recipients = append(out.Recipients, wrapped)
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	sealed, err := gcmSeal(fileKey, nonce, plain)
	if err != nil {
		return nil, err
	}
	out.Nonce = base64.StdEncoding.EncodeToString(nonce)
	out.Data = base64.StdEncoding.EncodeToString(sealed)
	return json.Marshal(out)
}

// Decrypt 用 identity 解开 Encrypt 产生的数据。
func Decrypt(data []byte, identity *ecdh.PrivateKey) ([]byte, error) {
	if identity == nil {
		return nil, errors.New("channel content is encrypted but no decrypt identity is configured")
	}
	file := encryptedFile{}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("invalid encrypted file, Error:%s", err)
	}
	if file.Version != encryptVersion {
		return nil, fmt.Errorf("unsupported encrypted file version [%d]", file.Version)
	}
	var fileKey []byte
	for _, recipient := range file.Recipients {
		if opened, err := unwrapFileKey(recipient, identity); err == nil {
			fileKey = opened
			break
		}
	}
	if fileKey == nil {
		return nil, errors.New("channel content is not encrypted for this identity")
	}
	nonce, err := base64.StdEncoding.DecodeString(file.Nonce)
	if err != nil {
		return nil, fmt.Errorf("invalid nonce in encrypted file, Error:%s", err)
	}
	sealed, err := base64.StdEncoding.DecodeString(file.Data)
	if err != nil {
		return nil, fmt.Errorf("invalid data in encrypted file, Error:%s", err)
	}
	return gcmOpen(fileKey, nonce, sealed)
}

// sealPart 用内容密钥加密一个 history 分片：随机 nonce（12 字节）+ AES-256-GCM 密文，二进制，不再套 base64。
// aad 绑定分片身份（来源、history id、序号），分片之间不能互换。
func sealPart(fileKey []byte, aad []byte, plain []byte) ([]byte, error) {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(fileKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plain, aad), nil
}

// openPart 解开 sealPart 产生的分片。
func openPart(fileKey []byte, aad []byte, data []byte) ([]byte, error) {
	if len(data) < 12 {
		return nil, errors.New("history part is too short")
	}
	block, err := aes.NewCipher(fileKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, data[:12], data[12:], aad)
}
