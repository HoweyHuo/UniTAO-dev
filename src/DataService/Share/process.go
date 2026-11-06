package Share

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"DataService/Channel"
	"DataService/DataHandler"
	"DataService/DataJournal/ProcessIface"

	"github.com/salesforce/UniTAO/lib/Schema/Record"
	"github.com/salesforce/UniTAO/lib/Util/Http"
)

// ChannelPublish 是 shared vDS 的 Journal process：把每条变更写进 outbox。
//
// Journal entry 处理失败会重试，同一条 entry 可能被写进 outbox 两次（分配到不同 seq）。
// 这不影响结果：同一条记录的 entry 由同一个 worker 按顺序处理，重复的 entry 总在
// 后续 entry 之前，而每条变更都是完整记录的 upsert 或删除，镜像按 seq 应用后状态一致。
type ChannelPublish struct {
	source string
	outbox *Outbox
	log    *log.Logger
}

func NewChannelPublish(source string, outbox *Outbox, logger *log.Logger) *ChannelPublish {
	if logger == nil {
		logger = log.Default()
	}
	return &ChannelPublish{source: source, outbox: outbox, log: logger}
}

func (p *ChannelPublish) Name() string {
	return "ChannelPublish"
}

func (p *ChannelPublish) Log(message string) {
	p.log.Printf("%s[%s]: %s", p.Name(), p.source, message)
}

func (p *ChannelPublish) HandleType(dataType string, version string) (bool, error) {
	// schema 记录要按 id 区分内部类型，在 ProcessEntry 中判断
	return DataHandler.IsSyncedRecord(dataType, ""), nil
}

func recordKey(data map[string]interface{}) (string, string) {
	if data == nil {
		return "", ""
	}
	dataType, _ := data[Record.DataType].(string)
	dataId, _ := data[Record.DataId].(string)
	return dataType, dataId
}

// BuildChanges 把一条 Journal entry 转成 Channel 变更（不含 seq）。
// schema 归档时 entry 的 before/after id 不同（X → X__ver），转成 upsert(after) + delete(before)。
func BuildChanges(source string, entry *ProcessIface.JournalEntry) []Channel.Change {
	ts := time.Now().UTC().Format(time.RFC3339)
	changes := []Channel.Change{}
	afterType, afterId := recordKey(entry.After)
	beforeType, beforeId := recordKey(entry.Before)
	if entry.After != nil && DataHandler.IsSyncedRecord(afterType, afterId) {
		changes = append(changes, Channel.Change{
			Source: source,
			Op:     Channel.OpUpsert,
			Type:   afterType,
			Id:     afterId,
			Record: entry.After,
			Ts:     ts,
		})
	}
	if entry.Before != nil && (entry.After == nil || beforeId != afterId || beforeType != afterType) && DataHandler.IsSyncedRecord(beforeType, beforeId) {
		changes = append(changes, Channel.Change{
			Source: source,
			Op:     Channel.OpDelete,
			Type:   beforeType,
			Id:     beforeId,
			Ts:     ts,
		})
	}
	return changes
}

func (p *ChannelPublish) ProcessEntry(dataType string, dataId string, entry *ProcessIface.JournalEntry) *Http.HttpError {
	changes := BuildChanges(p.source, entry)
	if len(changes) == 0 {
		return nil
	}
	if err := p.outbox.Add(changes); err != nil {
		return Http.WrapError(err, fmt.Sprintf("failed to add [%s/%s] to outbox", dataType, dataId), http.StatusInternalServerError)
	}
	p.Log(fmt.Sprintf("[%s/%s] queued as seq %d-%d", dataType, dataId, changes[0].Seq, changes[len(changes)-1].Seq))
	return nil
}
