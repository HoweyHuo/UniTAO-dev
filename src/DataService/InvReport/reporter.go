// Package InvReport 把本 vDS 的每一条记录变更以元数据（不含记录内容）上报给
// Inventory Service（记录变化订阅系统：POST {inventory.url}/report）。
//
// 以 JournalProcess 形式挂载（setupJournal 的 extra 参数，local 与 shared vDS 都生效；
// mirrored vDS 没有 Journal，其副本变更从 shared 源头侧上报）。投递保证来自 journal
// 自带的重试语义：处理失败条目留在 head、约 5 秒重投、单条记录内有序、DS 重启后不丢，
// 即至少一次（at-least-once）投递，Inventory 侧按全局 seq 排序。
package InvReport

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"DataService/Common"
	"DataService/DataJournal/ProcessIface"

	"github.com/salesforce/UniTAO/lib/Schema/Record"
	"github.com/salesforce/UniTAO/lib/Util/Http"
)

const (
	Name       = "InvReport"
	ReportPath = "report"
	// OpUpsert / OpDelete 与 DataService/Channel 的操作名一致。
	OpUpsert = "upsert"
	OpDelete = "delete"
)

// Change 是一条不携带记录内容的变更元数据；订阅者按需经 Inventory 查询接口取记录本身。
type Change struct {
	Type string `json:"type"`
	Id   string `json:"id"`
	Op   string `json:"op"`
	Ver  string `json:"ver,omitempty"` // 记录 __ver
	Ts   string `json:"ts,omitempty"`  // 变更时间（journal entry 时间）
}

// reportRequest 是 POST {inv}/report 的请求体。当前每个 journal entry 发一条 change，
// 协议按数组设计，为后续批量上报留口。
type reportRequest struct {
	DsId    string   `json:"dsId"`
	Vds     string   `json:"vds,omitempty"` // vDS 路径（local、shared/{name}），仅作展示
	Changes []Change `json:"changes"`
}

// Reporter 是把变更元数据上报给 Inventory 的 Journal process。
type Reporter struct {
	vdsPath string
	// invId 返回本 vDS 在 Inventory 的 dsId（self registration 分配，此前为空串）。
	invId  func() string
	invUrl string
	log    *log.Logger
}

// NewReporter 创建上报进程；invUrl 为空（DS 未链接 Inventory）时进程对任何类型都不生效。
func NewReporter(vdsPath string, invId func() string, invUrl string, logger *log.Logger) *Reporter {
	if logger == nil {
		logger = log.Default()
	}
	return &Reporter{vdsPath: vdsPath, invId: invId, invUrl: invUrl, log: logger}
}

func (r *Reporter) Name() string {
	return Name
}

func (r *Reporter) Log(message string) {
	r.log.Printf("%s[%s]: %s", r.Name(), r.vdsPath, message)
}

// HandleType 对所有非 DS 内部类型返回 true；journal/outbox/channelState/schema/record/
// cmtIdx/cmtSubscriber 是 DS 自己的内部类型，不上报（避免反馈循环）。
func (r *Reporter) HandleType(dataType string, version string) (bool, error) {
	if r.invUrl == "" {
		return false, nil
	}
	if _, internal := Common.InternalTypes[dataType]; internal {
		return false, nil
	}
	return true, nil
}

// ProcessEntry 把一条 journal entry 转成一条变更并 POST 给 Inventory。
// After 为空 = 删除，否则 = upsert（创建与更新共用，Ver 取写入后的记录版本）。
func (r *Reporter) ProcessEntry(dataType string, dataId string, entry *ProcessIface.JournalEntry) *Http.HttpError {
	if entry == nil {
		return Http.NewHttpError(fmt.Sprintf("journal entry of [%s/%s] is nil", dataType, dataId), http.StatusNotModified)
	}
	change := Change{Type: dataType, Id: dataId, Ts: normalizeTs(entry.Time)}
	if entry.After == nil {
		change.Op = OpDelete
		if entry.Before != nil {
			change.Ver, _ = entry.Before[Record.Version].(string)
		}
	} else {
		change.Op = OpUpsert
		change.Ver, _ = entry.After[Record.Version].(string)
	}
	return r.ProcessChanges([]Change{change})
}

// goTimeLayout 与 time.Time.String() 的格式一致；journal 的 entry.Time 用它写入
// （DataJournal/journalLib.go：time.Now().String()），带 UTC 时区时形如
// "2026-11-20 07:29:57.240172258 +0000 UTC m=+131.568673789"。
const goTimeLayout = "2006-01-02 15:04:05.999999999 -0700 MST"

// normalizeTs 把 journal entry 时间归一化为 RFC3339 UTC。
// entry.Time 是 Go 默认 String() 格式（含单调时钟读数，截断 " m=" 之后解析）；
// 已是 RFC3339 则原样换算；解析不了（或为空）退回报送时刻，误差为 journal 处理延迟（秒级）。
func normalizeTs(entryTime string) string {
	now := func() string { return time.Now().UTC().Format(time.RFC3339) }
	if entryTime == "" {
		return now()
	}
	if t, err := time.Parse(time.RFC3339, entryTime); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	raw := entryTime
	if idx := strings.Index(raw, " m="); idx >= 0 {
		raw = raw[:idx]
	}
	if t, err := time.Parse(goTimeLayout, raw); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return now()
}

// ProcessChanges 把一组变更元数据 POST 给 Inventory。任何失败（传输错误或非 2xx）
// 都返回 503，journal 保留条目稍后重试；dsId 未分配（self registration 未完成）同样 503。
func (r *Reporter) ProcessChanges(changes []Change) *Http.HttpError {
	if len(changes) == 0 {
		return nil
	}
	dsId := ""
	if r.invId != nil {
		dsId = r.invId()
	}
	if dsId == "" {
		return Http.NewHttpError(fmt.Sprintf("dsId of vDS [%s] not assigned yet (self registration pending)", r.vdsPath), http.StatusServiceUnavailable)
	}
	url, err := Http.URLPathJoin(r.invUrl, ReportPath)
	if err != nil {
		return Http.NewHttpError(fmt.Sprintf("failed to build report url for inventory=[%s], Error: %s", r.invUrl, err), http.StatusServiceUnavailable)
	}
	payload := reportRequest{DsId: dsId, Vds: r.vdsPath, Changes: changes}
	resp, code, err := Http.SubmitPayload(*url, http.MethodPost, nil, payload)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return Http.NewHttpError(fmt.Sprintf("failed to report [%d] change(s) of vDS [%s] to inventory [%s] (code=%d), Error: %s",
			len(changes), r.vdsPath, r.invUrl, code, err), http.StatusServiceUnavailable)
	}
	return nil
}
