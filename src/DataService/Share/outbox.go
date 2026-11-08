// Package Share 实现 shared vDS 的发布（outbox、ChannelPublish、snapshot）与 mirrored vDS 的同步应用。
package Share

import (
	"fmt"
	"sort"
	"strconv"
	"sync"

	"Data/DbIface"
	"DataService/Channel"
	"DataService/Common"

	"github.com/salesforce/UniTAO/lib/Schema/Record"
	"github.com/salesforce/UniTAO/lib/Util/Json"
)

const (
	stateVer       = "0.0.1"
	seqStateId     = "seq"
	publishStateId = "publish"
)

// Outbox 是 shared vDS 库里持久化的待发布变更，seq 单调递增，单实例写入。
// 记录存在 vDS 自己的数据表中：outbox/{seq（20 位补零）}，计数器为 channelState/seq。
type Outbox struct {
	db    DbIface.Database
	table string
	lock  sync.Mutex
}

// PublishState 记录发布进度。
type PublishState struct {
	LastPublished   uint64 `json:"lastPublished"`
	LastPublishTime string `json:"lastPublishTime,omitempty"`
	// LastCompactTime 为上次检查 / 压缩通道历史的时间
	LastCompactTime string `json:"lastCompactTime,omitempty"`
}

func NewOutbox(db DbIface.Database, table string) *Outbox {
	return &Outbox{db: db, table: table}
}

func seqId(seq uint64) string {
	return fmt.Sprintf("%020d", seq)
}

func (o *Outbox) getState(id string) (map[string]interface{}, error) {
	result, err := o.db.Get(map[string]interface{}{
		DbIface.Table:   o.table,
		Record.DataType: Common.KeyChannelState,
		Record.DataId:   id,
	})
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, nil
	}
	data, _ := result[0][Record.Data].(map[string]interface{})
	return data, nil
}

func (o *Outbox) putState(id string, data map[string]interface{}) error {
	record := Record.NewRecord(Common.KeyChannelState, stateVer, id, data)
	return o.db.Replace(o.table, map[string]interface{}{
		Record.DataType: Common.KeyChannelState,
		Record.DataId:   id,
	}, record.Map())
}

func toUint(value interface{}) uint64 {
	switch v := value.(type) {
	case float64:
		return uint64(v)
	case int:
		return uint64(v)
	case int64:
		return uint64(v)
	case uint64:
		return v
	case string:
		n, _ := strconv.ParseUint(v, 10, 64)
		return n
	}
	return 0
}

// CurrentSeq 返回最后分配的 seq（尚未分配过时为 0）。
func (o *Outbox) CurrentSeq() (uint64, error) {
	o.lock.Lock()
	defer o.lock.Unlock()
	data, err := o.getState(seqStateId)
	if err != nil || data == nil {
		return 0, err
	}
	return toUint(data["last"]), nil
}

// Add 为一批变更分配连续的 seq 并写入 outbox。
func (o *Outbox) Add(changes []Channel.Change) error {
	if len(changes) == 0 {
		return nil
	}
	o.lock.Lock()
	defer o.lock.Unlock()
	data, err := o.getState(seqStateId)
	if err != nil {
		return err
	}
	var last uint64
	if data != nil {
		last = toUint(data["last"])
	}
	for idx := range changes {
		changes[idx].Seq = last + uint64(idx) + 1
	}
	// 先写 outbox 记录再推进计数器：中途失败时计数器不动，重试会用相同 seq 覆盖写入，
	// seq 不会出现空号。发布方只发布 seq <= 计数器的记录，不会发出未提交的记录。
	for _, change := range changes {
		changeMap, err := Json.CopyToMap(change)
		if err != nil {
			return err
		}
		record := Record.NewRecord(Common.KeyOutbox, stateVer, seqId(change.Seq), changeMap)
		if err := o.db.Replace(o.table, map[string]interface{}{
			Record.DataType: Common.KeyOutbox,
			Record.DataId:   record.Id,
		}, record.Map()); err != nil {
			return err
		}
	}
	return o.putState(seqStateId, map[string]interface{}{"last": last + uint64(len(changes))})
}

// List 返回 seq > after 的变更，按 seq 升序，最多 limit 条（limit<=0 不限）。
func (o *Outbox) List(after uint64, limit int) ([]Channel.Change, error) {
	result, err := o.db.Get(map[string]interface{}{
		DbIface.Table:   o.table,
		Record.DataType: Common.KeyOutbox,
	})
	if err != nil {
		return nil, err
	}
	changes := []Channel.Change{}
	for _, item := range result {
		data, _ := item[Record.Data].(map[string]interface{})
		change := Channel.Change{}
		if err := Json.CopyTo(data, &change); err != nil {
			return nil, err
		}
		if change.Seq > after {
			changes = append(changes, change)
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Seq < changes[j].Seq })
	if limit > 0 && len(changes) > limit {
		changes = changes[:limit]
	}
	return changes, nil
}

// DeleteUpTo 删除 seq <= upTo 的 outbox 记录。
func (o *Outbox) DeleteUpTo(upTo uint64) error {
	result, err := o.db.Get(map[string]interface{}{
		DbIface.Table:   o.table,
		Record.DataType: Common.KeyOutbox,
	})
	if err != nil {
		return err
	}
	for _, item := range result {
		id, _ := item[Record.DataId].(string)
		seq, _ := strconv.ParseUint(id, 10, 64)
		if seq > upTo {
			continue
		}
		if err := o.db.Delete(o.table, map[string]interface{}{
			Record.DataType: Common.KeyOutbox,
			Record.DataId:   id,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (o *Outbox) PublishState() (*PublishState, error) {
	data, err := o.getState(publishStateId)
	if err != nil {
		return nil, err
	}
	state := &PublishState{}
	if data != nil {
		if err := Json.CopyTo(data, state); err != nil {
			return nil, err
		}
	}
	return state, nil
}

func (o *Outbox) SavePublishState(state *PublishState) error {
	data, err := Json.CopyToMap(state)
	if err != nil {
		return err
	}
	return o.putState(publishStateId, data)
}
