package Share

import (
	"crypto/ed25519"
	"fmt"
	"log"
	"sync"
	"time"

	"DataService/Channel"
	"DataService/DataHandler"
)

const (
	DefaultPublishIntervalSec     = 60
	DefaultSubscriberTimeoutHours = 24
	DefaultCompactIntervalHours   = 24
	MaxBatchChanges               = 1000

	// subscriber 在 /config 中显示的状态（除了 Channel.StateSubscribe / StateActive）
	SubscriberDead      = "dead"      // 本轮被判定死亡，进度文件已删除
	SubscriberGap       = "gap"       // 进度落在已删除的 batch 之前，等它重新订阅
	SubscriberUntrusted = "untrusted" // 不在接收方列表中，忽略
	SubscriberInvalid   = "invalid"   // 进度文件内容不合法，忽略
)

// SubscriberStatus 是 publisher 最近一轮看到的一个 subscriber。
type SubscriberStatus struct {
	Id      string `json:"id"`
	Request string `json:"request,omitempty"`
	State   string `json:"state"`
	Cursor  uint64 `json:"cursor"`
	Updated string `json:"updated,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// PublisherOptions 是 shared vDS 配置中与发布相关的参数，0 表示默认值。
type PublisherOptions struct {
	PublishIntervalSec     int
	SubscriberTimeoutHours int
	CompactIntervalHours   int
}

// Publisher 是 shared vDS 的后台发布器（见 plan/feature-channel-queue/plan.md）：
//   - 把 outbox 按 seq 连续打包成 queue batch；
//   - 为新订阅请求打包 history；
//   - 判定死亡的 subscriber，删除所有存活 subscriber 都处理过的 batch；
//   - 定期压缩通道历史。
type Publisher struct {
	Name            string
	handler         *DataHandler.Handler
	outbox          *Outbox
	channel         Channel.Publisher
	fingerprint     string
	publishInterval time.Duration
	timeout         time.Duration
	compactInterval time.Duration
	now             func() time.Time
	log             *log.Logger
	lock            sync.Mutex // 发布、history 打包与清理互斥
	statusLock      sync.Mutex
	lastError       string
	lastErrorTime   string
	subscribers     []SubscriberStatus
	lastSeq         uint64
	queueFrom       uint64
	stop            chan struct{}
}

func NewPublisher(name string, handler *DataHandler.Handler, outbox *Outbox, channel Channel.Publisher, signer ed25519.PrivateKey, opts PublisherOptions, logger *log.Logger) *Publisher {
	if logger == nil {
		logger = log.Default()
	}
	if opts.PublishIntervalSec <= 0 {
		opts.PublishIntervalSec = DefaultPublishIntervalSec
	}
	if opts.SubscriberTimeoutHours <= 0 {
		opts.SubscriberTimeoutHours = DefaultSubscriberTimeoutHours
	}
	if opts.CompactIntervalHours <= 0 {
		opts.CompactIntervalHours = DefaultCompactIntervalHours
	}
	if channel.RecipientCount() == 0 {
		logger.Printf("Publisher[shared/%s]: recipients file has no key; changes are not written to the queue until a subscriber's recipient is added (restart after adding)", name)
	}
	return &Publisher{
		Name:            name,
		handler:         handler,
		outbox:          outbox,
		channel:         channel,
		fingerprint:     Channel.Fingerprint(signer.Public().(ed25519.PublicKey)),
		publishInterval: time.Duration(opts.PublishIntervalSec) * time.Second,
		timeout:         time.Duration(opts.SubscriberTimeoutHours) * time.Hour,
		compactInterval: time.Duration(opts.CompactIntervalHours) * time.Hour,
		now:             time.Now,
		log:             logger,
		stop:            make(chan struct{}),
	}
}

// SetClock 替换时钟（测试用）。
func (p *Publisher) SetClock(now func() time.Time) {
	p.now = now
}

// SetSubscriberTimeout 替换 subscriber 超时（测试用）。
func (p *Publisher) SetSubscriberTimeout(timeout time.Duration) {
	p.timeout = timeout
}

func (p *Publisher) Log(message string) {
	p.log.Printf("Publisher[shared/%s]: %s", p.Name, message)
}

func (p *Publisher) Fingerprint() string {
	return p.fingerprint
}

func (p *Publisher) setError(err error) {
	p.statusLock.Lock()
	defer p.statusLock.Unlock()
	if err == nil {
		p.lastError = ""
		p.lastErrorTime = ""
		return
	}
	p.lastError = err.Error()
	p.lastErrorTime = time.Now().UTC().Format(time.RFC3339)
}

// Run 启动后台循环，直到 Stop。
func (p *Publisher) Run() {
	p.RunOnce()
	ticker := time.NewTicker(p.publishInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.RunOnce()
		}
	}
}

func (p *Publisher) Stop() {
	close(p.stop)
}

// RunOnce 执行一轮：发布积压变更 → 处理订阅、死亡判定与清理 → 到期时压缩历史。
func (p *Publisher) RunOnce() error {
	err := p.PublishPending()
	if err == nil {
		err = p.Maintain()
	}
	if err == nil {
		err = p.CompactIfDue()
	}
	if err != nil {
		p.Log(fmt.Sprintf("failed: %s", err))
	}
	p.setError(err)
	return err
}

// PublishPending 把 outbox 中尚未发布的变更按批推到 Channel。
func (p *Publisher) PublishPending() error {
	p.lock.Lock()
	defer p.lock.Unlock()
	state, err := p.outbox.PublishState()
	if err != nil {
		return err
	}
	if err := p.reconcileLocked(state); err != nil {
		return err
	}
	for {
		committed, err := p.outbox.CurrentSeq()
		if err != nil {
			return err
		}
		if committed <= state.LastPublished {
			return nil
		}
		changes, err := p.outbox.List(state.LastPublished, MaxBatchChanges)
		if err != nil {
			return err
		}
		batch := []Channel.Change{}
		expected := state.LastPublished + 1
		for _, change := range changes {
			if change.Seq > committed || change.Seq != expected {
				break
			}
			batch = append(batch, change)
			expected++
		}
		if len(batch) == 0 {
			return fmt.Errorf("outbox is missing seq %d (committed up to %d)", state.LastPublished+1, committed)
		}
		if err := p.channel.Publish(batch); err != nil {
			return err
		}
		last := batch[len(batch)-1].Seq
		if p.channel.RecipientCount() == 0 {
			p.Log(fmt.Sprintf("no recipients: advanced to seq %d without a queue batch", last))
		} else {
			p.Log(fmt.Sprintf("published seq %d-%d", batch[0].Seq, last))
		}
		state.LastPublished = last
		state.LastPublishTime = p.now().UTC().Format(time.RFC3339)
		if err := p.outbox.SavePublishState(state); err != nil {
			return err
		}
		if err := p.outbox.DeleteUpTo(last); err != nil {
			return err
		}
	}
}

// reconcileLocked 让本地发布进度与通道一致（需持有 p.lock）：
//   - 通道比本地新：上次写入通道后、保存进度前进程退出，按通道前移本地进度；
//   - 通道比本地旧（仓库被重置或更换）或是旧格式：已发布的变更已从 outbox 删除，无法补发，
//     让通道从本地进度重新开始；subscriber 会发现缺口并重新订阅。
func (p *Publisher) reconcileLocked(state *PublishState) error {
	view, err := p.channel.View()
	if err != nil {
		return err
	}
	switch {
	case view.OldFormat:
		p.Log(fmt.Sprintf("channel holds an old-format manifest; reset it at seq %d", state.LastPublished))
		return p.channel.Reset(state.LastPublished)
	case !view.Published:
		// 通道上还没有本来源：立即写出（签名的）manifest，不等第一条数据。
		// 订阅方由此能确认来源存在、校验指纹并开始订阅；git 不保存空目录，folder 的存在就体现为这份 manifest。
		p.Log(fmt.Sprintf("channel has no manifest of this source yet; initialize it at seq %d", state.LastPublished))
		return p.channel.Reset(state.LastPublished)
	case view.LastSeq > state.LastPublished:
		p.Log(fmt.Sprintf("channel is at seq %d, local progress %d; catch up", view.LastSeq, state.LastPublished))
		state.LastPublished = view.LastSeq
		if err := p.outbox.SavePublishState(state); err != nil {
			return err
		}
		return p.outbox.DeleteUpTo(view.LastSeq)
	case view.LastSeq < state.LastPublished:
		p.Log(fmt.Sprintf("channel is at seq %d, behind local progress %d; reset it, subscribers will re-subscribe", view.LastSeq, state.LastPublished))
		return p.channel.Reset(state.LastPublished)
	}
	return nil
}

func (p *Publisher) expired(published string, now time.Time) bool {
	ts, err := time.Parse(time.RFC3339, published)
	if err != nil {
		return false
	}
	return now.Sub(ts) > p.timeout
}

// Maintain 处理订阅请求、判定死亡 subscriber，并删除所有存活 subscriber 都处理过的 batch。
//
// 存活 subscriber 的进度：active 取 cursor；subscribe 取它 history 的 seq（还没有 history 的，
// 本轮会在当前 seq 生成）。所有存活 subscriber 中最小的进度之前的 batch 都可以删除；
// 没有存活 subscriber 时删除全部 batch，新 subscriber 总会拿到 history。
func (p *Publisher) Maintain() error {
	p.lock.Lock()
	defer p.lock.Unlock()
	state, err := p.outbox.PublishState()
	if err != nil {
		return err
	}
	view, err := p.channel.View()
	if err != nil {
		return err
	}
	if view.OldFormat {
		return nil // PublishPending 下一轮会 Reset
	}
	now := p.now()
	lastSeq := view.LastSeq
	histories := map[string]Channel.FileRef{}
	for _, ref := range view.Histories {
		histories[ref.Subscriber] = ref
	}
	present := map[string]Channel.Progress{}
	prune := Channel.Prune{}
	minCursor := lastSeq
	statuses := []SubscriberStatus{}
	needHistory := []Channel.Progress{}
	for _, sub := range view.Subscribers {
		present[sub.Id] = sub
		status := SubscriberStatus{Id: sub.Id, Request: sub.Request, State: sub.State, Cursor: sub.Cursor, Updated: sub.Updated}
		if !p.channel.Trusted(sub.Id) {
			status.State = SubscriberUntrusted
			status.Reason = "not in the recipients list"
			statuses = append(statuses, status)
			continue
		}
		history, hasHistory := histories[sub.Id]
		matching := hasHistory && history.Request == sub.Request
		live := false
		var cursor uint64
		dead := ""
		switch sub.State {
		case Channel.StateSubscribe:
			switch {
			case sub.Request == "":
				status.State = SubscriberInvalid
				status.Reason = "subscribe without request id"
			case !matching:
				needHistory = append(needHistory, sub)
				live, cursor = true, state.LastPublished
			case p.expired(history.Published, now):
				dead = fmt.Sprintf("history published at %s was not loaded within %s", history.Published, p.timeout)
			default:
				live, cursor = true, history.Seq
			}
		case Channel.StateActive:
			var first *Channel.FileRef
			for idx := range view.Queue {
				if view.Queue[idx].To > sub.Cursor {
					first = &view.Queue[idx]
					break
				}
			}
			switch {
			case sub.Cursor > lastSeq:
				status.State = SubscriberGap
				status.Reason = fmt.Sprintf("cursor %d is beyond published seq %d (channel was reset)", sub.Cursor, lastSeq)
			case sub.Cursor < lastSeq && (first == nil || first.From > sub.Cursor+1):
				status.State = SubscriberGap
				status.Reason = fmt.Sprintf("seq %d is no longer on the channel", sub.Cursor+1)
			case first != nil && p.expired(first.Published, now):
				dead = fmt.Sprintf("batch %d-%d published at %s was not processed within %s", first.From, first.To, first.Published, p.timeout)
			default:
				live, cursor = true, sub.Cursor
			}
			if hasHistory && (!matching || sub.Cursor >= history.Seq) {
				prune.Histories = append(prune.Histories, history.File)
			}
		default:
			status.State = SubscriberInvalid
			status.Reason = fmt.Sprintf("unknown state [%s]", sub.State)
		}
		if dead != "" {
			p.Log(fmt.Sprintf("subscriber %s is dead: %s", sub.Id, dead))
			status.State = SubscriberDead
			status.Reason = dead
			prune.Subscribers = append(prune.Subscribers, sub.Id)
			if hasHistory {
				prune.Histories = append(prune.Histories, history.File)
			}
		}
		if live && cursor < minCursor {
			minCursor = cursor
		}
		statuses = append(statuses, status)
	}
	// 进度文件已不存在的 subscriber 的 history
	for id, ref := range histories {
		if _, ok := present[id]; !ok {
			prune.Histories = append(prune.Histories, ref.File)
		}
	}
	if len(needHistory) > 0 {
		records, httpErr := p.handler.SyncedRecords()
		if httpErr != nil {
			return httpErr
		}
		h := &Channel.History{Seq: state.LastPublished, Source: p.Name, Ts: now.UTC().Format(time.RFC3339), Records: records}
		for _, sub := range needHistory {
			if err := p.channel.PublishHistory(sub.Id, sub.Request, h); err != nil {
				return err
			}
			p.Log(fmt.Sprintf("history at seq %d with %d records published for subscriber %s", h.Seq, len(records), sub.Id))
		}
	}
	prune.QueueUpTo = minCursor
	needPrune := len(prune.Histories) > 0 || len(prune.Subscribers) > 0
	for _, ref := range view.Queue {
		if ref.To <= minCursor {
			needPrune = true
		}
	}
	if needPrune {
		if err := p.channel.Prune(prune); err != nil {
			return err
		}
		p.Log(fmt.Sprintf("pruned queue up to seq %d, %d histories, %d dead subscribers", minCursor, len(prune.Histories), len(prune.Subscribers)))
	}
	queueFrom := lastSeq + 1
	for _, ref := range view.Queue {
		if ref.To > minCursor {
			queueFrom = ref.From
			break
		}
	}
	p.statusLock.Lock()
	p.subscribers = statuses
	p.lastSeq = lastSeq
	p.queueFrom = queueFrom
	p.statusLock.Unlock()
	return nil
}

// CompactIfDue 距上次检查超过 compactInterval 时压缩通道历史（dir 通道无操作）。
func (p *Publisher) CompactIfDue() error {
	p.lock.Lock()
	defer p.lock.Unlock()
	state, err := p.outbox.PublishState()
	if err != nil {
		return err
	}
	now := p.now()
	if state.LastCompactTime != "" {
		if last, err := time.Parse(time.RFC3339, state.LastCompactTime); err == nil && now.Sub(last) < p.compactInterval {
			return nil
		}
	}
	compacted, err := p.channel.Compact()
	if err != nil {
		return err
	}
	if compacted {
		p.Log("channel history compacted")
	}
	state.LastCompactTime = now.UTC().Format(time.RFC3339)
	return p.outbox.SavePublishState(state)
}

// Status 返回 /{vDS}/config 中展示的发布状态。
func (p *Publisher) Status() map[string]interface{} {
	result := map[string]interface{}{
		"fingerprint":            p.fingerprint,
		"subscriberTimeoutHours": p.timeout.Hours(),
		"compactIntervalHours":   p.compactInterval.Hours(),
		"recipients":             p.channel.RecipientCount(),
	}
	if seq, err := p.outbox.CurrentSeq(); err == nil {
		result["outboxSeq"] = seq
	}
	if state, err := p.outbox.PublishState(); err == nil {
		result["lastPublishedSeq"] = state.LastPublished
		result["lastPublishTime"] = state.LastPublishTime
		result["lastCompactTime"] = state.LastCompactTime
	}
	p.statusLock.Lock()
	defer p.statusLock.Unlock()
	subscribers := p.subscribers
	if subscribers == nil {
		subscribers = []SubscriberStatus{}
	}
	result["subscribers"] = subscribers
	result["queueFromSeq"] = p.queueFrom
	if p.lastError != "" {
		result["lastError"] = p.lastError
		result["lastErrorTime"] = p.lastErrorTime
	}
	return result
}
