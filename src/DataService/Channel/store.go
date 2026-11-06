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
	"sync"
	"time"
)

// 通道目录布局（dir 与 git 通道相同）：
//
//	<root>/<source>/manifest.json      公钥、最新 snapshot、segment 列表及各文件 sha256
//	<root>/<source>/manifest.sig       manifest.json 的 ed25519 签名（base64）
//	<root>/<source>/snapshots/<seq>.jsonl.gz[.enc]
//	<root>/<source>/segments/<from>-<to>.jsonl.gz[.enc]
//
// manifest 签名覆盖所有文件的 sha256，因此只需校验 manifest 签名与文件摘要。
const (
	manifestFile    = "manifest.json"
	manifestSigFile = "manifest.sig"
	snapshotDir     = "snapshots"
	segmentDir      = "segments"
	manifestVersion = 1
)

type FileRef struct {
	From      uint64 `json:"from,omitempty"`
	To        uint64 `json:"to,omitempty"`
	Seq       uint64 `json:"seq,omitempty"`
	File      string `json:"file"`
	Sha256    string `json:"sha256"`
	Encrypted bool   `json:"encrypted,omitempty"`
}

type Manifest struct {
	Version     int       `json:"version"`
	Source      string    `json:"source"`
	PublicKey   string    `json:"publicKey"`
	Fingerprint string    `json:"fingerprint"`
	Snapshot    *FileRef  `json:"snapshot,omitempty"`
	Segments    []FileRef `json:"segments"`
	Updated     string    `json:"updated"`
}

func (m *Manifest) maxSeq() uint64 {
	var max uint64
	if m.Snapshot != nil {
		max = m.Snapshot.Seq
	}
	for _, seg := range m.Segments {
		if seg.To > max {
			max = seg.To
		}
	}
	return max
}

type snapshotHeader struct {
	Seq    uint64 `json:"seq"`
	Source string `json:"source"`
	Ts     string `json:"ts"`
	Count  int    `json:"count"`
}

// fileStore 在一个目录上实现 Publisher 与 Subscriber。git 通道在其外层加上 pull / commit / push。
type fileStore struct {
	root       string
	source     string
	signer     ed25519.PrivateKey
	recipients []*ecdh.PublicKey
	identity   *ecdh.PrivateKey
	trusted    string
	log        *log.Logger
	lock       sync.Mutex
	manifest   *Manifest
	// git 通道的钩子：读之前同步远端，写之后提交推送
	beforeRead  func() error
	afterWrite  func(message string) error
	beforeWrite func() error
}

func newFileStore(root string, source string, signer ed25519.PrivateKey, recipients []*ecdh.PublicKey, identity *ecdh.PrivateKey, trusted string, logger *log.Logger) *fileStore {
	return &fileStore{
		root:       root,
		source:     source,
		signer:     signer,
		recipients: recipients,
		identity:   identity,
		trusted:    trusted,
		log:        logger,
	}
}

func (s *fileStore) dir() string {
	return filepath.Join(s.root, s.source)
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

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// readManifest 读取并校验 manifest：签名有效、来源一致；trusted 非空时公钥指纹必须等于它。
func (s *fileStore) readManifest(trusted string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(s.dir(), manifestFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotPublished
		}
		return nil, err
	}
	sigData, err := os.ReadFile(filepath.Join(s.dir(), manifestSigFile))
	if err != nil {
		return nil, fmt.Errorf("manifest signature of source [%s] missing, Error:%s", s.source, err)
	}
	m := Manifest{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("invalid manifest of source [%s], Error:%s", s.source, err)
	}
	if m.Source != s.source {
		return nil, fmt.Errorf("manifest source [%s] does not match expected source [%s]", m.Source, s.source)
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
	sig, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(sigData)))
	if err != nil {
		return nil, fmt.Errorf("invalid manifest signature of source [%s], Error:%s", s.source, err)
	}
	if !ed25519.Verify(pub, data, sig) {
		return nil, fmt.Errorf("manifest signature of source [%s] verification failed", s.source)
	}
	m.Fingerprint = fingerprint
	return &m, nil
}

func (s *fileStore) writeManifest(m *Manifest) error {
	m.Updated = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	sig := ed25519.Sign(s.signer, data)
	if err := writeFileAtomic(filepath.Join(s.dir(), manifestFile), data); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir(), manifestSigFile), []byte(base64.StdEncoding.EncodeToString(sig)))
}

// loadOwnManifest 读取自己发布过的 manifest；不存在则新建。已存在但公钥不同说明同名来源冲突。
func (s *fileStore) loadOwnManifest() (*Manifest, error) {
	pub := s.signer.Public().(ed25519.PublicKey)
	m, err := s.readManifest("")
	if err != nil {
		if errors.Is(err, ErrNotPublished) {
			return &Manifest{
				Version:     manifestVersion,
				Source:      s.source,
				PublicKey:   base64.StdEncoding.EncodeToString(pub),
				Fingerprint: Fingerprint(pub),
				Segments:    []FileRef{},
			}, nil
		}
		return nil, err
	}
	if m.Fingerprint != Fingerprint(pub) {
		return nil, fmt.Errorf("channel already holds source [%s] signed by another key [%s], refuse to overwrite", s.source, m.Fingerprint)
	}
	return m, nil
}

func (s *fileStore) encode(plain []byte) ([]byte, bool, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(plain); err != nil {
		return nil, false, err
	}
	if err := zw.Close(); err != nil {
		return nil, false, err
	}
	if len(s.recipients) == 0 {
		return buf.Bytes(), false, nil
	}
	enc, err := Encrypt(buf.Bytes(), s.recipients)
	return enc, true, err
}

func (s *fileStore) decode(ref FileRef) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(s.dir(), ref.File))
	if err != nil {
		return nil, fmt.Errorf("failed to read channel file [%s], Error:%s", ref.File, err)
	}
	if sha256Hex(data) != ref.Sha256 {
		return nil, fmt.Errorf("channel file [%s] digest does not match manifest", ref.File)
	}
	if ref.Encrypted {
		data, err = Decrypt(data, s.identity)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt channel file [%s], Error:%s", ref.File, err)
		}
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

func fileSuffix(encrypted bool) string {
	if encrypted {
		return ".jsonl.gz.enc"
	}
	return ".jsonl.gz"
}

// ---------- Publisher ----------

func (s *fileStore) prepareWrite() (*Manifest, error) {
	if s.beforeWrite != nil {
		if err := s.beforeWrite(); err != nil {
			return nil, err
		}
	}
	return s.loadOwnManifest()
}

func (s *fileStore) commit(message string) error {
	if s.afterWrite != nil {
		return s.afterWrite(message)
	}
	return nil
}

func (s *fileStore) Publish(changes []Change) error {
	if len(changes) == 0 {
		return nil
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	m, err := s.prepareWrite()
	if err != nil {
		return err
	}
	if last := m.maxSeq(); changes[0].Seq != last+1 {
		return fmt.Errorf("changes to publish must continue from seq [%d], got [%d]", last+1, changes[0].Seq)
	}
	var buf bytes.Buffer
	for idx, change := range changes {
		if idx > 0 && change.Seq != changes[idx-1].Seq+1 {
			return fmt.Errorf("changes to publish are not continuous: seq [%d] after [%d]", change.Seq, changes[idx-1].Seq)
		}
		line, err := json.Marshal(change)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	data, encrypted, err := s.encode(buf.Bytes())
	if err != nil {
		return err
	}
	from := changes[0].Seq
	to := changes[len(changes)-1].Seq
	name := filepath.Join(segmentDir, fmt.Sprintf("%020d-%020d%s", from, to, fileSuffix(encrypted)))
	if err := writeFileAtomic(filepath.Join(s.dir(), name), data); err != nil {
		return err
	}
	m.Segments = append(m.Segments, FileRef{From: from, To: to, File: filepath.ToSlash(name), Sha256: sha256Hex(data), Encrypted: encrypted})
	if err := s.writeManifest(m); err != nil {
		return err
	}
	if err := s.commit(fmt.Sprintf("%s: publish seq %d-%d", s.source, from, to)); err != nil {
		return err
	}
	s.manifest = m
	return nil
}

func (s *fileStore) PublishSnapshot(snap *Snapshot) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	m, err := s.prepareWrite()
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	header, _ := json.Marshal(snapshotHeader{Seq: snap.Seq, Source: s.source, Ts: snap.Ts, Count: len(snap.Records)})
	buf.Write(header)
	buf.WriteByte('\n')
	for _, record := range snap.Records {
		line, err := json.Marshal(record)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	data, encrypted, err := s.encode(buf.Bytes())
	if err != nil {
		return err
	}
	name := filepath.Join(snapshotDir, fmt.Sprintf("%020d%s", snap.Seq, fileSuffix(encrypted)))
	if err := writeFileAtomic(filepath.Join(s.dir(), name), data); err != nil {
		return err
	}
	oldFiles := []string{}
	if m.Snapshot != nil && m.Snapshot.File != filepath.ToSlash(name) {
		oldFiles = append(oldFiles, m.Snapshot.File)
	}
	kept := []FileRef{}
	for _, seg := range m.Segments {
		if seg.To <= snap.Seq {
			oldFiles = append(oldFiles, seg.File)
			continue
		}
		kept = append(kept, seg)
	}
	m.Snapshot = &FileRef{Seq: snap.Seq, File: filepath.ToSlash(name), Sha256: sha256Hex(data), Encrypted: encrypted}
	m.Segments = kept
	if err := s.writeManifest(m); err != nil {
		return err
	}
	for _, file := range oldFiles {
		if err := os.Remove(filepath.Join(s.dir(), file)); err != nil && !os.IsNotExist(err) {
			s.log.Printf("Channel[%s]: failed to remove old file [%s], Error:%s", s.source, file, err)
		}
	}
	if err := s.commit(fmt.Sprintf("%s: snapshot seq %d", s.source, snap.Seq)); err != nil {
		return err
	}
	s.manifest = m
	return nil
}

func (s *fileStore) currentOwnManifest() (*Manifest, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	// 每次都读通道上的实际状态（不用缓存），这样通道被重置或被别处推进时发布端能察觉
	if s.beforeRead != nil {
		if err := s.beforeRead(); err != nil {
			return nil, err
		}
	}
	m, err := s.loadOwnManifest()
	if err != nil {
		return nil, err
	}
	s.manifest = m
	return m, nil
}

func (s *fileStore) LastPublishedSeq() (uint64, error) {
	m, err := s.currentOwnManifest()
	if err != nil {
		return 0, err
	}
	return m.maxSeq(), nil
}

func (s *fileStore) LatestSnapshotSeq() (uint64, bool, error) {
	m, err := s.currentOwnManifest()
	if err != nil {
		return 0, false, err
	}
	if m.Snapshot == nil {
		return 0, false, nil
	}
	return m.Snapshot.Seq, true, nil
}

// ---------- Subscriber ----------

func (s *fileStore) Refresh() error {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.beforeRead != nil {
		if err := s.beforeRead(); err != nil {
			return err
		}
	}
	m, err := s.readManifest(s.trusted)
	if err != nil {
		s.manifest = nil
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

func (s *fileStore) verifiedManifest() (*Manifest, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.manifest == nil {
		return nil, errors.New("channel not refreshed or source not verified")
	}
	return s.manifest, nil
}

func (s *fileStore) Fetch(cursor uint64, limit int) ([]Change, error) {
	m, err := s.verifiedManifest()
	if err != nil {
		return nil, err
	}
	if m.maxSeq() <= cursor {
		return []Change{}, nil
	}
	segments := []FileRef{}
	for _, seg := range m.Segments {
		if seg.To > cursor {
			segments = append(segments, seg)
		}
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i].From < segments[j].From })
	if len(segments) == 0 || segments[0].From > cursor+1 {
		return nil, ErrGap
	}
	expected := cursor + 1
	result := []Change{}
	for _, seg := range segments {
		if limit > 0 && len(result) >= limit {
			break
		}
		if seg.From > expected {
			break // 中间缺 segment：先返回已连续的部分，下次 Fetch 会得到 ErrGap
		}
		plain, err := s.decode(seg)
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
				return nil, fmt.Errorf("invalid change in [%s], Error:%s", seg.File, err)
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

func (s *fileStore) LatestSnapshot() (*Snapshot, error) {
	m, err := s.verifiedManifest()
	if err != nil {
		return nil, err
	}
	if m.Snapshot == nil {
		return nil, ErrNoSnapshot
	}
	plain, err := s.decode(*m.Snapshot)
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(plain))
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	snap := &Snapshot{Records: []map[string]interface{}{}}
	first := true
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if first {
			header := snapshotHeader{}
			if err := json.Unmarshal(line, &header); err != nil {
				return nil, fmt.Errorf("invalid snapshot header, Error:%s", err)
			}
			if header.Seq != m.Snapshot.Seq || header.Source != s.source {
				return nil, fmt.Errorf("snapshot header [%s@%d] does not match manifest [%s@%d]", header.Source, header.Seq, s.source, m.Snapshot.Seq)
			}
			snap.Seq = header.Seq
			snap.Source = header.Source
			snap.Ts = header.Ts
			first = false
			continue
		}
		record := map[string]interface{}{}
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("invalid snapshot record, Error:%s", err)
		}
		snap.Records = append(snap.Records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if first {
		return nil, errors.New("snapshot file is empty")
	}
	return snap, nil
}
