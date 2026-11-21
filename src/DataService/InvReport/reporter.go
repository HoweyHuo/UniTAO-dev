// Package InvReport 把本 vDS 的记录变更以记录位置（不含内容、不含操作）通知给
// Inventory Service（POST {inventory.url}/report），由 Inventory 推送给订阅者。
//
// 通知是 best-effort 的脏标记：发送失败只记日志，永不向 journal 返回错误——
// 条目照常归档，不重试、不滞留 head。订阅者收到位置后回查 Inventory 拿当前态，
// 漏掉的通知由订阅者自己的周期对账弥补（Inventory 是 source of truth）。
// 以 JournalProcess 形式挂载（setupJournal 的 extra 参数，local 与 shared vDS 都生效；
// mirrored vDS 没有 Journal，其副本变更从 shared 源头侧上报）；schema 等 DS 内部类型不上报。
package InvReport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"DataService/Common"
	"DataService/DataJournal/ProcessIface"

	"github.com/salesforce/UniTAO/lib/Util/Http"
)

const (
	Name       = "InvReport"
	ReportPath = "report"
	// DefaultReportTimeoutSec 单次上报的超时：Inventory 卡住不能拖住 journal worker。
	DefaultReportTimeoutSec = 5
)

// Change 是一条不携带内容与操作的记录位置；ds 由 Inventory 以请求里的 dsId 盖章。
type Change struct {
	Type string `json:"type"`
	Id   string `json:"id"`
}

// reportRequest 是 POST {inv}/report 的请求体。按数组设计，为批量上报留口。
type reportRequest struct {
	DsId    string   `json:"dsId"`
	Changes []Change `json:"changes"`
}

// Reporter 把变更位置通知给 Inventory 的 Journal process。
type Reporter struct {
	vdsPath string
	// invId 返回本 vDS 在 Inventory 的 dsId（self registration 分配，此前为空串）。
	invId  func() string
	invUrl string
	log    *log.Logger
	client *http.Client
}

// NewReporter 创建上报进程；invUrl 为空（DS 未链接 Inventory）时进程对任何类型都不生效。
func NewReporter(vdsPath string, invId func() string, invUrl string, logger *log.Logger) *Reporter {
	if logger == nil {
		logger = log.Default()
	}
	return &Reporter{
		vdsPath: vdsPath,
		invId:   invId,
		invUrl:  invUrl,
		log:     logger,
		client:  &http.Client{Timeout: time.Duration(DefaultReportTimeoutSec) * time.Second},
	}
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

// ProcessEntry 把一条 journal entry 的记录位置 POST 给 Inventory。
// 任何失败（dsId 未分配、Inventory 不可达、非 2xx）都只记日志并返回 nil：
// journal 条目照常归档，本进程不承诺送达。
func (r *Reporter) ProcessEntry(dataType string, dataId string, entry *ProcessIface.JournalEntry) *Http.HttpError {
	if entry == nil {
		return Http.NewHttpError(fmt.Sprintf("journal entry of [%s/%s] is nil", dataType, dataId), http.StatusNotModified)
	}
	r.ProcessChanges([]Change{{Type: dataType, Id: dataId}})
	return nil
}

// ProcessChanges 把一组记录位置 POST 给 Inventory；失败即弃，不重试。
func (r *Reporter) ProcessChanges(changes []Change) {
	if len(changes) == 0 {
		return
	}
	dsId := ""
	if r.invId != nil {
		dsId = r.invId()
	}
	if dsId == "" {
		r.Log(fmt.Sprintf("dsId not assigned yet (self registration pending), drop %d change notification(s)", len(changes)))
		return
	}
	url, err := Http.URLPathJoin(r.invUrl, ReportPath)
	if err != nil {
		r.Log(fmt.Sprintf("failed to build report url for inventory=[%s], Error: %s", r.invUrl, err))
		return
	}
	body, err := json.Marshal(reportRequest{DsId: dsId, Changes: changes})
	if err != nil {
		r.Log(fmt.Sprintf("failed to marshal report of %d change(s), Error: %s", len(changes), err))
		return
	}
	request, err := http.NewRequest(http.MethodPost, *url, bytes.NewReader(body))
	if err != nil {
		r.Log(fmt.Sprintf("failed to build report request to [%s], Error: %s", *url, err))
		return
	}
	request.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(request)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		r.Log(fmt.Sprintf("failed to report [%d] change(s) of vDS [%s] to inventory [%s], Error: %s", len(changes), r.vdsPath, r.invUrl, err))
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		r.Log(fmt.Sprintf("inventory [%s] rejected report of [%d] change(s) with code=%d, drop", r.invUrl, len(changes), resp.StatusCode))
	}
}
