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
	DefaultPublishIntervalSec  = 60
	DefaultSnapshotIntervalSec = 86400
	MaxSegmentChanges          = 1000
)

// Publisher 是 shared vDS 的后台发布器：定期把 outbox 推到 Channel，并定期生成 snapshot。
type Publisher struct {
	Name             string
	handler          *DataHandler.Handler
	outbox           *Outbox
	channel          Channel.Publisher
	fingerprint      string
	publishInterval  time.Duration
	snapshotInterval time.Duration
	log              *log.Logger
	lock             sync.Mutex // 发布与 snapshot 互斥
	statusLock       sync.Mutex
	lastError        string
	lastErrorTime    string
	stop             chan struct{}
}

func NewPublisher(name string, handler *DataHandler.Handler, outbox *Outbox, channel Channel.Publisher, signer ed25519.PrivateKey, publishIntervalSec int, snapshotIntervalSec int, logger *log.Logger) *Publisher {
	if logger == nil {
		logger = log.Default()
	}
	if publishIntervalSec <= 0 {
		publishIntervalSec = DefaultPublishIntervalSec
	}
	if snapshotIntervalSec <= 0 {
		snapshotIntervalSec = DefaultSnapshotIntervalSec
	}
	return &Publisher{
		Name:             name,
		handler:          handler,
		outbox:           outbox,
		channel:          channel,
		fingerprint:      Channel.Fingerprint(signer.Public().(ed25519.PublicKey)),
		publishInterval:  time.Duration(publishIntervalSec) * time.Second,
		snapshotInterval: time.Duration(snapshotIntervalSec) * time.Second,
		log:              logger,
		stop:             make(chan struct{}),
	}
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

// RunOnce 发布积压的变更；到期（或通道上还没有 snapshot）时生成 snapshot。
func (p *Publisher) RunOnce() {
	err := p.PublishPending()
	if err == nil {
		err = p.snapshotIfDue()
	}
	if err != nil {
		p.Log(fmt.Sprintf("failed: %s", err))
	}
	p.setError(err)
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
		changes, err := p.outbox.List(state.LastPublished, MaxSegmentChanges)
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
		state.LastPublishTime = time.Now().UTC().Format(time.RFC3339)
		if err := p.outbox.SavePublishState(state); err != nil {
			return err
		}
		if err := p.outbox.DeleteUpTo(last); err != nil {
			return err
		}
	}
}

func (p *Publisher) snapshotIfDue() error {
	state, err := p.outbox.PublishState()
	if err != nil {
		return err
	}
	if state.HasSnapshot {
		_, ok, err := p.channel.LatestSnapshotSeq()
		if err != nil {
			return err
		}
		if ok && state.LastSnapshotTime != "" {
			last, err := time.Parse(time.RFC3339, state.LastSnapshotTime)
			if err == nil && time.Since(last) < p.snapshotInterval {
				return nil
			}
		}
	}
	return p.Snapshot()
}

// reconcileLocked 让本地发布进度与通道一致（需持有 p.lock）：
//   - 通道比本地新：上次写入通道后、保存进度前进程退出，按通道前移本地进度；
//   - 通道比本地旧：通道被重置或换了仓库，已发布的变更已从 outbox 删除，只能先发布一个当前 seq 的 snapshot，
//     镜像方会因 gap 从这个 snapshot 重建。
func (p *Publisher) reconcileLocked(state *PublishState) error {
	channelLast, err := p.channel.LastPublishedSeq()
	if err != nil {
		return err
	}
	switch {
	case channelLast > state.LastPublished:
		p.Log(fmt.Sprintf("channel is at seq %d, local progress %d; catch up", channelLast, state.LastPublished))
		state.LastPublished = channelLast
		if err := p.outbox.SavePublishState(state); err != nil {
			return err
		}
		return p.outbox.DeleteUpTo(channelLast)
	case channelLast < state.LastPublished:
		p.Log(fmt.Sprintf("channel is at seq %d, behind local progress %d; publish a snapshot to resync", channelLast, state.LastPublished))
		return p.snapshotLocked(state)
	}
	return nil
}

// Snapshot 生成全量快照并发布。快照的 seq 为当前已发布的最大 seq：
// 先发布积压变更，再在发布锁内扫描数据，保证 snapshot 至少包含 seq 及之前的所有变更。
// 扫描期间新写入的数据可能比 seq 更新，订阅方从 seq+1 回放（完整记录 upsert）后结果一致。
func (p *Publisher) Snapshot() error {
	if err := p.PublishPending(); err != nil {
		return err
	}
	p.lock.Lock()
	defer p.lock.Unlock()
	state, err := p.outbox.PublishState()
	if err != nil {
		return err
	}
	return p.snapshotLocked(state)
}

func (p *Publisher) snapshotLocked(state *PublishState) error {
	records, httpErr := p.handler.SyncedRecords()
	if httpErr != nil {
		return httpErr
	}
	snap := &Channel.Snapshot{
		Seq:     state.LastPublished,
		Source:  p.Name,
		Ts:      time.Now().UTC().Format(time.RFC3339),
		Records: records,
	}
	if err := p.channel.PublishSnapshot(snap); err != nil {
		return err
	}
	p.Log(fmt.Sprintf("snapshot published at seq %d with %d records", snap.Seq, len(records)))
	state.HasSnapshot = true
	state.LastSnapshot = snap.Seq
	state.LastSnapshotTime = snap.Ts
	return p.outbox.SavePublishState(state)
}

// Status 返回 /{vDS}/config 中展示的发布状态。
func (p *Publisher) Status() map[string]interface{} {
	result := map[string]interface{}{
		"fingerprint": p.fingerprint,
	}
	if seq, err := p.outbox.CurrentSeq(); err == nil {
		result["outboxSeq"] = seq
	}
	if state, err := p.outbox.PublishState(); err == nil {
		result["lastPublishedSeq"] = state.LastPublished
		result["lastPublishTime"] = state.LastPublishTime
		if state.HasSnapshot {
			result["latestSnapshotSeq"] = state.LastSnapshot
			result["latestSnapshotTime"] = state.LastSnapshotTime
		}
	}
	p.statusLock.Lock()
	defer p.statusLock.Unlock()
	if p.lastError != "" {
		result["lastError"] = p.lastError
		result["lastErrorTime"] = p.lastErrorTime
	}
	return result
}
