// Package Subscription 实现记录变化订阅的 Inventory 侧：订阅者注册表与变更推送
// （report 到达即异步扇出，转发完即忘）。
//
// subscription 本身是 schema 描述的数据类型（与 inventory / referral 同一批 Inventory 内部类型）：
// 结构由 SchemaRecord 定义、经 GET /schema/subscription 自文档、注册载荷走 lib/Schema 校验。
// 加字段 = 提一版新 schema，不用改 Inventory 代码。记录 __id 是 endpoint 的哈希
// （记录 id 不允许含 "/"，见 lib/Schema JsonKey.InvalidKeyChars），endpoint 本体在 data 里，
// 同一 endpoint 幂等 upsert。unreachable / lastError / lastAttempt 是系统回填字段
// （indexTemplate / cmtIdx 的先例）：进 schema、Inventory 写、订阅者只读。
//
// 事件是 best-effort 的脏标记：载荷只有记录位置（ds/type/id），不带 op/ver/ts——
// 订阅者收到后回查 Inventory（GET /{type}/{ds}/{id}，404 = 删除）。推送失败不重试、
// 不向任何人报错，只在订阅者记录上标注不可达与最新错误（节流写回，给运维看，不驱动逻辑）；
// 成功的推送清除标注。Inventory 不为订阅者保留任何事件数据：没有 change 表、没有游标、
// 没有 ack。订阅者的周期性对账（GET /{type}?ver=1 拿 {ds,id,ver} 列表 diff）发现新增/删除与
// schema 版本升级；ver 是记录的 __ver，内容修改时不变，内容修改靠推送发现，需要更强保证的
// 订阅者自己回查内容（如比较内容指纹）。
//
// 重置：mirrored vDS 用 history 整体替换本表时不逐条上报，而是上报 reset（changes 为空）；
// 推送体里变成 resets: [dsId]，只按订阅的 ds 过滤、不按 types 过滤，订阅者把该 ds 的数据
// 全部按可能修改处理。
//
// 订阅者的存活用租约表达：注册/心跳是幂等 upsert（POST /subscription），超过 TTL 没有心跳
// 的记录被周期清扫（Sweep）删除——这是纯 GC，判错的代价只是下次心跳重新注册。
// 判死只看心跳缺席，不看推送失败：活着但 endpoint 坏的订阅者留在表里被运维看见。
//
// 旧版本记录的迁移（Sweep 每轮执行，Inventory 启动时先执行一次，幂等）：
//   - 没有 endpoint 的记录：拉取模式（PR #70，change 表 + 游标）时代的订阅，每次注册都新建一条、
//     没有 endpoint 也没有 registered，既不能心跳也不能按 endpoint 注销 → 直接删除；
//   - __id 不是 subId(endpoint) 的记录：推送模式初版（PR #75）以 endpoint 原文为 __id →
//     改存到 subId(endpoint)（已有同 endpoint 的新记录时保留新的），再删除旧记录，推送不中断。
package Subscription

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"Data/DbIface"

	"github.com/salesforce/UniTAO/lib/Schema"
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
	// schema 方言不支持数值下限（无 minimum 关键字），这条留在 Go 里。
	MinTtlSec = 60
	// DefaultPushTimeoutSec 单次推送的超时：慢订阅者不能占用推送资源太久。
	DefaultPushTimeoutSec = 5
	// EventQueueCap 分发队列容量：满则丢弃（best-effort，丢弃要显式）。
	EventQueueCap = 1024
	// MarkThrottleSec 失败标注写回的最小间隔：死订阅者 + 事件密集时避免每事件一次写。
	MarkThrottleSec = 60
)

// SchemaRecord 是 subscription 类型的 schema，与 inventory / referral 同形式的内嵌常量。
// 结构性校验（类型、必填、未知字段）由它负责；http(s) URL 与 ttlSec 下限是语义约束，
// schema 方言表达不了，留在 Go 侧（validateEndpoint / MinTtlSec）。
const SchemaRecord = `{
	"__id": "subscription",
	"__type": "schema",
	"__ver": "0.0.1",
	"data": {
		"name": "subscription",
		"description": "record change subscription: the Inventory pushes change positions (ds/type/id) to the endpoint as best-effort hints; failed pushes are never retried, only annotated. subscribers guarantee their own correctness via periodic reconciliation (GET /{type}?ver=1). the record id is a hash of the endpoint; re-registering the same endpoint is an idempotent upsert that refreshes the lease",
		"version": "0.0.1",
		"additionalProperties": false,
		"properties": {
			"endpoint": {
				"type": "string",
				"description": "http(s) URL the Inventory pushes change positions to; identifies the subscription"
			},
			"name": {
				"type": "string",
				"required": false,
				"description": "human readable label for operators"
			},
			"types": {
				"type": "array",
				"items": {
					"type": "string"
				},
				"required": false,
				"description": "filter: only changes of these data types are pushed; empty = all types"
			},
			"ds": {
				"type": "string",
				"required": false,
				"description": "filter: only changes from this Data Service are pushed; empty = all DS"
			},
			"ttlSec": {
				"type": "integer",
				"required": false,
				"description": "lease the subscriber declares for itself; heartbeats must arrive within it or the subscription is swept. default 86400, minimum 60"
			},
			"created": {
				"type": "string",
				"required": false,
				"description": "system: first registration time (RFC3339 UTC)"
			},
			"registered": {
				"type": "string",
				"required": false,
				"description": "system: last registration/heartbeat time (RFC3339 UTC); the lease expires at registered + ttlSec"
			},
			"unreachable": {
				"type": "boolean",
				"required": false,
				"description": "system: the last push failed; an annotation for operators, it never disables pushing"
			},
			"lastError": {
				"type": "string",
				"required": false,
				"description": "system: latest push error"
			},
			"lastAttempt": {
				"type": "string",
				"required": false,
				"description": "system: last push attempt time (RFC3339 UTC)"
			}
		}
	}
}`

// Subscription 是一个订阅者注册记录的运行时视图（__id = subId(Endpoint)，字段即 schema 的 data）。
type Subscription struct {
	Endpoint string   `json:"endpoint"` // 接收推送的 URL，业务主键：同一 endpoint 重复注册 = 幂等 upsert
	Name     string   `json:"name,omitempty"`
	Types    []string `json:"types,omitempty"`  // 空 = 所有类型
	Ds       string   `json:"ds,omitempty"`     // 空 = 所有 DS
	TtlSec   int      `json:"ttlSec,omitempty"` // 订阅者声明的租约；0 = 默认值
	Created  string   `json:"created"`
	// Registered 最近一次注册/心跳时间；超过 TTL 未心跳被 Sweep 删除。
	Registered string `json:"registered"`
	// Unreachable / LastError / LastAttempt 是系统回填的健康标注，只给人看。
	Unreachable bool   `json:"unreachable,omitempty"`
	LastError   string `json:"lastError,omitempty"`
	LastAttempt string `json:"lastAttempt,omitempty"`

	// recordId 是记录在库里的真实 __id（不进 data）。正常等于 subId(Endpoint)；
	// 旧版本留下的记录不等（见 Sweep 的迁移），删除必须按它而不是按 subId 重算。
	recordId string
}

// Change 是一条变更通知，只携带记录位置：订阅者经 `GET /{type}/{ds}/{id}` 回查，
// 记录不存在即删除。不带 op/ver/ts——那些都能由回查推导或对账替代。
type Change struct {
	Ds   string `json:"ds"`
	Type string `json:"type"`
	Id   string `json:"id"`
}

// ReportRequest 是 DS 上报（POST /report）的请求体；Ds 由服务端以 dsId 盖章。
// Reset 为 true 表示该 ds 的数据整体替换过（mirrored vDS 用 history 替换本表），
// 不逐条列出变化，此时 Changes 可以为空；推送时变成 PushRequest.Resets 里的一项。
type ReportRequest struct {
	DsId    string   `json:"dsId"`
	Changes []Change `json:"changes"`
	Reset   bool     `json:"reset,omitempty"`
}

// ReportResponse 只报告受理数量：受理 ≠ 送达，推送是 best-effort。
type ReportResponse struct {
	Accepted int `json:"accepted"`
}

// PushRequest 是推送给订阅者的 body：{"changes":[{ds,type,id}...],"resets":["dsId"...]}。
// Resets 列出整体替换过的 ds（订阅者应把该 ds 的数据全部按可能修改处理），只按订阅的 ds 过滤、
// 不按 types 过滤；没有重置时省略。只有重置时 Changes 是空数组 []（不是 null）。
// 两个字段都是推送模式之后新增的语义，旧订阅者忽略 resets 即可。
type PushRequest struct {
	Changes []Change `json:"changes"`
	Resets  []string `json:"resets,omitempty"`
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
	ops      *Schema.SchemaOps // subscription 类型的校验器（SchemaRecord 编译而来）
	markMu   sync.Mutex
	lastMark map[string]time.Time // 失败标注的节流（内存缓存，丢了只多写一次）
}

func NewManager(db DbIface.Database, opts Options, logger *log.Logger) (*Manager, error) {
	if logger == nil {
		logger = log.Default()
	}
	timeout := time.Duration(DefaultPushTimeoutSec) * time.Second
	if opts.PushTimeoutSec > 0 {
		timeout = time.Duration(opts.PushTimeoutSec) * time.Second
	}
	schemaRecord, err := Record.LoadStr(SchemaRecord)
	if err != nil {
		return nil, fmt.Errorf("failed to parse the embedded subscription schema. Error: %s", err)
	}
	ops, err := Schema.LoadSchemaOpsRecord(schemaRecord)
	if err != nil {
		return nil, fmt.Errorf("failed to compile the embedded subscription schema. Error: %s", err)
	}
	m := &Manager{
		db:       db,
		now:      time.Now,
		log:      logger,
		events:   make(chan *ReportRequest, EventQueueCap),
		client:   &http.Client{Timeout: timeout},
		ops:      ops,
		lastMark: map[string]time.Time{},
	}
	go m.dispatchLoop()
	return m, nil
}

// SetClock 替换时钟（测试用），同 Share.Publisher.SetClock。
func (m *Manager) SetClock(now func() time.Time) {
	m.now = now
}

func (m *Manager) nowStr() string {
	return m.now().UTC().Format(time.RFC3339)
}

// subId 由 endpoint 确定性生成记录 __id：记录 id 不允许含 "/"（REST 路径安全），
// 而 URL 一定有；哈希保住「同一 endpoint → 同一条记录」的幂等 upsert。
func subId(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return "sub-" + hex.EncodeToString(sum[:])[:16]
}

// Ingest 受理一批上报：校验、盖章、进入分发队列后即返回（202）。
// 队列满则整批丢弃并记日志——best-effort 通道里丢弃是设计行为，不阻塞 DS。
func (m *Manager) Ingest(req *ReportRequest) (*ReportResponse, *Http.HttpError) {
	if req.DsId == "" {
		return nil, Http.NewHttpError("missing dsId in change report", http.StatusBadRequest)
	}
	if len(req.Changes) == 0 && !req.Reset {
		return nil, Http.NewHttpError("missing changes in change report", http.StatusBadRequest)
	}
	if req.Changes == nil {
		req.Changes = []Change{}
	}
	for idx, change := range req.Changes {
		if change.Type == "" || change.Id == "" {
			return nil, Http.NewHttpError(fmt.Sprintf("changes[%d] missing type or id", idx), http.StatusBadRequest)
		}
	}
	select {
	case m.events <- req:
	default:
		m.log.Printf("event queue full, drop [%d] change(s) (reset=%t) of ds=[%s]", len(req.Changes), req.Reset, req.DsId)
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
	subs, hx := m.list()
	if hx != nil {
		m.log.Printf("failed to load subscriptions for dispatch: %s", hx.Message)
		return
	}
	for idx := range subs {
		sub := &subs[idx]
		if sub.Endpoint == "" || sub.recordId != subId(sub.Endpoint) {
			continue // 旧版本记录，等 Sweep 迁移或删除（迁移后的新记录照常收到推送）
		}
		matched := []Change{}
		for _, change := range req.Changes {
			if m.match(sub, &change) {
				matched = append(matched, change)
			}
		}
		var resets []string
		if req.Reset && (sub.Ds == "" || sub.Ds == req.DsId) {
			resets = []string{req.DsId} // 重置只按 ds 过滤：任何类型都可能变了
		}
		if len(matched) == 0 && len(resets) == 0 {
			continue
		}
		go m.push(sub, PushRequest{Changes: matched, Resets: resets})
	}
}

// push 向一个订阅者推送一批变更。失败只标注（节流），绝不重试；成功清除标注。
func (m *Manager) push(sub *Subscription, payload PushRequest) {
	body, err := json.Marshal(payload)
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
	sub, hx := m.loadByEndpoint(endpoint)
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

// Register 登记或心跳一个订阅者：入参是订阅者声明的 data（endpoint/name/types/ds/ttlSec），
// 先经 schema 校验（结构、类型、未知字段），再做 schema 表达不了的语义检查，最后幂等 upsert
// ——同一 endpoint 刷新 registered、更新过滤条件；保留 created 与健康标注
// （心跳只证明订阅者进程活着，不证明 endpoint 可达）。返回保存后的记录信封。
func (m *Manager) Register(input map[string]interface{}) (map[string]interface{}, *Http.HttpError) {
	if input == nil {
		input = map[string]interface{}{}
	}
	endpoint, _ := input["endpoint"].(string)
	if hx := validateEndpoint(endpoint); hx != nil {
		return nil, hx
	}
	if rawTypes, ok := input["types"].([]interface{}); ok {
		for _, item := range rawTypes {
			dataType, _ := item.(string)
			if dataType == "" {
				return nil, Http.NewHttpError("types must not contain empty value", http.StatusBadRequest)
			}
		}
	}
	// schema 校验订阅者声明的部分（系统回填字段此刻还不在里面，它们都是 required:false）
	record := Record.NewRecord(KeySubscription, stateVer, subId(endpoint), input)
	if err := m.ops.ValidateRecord(record); err != nil {
		return nil, Http.NewHttpError(fmt.Sprintf("invalid subscription data. Error: %s", err), http.StatusBadRequest)
	}
	// 语义默认值：租约缺省/过小时的钳位（schema 方言无数值下限）
	data := map[string]interface{}{}
	for key, value := range input {
		data[key] = value
	}
	ttl := DefaultTtlSec
	if raw, ok := input["ttlSec"].(float64); ok && int(raw) > 0 {
		ttl = int(raw)
	}
	if ttl < MinTtlSec {
		ttl = MinTtlSec
	}
	data["ttlSec"] = ttl

	m.lock.Lock()
	defer m.lock.Unlock()
	nowStr := m.nowStr()
	if existing, hx := m.loadByEndpoint(endpoint); hx == nil {
		// 系统回填字段跨心跳保留
		data["created"] = existing.Created
		data["unreachable"] = existing.Unreachable
		if existing.LastError != "" {
			data["lastError"] = existing.LastError
		}
		if existing.LastAttempt != "" {
			data["lastAttempt"] = existing.LastAttempt
		}
	} else {
		data["created"] = nowStr
	}
	data["registered"] = nowStr
	saved := Record.NewRecord(KeySubscription, stateVer, subId(endpoint), data)
	if err := m.db.Replace(KeySubscription, map[string]interface{}{
		Record.DataType: KeySubscription,
		Record.DataId:   saved.Id,
	}, saved.Map()); err != nil {
		return nil, wrapDbError("failed to save subscription", err)
	}
	return saved.Map(), nil
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

// List 返回所有订阅记录的信封（{__type, __id, __ver, data}），与其他数据的形状一致。
func (m *Manager) List() ([]map[string]interface{}, *Http.HttpError) {
	m.lock.Lock()
	defer m.lock.Unlock()
	result, err := m.db.Get(map[string]interface{}{
		DbIface.Table:   KeySubscription,
		Record.DataType: KeySubscription,
	})
	if err != nil {
		return nil, wrapDbError("failed to load subscriptions", err)
	}
	return result, nil
}

// list 返回订阅记录的运行时视图（内部用：过滤、清扫）。
func (m *Manager) list() ([]Subscription, *Http.HttpError) {
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
		Record.DataId:   subId(endpoint),
	}); err != nil {
		return wrapDbError(fmt.Sprintf("failed to delete subscription [%s]", endpoint), err)
	}
	return nil
}

// Sweep 租约清扫：先迁移旧版本记录（见包注释），再删除超过 TTL 未心跳的订阅
// （纯 GC，判错的代价 = 下次心跳重新注册）。删除一律按记录的真实 __id。
func (m *Manager) Sweep() error {
	m.lock.Lock()
	defer m.lock.Unlock()
	subs, err := m.listSubscriptions()
	if err != nil {
		return err
	}
	migrated := map[string]bool{} // 本轮迁移后的 endpoint：租约按迁移后的记录判断
	for idx := range subs {
		sub := &subs[idx]
		if sub.Endpoint == "" {
			m.log.Printf("subscription record [%s] has no endpoint (left by the pull-model version), remove it", sub.recordId)
			if err := m.deleteRecord(sub.recordId); err != nil {
				return err
			}
			continue
		}
		if sub.recordId == subId(sub.Endpoint) {
			continue
		}
		if err := m.migrateLegacyId(sub, subs); err != nil {
			return err
		}
		migrated[sub.Endpoint] = true
	}
	if len(migrated) > 0 {
		if subs, err = m.listSubscriptions(); err != nil {
			return err
		}
	}
	now := m.now()
	for idx := range subs {
		sub := &subs[idx]
		if sub.Endpoint == "" || sub.recordId != subId(sub.Endpoint) {
			continue
		}
		registered, err := time.Parse(time.RFC3339, sub.Registered)
		if err != nil {
			// 解析不了的健康/时间字段不该销毁注册：跳过，等下次心跳重写修复
			continue
		}
		ttl := time.Duration(sub.TtlSec) * time.Second
		if now.Sub(registered) > ttl {
			m.log.Printf("subscription [%s] lease expired (registered=[%s], ttl=%s), remove it", sub.Endpoint, sub.Registered, ttl)
			if err := m.deleteRecord(sub.recordId); err != nil {
				return err
			}
		}
	}
	return nil
}

// migrateLegacyId 把 __id 不是 subId(endpoint) 的旧记录改存到 subId(endpoint)：
// 已有新记录（订阅者升级后已重新注册）时保留新的，只删旧的。调用方须持有 m.lock。
func (m *Manager) migrateLegacyId(sub *Subscription, all []Subscription) error {
	newId := subId(sub.Endpoint)
	exists := false
	for idx := range all {
		if all[idx].recordId == newId {
			exists = true
			break
		}
	}
	if !exists {
		if err := m.saveSubscription(sub); err != nil {
			return fmt.Errorf("failed to migrate subscription [%s] from __id [%s]. Error: %s", sub.Endpoint, sub.recordId, err)
		}
	}
	m.log.Printf("subscription [%s] migrated from legacy __id [%s] to [%s] (kept existing: %t)", sub.Endpoint, sub.recordId, newId, exists)
	return m.deleteRecord(sub.recordId)
}

// deleteRecord 按真实 __id 删除一条订阅记录。调用方须持有 m.lock。
func (m *Manager) deleteRecord(recordId string) error {
	if err := m.db.Delete(KeySubscription, map[string]interface{}{
		Record.DataType: KeySubscription,
		Record.DataId:   recordId,
	}); err != nil {
		return fmt.Errorf("failed to delete subscription record [%s]. Error: %s", recordId, err)
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
		sub.recordId, _ = item[Record.DataId].(string)
		subs = append(subs, sub)
	}
	return subs, nil
}

// loadByEndpoint 以 endpoint 定位订阅并解析为运行时视图；不存在返回 404。调用方须持有 m.lock。
func (m *Manager) loadByEndpoint(endpoint string) (*Subscription, *Http.HttpError) {
	result, err := m.db.Get(map[string]interface{}{
		DbIface.Table:   KeySubscription,
		Record.DataType: KeySubscription,
		Record.DataId:   subId(endpoint),
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
	sub.recordId, _ = result[0][Record.DataId].(string)
	return sub, nil
}

func (m *Manager) saveSubscription(sub *Subscription) error {
	subMap, err := Json.CopyToMap(sub)
	if err != nil {
		return err
	}
	record := Record.NewRecord(KeySubscription, stateVer, subId(sub.Endpoint), subMap)
	return m.db.Replace(KeySubscription, map[string]interface{}{
		Record.DataType: KeySubscription,
		Record.DataId:   record.Id,
	}, record.Map())
}

func wrapDbError(msg string, err error) *Http.HttpError {
	return Http.NewHttpError(fmt.Sprintf("%s. Error: %s", msg, err), http.StatusInternalServerError)
}
