package Channel

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 通道目录布局（dir 与 git 通道相同），<folder> 为 vDS 在通道上的目录：
//
//	<folder>/manifest.json                      publisher 写：公钥、lastSeq、queue 与 history 列表、各文件 sha256、history 授权
//	<folder>/manifest.sig                       manifest.json 的 ed25519 签名（base64）
//	<folder>/queue/<from>-<to>.jsonl.gz.enc     增量 batch，对所有 subscriber 加密
//	<folder>/history/<hid>/<n>.jsonl.gz.aes     全量 history 的第 n 个分片，用这份 history 的内容密钥加密
//	<folder>/subscribers/<id>.json              subscriber 自己写的进度（不在 manifest 中）
//
// manifest 签名覆盖 queue 与 history 分片的 sha256 以及 history 的授权，因此只需校验 manifest 签名与文件摘要。
// history 的内容密钥在 manifest 的 grants 里为每个被授权的 subscriber 各包装一份：给新订阅方复用已有的
// history 只需要在 manifest 里加一条授权，不重写分片。
const (
	manifestFile    = "manifest.json"
	manifestSigFile = "manifest.sig"
	queueDir        = "queue"
	historyDir      = "history"
	subscriberDir   = "subscribers"
	manifestVersion = 3
	fileSuffix      = ".jsonl.gz.enc"
	partSuffix      = ".jsonl.gz.aes"
	writeRetry      = 5
	partAADPrefix   = "UniTAO-history-v1"

	// DefaultHistoryPartBytes 是 history 分片的目标大小（gzip 之后）。远低于 GitHub 等托管方的单文件上限（100MB）。
	DefaultHistoryPartBytes = 32 << 20
)

// historyPartBytes 是当前的分片目标大小，测试可以用 SetHistoryPartBytes 调小。
var historyPartBytes = DefaultHistoryPartBytes

// SetHistoryPartBytes 替换 history 分片的目标大小（测试用），返回恢复原值的函数。
func SetHistoryPartBytes(n int) func() {
	old := historyPartBytes
	historyPartBytes = n
	return func() { historyPartBytes = old }
}

// 旧格式（version 1）留下的目录，Reset 时删除
var oldFormatDirs = []string{"snapshots", "segments"}

// errOldFormat 表示通道上是旧格式的 manifest。
var errOldFormat = errors.New("channel holds an old-format manifest")

// FileRef 是 manifest 里的一个 queue batch。
type FileRef struct {
	From      uint64 `json:"from"`
	To        uint64 `json:"to"`
	File      string `json:"file"`
	Sha256    string `json:"sha256"`
	Published string `json:"published"`
}

// HistoryRef 是 manifest 里的一份 history：seq 时刻的全量，切成若干分片，授权给若干订阅请求。
type HistoryRef struct {
	Id        string     `json:"id"`
	Seq       uint64     `json:"seq"`
	Records   int        `json:"records"`
	Parts     []PartRef  `json:"parts"`
	Published string     `json:"published"`
	Grants    []GrantRef `json:"grants"`
}

// PartRef 是 history 的一个分片。
type PartRef struct {
	File   string `json:"file"`
	Sha256 string `json:"sha256"`
	Size   int    `json:"size"`
}

// GrantRef 是一条授权：history 的内容密钥为 subscriber 包装的一份（同 Encrypt 的接收方格式）。
type GrantRef struct {
	Subscriber   string `json:"subscriber"`
	Request      string `json:"request"`
	EphemeralKey string `json:"epk"`
	WrappedKey   string `json:"key"`
	Granted      string `json:"granted"`
}

// Grant 返回给 subscriber 这个订阅请求的授权，没有时返回 nil。
func (h *HistoryRef) Grant(subscriber string, request string) *GrantRef {
	for idx := range h.Grants {
		if h.Grants[idx].Subscriber == subscriber && h.Grants[idx].Request == request {
			return &h.Grants[idx]
		}
	}
	return nil
}

type Manifest struct {
	Version     int          `json:"version"`
	Source      string       `json:"source"`
	PublicKey   string       `json:"publicKey"`
	Fingerprint string       `json:"fingerprint"`
	LastSeq     uint64       `json:"lastSeq"`
	Queue       []FileRef    `json:"queue"`
	Histories   []HistoryRef `json:"histories"`
	Updated     string       `json:"updated"`
}

// backend 隔离存储方式：dir 直接读写目录；git 在读之前拉取、写之后提交推送。
// 同一个 backend 可能被多个 vDS 共用（同一个 git 工作目录），所有文件访问都要持有它的锁。
type backend interface {
	Lock()
	Unlock()
	// Pull 在读之前拿到远端最新内容。
	Pull() error
	// Prepare 在写之前把工作目录对齐到远端最新内容，丢弃本地未推送的修改。
	Prepare() error
	// Commit 提交 folder 下的修改并推送；远端在此期间有变化时返回 errRetry，调用方重做整个写入。
	Commit(folder string, message string) error
	// Compact 用当前文件树替换整个历史；没有可压缩的历史时返回 false。
	Compact(message string) (bool, error)
}

// errRetry 表示推送被拒（远端有新提交），需要重新对齐后重做写入。
var errRetry = errors.New("remote changed while writing, retry")

type dirBackend struct {
	mu sync.Mutex
}

func newDirBackend() *dirBackend                             { return &dirBackend{} }
func (d *dirBackend) Lock()                                  { d.mu.Lock() }
func (d *dirBackend) Unlock()                                { d.mu.Unlock() }
func (d *dirBackend) Pull() error                            { return nil }
func (d *dirBackend) Prepare() error                         { return nil }
func (d *dirBackend) Commit(folder string, msg string) error { return nil }
func (d *dirBackend) Compact(message string) (bool, error)   { return false, nil }

// fileStore 在一个目录上实现 Publisher 与 Subscriber。
type fileStore struct {
	be     backend
	root   string
	folder string
	source string
	log    *log.Logger
	// publisher
	signer     ed25519.PrivateKey
	recipients map[string]*ecdh.PublicKey // subscriber id -> 公钥
	// 本进程打包的 history 的内容密钥（history id -> 密钥），只在内存里；重启后旧 history 不能再增加授权
	keyLock     sync.Mutex
	historyKeys map[string][]byte
	// subscriber
	identity *ecdh.PrivateKey
	selfId   string
	trusted  string
	// subscriber 最近一次 Refresh 的结果
	lock     sync.Mutex
	manifest *Manifest
	progress *Progress
}

func newFileStore(be backend, root string, folder string, source string, logger *log.Logger) *fileStore {
	return &fileStore{be: be, root: root, folder: folder, source: source, log: logger}
}

func (s *fileStore) dir() string {
	return filepath.Join(s.root, filepath.FromSlash(s.folder))
}

func (s *fileStore) path(rel string) string {
	return filepath.Join(s.dir(), filepath.FromSlash(rel))
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// write 在 backend 锁内执行一次写入：对齐远端 → fn 修改文件 → 提交推送；推送被拒时从头重做。
// fn 返回 false 表示没有需要写的内容。
func (s *fileStore) write(message string, fn func() (bool, error)) error {
	s.be.Lock()
	defer s.be.Unlock()
	for attempt := 0; attempt < writeRetry; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 300 * time.Millisecond)
		}
		if err := s.be.Prepare(); err != nil {
			return err
		}
		changed, err := fn()
		if err != nil || !changed {
			return err
		}
		err = s.be.Commit(s.folder, message)
		if errors.Is(err, errRetry) {
			s.log.Printf("Channel[%s]: %s, attempt %d", s.folder, err, attempt+1)
			continue
		}
		return err
	}
	return fmt.Errorf("channel [%s]: gave up writing after %d attempts, the remote keeps changing", s.folder, writeRetry)
}

// readManifest 读取并校验 manifest：版本正确、签名有效、来源一致；trusted 非空时公钥指纹必须等于它。
func (s *fileStore) readManifest(trusted string) (*Manifest, error) {
	data, err := os.ReadFile(s.path(manifestFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotPublished
		}
		return nil, err
	}
	m := Manifest{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("invalid manifest in [%s], Error:%s", s.folder, err)
	}
	if m.Version != manifestVersion {
		return nil, errOldFormat
	}
	if m.Source != s.source {
		return nil, fmt.Errorf("manifest in [%s] belongs to source [%s], expect [%s]", s.folder, m.Source, s.source)
	}
	pubRaw, err := base64.StdEncoding.DecodeString(m.PublicKey)
	if err != nil || len(pubRaw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid public key in manifest of source [%s]", s.source)
	}
	pub := ed25519.PublicKey(pubRaw)
	fingerprint := Fingerprint(pub)
	if trusted != "" && fingerprint != trusted {
		return nil, fmt.Errorf("source [%s] public key fingerprint [%s] does not match trusted fingerprint [%s]", s.source, fingerprint, trusted)
	}
	sigData, err := os.ReadFile(s.path(manifestSigFile))
	if err != nil {
		return nil, fmt.Errorf("manifest signature of source [%s] missing, Error:%s", s.source, err)
	}
	sig, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(sigData)))
	if err != nil {
		return nil, fmt.Errorf("invalid manifest signature of source [%s], Error:%s", s.source, err)
	}
	if !ed25519.Verify(pub, data, sig) {
		return nil, fmt.Errorf("manifest signature of source [%s] verification failed", s.source)
	}
	m.Fingerprint = fingerprint
	sort.Slice(m.Queue, func(i, j int) bool { return m.Queue[i].From < m.Queue[j].From })
	return &m, nil
}

func (s *fileStore) writeManifest(m *Manifest) error {
	m.Updated = now()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	sig := ed25519.Sign(s.signer, data)
	if err := writeFileAtomic(s.path(manifestFile), data); err != nil {
		return err
	}
	return writeFileAtomic(s.path(manifestSigFile), []byte(base64.StdEncoding.EncodeToString(sig)))
}

func (s *fileStore) newManifest(lastSeq uint64) *Manifest {
	pub := s.signer.Public().(ed25519.PublicKey)
	return &Manifest{
		Version:     manifestVersion,
		Source:      s.source,
		PublicKey:   base64.StdEncoding.EncodeToString(pub),
		Fingerprint: Fingerprint(pub),
		LastSeq:     lastSeq,
		Queue:       []FileRef{},
		Histories:   []HistoryRef{},
	}
}

// loadOwnManifest 读取自己发布过的 manifest；不存在时新建。已存在但公钥不同说明同名来源冲突。
// 旧格式返回 errOldFormat。
func (s *fileStore) loadOwnManifest() (*Manifest, bool, error) {
	pub := s.signer.Public().(ed25519.PublicKey)
	m, err := s.readManifest("")
	if err != nil {
		if errors.Is(err, ErrNotPublished) {
			return s.newManifest(0), false, nil
		}
		return nil, false, err
	}
	if m.Fingerprint != Fingerprint(pub) {
		return nil, false, fmt.Errorf("channel folder [%s] already holds source [%s] signed by another key [%s], refuse to overwrite", s.folder, s.source, m.Fingerprint)
	}
	return m, true, nil
}

func (s *fileStore) encode(plain []byte, recipients []*ecdh.PublicKey) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(plain); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return Encrypt(buf.Bytes(), recipients)
}

func (s *fileStore) decode(ref FileRef) ([]byte, error) {
	data, err := os.ReadFile(s.path(ref.File))
	if err != nil {
		return nil, fmt.Errorf("failed to read channel file [%s], Error:%s", ref.File, err)
	}
	if sha256Hex(data) != ref.Sha256 {
		return nil, fmt.Errorf("channel file [%s] digest does not match manifest", ref.File)
	}
	data, err = Decrypt(data, s.identity)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt channel file [%s], Error:%s", ref.File, err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var out bytes.Buffer
	if _, err := out.ReadFrom(zr); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (s *fileStore) allRecipients() []*ecdh.PublicKey {
	ids := make([]string, 0, len(s.recipients))
	for id := range s.recipients {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := []*ecdh.PublicKey{}
	for _, id := range ids {
		result = append(result, s.recipients[id])
	}
	return result
}

// readSubscribers 读取 subscribers/ 下所有进度文件。文件名就是 id，内容里的 id 不一致的忽略。
func (s *fileStore) readSubscribers() ([]Progress, error) {
	entries, err := os.ReadDir(s.path(subscriberDir))
	if err != nil {
		if os.IsNotExist(err) {
			return []Progress{}, nil
		}
		return nil, err
	}
	result := []Progress{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		p, err := s.readProgress(id)
		if err != nil {
			s.log.Printf("Channel[%s]: ignore subscriber file [%s], Error:%s", s.folder, name, err)
			continue
		}
		if p != nil {
			result = append(result, *p)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Id < result[j].Id })
	return result, nil
}

func (s *fileStore) readProgress(id string) (*Progress, error) {
	data, err := os.ReadFile(s.path(subscriberDir + "/" + id + ".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	p := Progress{}
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	if p.Id != id {
		return nil, fmt.Errorf("subscriber id [%s] in file does not match file name", p.Id)
	}
	return &p, nil
}

// ---------- Publisher ----------

func (s *fileStore) RecipientCount() int {
	return len(s.recipients)
}

func (s *fileStore) Trusted(subscriber string) bool {
	_, ok := s.recipients[subscriber]
	return ok
}

func (s *fileStore) View() (*View, error) {
	s.be.Lock()
	defer s.be.Unlock()
	if err := s.be.Pull(); err != nil {
		return nil, err
	}
	view := &View{Queue: []FileRef{}, Histories: []HistoryRef{}}
	m, published, err := s.loadOwnManifest()
	switch {
	case errors.Is(err, errOldFormat):
		view.OldFormat = true
	case err != nil:
		return nil, err
	default:
		view.Published = published
		view.LastSeq = m.LastSeq
		view.Queue = m.Queue
		view.Histories = m.Histories
	}
	subscribers, err := s.readSubscribers()
	if err != nil {
		return nil, err
	}
	view.Subscribers = subscribers
	return view, nil
}

// ownManifest 在写入中读取自己的 manifest；旧格式时报错（应先 Reset）。
func (s *fileStore) ownManifest() (*Manifest, error) {
	m, _, err := s.loadOwnManifest()
	if errors.Is(err, errOldFormat) {
		return nil, fmt.Errorf("channel folder [%s] holds an old-format manifest, reset it first", s.folder)
	}
	return m, err
}

func (s *fileStore) Publish(changes []Change) error {
	if len(changes) == 0 {
		return nil
	}
	from := changes[0].Seq
	to := changes[len(changes)-1].Seq
	message := fmt.Sprintf("%s: publish seq %d-%d", s.folder, from, to)
	if len(s.recipients) == 0 {
		message = fmt.Sprintf("%s: advance to seq %d (no recipients)", s.folder, to)
	}
	return s.write(message, func() (bool, error) {
		m, err := s.ownManifest()
		if err != nil {
			return false, err
		}
		if from != m.LastSeq+1 {
			return false, fmt.Errorf("changes to publish must continue from seq [%d], got [%d]", m.LastSeq+1, from)
		}
		var buf bytes.Buffer
		for idx, change := range changes {
			if idx > 0 && change.Seq != changes[idx-1].Seq+1 {
				return false, fmt.Errorf("changes to publish are not continuous: seq [%d] after [%d]", change.Seq, changes[idx-1].Seq)
			}
			line, err := json.Marshal(change)
			if err != nil {
				return false, err
			}
			buf.Write(line)
			buf.WriteByte('\n')
		}
		if len(s.recipients) == 0 {
			// 没有接收方：不写 queue batch，只推进 LastSeq（见 Publisher.Publish 的说明）
			m.LastSeq = to
			return true, s.writeManifest(m)
		}
		data, err := s.encode(buf.Bytes(), s.allRecipients())
		if err != nil {
			return false, err
		}
		name := fmt.Sprintf("%s/%020d-%020d%s", queueDir, from, to, fileSuffix)
		if err := writeFileAtomic(s.path(name), data); err != nil {
			return false, err
		}
		m.Queue = append(m.Queue, FileRef{From: from, To: to, File: name, Sha256: sha256Hex(data), Published: now()})
		m.LastSeq = to
		return true, s.writeManifest(m)
	})
}

// spooledHistory 是打包好、暂存在本地临时目录里的 history 分片。临时目录不在通道的工作目录里：
// git 写入前对齐远端时会清掉未跟踪的文件，推送被拒重做时要能再写一遍。
type spooledHistory struct {
	dir     string
	parts   []spooledPart
	records int
}

type spooledPart struct {
	name   string // 通道上的路径
	spool  string // 临时文件
	sha256 string
	size   int
}

func (h *spooledHistory) cleanup() {
	os.RemoveAll(h.dir)
}

func partAAD(source string, id string, index int) []byte {
	return []byte(fmt.Sprintf("%s/%s/%s/%d", partAADPrefix, source, id, index))
}

// encodeHistory 边读记录边写成 JSONL、gzip，按压缩后的大小切片（达到 historyPartBytes 就开始新的分片，
// deflate 有内部缓冲，分片会略大于目标），每片用内容密钥单独加密后写进临时目录。内存里最多只有一个分片。
// 没有记录时也有一个（空的）分片。
func (s *fileStore) encodeHistory(id string, key []byte, records func(fn func(record map[string]interface{}) error) error) (*spooledHistory, error) {
	dir, err := os.MkdirTemp("", "unitao-history-")
	if err != nil {
		return nil, fmt.Errorf("failed to create spool directory for history, Error:%s", err)
	}
	spooled := &spooledHistory{dir: dir}
	done := false
	defer func() {
		if !done {
			spooled.cleanup()
		}
	}()
	var buf bytes.Buffer
	var zw *gzip.Writer
	pending := 0
	flush := func() error {
		if zw == nil {
			zw = gzip.NewWriter(&buf)
		}
		if err := zw.Close(); err != nil {
			return err
		}
		index := len(spooled.parts) + 1
		sealed, err := sealPart(key, partAAD(s.source, id, index), buf.Bytes())
		if err != nil {
			return err
		}
		buf.Reset()
		zw = nil
		pending = 0
		spool := filepath.Join(dir, fmt.Sprintf("%04d", index))
		if err := os.WriteFile(spool, sealed, 0o600); err != nil {
			return fmt.Errorf("failed to spool history part, Error:%s", err)
		}
		spooled.parts = append(spooled.parts, spooledPart{
			name:   fmt.Sprintf("%s/%s/%04d%s", historyDir, id, index, partSuffix),
			spool:  spool,
			sha256: sha256Hex(sealed),
			size:   len(sealed),
		})
		return nil
	}
	err = records(func(record map[string]interface{}) error {
		if zw == nil {
			zw = gzip.NewWriter(&buf)
		}
		line, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if _, err := zw.Write(append(line, '\n')); err != nil {
			return err
		}
		pending++
		spooled.records++
		if buf.Len() >= historyPartBytes {
			return flush()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if pending > 0 || len(spooled.parts) == 0 {
		if err := flush(); err != nil {
			return nil, err
		}
	}
	done = true
	return spooled, nil
}

// copyFileAtomic 把 src 流式复制到 dst（先写临时文件再改名）。
func copyFileAtomic(src string, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// makeGrants 为每个授权包装内容密钥；subscriber 必须在白名单里。
func (s *fileStore) makeGrants(key []byte, grants []HistoryGrant) ([]GrantRef, error) {
	result := []GrantRef{}
	for _, grant := range grants {
		recipient, ok := s.recipients[grant.Subscriber]
		if !ok {
			return nil, fmt.Errorf("subscriber [%s] is not in the recipients list", grant.Subscriber)
		}
		if grant.Request == "" {
			return nil, fmt.Errorf("grant for subscriber [%s] has no request id", grant.Subscriber)
		}
		wrapped, err := wrapFileKey(key, recipient)
		if err != nil {
			return nil, err
		}
		result = append(result, GrantRef{
			Subscriber:   grant.Subscriber,
			Request:      grant.Request,
			EphemeralKey: wrapped.EphemeralKey,
			WrappedKey:   wrapped.WrappedKey,
			Granted:      now(),
		})
	}
	return result, nil
}

func (s *fileStore) historyKey(id string) []byte {
	s.keyLock.Lock()
	defer s.keyLock.Unlock()
	return s.historyKeys[id]
}

func (s *fileStore) setHistoryKey(id string, key []byte) {
	s.keyLock.Lock()
	defer s.keyLock.Unlock()
	if key == nil {
		delete(s.historyKeys, id)
		return
	}
	if s.historyKeys == nil {
		s.historyKeys = map[string][]byte{}
	}
	s.historyKeys[id] = key
}

func (s *fileStore) PublishHistory(h *History, grants []HistoryGrant) (*HistoryRef, error) {
	if len(grants) == 0 {
		return nil, errors.New("a history needs at least one grant")
	}
	key, err := newFileKey()
	if err != nil {
		return nil, err
	}
	refs, err := s.makeGrants(key, grants)
	if err != nil {
		return nil, err
	}
	id := NewRequestId()
	// 读记录、压缩、加密在锁外做一次，分片暂存在临时目录；推送被拒重做时只重新复制文件
	spooled, err := s.encodeHistory(id, key, h.Records)
	if err != nil {
		return nil, err
	}
	defer spooled.cleanup()
	var result HistoryRef
	message := fmt.Sprintf("%s: history %s seq %d (%d parts) for %d subscribers", s.folder, shortId(id), h.Seq, len(spooled.parts), len(refs))
	err = s.write(message, func() (bool, error) {
		m, err := s.ownManifest()
		if err != nil {
			return false, err
		}
		if h.Seq > m.LastSeq {
			return false, fmt.Errorf("history seq [%d] is beyond published seq [%d]", h.Seq, m.LastSeq)
		}
		ref := HistoryRef{Id: id, Seq: h.Seq, Records: spooled.records, Parts: []PartRef{}, Published: now(), Grants: refs}
		for _, part := range spooled.parts {
			if err := copyFileAtomic(part.spool, s.path(part.name)); err != nil {
				return false, err
			}
			ref.Parts = append(ref.Parts, PartRef{File: part.name, Sha256: part.sha256, Size: part.size})
		}
		m.Histories = append(m.Histories, ref)
		result = ref
		return true, s.writeManifest(m)
	})
	if err != nil {
		return nil, err
	}
	s.setHistoryKey(id, key)
	return &result, nil
}

func (s *fileStore) CanGrant(id string) bool {
	return s.historyKey(id) != nil
}

func (s *fileStore) GrantHistory(id string, grants []HistoryGrant) (*HistoryRef, error) {
	key := s.historyKey(id)
	if key == nil {
		return nil, ErrHistoryKeyUnknown
	}
	refs, err := s.makeGrants(key, grants)
	if err != nil {
		return nil, err
	}
	var result HistoryRef
	err = s.write(fmt.Sprintf("%s: grant history %s to %d subscribers", s.folder, shortId(id), len(refs)), func() (bool, error) {
		m, err := s.ownManifest()
		if err != nil {
			return false, err
		}
		for idx := range m.Histories {
			if m.Histories[idx].Id != id {
				continue
			}
			granted := map[string]bool{}
			for _, ref := range refs {
				granted[ref.Subscriber] = true
			}
			kept := []GrantRef{}
			for _, ref := range m.Histories[idx].Grants {
				if !granted[ref.Subscriber] {
					kept = append(kept, ref)
				}
			}
			m.Histories[idx].Grants = append(kept, refs...)
			result = m.Histories[idx]
			return true, s.writeManifest(m)
		}
		return false, fmt.Errorf("history [%s] is no longer on the channel", id)
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func (s *fileStore) Prune(p Prune) error {
	parts := []string{fmt.Sprintf("queue up to seq %d", p.QueueUpTo)}
	if len(p.Histories) > 0 {
		parts = append(parts, fmt.Sprintf("%d histories", len(p.Histories)))
	}
	if len(p.Subscribers) > 0 {
		parts = append(parts, fmt.Sprintf("%d dead subscribers", len(p.Subscribers)))
	}
	err := s.write(fmt.Sprintf("%s: prune %s", s.folder, strings.Join(parts, ", ")), func() (bool, error) {
		m, err := s.ownManifest()
		if err != nil {
			return false, err
		}
		changed := false
		keptQueue := []FileRef{}
		for _, ref := range m.Queue {
			if ref.To <= p.QueueUpTo {
				if err := removeFile(s.path(ref.File)); err != nil {
					return false, err
				}
				changed = true
				continue
			}
			keptQueue = append(keptQueue, ref)
		}
		drop := map[string]bool{}
		for _, id := range p.Histories {
			drop[id] = true
		}
		keptHistories := []HistoryRef{}
		for _, ref := range m.Histories {
			if drop[ref.Id] {
				for _, part := range ref.Parts {
					if err := removeFile(s.path(part.File)); err != nil {
						return false, err
					}
				}
				os.Remove(s.path(historyDir + "/" + ref.Id)) // 空目录（git 不跟踪目录，dir 通道要删掉）
				changed = true
				continue
			}
			keptHistories = append(keptHistories, ref)
		}
		for _, id := range p.Subscribers {
			file := s.path(subscriberDir + "/" + id + ".json")
			if _, err := os.Stat(file); err == nil {
				if err := removeFile(file); err != nil {
					return false, err
				}
				changed = true
			}
		}
		if !changed {
			return false, nil
		}
		m.Queue = keptQueue
		m.Histories = keptHistories
		return true, s.writeManifest(m)
	})
	if err == nil {
		for _, id := range p.Histories {
			s.setHistoryKey(id, nil)
		}
	}
	return err
}

func (s *fileStore) Reset(lastSeq uint64) error {
	s.keyLock.Lock()
	s.historyKeys = nil
	s.keyLock.Unlock()
	return s.write(fmt.Sprintf("%s: reset at seq %d", s.folder, lastSeq), func() (bool, error) {
		if m, published, err := s.loadOwnManifest(); err == nil && published && m.LastSeq == lastSeq && len(m.Queue) == 0 && len(m.Histories) == 0 {
			return false, nil
		} else if err != nil && !errors.Is(err, errOldFormat) {
			return false, err
		}
		for _, dir := range append([]string{queueDir, historyDir}, oldFormatDirs...) {
			if err := os.RemoveAll(s.path(dir)); err != nil {
				return false, err
			}
		}
		return true, s.writeManifest(s.newManifest(lastSeq))
	})
}

func (s *fileStore) Compact() (bool, error) {
	s.be.Lock()
	defer s.be.Unlock()
	return s.be.Compact(fmt.Sprintf("compact history (by %s)", s.folder))
}

// ---------- Subscriber ----------

func (s *fileStore) Id() string {
	return s.selfId
}

func (s *fileStore) Refresh() error {
	s.be.Lock()
	defer s.be.Unlock()
	if err := s.be.Pull(); err != nil {
		return err
	}
	progress, err := s.readProgress(s.selfId)
	if err != nil {
		return fmt.Errorf("failed to read own progress file, Error:%s", err)
	}
	m, err := s.readManifest(s.trusted)
	s.lock.Lock()
	defer s.lock.Unlock()
	s.progress = progress
	if err != nil {
		s.manifest = nil
		if errors.Is(err, errOldFormat) {
			return fmt.Errorf("%w (old format)", ErrNotPublished)
		}
		return err
	}
	s.manifest = m
	return nil
}

func (s *fileStore) SourceFingerprint() string {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.manifest == nil {
		return ""
	}
	return s.manifest.Fingerprint
}

func (s *fileStore) LastSeq() uint64 {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.manifest == nil {
		return 0
	}
	return s.manifest.LastSeq
}

func (s *fileStore) Progress() *Progress {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.progress == nil {
		return nil
	}
	p := *s.progress
	return &p
}

func (s *fileStore) WriteProgress(p Progress) error {
	p.Id = s.selfId
	err := s.write(fmt.Sprintf("%s: subscriber %s %s cursor %d", s.folder, shortId(s.selfId), p.State, p.Cursor), func() (bool, error) {
		p.Updated = now()
		data, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			return false, err
		}
		return true, writeFileAtomic(s.path(subscriberDir+"/"+s.selfId+".json"), data)
	})
	if err == nil {
		s.lock.Lock()
		s.progress = &p
		s.lock.Unlock()
	}
	return err
}

func (s *fileStore) verifiedManifest() (*Manifest, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.manifest == nil {
		return nil, errors.New("channel not refreshed or source not verified")
	}
	return s.manifest, nil
}

func (s *fileStore) History(request string) (*HistoryReader, error) {
	m, err := s.verifiedManifest()
	if err != nil {
		return nil, err
	}
	var ref *HistoryRef
	var grant *GrantRef
	for idx := range m.Histories {
		if g := m.Histories[idx].Grant(s.selfId, request); g != nil && (ref == nil || m.Histories[idx].Seq > ref.Seq) {
			ref, grant = &m.Histories[idx], g
		}
	}
	if ref == nil {
		return nil, ErrNoHistory
	}
	if s.identity == nil {
		return nil, errors.New("channel content is encrypted but no decrypt identity is configured")
	}
	key, err := unwrapFileKey(encryptedRecipient{EphemeralKey: grant.EphemeralKey, WrappedKey: grant.WrappedKey}, s.identity)
	if err != nil {
		return nil, fmt.Errorf("grant of history [%s] cannot be opened with this identity, Error:%s", ref.Id, err)
	}
	history := *ref
	return &HistoryReader{
		Id:        history.Id,
		Seq:       history.Seq,
		Source:    m.Source,
		Published: history.Published,
		Records:   history.Records,
		Parts:     len(history.Parts),
		each: func(fn func(record map[string]interface{}) error) error {
			return s.readHistory(&history, key, fn)
		},
	}, nil
}

// readHistory 逐个分片校验摘要、解密、解压并逐行回调；最后核对记录数与 manifest 一致。
func (s *fileStore) readHistory(ref *HistoryRef, key []byte, fn func(record map[string]interface{}) error) error {
	count := 0
	for idx, part := range ref.Parts {
		s.be.Lock()
		data, err := os.ReadFile(s.path(part.File))
		s.be.Unlock()
		if err != nil {
			return fmt.Errorf("failed to read history part [%s], Error:%s", part.File, err)
		}
		if sha256Hex(data) != part.Sha256 {
			return fmt.Errorf("history part [%s] digest does not match manifest", part.File)
		}
		plain, err := openPart(key, partAAD(s.source, ref.Id, idx+1), data)
		if err != nil {
			return fmt.Errorf("failed to decrypt history part [%s], Error:%s", part.File, err)
		}
		zr, err := gzip.NewReader(bytes.NewReader(plain))
		if err != nil {
			return fmt.Errorf("invalid history part [%s], Error:%s", part.File, err)
		}
		scanner := bufio.NewScanner(zr)
		scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			record := map[string]interface{}{}
			if err := json.Unmarshal(line, &record); err != nil {
				zr.Close()
				return fmt.Errorf("invalid history record in [%s], Error:%s", part.File, err)
			}
			if err := fn(record); err != nil {
				zr.Close()
				return err
			}
			count++
		}
		zr.Close()
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("failed to read history part [%s], Error:%s", part.File, err)
		}
	}
	if count != ref.Records {
		return fmt.Errorf("history [%s] has %d records, manifest says %d", ref.Id, count, ref.Records)
	}
	return nil
}

func (s *fileStore) Fetch(cursor uint64, limit int) ([]Change, error) {
	m, err := s.verifiedManifest()
	if err != nil {
		return nil, err
	}
	if m.LastSeq <= cursor {
		return []Change{}, nil
	}
	batches := []FileRef{}
	for _, ref := range m.Queue {
		if ref.To > cursor {
			batches = append(batches, ref)
		}
	}
	if len(batches) == 0 || batches[0].From > cursor+1 {
		return nil, ErrGap
	}
	s.be.Lock()
	defer s.be.Unlock()
	expected := cursor + 1
	result := []Change{}
	for _, ref := range batches {
		if limit > 0 && len(result) >= limit {
			break
		}
		if ref.From > expected {
			break // 中间缺 batch：先返回已连续的部分，下次 Fetch 会得到 ErrGap
		}
		plain, err := s.decode(ref)
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(bytes.NewReader(plain))
		scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			change := Change{}
			if err := json.Unmarshal(line, &change); err != nil {
				return nil, fmt.Errorf("invalid change in [%s], Error:%s", ref.File, err)
			}
			if change.Seq < expected {
				continue
			}
			if change.Seq > expected {
				return nil, ErrGap
			}
			result = append(result, change)
			expected++
		}
		if err := scanner.Err(); err != nil {
			return nil, err
		}
	}
	if len(result) == 0 {
		return nil, ErrGap
	}
	return result, nil
}

func shortId(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
