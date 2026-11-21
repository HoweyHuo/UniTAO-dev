// Package Subscription 实现记录变化订阅的 Inventory 侧：订阅者注册表（endpoint 为主键，
// 存 Inventory 自己库的 subscription 表）与变更推送（report 到达即异步扇出，转发完即忘）。
//
// 事件是 best-effort 的脏标记：载荷只有记录位置（ds/type/id），不带 op/ver/ts——
// 订阅者收到后回查 Inventory（GET /{type}/{ds}/{id}，404 = 删除）。推送失败不重试、
// 不向任何人报错，只在订阅者记录上标注不可达与最新错误（节流写回，给运维看，不驱动逻辑）；
// 成功的推送清除标注。Inventory 不为订阅者保留任何事件数据：没有 change 表、没有游标、
// 没有 ack。正确性由订阅者自己的周期性对账承担（GET /{type}?ver=1 拿 {ds,id,ver} 列表 diff）。
//
// 订阅者的存活用租约表达：注册/心跳是幂等 upsert（POST /subscription），超过 TTL 没有心跳
// 的记录被周期清扫（Sweep）删除——这是纯 GC，判错的代价只是下次心跳重新注册。
// 判死只看心跳缺席，不看推送失败：活着但 endpoint 坏的订阅者留在表里被运维看见。
package Subscription

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"Data/DbIface"

	"github.com/salesforce/UniTAO/lib/Schema/Record"
	"github.com/salesforce/UniTAO/lib/Util/Http"
	"github.com/salesforce/UniTAO/lib/Util/Json"
)

const (
	// KeySubscription 订阅注册表的类型名与表名。
	KeySubscription = "subscription"
	stateVer        = "0.0.1"

	// ReportPath 是 DS 上报变更的路由段（POST /report）。
	ReportPath = "report"
	// SubPath 是订阅路由段（/subscription）。
	SubPath = "subscription"

	// DefaultTtlSec 订阅者未声明 ttlSec 时的默认租约（24 小时）。
	DefaultTtlSec = 86400
	// MinTtlSec 租约下限：心跳间隔必须远小于租约，过短的租约没有意义。
	MinTtlSec = 60
	// DefaultPushTimeoutSec 单次推送的超时：慢订阅者不能占用推送资源太久。
	DefaultPushTimeoutSec = 5
	// EventQueueCap 分发队列容量：满则丢弃（best-effort，丢弃要显式）。
	EventQueueCap = 1024
	// MarkThrottleSec 失败标注写回的最小间隔：死订阅者 + 事件密集时避免每事件一次写。
	MarkThrottleSec = 60
)

// Subscription 是一个订阅者的注册记录（__id = Endpoint）。
type Subscription struct {
	Endpoint string   `json:"endpoint"` // 接收推送的 URL，主键：同一 endpoint 重复注册 = 幂等 upsert
	Name     string   `json:"name,omitempty"`
	Types    []string `json:"types,omitempty"`  // 空 = 所有类型
	Ds       string   `json:"ds,omitempty"`     // 空 = 所有 DS
	TtlSec   int      `json:"ttlSec,omitempty"` // 订阅者声明的租约；0 = 默认值
	Created  string   `json:"created"`
	// Registered 最近一次注册/心跳时间；超过 TTL 未心跳被 Sweep 删除。
	Registered string `json:"registered"`
	// Unreachable / LastError / LastAttempt 是最近一次推送的健康标注，只给人看。
	Unreachable bool   `json:"unreachable,omitempty"`
	LastError   string `json:"lastError,omitempty"`
	LastAttempt string `json:"lastAttempt,omitempty"`
}

// RegisterRequest 注册（或心跳）订阅。同一 endpoint 幂等 upsert：刷新租约、更新过滤条件。
type RegisterRequest struct {
	Endpoint string   `json:"endpoint"`
	Name     string   `json:"name,omitempty"`
	Types    []string `json:"types,omitempty"`
	Ds       string   `json:"ds,omitempty"`
	TtlSec   int      `json:"ttlSec,omitempty"`
}

// Change 是一条变更通知，只携带记录位置：订阅者经 `GET /{type}/{ds}/{id}` 回查，
// 记录不存在即删除。不带 op/ver/ts——那些都能由回查推导或对账替代。
type Change struct {
	Ds   string `json:"ds"`
	Type string `json:"type"`
	Id   string `json:"id"`
}

// ReportRequest 是 DS 上报（POST /report）的请求体；Ds 由服务端以 dsId 盖章。
type ReportRequest struct {
	DsId    string   `json:"dsId"`
	Changes []Change `json:"changes"`
}

// ReportResponse 只报告受理数量：受理 ≠ 送达，推送是 best-effort。
type ReportResponse struct {
	Accepted int `json:"accepted"`
}

// PushRequest 是推送给订阅者的 body。
type PushRequest struct {
	Changes []Change `json:"changes"`
}

// Options 是推送参数，<=0 时使用默认值。
type Options struct {
	PushTimeoutSec int
}

// Manager 管理订阅注册表与推送扇出。单进程假设：注册表操作经一把进程内锁串行
// （与 DataSync 的 syncLock 同一并发模型）；推送在独立 goroutine 里带超时执行。
type Manager struct {
	db       DbIface.Database
	lock     sync.Mutex
	now      func() time.Time
	log      *log.Logger
	events   chan *ReportRequest
	client   *http.Client
	markMu   sync.Mutex
	lastMark map[string]time.Time // 失败标注的节流（内存缓存，丢了只多写一次）
}

func NewManager(db DbIface.Database, opts Options, logger *log.Logger) *Manager {
	if logger == nil {
		logger = log.Default()
	}
	timeout := time.Duration(DefaultPushTimeoutSec) * time.Second
	if opts.PushTimeoutSec > 0 {
		timeout = time.Duration(opts.PushTimeoutSec) * time.Second
	}
	m := &Manager{
		db:       db,
		now:      time.Now,
		log:      logger,
		events:   make(chan *ReportRequest, EventQueueCap),
		client:   &http.Client{Timeout: timeout},
		lastMark: map[string]time.Time{},
	}
	go m.dispatchLoop()
	return m
}

// SetClock 替换时钟（测试用），同 Share.Publisher.SetClock。
func (m *Manager) SetClock(now func() time.Time) {
	m.now = now
}

func (m *Manager) nowStr() string {
	return m.now().UTC().Format(time.RFC3339)
}

// Ingest 受理一批上报：校验、盖章、进入分发队列后即返回（202）。
// 队列满则整批丢弃并记日志——best-effort 通道里丢弃是设计行为，不阻塞 DS。
func (m *Manager) Ingest(req *ReportRequest) (*ReportResponse, *Http.HttpError) {
	if req.DsId == "" {
		return nil, Http.NewHttpError("missing dsId in change report", http.StatusBadRequest)
	}
	if len(req.Changes) == 0 {
		return nil, Http.NewHttpError("missing changes in change report", http.StatusBadRequest)
	}
	for idx, change := range req.Changes {
		if change.Type == "" || change.Id == "" {
			return nil, Http.NewHttpError(fmt.Sprintf("changes[%d] missing type or id", idx), http.StatusBadRequest)
		}
	}
	select {
	case m.events <- req:
	default:
		m.log.Printf("event queue full, drop [%d] change(s) of ds=[%s]", len(req.Changes), req.DsId)
	}
	return &ReportResponse{Accepted: len(req.Changes)}, nil
}

// dispatchLoop 从队列取上报并扇出；Dispatch 单独导出供测试同步执行。
func (m *Manager) dispatchLoop() {
	for req := range m.events {
		m.Dispatch(req)
	}
}

// Dispatch 把一批变更推送给所有匹配的订阅者。Ds 在这里以 dsId 盖章（推送位置可直接用于
// GET /{type}/{ds}/{id}）。每个订阅者一个 goroutine、各自带超时，慢订阅者互不拖累。
// 转发完即忘：不记录已推送什么，也不为失败重试。
func (m *Manager) Dispatch(req *ReportRequest) {
	for idx := range req.Changes {
		req.Changes[idx].Ds = req.DsId
	}
	subs, hx := m.List()
	if hx != nil {
		m.log.Printf("failed to load subscriptions for dispatch: %s", hx.Message)
		return
	}
	for idx := range subs {
		sub := &subs[idx]
		matched := []Change{}
		for _, change := range req.Changes {
			if m.match(sub, &change) {
				matched = append(matched, change)
			}
		}
		if len(matched) == 0 {
			continue
		}
		go m.push(sub, matched)
	}
}

// push 向一个订阅者推送一批变更。失败只标注（节流），绝不重试；成功清除标注。
func (m *Manager) push(sub *Subscription, changes []Change) {
	body, err := json.Marshal(PushRequest{Changes: changes})
	if err != nil {
		m.log.Printf("failed to marshal push to [%s]: %s", sub.Endpoint, err)
		return
	}
	request, err := http.NewRequest(http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		m.markResult(sub.Endpoint, false, fmt.Sprintf("invalid endpoint: %s", err))
		return
	}
	request.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(request)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err == nil && resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		m.markResult(sub.Endpoint, true, "")
		return
	}
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	m.markResult(sub.Endpoint, false, fmt.Sprintf("push failed (code=%d): %s", status, err))
}

// markResult 记录推送结果：成功且原标注为不可达 → 清除标注（一次写）；
// 失败 → 标注不可达 + 最新错误，按 MarkThrottleSec 节流写回。
func (m *Manager) markResult(endpoint string, ok bool, errMsg string) {
	m.markMu.Lock()
	last, throttled := m.lastMark[endpoint]
	if !ok {
		m.lastMark[endpoint] = m.now()
	}
	m.markMu.Unlock()
	m.lock.Lock()
	defer m.lock.Unlock()
	sub, hx := m.loadSubscription(endpoint)
	if hx != nil {
		return // 订阅者已注销，标注无处可写
	}
	if ok {
		if !sub.Unreachable {
			return // 健康路径上的成功不产生任何写
		}
		sub.Unreachable = false
		sub.LastError = ""
		sub.LastAttempt = m.nowStr()
		if err := m.saveSubscription(sub); err != nil {
			m.log.Printf("failed to clear unreachable mark of [%s]: %s", endpoint, err)
		}
		return
	}
	now := m.now()
	if sub.Unreachable && sub.LastError == errMsg && throttled && now.Sub(last) < time.Duration(MarkThrottleSec)*time.Second {
		return // 同样的错误、节流窗口内：不重复写
	}
	sub.Unreachable = true
	sub.LastError = errMsg
	sub.LastAttempt = m.nowStr()
	if err := m.saveSubscription(sub); err != nil {
		m.log.Printf("failed to mark [%s] unreachable: %s", endpoint, err)
	}
}

// Register 登记或心跳一个订阅者：同一 endpoint 幂等 upsert，刷新 Registered、更新过滤条件；
// 保留 Created 与健康标注（心跳只证明订阅者进程活着，不证明 endpoint 可达）。
func (m *Manager) Register(req *RegisterRequest) (*Subscription, *Http.HttpError) {
	if req == nil {
		req = &RegisterRequest{}
	}
	if hx := validateEndpoint(req.Endpoint); hx != nil {
		return nil, hx
	}
	for _, dataType := range req.Types {
		if dataType == "" {
			return nil, Http.NewHttpError("types must not contain empty value", http.StatusBadRequest)
		}
	}
	ttl := req.TtlSec
	if ttl <= 0 {
		ttl = DefaultTtlSec
	}
	if ttl < MinTtlSec {
		ttl = MinTtlSec
	}
	m.lock.Lock()
	defer m.lock.Unlock()
	nowStr := m.nowStr()
	sub := &Subscription{
		Endpoint:   req.Endpoint,
		Name:       req.Name,
		Types:      req.Types,
		Ds:         req.Ds,
		TtlSec:     ttl,
		Registered: nowStr,
	}
	if existing, hx := m.loadSubscription(req.Endpoint); hx == nil {
		sub.Created = existing.Created
		sub.Unreachable = existing.Unreachable
		sub.LastError = existing.LastError
		sub.LastAttempt = existing.LastAttempt
	} else {
		sub.Created = nowStr
	}
	if err := m.saveSubscription(sub); err != nil {
		return nil, wrapDbError("failed to save subscription", err)
	}
	return sub, nil
}

func validateEndpoint(endpoint string) *Http.HttpError {
	if endpoint == "" {
		return Http.NewHttpError("missing endpoint in subscription", http.StatusBadRequest)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return Http.NewHttpError(fmt.Sprintf("invalid endpoint=[%s], expect http(s) URL", endpoint), http.StatusBadRequest)
	}
	return nil
}

// List 返回所有订阅记录（含健康标注）。
func (m *Manager) List() ([]Subscription, *Http.HttpError) {
	m.lock.Lock()
	defer m.lock.Unlock()
	subs, err := m.listSubscriptions()
	if err != nil {
		return nil, wrapDbError("failed to load subscriptions", err)
	}
	return subs, nil
}

// Delete 注销订阅；幂等，记录不存在也返回成功。
func (m *Manager) Delete(endpoint string) *Http.HttpError {
	if endpoint == "" {
		return Http.NewHttpError("missing endpoint, use DELETE /subscription?url={endpoint}", http.StatusBadRequest)
	}
	m.lock.Lock()
	defer m.lock.Unlock()
	if err := m.db.Delete(KeySubscription, map[string]interface{}{
		Record.DataType: KeySubscription,
		Record.DataId:   endpoint,
	}); err != nil {
		return wrapDbError(fmt.Sprintf("failed to delete subscription [%s]", endpoint), err)
	}
	return nil
}

// Sweep 租约清扫：删除超过 TTL 未心跳的订阅（纯 GC，判错的代价 = 下次心跳重新注册）。
func (m *Manager) Sweep() error {
	m.lock.Lock()
	defer m.lock.Unlock()
	subs, err := m.listSubscriptions()
	if err != nil {
		return err
	}
	now := m.now()
	for idx := range subs {
		sub := &subs[idx]
		registered, err := time.Parse(time.RFC3339, sub.Registered)
		if err != nil {
			// 解析不了的健康/时间字段不该销毁注册：跳过，等下次心跳重写修复
			continue
		}
		ttl := time.Duration(sub.TtlSec) * time.Second
		if now.Sub(registered) > ttl {
			m.log.Printf("subscription [%s] lease expired (registered=[%s], ttl=%s), remove it", sub.Endpoint, sub.Registered, ttl)
			if err := m.db.Delete(KeySubscription, map[string]interface{}{
				Record.DataType: KeySubscription,
				Record.DataId:   sub.Endpoint,
			}); err != nil {
				return fmt.Errorf("failed to delete subscription [%s]. Error: %s", sub.Endpoint, err)
			}
		}
	}
	return nil
}

// match 判断一条变更是否命中订阅的过滤条件：types 空 = 所有类型，ds 空 = 所有 DS。
func (m *Manager) match(sub *Subscription, change *Change) bool {
	if len(sub.Types) > 0 {
		found := false
		for _, dataType := range sub.Types {
			if dataType == change.Type {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if sub.Ds != "" && sub.Ds != change.Ds {
		return false
	}
	return true
}

func (m *Manager) listSubscriptions() ([]Subscription, error) {
	result, err := m.db.Get(map[string]interface{}{
		DbIface.Table:   KeySubscription,
		Record.DataType: KeySubscription,
	})
	if err != nil {
		return nil, err
	}
	subs := []Subscription{}
	for _, item := range result {
		data, _ := item[Record.Data].(map[string]interface{})
		sub := Subscription{}
		if err := Json.CopyTo(data, &sub); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, nil
}

// loadSubscription 读取并解析一条订阅记录；不存在返回 404。调用方须持有 m.lock。
func (m *Manager) loadSubscription(endpoint string) (*Subscription, *Http.HttpError) {
	result, err := m.db.Get(map[string]interface{}{
		DbIface.Table:   KeySubscription,
		Record.DataType: KeySubscription,
		Record.DataId:   endpoint,
	})
	if err != nil {
		return nil, wrapDbError(fmt.Sprintf("failed to load subscription [%s]", endpoint), err)
	}
	if len(result) == 0 {
		return nil, Http.NewHttpError(fmt.Sprintf("subscription [%s] does not exist", endpoint), http.StatusNotFound)
	}
	data, _ := result[0][Record.Data].(map[string]interface{})
	sub := &Subscription{}
	if err := Json.CopyTo(data, sub); err != nil {
		return nil, wrapDbError(fmt.Sprintf("failed to parse subscription [%s]", endpoint), err)
	}
	return sub, nil
}

func (m *Manager) saveSubscription(sub *Subscription) error {
	subMap, err := Json.CopyToMap(sub)
	if err != nil {
		return err
	}
	record := Record.NewRecord(KeySubscription, stateVer, sub.Endpoint, subMap)
	return m.db.Replace(KeySubscription, map[string]interface{}{
		Record.DataType: KeySubscription,
		Record.DataId:   sub.Endpoint,
	}, record.Map())
}

func wrapDbError(msg string, err error) *Http.HttpError {
	return Http.NewHttpError(fmt.Sprintf("%s. Error: %s", msg, err), http.StatusInternalServerError)
}
