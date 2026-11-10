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

	// mirror 在 /config 中显示的订阅阶段
	PhaseNone    = "none"    // 还没有订阅
	PhaseWaiting = "waiting" // 已请求订阅，等待 publisher 打包 history
	PhaseActive  = "active"  // 已加载 history，按 queue 消费
)

// MirrorState 是 mirrored vDS 需要持久化的同步进度（保存在 state.json）。
// 本地状态是真实进度，通道上的进度文件只是汇报。
type MirrorState struct {
	Request           string // 当前订阅请求 id，空表示还没订阅
	Loaded            bool   // 当前请求的 history 已加载
	Cursor            uint64 // 已应用到的最大 seq
	SourceFingerprint string
}

// Mirror 是 mirrored vDS 的后台同步器（subscriber）：订阅、加载 history、按顺序消费 queue、汇报进度。
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

// subscribe 发起一个新的订阅请求。旧数据保留到新 history 加载时整体替换。
func (m *Mirror) subscribe(state MirrorState, why string) error {
	state.Request = Channel.NewRequestId()
	state.Loaded = false
	state.Cursor = 0
	if err := m.saveState(state); err != nil {
		return err
	}
	m.Log(fmt.Sprintf("subscribe with request %s: %s", state.Request, why))
	return m.channel.WriteProgress(Channel.Progress{Request: state.Request, State: Channel.StateSubscribe})
}

func (m *Mirror) syncOnce() error {
	refreshErr := m.channel.Refresh()
	if refreshErr != nil && !errors.Is(refreshErr, Channel.ErrNotPublished) {
		return refreshErr
	}
	state := m.loadState()
	own := m.channel.Progress()
	if refreshErr != nil {
		// 来源还没发布：先登记订阅，publisher 上线后为我们打包 history
		switch {
		case state.Request == "":
			return m.subscribe(state, "first subscription")
		case !state.Loaded && own == nil:
			return m.channel.WriteProgress(Channel.Progress{Request: state.Request, State: Channel.StateSubscribe})
		}
		return nil
	}
	if fingerprint := m.channel.SourceFingerprint(); state.SourceFingerprint != fingerprint {
		state.SourceFingerprint = fingerprint
		if err := m.saveState(state); err != nil {
			return err
		}
	}
	lastSeq := m.channel.LastSeq()
	switch {
	case state.Request == "":
		return m.subscribe(state, "first subscription")
	case own == nil && state.Loaded:
		return m.subscribe(state, "progress file was removed (considered dead by the publisher)")
	case own != nil && own.Request != state.Request:
		return m.subscribe(state, fmt.Sprintf("progress file belongs to request %s", own.Request))
	case state.Loaded && own != nil && own.Cursor > state.Cursor:
		return m.subscribe(state, fmt.Sprintf("reported cursor %d is ahead of local cursor %d (local state lost)", own.Cursor, state.Cursor))
	case !state.Loaded && own != nil && own.State == Channel.StateActive:
		return m.subscribe(state, "progress file is active but the history was not loaded locally (local state lost)")
	case state.Loaded && state.Cursor > lastSeq:
		return m.subscribe(state, fmt.Sprintf("local cursor %d is beyond published seq %d (channel was reset)", state.Cursor, lastSeq))
	}
	if !state.Loaded {
		if own == nil {
			return m.channel.WriteProgress(Channel.Progress{Request: state.Request, State: Channel.StateSubscribe})
		}
		history, err := m.channel.History(state.Request)
		if errors.Is(err, Channel.ErrNoHistory) {
			return nil // 等 publisher 打包
		}
		if err != nil {
			return err
		}
		if err := m.load(history); err != nil {
			return err
		}
		state = m.loadState()
	}
	for {
		changes, err := m.channel.Fetch(state.Cursor, MaxFetchChanges)
		if errors.Is(err, Channel.ErrGap) {
			return m.subscribe(state, fmt.Sprintf("seq %d is no longer on the channel", state.Cursor+1))
		}
		if err != nil {
			return err
		}
		if len(changes) == 0 {
			break
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
	// 只有进度变化时才写，空闲时不产生提交
	own = m.channel.Progress()
	if own == nil || own.State != Channel.StateActive || own.Cursor != state.Cursor || own.Request != state.Request {
		return m.channel.WriteProgress(Channel.Progress{Request: state.Request, State: Channel.StateActive, Cursor: state.Cursor})
	}
	return nil
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

// load 用 history 整体替换本 vDS 的同步数据。分片逐个读取，不把整份 history 放进内存。
func (m *Mirror) load(history *Channel.HistoryReader) error {
	if history.Source != m.Source {
		return fmt.Errorf("history comes from source [%s], expect [%s]", history.Source, m.Source)
	}
	before, httpErr := m.handler.ListDataTypes()
	if httpErr != nil {
		return httpErr
	}
	if httpErr := m.handler.ClearSynced(); httpErr != nil {
		return httpErr
	}
	err := history.Each(func(record map[string]interface{}) error {
		if httpErr := m.handler.ApplyUpsert(record); httpErr != nil {
			return fmt.Errorf("failed to load history record: %s", httpErr)
		}
		return nil
	})
	if err != nil {
		// 本地数据已被部分替换；Loaded 仍为 false，下一轮从头重新加载
		return err
	}
	after, httpErr := m.handler.ListDataTypes()
	if httpErr != nil {
		return httpErr
	}
	m.notifyRemovedTypes(before, after)
	state := m.loadState()
	state.Loaded = true
	state.Cursor = history.Seq
	if err := m.saveState(state); err != nil {
		return err
	}
	m.Log(fmt.Sprintf("loaded history %s at seq %d with %d records in %d parts", history.Id, history.Seq, history.Records, history.Parts))
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
	phase := PhaseNone
	switch {
	case state.Request != "" && state.Loaded:
		phase = PhaseActive
	case state.Request != "":
		phase = PhaseWaiting
	}
	result := map[string]interface{}{
		"source":             m.Source,
		"subscriberId":       m.channel.Id(),
		"trustedFingerprint": m.Trusted,
		"sourceFingerprint":  state.SourceFingerprint,
		"subscription":       phase,
		"request":            state.Request,
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
