package Share

import (
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
	DefaultHistoryReuseHours      = 24
	MaxBatchChanges               = 1000

	// subscriber 在 /config 中显示的状态（除了 Channel.StateSubscribe / StateActive）
	SubscriberDead    = "dead"    // 本轮被判定死亡，进度文件已删除
	SubscriberGap     = "gap"     // 进度落在已删除的 batch 之前或属于旧 epoch，等它重新订阅；进度文件超时未更新则判死
	SubscriberInvalid = "invalid" // 进度文件内容不合法，忽略
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

// HistoryStatus 是通道上的一份 history（最近一轮 Maintain 看到的）。
type HistoryStatus struct {
	Id        string `json:"id"`
	Seq       uint64 `json:"seq"`
	Records   int    `json:"records"`
	Parts     int    `json:"parts"`
	Bytes     int    `json:"bytes"`
	Published string `json:"published"`
	Grants    int    `json:"grants"`
	Pending   int    `json:"pending"`  // 还没加载完的分配
	Reusable  bool   `json:"reusable"` // 新订阅方能否直接分到它
}

// PublisherOptions 是 shared vDS 配置中与发布相关的参数，0 表示默认值。
type PublisherOptions struct {
	PublishIntervalSec     int
	SubscriberTimeoutHours int
	CompactIntervalHours   int
	HistoryReuseHours      int
}

// Publisher 是 shared vDS 的后台发布器（见 plan/feature-channel-queue/plan.md）：
//   - 把 outbox 按 seq 连续打包成 queue batch；
//   - 为新订阅请求提供 history：复用期内分给已有的 history，否则打包一份新的；
//   - 判定死亡的 subscriber，删除所有存活 subscriber 都处理过的 batch；
//   - 定期压缩通道历史。
type Publisher struct {
	Name            string
	handler         *DataHandler.Handler
	outbox          *Outbox
	channel         Channel.Publisher
	sourceId        string
	publishInterval time.Duration
	timeout         time.Duration
	compactInterval time.Duration
	reuseWindow     time.Duration
	now             func() time.Time
	log             *log.Logger
	lock            sync.Mutex // 发布、history 打包与清理互斥
	statusLock      sync.Mutex
	lastError       string
	lastErrorTime   string
	subscribers     []SubscriberStatus
	histories       []HistoryStatus
	lastSeq         uint64
	queueFrom       uint64
	stop            chan struct{}
}

// NewPublisher 创建发布器。sourceId 是来源的身份（Channel.SourceId），只用于展示。
func NewPublisher(name string, handler *DataHandler.Handler, outbox *Outbox, channel Channel.Publisher, sourceId string, opts PublisherOptions, logger *log.Logger) *Publisher {
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
	if opts.HistoryReuseHours <= 0 {
		opts.HistoryReuseHours = DefaultHistoryReuseHours
	}
	return &Publisher{
		Name:            name,
		handler:         handler,
		outbox:          outbox,
		channel:         channel,
		sourceId:        sourceId,
		publishInterval: time.Duration(opts.PublishIntervalSec) * time.Second,
		timeout:         time.Duration(opts.SubscriberTimeoutHours) * time.Hour,
		compactInterval: time.Duration(opts.CompactIntervalHours) * time.Hour,
		reuseWindow:     time.Duration(opts.HistoryReuseHours) * time.Hour,
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

// SetHistoryReuseWindow 替换 history 复用期（测试用）。
func (p *Publisher) SetHistoryReuseWindow(window time.Duration) {
	p.reuseWindow = window
}

func (p *Publisher) Log(message string) {
	p.log.Printf("Publisher[shared/%s]: %s", p.Name, message)
}

func (p *Publisher) SourceId() string {
	return p.sourceId
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
		p.Log(fmt.Sprintf("published seq %d-%d", batch[0].Seq, last))
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
//   - 通道上还没有本来源、是旧格式，或 manifest 是另一个 DS 实例写的（重建的实例接管 folder，或两个实例
//     配置了同一个 folder）：从本地进度 Reset，换新的 epoch，subscriber 重新订阅；
//   - 通道比本地新、但没有超过 outbox 已提交的 seq：上次写入通道后、保存进度前进程退出，按通道前移本地进度；
//   - 通道比 outbox 已提交的 seq 还新：本地状态丢失或从备份恢复，通道上的变更与本地数据对不上，Reset；
//   - 通道比本地旧（仓库被重置或更换）：已发布的变更已从 outbox 删除，无法补发，Reset。
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
		// 通道上还没有本来源：立即写出 manifest，不等第一条数据。
		// 订阅方由此能确认来源存在并开始订阅；git 不保存空目录，folder 的存在就体现为这份 manifest。
		p.Log(fmt.Sprintf("channel has no manifest of this source yet; initialize it at seq %d", state.LastPublished))
		return p.channel.Reset(state.LastPublished)
	case view.Foreign:
		p.Log(fmt.Sprintf("WARNING: channel folder was written by DS instance [%s]; take it over and reset at seq %d, subscribers will re-subscribe (if that instance still runs, two instances share this folder)", view.Owner, state.LastPublished))
		return p.channel.Reset(state.LastPublished)
	case view.LastSeq > state.LastPublished:
		committed, err := p.outbox.CurrentSeq()
		if err != nil {
			return err
		}
		if view.LastSeq > committed {
			p.Log(fmt.Sprintf("channel is at seq %d, beyond the local outbox seq %d (local state lost or restored from a backup); reset at seq %d, subscribers will re-subscribe", view.LastSeq, committed, state.LastPublished))
			return p.channel.Reset(state.LastPublished)
		}
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

func (p *Publisher) olderThan(ts string, window time.Duration, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return false
	}
	return now.Sub(t) > window
}

func (p *Publisher) expired(published string, now time.Time) bool {
	return p.olderThan(published, p.timeout, now)
}

// findGrant 返回分给 subscriber 这个订阅请求的 history（有多份时取 seq 最大的）。
func findGrant(histories []Channel.HistoryRef, subscriber string, request string) (*Channel.HistoryRef, *Channel.GrantRef) {
	var found *Channel.HistoryRef
	var grant *Channel.GrantRef
	for idx := range histories {
		if g := histories[idx].Grant(subscriber, request); g != nil && (found == nil || histories[idx].Seq > found.Seq) {
			found, grant = &histories[idx], g
		}
	}
	return found, grant
}

// queueCovers 判断通道能否从 seq+1 连续提供变更到 lastSeq（queue 只从头部删除，尾部总是连续的）。
func queueCovers(view *Channel.View, seq uint64) bool {
	if seq >= view.LastSeq {
		return seq == view.LastSeq
	}
	for _, ref := range view.Queue {
		if ref.To > seq {
			return ref.From <= seq+1
		}
	}
	return false
}

// reusable 判断一份 history 能否分给新订阅方：在复用期内、之后的 queue 还完整。
func (p *Publisher) reusable(view *Channel.View, h *Channel.HistoryRef, now time.Time) bool {
	return !p.olderThan(h.Published, p.reuseWindow, now) && queueCovers(view, h.Seq)
}

// Maintain 处理订阅请求、判定死亡 subscriber，并删除所有存活 subscriber 都处理过的 batch 与 history。
//
// 存活 subscriber 的进度：active 取 cursor；subscribe 取分给它的 history 的 seq。所有存活 subscriber
// 中最小的进度之前的 batch 都可以删除；没有存活 subscriber 时删除全部 batch，新 subscriber 总会拿到 history。
//
// 新订阅请求优先分给复用期内最新的 history（只改 manifest），没有可复用的才打包一份新的，
// 本轮的所有新请求共用它。一份 history 的所有分配都不再等待（订阅方已 active、换了请求、死亡或进度文件
// 已删除）时删除它。
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
	if view.OldFormat || !view.Published || view.Foreign {
		return nil // PublishPending 下一轮会 Reset
	}
	now := p.now()
	lastSeq := view.LastSeq
	waiting := map[string]string{} // 正在等 history 的存活 subscriber -> 订阅请求
	prune := Channel.Prune{}
	minCursor := lastSeq
	statuses := []SubscriberStatus{}
	needHistory := []Channel.Progress{}
	for _, sub := range view.Subscribers {
		status := SubscriberStatus{Id: sub.Id, Request: sub.Request, State: sub.State, Cursor: sub.Cursor, Updated: sub.Updated}
		live := false
		var cursor uint64
		dead := ""
		switch sub.State {
		case Channel.StateSubscribe:
			history, grant := findGrant(view.Histories, sub.Id, sub.Request)
			switch {
			case sub.Request == "":
				status.State = SubscriberInvalid
				status.Reason = "subscribe without request id"
			case grant == nil:
				needHistory = append(needHistory, sub)
			case p.expired(grant.Granted, now):
				dead = fmt.Sprintf("history %s granted at %s was not loaded within %s", shortId(history.Id), grant.Granted, p.timeout)
			default:
				live, cursor = true, history.Seq
				waiting[sub.Id] = sub.Request
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
			case sub.Epoch != view.Epoch:
				status.State = SubscriberGap
				status.Reason = fmt.Sprintf("cursor belongs to epoch %s, the channel is at epoch %s (channel was reset)", shortId(sub.Epoch), shortId(view.Epoch))
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
		default:
			status.State = SubscriberInvalid
			status.Reason = fmt.Sprintf("unknown state [%s]", sub.State)
		}
		// 活着的 mirror 遇到 gap 会立即重新订阅、改写进度文件；gap 的进度文件超时未变，说明它已经不在了。
		// 已追平的 active 进度不能按 updated 判死：mirror 只在进度变化时写，空闲时 updated 本来就不动。
		if status.State == SubscriberGap && p.expired(sub.Updated, now) {
			dead = fmt.Sprintf("%s, and its progress was not updated since %s (%s)", status.Reason, sub.Updated, p.timeout)
		}
		if dead != "" {
			p.Log(fmt.Sprintf("subscriber %s is dead: %s", sub.Id, dead))
			status.State = SubscriberDead
			status.Reason = dead
			prune.Subscribers = append(prune.Subscribers, sub.Id)
		}
		if live && cursor < minCursor {
			minCursor = cursor
		}
		statuses = append(statuses, status)
	}
	if len(needHistory) > 0 {
		provided, err := p.provideHistory(view, state, needHistory, now)
		if err != nil {
			return err
		}
		// 把更新后的条目放回 view，新授权的订阅方也在等它
		replaced := false
		for idx := range view.Histories {
			if view.Histories[idx].Id == provided.Id {
				view.Histories[idx], replaced = *provided, true
			}
		}
		if !replaced {
			view.Histories = append(view.Histories, *provided)
		}
		for _, sub := range needHistory {
			waiting[sub.Id] = sub.Request
		}
		if provided.Seq < minCursor {
			minCursor = provided.Seq
		}
	}
	// history：所有分配都不再等待时删除
	historyStatus := []HistoryStatus{}
	for idx := range view.Histories {
		h := &view.Histories[idx]
		pending := 0
		for _, grant := range h.Grants {
			if request, ok := waiting[grant.Subscriber]; ok && request == grant.Request {
				pending++
			}
		}
		if pending == 0 {
			prune.Histories = append(prune.Histories, h.Id)
			continue
		}
		bytes := 0
		for _, part := range h.Parts {
			bytes += part.Size
		}
		historyStatus = append(historyStatus, HistoryStatus{
			Id: h.Id, Seq: h.Seq, Records: h.Records, Parts: len(h.Parts), Bytes: bytes, Published: h.Published,
			Grants: len(h.Grants), Pending: pending, Reusable: p.reusable(view, h, now),
		})
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
	p.histories = historyStatus
	p.lastSeq = lastSeq
	p.queueFrom = queueFrom
	p.statusLock.Unlock()
	return nil
}

// provideHistory 给新订阅请求一份 history：复用期内最新的可复用 history 直接分给它们，否则打包一份新的。
// 返回更新后的 history 条目（订阅方加载后从它的 seq 开始消费 queue）。
func (p *Publisher) provideHistory(view *Channel.View, state *PublishState, subs []Channel.Progress, now time.Time) (*Channel.HistoryRef, error) {
	grants := []Channel.HistoryGrant{}
	for _, sub := range subs {
		grants = append(grants, Channel.HistoryGrant{Subscriber: sub.Id, Request: sub.Request})
	}
	var reuse *Channel.HistoryRef
	for idx := range view.Histories {
		h := &view.Histories[idx]
		if p.reusable(view, h, now) && (reuse == nil || h.Seq > reuse.Seq) {
			reuse = h
		}
	}
	if reuse != nil {
		ref, err := p.channel.GrantHistory(reuse.Id, grants)
		if err != nil {
			return nil, err
		}
		for _, sub := range subs {
			p.Log(fmt.Sprintf("history %s at seq %d granted to subscriber %s", shortId(ref.Id), ref.Seq, sub.Id))
		}
		return ref, nil
	}
	h := &Channel.History{Seq: state.LastPublished, Source: p.Name, Ts: now.UTC().Format(time.RFC3339), Records: p.handler.EachSyncedRecord}
	ref, err := p.channel.PublishHistory(h, grants)
	if err != nil {
		return nil, err
	}
	for _, sub := range subs {
		p.Log(fmt.Sprintf("history %s at seq %d with %d records in %d parts published for subscriber %s", shortId(ref.Id), ref.Seq, ref.Records, len(ref.Parts), sub.Id))
	}
	return ref, nil
}

func shortId(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
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
		"sourceId":               p.sourceId,
		"subscriberTimeoutHours": p.timeout.Hours(),
		"compactIntervalHours":   p.compactInterval.Hours(),
		"historyReuseHours":      p.reuseWindow.Hours(),
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
	histories := p.histories
	if histories == nil {
		histories = []HistoryStatus{}
	}
	result["histories"] = histories
	result["queueFromSeq"] = p.queueFrom
	if p.lastError != "" {
		result["lastError"] = p.lastError
		result["lastErrorTime"] = p.lastErrorTime
	}
	return result
}
