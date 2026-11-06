package Share

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"DataService/Channel"
	"DataService/DataHandler"

	"github.com/salesforce/UniTAO/lib/Schema/JsonKey"
	"github.com/salesforce/UniTAO/lib/Util/Http"
)

const (
	DefaultPullIntervalSec = 60
	MaxFetchChanges        = 5000
)

// MirrorState 是 mirrored vDS 需要持久化的同步进度（保存在 state.json）。
type MirrorState struct {
	Loaded            bool
	Cursor            uint64
	SourceFingerprint string
}

// Mirror 是 mirrored vDS 的后台同步器：定期从 Channel 拉取并应用变更。
type Mirror struct {
	Source       string // 来源共享名
	LocalName    string // 本网使用的名字
	Trusted      string // 可信指纹
	handler      *DataHandler.Handler
	channel      Channel.Subscriber
	pullInterval time.Duration
	loadState    func() MirrorState
	saveState    func(MirrorState) error
	log          *log.Logger
	runLock      sync.Mutex
	statusLock   sync.Mutex
	paused       bool
	reason       string
	lastSync     string
	stop         chan struct{}
}

func NewMirror(source string, localName string, trusted string, handler *DataHandler.Handler, channel Channel.Subscriber, pullIntervalSec int, loadState func() MirrorState, saveState func(MirrorState) error, logger *log.Logger) *Mirror {
	if logger == nil {
		logger = log.Default()
	}
	if pullIntervalSec <= 0 {
		pullIntervalSec = DefaultPullIntervalSec
	}
	return &Mirror{
		Source:       source,
		LocalName:    localName,
		Trusted:      trusted,
		handler:      handler,
		channel:      channel,
		pullInterval: time.Duration(pullIntervalSec) * time.Second,
		loadState:    loadState,
		saveState:    saveState,
		log:          logger,
		stop:         make(chan struct{}),
	}
}

func (m *Mirror) Log(message string) {
	m.log.Printf("Mirror[mirrored/%s<-%s]: %s", m.LocalName, m.Source, message)
}

func (m *Mirror) setStatus(paused bool, reason string) {
	m.statusLock.Lock()
	defer m.statusLock.Unlock()
	m.paused = paused
	m.reason = reason
	if !paused {
		m.lastSync = time.Now().UTC().Format(time.RFC3339)
	}
}

func (m *Mirror) Run() {
	m.SyncOnce()
	ticker := time.NewTicker(m.pullInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.SyncOnce()
		}
	}
}

func (m *Mirror) Stop() {
	close(m.stop)
}

// SyncOnce 拉取一次并应用。出错时把状态设为暂停并记录原因，下个周期重试。
func (m *Mirror) SyncOnce() error {
	m.runLock.Lock()
	defer m.runLock.Unlock()
	err := m.syncOnce()
	if err != nil {
		m.Log(fmt.Sprintf("paused: %s", err))
		m.setStatus(true, err.Error())
		return err
	}
	m.setStatus(false, "")
	return nil
}

func (m *Mirror) syncOnce() error {
	if err := m.channel.Refresh(); err != nil {
		if errors.Is(err, Channel.ErrNotPublished) {
			return fmt.Errorf("source [%s] has not published anything yet", m.Source)
		}
		return err
	}
	state := m.loadState()
	fingerprint := m.channel.SourceFingerprint()
	if state.SourceFingerprint != fingerprint {
		state.SourceFingerprint = fingerprint
		if err := m.saveState(state); err != nil {
			return err
		}
	}
	rebuilt := false
	if !state.Loaded {
		if err := m.rebuild(); err != nil {
			return err
		}
		rebuilt = true
		state = m.loadState()
	}
	for {
		changes, err := m.channel.Fetch(state.Cursor, MaxFetchChanges)
		if errors.Is(err, Channel.ErrGap) {
			if rebuilt {
				return fmt.Errorf("channel cannot serve changes after snapshot seq %d", state.Cursor)
			}
			rebuilt = true
			m.Log(fmt.Sprintf("cannot continue from cursor %d, rebuild from snapshot", state.Cursor))
			if err := m.rebuild(); err != nil {
				return err
			}
			state = m.loadState()
			continue
		}
		if err != nil {
			return err
		}
		if len(changes) == 0 {
			return nil
		}
		for _, change := range changes {
			if change.Seq != state.Cursor+1 {
				return fmt.Errorf("unexpected seq %d after cursor %d", change.Seq, state.Cursor)
			}
			if change.Source != m.Source {
				return fmt.Errorf("change seq %d comes from source [%s], expect [%s]", change.Seq, change.Source, m.Source)
			}
			if err := m.apply(change); err != nil {
				return fmt.Errorf("failed to apply seq %d [%s %s/%s]: %s", change.Seq, change.Op, change.Type, change.Id, err)
			}
			state.Cursor = change.Seq
		}
		if err := m.saveState(state); err != nil {
			return err
		}
		m.Log(fmt.Sprintf("applied up to seq %d", state.Cursor))
	}
}

func (m *Mirror) apply(change Channel.Change) *Http.HttpError {
	switch change.Op {
	case Channel.OpUpsert:
		if change.Record == nil {
			return Http.NewHttpError("upsert without record", 400)
		}
		return m.handler.ApplyUpsert(change.Record)
	case Channel.OpDelete:
		return m.handler.ApplyDelete(change.Type, change.Id)
	}
	return Http.NewHttpError(fmt.Sprintf("unknown op [%s]", change.Op), 400)
}

// rebuild 清空本 vDS 的同步数据，从最新 snapshot 重新加载。
func (m *Mirror) rebuild() error {
	snap, err := m.channel.LatestSnapshot()
	if err != nil {
		if errors.Is(err, Channel.ErrNoSnapshot) {
			return fmt.Errorf("source [%s] has no snapshot yet, wait for it", m.Source)
		}
		return err
	}
	if snap.Source != m.Source {
		return fmt.Errorf("snapshot comes from source [%s], expect [%s]", snap.Source, m.Source)
	}
	before, httpErr := m.handler.ListDataTypes()
	if httpErr != nil {
		return httpErr
	}
	if httpErr := m.handler.ClearSynced(); httpErr != nil {
		return httpErr
	}
	for _, record := range snap.Records {
		if httpErr := m.handler.ApplyUpsert(record); httpErr != nil {
			return fmt.Errorf("failed to load snapshot record: %s", httpErr)
		}
	}
	after, httpErr := m.handler.ListDataTypes()
	if httpErr != nil {
		return httpErr
	}
	m.notifyRemovedTypes(before, after)
	state := m.loadState()
	state.Loaded = true
	state.Cursor = snap.Seq
	if err := m.saveState(state); err != nil {
		return err
	}
	m.Log(fmt.Sprintf("rebuilt from snapshot seq %d with %d records", snap.Seq, len(snap.Records)))
	return nil
}

func (m *Mirror) notifyRemovedTypes(before []string, after []string) {
	if m.handler.TypeChangeNotify == nil {
		return
	}
	remaining := map[string]bool{}
	for _, t := range after {
		remaining[t] = true
	}
	for _, t := range before {
		if !remaining[t] && t != JsonKey.Schema {
			m.handler.TypeChangeNotify(t)
		}
	}
}

// Status 返回 /{vDS}/config 中展示的同步状态。
func (m *Mirror) Status() map[string]interface{} {
	state := m.loadState()
	m.statusLock.Lock()
	defer m.statusLock.Unlock()
	status := "ok"
	if m.paused {
		status = "paused"
	}
	result := map[string]interface{}{
		"source":             m.Source,
		"trustedFingerprint": m.Trusted,
		"sourceFingerprint":  state.SourceFingerprint,
		"loaded":             state.Loaded,
		"cursor":             state.Cursor,
		"status":             status,
		"lastSyncTime":       m.lastSync,
	}
	if m.paused {
		result["reason"] = m.reason
	}
	return result
}
