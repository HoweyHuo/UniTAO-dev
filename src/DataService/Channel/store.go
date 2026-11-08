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
//	<folder>/manifest.json                      publisher 写：公钥、lastSeq、queue 与 history 列表及各文件 sha256
//	<folder>/manifest.sig                       manifest.json 的 ed25519 签名（base64）
//	<folder>/queue/<from>-<to>.jsonl.gz.enc     增量 batch，对所有 subscriber 加密
//	<folder>/history/<id>-<seq>-<req>.jsonl.gz.enc   给一个 subscriber 的全量 batch，只对它加密
//	<folder>/subscribers/<id>.json              subscriber 自己写的进度（不在 manifest 中）
//
// manifest 签名覆盖 queue 与 history 文件的 sha256，因此只需校验 manifest 签名与文件摘要。
const (
	manifestFile    = "manifest.json"
	manifestSigFile = "manifest.sig"
	queueDir        = "queue"
	historyDir      = "history"
	subscriberDir   = "subscribers"
	manifestVersion = 2
	fileSuffix      = ".jsonl.gz.enc"
	writeRetry      = 5
)

// 旧格式（version 1）留下的目录，Reset 时删除
var oldFormatDirs = []string{"snapshots", "segments"}

// errOldFormat 表示通道上是旧格式的 manifest。
var errOldFormat = errors.New("channel holds an old-format manifest")

type FileRef struct {
	From       uint64 `json:"from,omitempty"`
	To         uint64 `json:"to,omitempty"`
	Seq        uint64 `json:"seq,omitempty"`
	Subscriber string `json:"subscriber,omitempty"`
	Request    string `json:"request,omitempty"`
	File       string `json:"file"`
	Sha256     string `json:"sha256"`
	Published  string `json:"published"`
}

type Manifest struct {
	Version     int       `json:"version"`
	Source      string    `json:"source"`
	PublicKey   string    `json:"publicKey"`
	Fingerprint string    `json:"fingerprint"`
	LastSeq     uint64    `json:"lastSeq"`
	Queue       []FileRef `json:"queue"`
	Histories   []FileRef `json:"histories"`
	Updated     string    `json:"updated"`
}

type historyHeader struct {
	Seq        uint64 `json:"seq"`
	Source     string `json:"source"`
	Subscriber string `json:"subscriber"`
	Request    string `json:"request"`
	Ts         string `json:"ts"`
	Count      int    `json:"count"`
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
		Histories:   []FileRef{},
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
	view := &View{Queue: []FileRef{}, Histories: []FileRef{}}
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
	return s.write(fmt.Sprintf("%s: publish seq %d-%d", s.folder, from, to), func() (bool, error) {
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

func (s *fileStore) PublishHistory(subscriber string, request string, h *History) error {
	recipient, ok := s.recipients[subscriber]
	if !ok {
		return fmt.Errorf("subscriber [%s] is not in the recipients list", subscriber)
	}
	return s.write(fmt.Sprintf("%s: history seq %d for subscriber %s", s.folder, h.Seq, shortId(subscriber)), func() (bool, error) {
		m, err := s.ownManifest()
		if err != nil {
			return false, err
		}
		if h.Seq > m.LastSeq {
			return false, fmt.Errorf("history seq [%d] is beyond published seq [%d]", h.Seq, m.LastSeq)
		}
		var buf bytes.Buffer
		header, _ := json.Marshal(historyHeader{Seq: h.Seq, Source: s.source, Subscriber: subscriber, Request: request, Ts: h.Ts, Count: len(h.Records)})
		buf.Write(header)
		buf.WriteByte('\n')
		for _, record := range h.Records {
			line, err := json.Marshal(record)
			if err != nil {
				return false, err
			}
			buf.Write(line)
			buf.WriteByte('\n')
		}
		data, err := s.encode(buf.Bytes(), []*ecdh.PublicKey{recipient})
		if err != nil {
			return false, err
		}
		name := fmt.Sprintf("%s/%s-%020d-%s%s", historyDir, subscriber, h.Seq, shortId(request), fileSuffix)
		if err := writeFileAtomic(s.path(name), data); err != nil {
			return false, err
		}
		kept := []FileRef{}
		for _, ref := range m.Histories {
			if ref.Subscriber == subscriber {
				if ref.File != name {
					if err := removeFile(s.path(ref.File)); err != nil {
						return false, err
					}
				}
				continue
			}
			kept = append(kept, ref)
		}
		m.Histories = append(kept, FileRef{Seq: h.Seq, Subscriber: subscriber, Request: request, File: name, Sha256: sha256Hex(data), Published: now()})
		return true, s.writeManifest(m)
	})
}

func (s *fileStore) Prune(p Prune) error {
	return s.write(fmt.Sprintf("%s: prune queue up to seq %d", s.folder, p.QueueUpTo), func() (bool, error) {
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
		for _, file := range p.Histories {
			drop[file] = true
		}
		keptHistories := []FileRef{}
		for _, ref := range m.Histories {
			if drop[ref.File] {
				if err := removeFile(s.path(ref.File)); err != nil {
					return false, err
				}
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
}

func (s *fileStore) Reset(lastSeq uint64) error {
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

func (s *fileStore) History(request string) (*History, error) {
	m, err := s.verifiedManifest()
	if err != nil {
		return nil, err
	}
	var ref *FileRef
	for idx := range m.Histories {
		if m.Histories[idx].Subscriber == s.selfId && m.Histories[idx].Request == request {
			ref = &m.Histories[idx]
		}
	}
	if ref == nil {
		return nil, ErrNoHistory
	}
	s.be.Lock()
	plain, err := s.decode(*ref)
	s.be.Unlock()
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(plain))
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	h := &History{Records: []map[string]interface{}{}}
	first := true
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if first {
			header := historyHeader{}
			if err := json.Unmarshal(line, &header); err != nil {
				return nil, fmt.Errorf("invalid history header, Error:%s", err)
			}
			if header.Seq != ref.Seq || header.Source != s.source || header.Subscriber != s.selfId || header.Request != request {
				return nil, fmt.Errorf("history header does not match manifest entry [%s]", ref.File)
			}
			h.Seq = header.Seq
			h.Source = header.Source
			h.Ts = header.Ts
			first = false
			continue
		}
		record := map[string]interface{}{}
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("invalid history record, Error:%s", err)
		}
		h.Records = append(h.Records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if first {
		return nil, errors.New("history file is empty")
	}
	return h, nil
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
