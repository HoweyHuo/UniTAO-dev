// Package Subscription 实现记录变化订阅的 Inventory 侧：Data Service 上报的变更日志（change）、
// 订阅登记与游标推进（pull/ack）、过期清理（Prune）。
//
// 变更行与 seq 计数器同存于 Inventory 自己数据库的 change 表：
// change/{seq（20 位补零）}，计数器为 changeState/seq——与 DataService/Share 的
// outbox/channelState 同构：先写行再推进计数器，中途失败时重试覆盖同一 seq，seq 永无空洞。
package Subscription

import (
	"fmt"
	"sort"
	"strconv"

	"Data/DbIface"

	"github.com/salesforce/UniTAO/lib/Schema/Record"
	"github.com/salesforce/UniTAO/lib/Util/Json"
)

const (
	// KeyChange 变更日志的类型名与表名；变更行与 changeState 计数器都在这张表里。
	KeyChange = "change"
	// KeySubscription 订阅记录的类型名与表名。
	KeySubscription = "subscription"
	// KeyChangeState seq 计数器的记录类型（changeState/seq）。
	KeyChangeState = "changeState"
	// SeqStateId 计数器记录的 id（changeState/seq）。
	SeqStateId = "seq"
	stateVer   = "0.0.1"

	// ReportPath 是 DS 上报变更的路由段（POST /report）。
	ReportPath = "report"
	// SubPath 是订阅路由段（/subscription[/...]）。
	SubPath = "subscription"
)

// Change 是一条变更元数据，不携带记录内容；订阅者按需经 Inventory 的查询接口取记录本身。
type Change struct {
	Seq  uint64 `json:"seq"`
	Ds   string `json:"ds"`
	Vds  string `json:"vds,omitempty"`
	Type string `json:"type"`
	Id   string `json:"id"`
	Op   string `json:"op"`
	Ver  string `json:"ver,omitempty"`
	Ts   string `json:"ts,omitempty"`
}

func seqId(seq uint64) string {
	return fmt.Sprintf("%020d", seq)
}

// toUint 兼容 JSON 反序列化出的各种数字表示。
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

// readHead 返回已提交的 seq 高水位（尚未分配过任何变更时为 0）。
// 计数器是提交点：seq <= 计数器的行才对外可见。
func readHead(db DbIface.Database) (uint64, error) {
	result, err := db.Get(map[string]interface{}{
		DbIface.Table:   KeyChange,
		Record.DataType: KeyChangeState,
		Record.DataId:   SeqStateId,
	})
	if err != nil {
		return 0, err
	}
	if len(result) == 0 {
		return 0, nil
	}
	data, _ := result[0][Record.Data].(map[string]interface{})
	return toUint(data["last"]), nil
}

// writeHead 推进计数器。只在变更行全部落库后调用（先写行、后推进）。
func writeHead(db DbIface.Database, last uint64) error {
	record := Record.NewRecord(KeyChangeState, stateVer, SeqStateId, map[string]interface{}{"last": last})
	return db.Replace(KeyChange, map[string]interface{}{
		Record.DataType: KeyChangeState,
		Record.DataId:   SeqStateId,
	}, record.Map())
}

// loadChanges 读取所有可见（seq <= highWater）的变更行，按 seq 升序返回。
// seq > highWater 的行是崩溃遗留的未提交行，对任何读者都不可见（重试会用相同 seq 覆盖）。
func loadChanges(db DbIface.Database, highWater uint64) ([]Change, error) {
	result, err := db.Get(map[string]interface{}{
		DbIface.Table:   KeyChange,
		Record.DataType: KeyChange,
	})
	if err != nil {
		return nil, err
	}
	changes := []Change{}
	for _, item := range result {
		data, _ := item[Record.Data].(map[string]interface{})
		change := Change{}
		if err := Json.CopyTo(data, &change); err != nil {
			return nil, err
		}
		if change.Seq == 0 || change.Seq > highWater {
			continue
		}
		changes = append(changes, change)
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Seq < changes[j].Seq })
	return changes, nil
}

// saveChange 写一条变更行（id 为 seq 的 20 位补零，与 Share 的 outbox 行同构）。
func saveChange(db DbIface.Database, change *Change) error {
	changeMap, err := Json.CopyToMap(change)
	if err != nil {
		return err
	}
	record := Record.NewRecord(KeyChange, stateVer, seqId(change.Seq), changeMap)
	return db.Replace(KeyChange, map[string]interface{}{
		Record.DataType: KeyChange,
		Record.DataId:   record.Id,
	}, record.Map())
}

// deleteChange 删除一条变更行。
func deleteChange(db DbIface.Database, id string) error {
	return db.Delete(KeyChange, map[string]interface{}{
		Record.DataType: KeyChange,
		Record.DataId:   id,
	})
}

// floorOf 返回现存最老变更的 seq；空 log 时返回 0（调用方以 head+1 为界）。
func floorOf(changes []Change) uint64 {
	if len(changes) == 0 {
		return 0
	}
	return changes[0].Seq
}
