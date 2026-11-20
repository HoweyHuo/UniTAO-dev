package Subscription

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"Data/DbIface"

	"github.com/salesforce/UniTAO/lib/Schema/Record"
	"github.com/salesforce/UniTAO/lib/Util/Http"
	"github.com/salesforce/UniTAO/lib/Util/Json"
)

const (
	// OpUpsert / OpDelete 与 DataService/Channel 的操作名一致。
	OpUpsert = "upsert"
	OpDelete = "delete"

	// DefaultRetentionHours 变更保留时长：即使订阅者落后也最多保留 7 天。
	DefaultRetentionHours = 168
	// DefaultSubscriberTimeoutHours 订阅者无活动（pull/ack 都算）判死的时长。
	DefaultSubscriberTimeoutHours = 24
	// DefaultPullLimit / MaxPullLimit 单次拉取的扫描行数上下限。
	DefaultPullLimit = 100
	MaxPullLimit     = 1000
)

// Subscription 是一个订阅者的登记记录（__id = Id）。
type Subscription struct {
	Id      string   `json:"id"`
	Name    string   `json:"name,omitempty"`
	Types   []string `json:"types,omitempty"` // 空 = 所有类型
	Ds      string   `json:"ds,omitempty"`    // 空 = 所有 DS
	Cursor  uint64   `json:"cursor"`          // 已确认消费到的 seq
	Created string   `json:"created"`
	Updated string   `json:"updated"` // 最近一次 pull/ack 的活跃时间
}

// RegisterRequest 注册订阅。From 为 nil 时从注册时刻的 head 开始（只收今后的变更）；
// From=0 表示回放保留期内的全部历史；From=N>0 表示从 seq=N+1 开始（落在已清理区间时首次 pull 得 410）。
type RegisterRequest struct {
	Name  string   `json:"name,omitempty"`
	Types []string `json:"types,omitempty"`
	Ds    string   `json:"ds,omitempty"`
	From  *uint64  `json:"from,omitempty"`
}

type RegisterResponse struct {
	SubscriberId string `json:"subscriberId"`
	Cursor       uint64 `json:"cursor"` // 实际起始游标
	Head         uint64 `json:"head"`
}

// PullResponse 是 GET /subscription/{id}/changes 的应答。
// Next 是本次扫描水位：订阅者应 ack 该值（或下次以 after=Next 续读）。
type PullResponse struct {
	SubscriberId string   `json:"subscriberId"`
	After        uint64   `json:"after"`
	Head         uint64   `json:"head"`
	Floor        uint64   `json:"floor,omitempty"` // 现存最老 seq（空 log 时为 head+1）
	Changes      []Change `json:"changes"`
	Next         uint64   `json:"next"`
}

type AckRequest struct {
	Cursor uint64 `json:"cursor"`
}

type AckResponse struct {
	SubscriberId string `json:"subscriberId"`
	Cursor       uint64 `json:"cursor"`
}

type StatusResponse struct {
	Subscription Subscription `json:"subscription"`
	Head         uint64       `json:"head"`
}

// ReportRequest 是 DS 上报（POST /report）的请求体；Changes 里的 Seq/Ds/Vds 由服务端填充。
type ReportRequest struct {
	DsId    string   `json:"dsId"`
	Vds     string   `json:"vds,omitempty"`
	Changes []Change `json:"changes"`
}

type ReportResponse struct {
	Accepted int    `json:"accepted"`
	Head     uint64 `json:"head"`
}

// Options 是 Prune 的清理参数，<=0 时使用默认值。
type Options struct {
	RetentionHours         int
	SubscriberTimeoutHours int
}

// Manager 管理 Inventory 自己库里的 change 日志与订阅记录。
// 单进程假设：全部操作经一把进程内锁串行，与 DataSync 的 syncLock 同一并发模型。
type Manager struct {
	db        DbIface.Database
	lock      sync.Mutex
	now       func() time.Time
	retention time.Duration
	timeout   time.Duration
	log       *log.Logger
}

func NewManager(db DbIface.Database, opts Options, logger *log.Logger) *Manager {
	if logger == nil {
		logger = log.Default()
	}
	retention := time.Duration(DefaultRetentionHours) * time.Hour
	if opts.RetentionHours > 0 {
		retention = time.Duration(opts.RetentionHours) * time.Hour
	}
	timeout := time.Duration(DefaultSubscriberTimeoutHours) * time.Hour
	if opts.SubscriberTimeoutHours > 0 {
		timeout = time.Duration(opts.SubscriberTimeoutHours) * time.Hour
	}
	return &Manager{
		db:        db,
		now:       time.Now,
		retention: retention,
		timeout:   timeout,
		log:       logger,
	}
}

// SetClock 替换时钟（测试用），同 Share.Publisher.SetClock。
func (m *Manager) SetClock(now func() time.Time) {
	m.now = now
}

// SetRetention 替换保留时长（测试用）。
func (m *Manager) SetRetention(d time.Duration) {
	m.retention = d
}

// SetSubscriberTimeout 替换订阅者判死时长（测试用）。
func (m *Manager) SetSubscriberTimeout(d time.Duration) {
	m.timeout = d
}

func (m *Manager) nowStr() string {
	return m.now().UTC().Format(time.RFC3339)
}

// Ingest 同步落库一批上报的变更：分配连续全局 seq（先写行、后推进计数器），
// 返回即代表已持久（2xx 才会让 DS 的 journal 归档条目）。
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
		if change.Op != OpUpsert && change.Op != OpDelete {
			return nil, Http.NewHttpError(fmt.Sprintf("changes[%d] invalid op=[%s], only [%s, %s] supported", idx, change.Op, OpUpsert, OpDelete), http.StatusBadRequest)
		}
	}
	m.lock.Lock()
	defer m.lock.Unlock()
	last, err := readHead(m.db)
	if err != nil {
		return nil, wrapDbError("failed to read change seq counter", err)
	}
	for idx := range req.Changes {
		change := &req.Changes[idx]
		change.Seq = last + uint64(idx) + 1
		change.Ds = req.DsId
		change.Vds = req.Vds
		// 先写行再推进计数器：中途失败计数器不动，DS 重试覆盖同一 seq，seq 永无空洞。
		if err := saveChange(m.db, change); err != nil {
			return nil, wrapDbError(fmt.Sprintf("failed to save change [%s/%s]", change.Type, change.Id), err)
		}
	}
	if err := writeHead(m.db, last+uint64(len(req.Changes))); err != nil {
		return nil, wrapDbError("failed to advance change seq counter", err)
	}
	return &ReportResponse{Accepted: len(req.Changes), Head: last + uint64(len(req.Changes))}, nil
}

// Register 登记一个订阅者，返回分配的 id 与实际起始游标。
func (m *Manager) Register(req *RegisterRequest) (*RegisterResponse, *Http.HttpError) {
	if req == nil {
		req = &RegisterRequest{}
	}
	for _, dataType := range req.Types {
		if dataType == "" {
			return nil, Http.NewHttpError("types must not contain empty value", http.StatusBadRequest)
		}
	}
	m.lock.Lock()
	defer m.lock.Unlock()
	head, err := readHead(m.db)
	if err != nil {
		return nil, wrapDbError("failed to read change seq counter", err)
	}
	cursor := head
	if req.From != nil {
		cursor = *req.From
		if cursor > head {
			cursor = head
		}
	}
	id, err := newSubscriberId()
	if err != nil {
		return nil, wrapDbError("failed to generate subscriber id", err)
	}
	nowStr := m.nowStr()
	sub := Subscription{
		Id:      id,
		Name:    req.Name,
		Types:   req.Types,
		Ds:      req.Ds,
		Cursor:  cursor,
		Created: nowStr,
		Updated: nowStr,
	}
	if err := m.saveSubscription(&sub); err != nil {
		return nil, wrapDbError("failed to save subscription", err)
	}
	return &RegisterResponse{SubscriberId: id, Cursor: cursor, Head: head}, nil
}

// Pull 按 after（缺省用登记的 cursor）扫描变更，读无副作用。
// limit 计扫描行数而非匹配行数，过滤型订阅才能推进过不匹配的行；
// 游标落在已清理区间（after+1 < 现存最老 seq）时返回 410，绝不静默跳过。
func (m *Manager) Pull(id string, after *uint64, limit int) (*PullResponse, *Http.HttpError) {
	m.lock.Lock()
	defer m.lock.Unlock()
	sub, hx := m.loadSubscription(id)
	if hx != nil {
		return nil, hx
	}
	head, err := readHead(m.db)
	if err != nil {
		return nil, wrapDbError("failed to read change seq counter", err)
	}
	start := sub.Cursor
	if after != nil {
		start = *after
	}
	if start > head {
		return nil, Http.NewHttpError(fmt.Sprintf("after=[%d] is ahead of head=[%d]", start, head), http.StatusBadRequest)
	}
	changes, err := loadChanges(m.db, head)
	if err != nil {
		return nil, wrapDbError("failed to load changes", err)
	}
	floor := floorOf(changes)
	if floor == 0 {
		floor = head + 1
	}
	if start+1 < floor {
		hx := Http.NewHttpError(fmt.Sprintf("cursor=[%d] falls behind the retained log (floor=[%d], head=[%d]), please re-subscribe", start, floor, head), http.StatusGone)
		hx.Payload = map[string]interface{}{"floor": floor, "head": head}
		return nil, hx
	}
	if limit <= 0 {
		limit = DefaultPullLimit
	}
	if limit > MaxPullLimit {
		limit = MaxPullLimit
	}
	matched := []Change{}
	scanned := 0
	lastScanned := start
	for _, change := range changes {
		if change.Seq <= start {
			continue
		}
		if scanned >= limit {
			break
		}
		scanned++
		lastScanned = change.Seq
		if m.match(sub, &change) {
			matched = append(matched, change)
		}
	}
	next := head
	if scanned >= limit {
		next = lastScanned
	}
	return &PullResponse{
		SubscriberId: id,
		After:        start,
		Head:         head,
		Floor:        floor,
		Changes:      matched,
		Next:         next,
	}, nil
}

// Ack 推进订阅者游标（只前进；回退是 no-op 但仍刷新活跃时间），返回存储后的游标。
func (m *Manager) Ack(id string, cursor uint64) (*AckResponse, *Http.HttpError) {
	m.lock.Lock()
	defer m.lock.Unlock()
	sub, hx := m.loadSubscription(id)
	if hx != nil {
		return nil, hx
	}
	head, err := readHead(m.db)
	if err != nil {
		return nil, wrapDbError("failed to read change seq counter", err)
	}
	if cursor > head {
		return nil, Http.NewHttpError(fmt.Sprintf("cursor=[%d] is ahead of head=[%d]", cursor, head), http.StatusBadRequest)
	}
	if cursor > sub.Cursor {
		sub.Cursor = cursor
	}
	sub.Updated = m.nowStr()
	if err := m.saveSubscription(sub); err != nil {
		return nil, wrapDbError("failed to save subscription", err)
	}
	return &AckResponse{SubscriberId: id, Cursor: sub.Cursor}, nil
}

// Get 返回订阅状态；未知 id（从未存在或已被判死清理）返回 404。
func (m *Manager) Get(id string) (*StatusResponse, *Http.HttpError) {
	m.lock.Lock()
	defer m.lock.Unlock()
	sub, hx := m.loadSubscription(id)
	if hx != nil {
		return nil, hx
	}
	head, err := readHead(m.db)
	if err != nil {
		return nil, wrapDbError("failed to read change seq counter", err)
	}
	return &StatusResponse{Subscription: *sub, Head: head}, nil
}

// List 返回所有订阅记录。
func (m *Manager) List() ([]Subscription, *Http.HttpError) {
	m.lock.Lock()
	defer m.lock.Unlock()
	result, err := m.db.Get(map[string]interface{}{
		DbIface.Table:   KeySubscription,
		Record.DataType: KeySubscription,
	})
	if err != nil {
		return nil, wrapDbError("failed to load subscriptions", err)
	}
	subs := []Subscription{}
	for _, item := range result {
		data, _ := item[Record.Data].(map[string]interface{})
		sub := Subscription{}
		if err := Json.CopyTo(data, &sub); err != nil {
			return nil, wrapDbError("failed to parse subscription", err)
		}
		subs = append(subs, sub)
	}
	return subs, nil
}

// Delete 注销订阅；幂等，记录不存在也返回成功。
func (m *Manager) Delete(id string) *Http.HttpError {
	m.lock.Lock()
	defer m.lock.Unlock()
	if err := m.db.Delete(KeySubscription, map[string]interface{}{
		Record.DataType: KeySubscription,
		Record.DataId:   id,
	}); err != nil {
		return wrapDbError(fmt.Sprintf("failed to delete subscription [%s]", id), err)
	}
	return nil
}

// Prune 清理：删除所有活跃订阅者都已消费（seq <= min(cursor)）或超过保留期的变更行；
// 判死并删除 updated 超时的订阅者。无活跃订阅者时不因 cursor 清理（只按保留期兜底）。
func (m *Manager) Prune() error {
	m.lock.Lock()
	defer m.lock.Unlock()
	head, err := readHead(m.db)
	if err != nil {
		return fmt.Errorf("failed to read change seq counter. Error: %s", err)
	}
	changes, err := loadChanges(m.db, head)
	if err != nil {
		return fmt.Errorf("failed to load changes. Error: %s", err)
	}
	subs, err := m.listSubscriptions()
	if err != nil {
		return err
	}
	now := m.now()
	minCursor := head
	hasLive := false
	for idx := range subs {
		sub := &subs[idx]
		if m.isLive(sub, now) {
			hasLive = true
			if sub.Cursor < minCursor {
				minCursor = sub.Cursor
			}
		}
	}
	if !hasLive {
		minCursor = head
	}
	retainFrom := now.Add(-m.retention)
	removed := 0
	for _, change := range changes {
		ts, err := time.Parse(time.RFC3339, change.Ts)
		if err != nil {
			ts = time.Time{} // 无法解析的时间视为最老，交由保留期清理
		}
		if change.Seq <= minCursor || ts.Before(retainFrom) {
			if err := deleteChange(m.db, seqId(change.Seq)); err != nil {
				return fmt.Errorf("failed to delete change [%d]. Error: %s", change.Seq, err)
			}
			removed++
		}
	}
	for idx := range subs {
		sub := &subs[idx]
		if !m.isLive(sub, now) {
			m.log.Printf("subscription [%s] is stale (updated=[%s]), remove it", sub.Id, sub.Updated)
			if err := m.db.Delete(KeySubscription, map[string]interface{}{
				Record.DataType: KeySubscription,
				Record.DataId:   sub.Id,
			}); err != nil {
				return fmt.Errorf("failed to delete subscription [%s]. Error: %s", sub.Id, err)
			}
		}
	}
	if removed > 0 {
		m.log.Printf("pruned %d changes (minCursor=[%d], head=[%d])", removed, minCursor, head)
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

// isLive 判断订阅者是否活跃（pull/ack 都会刷新 updated）。
func (m *Manager) isLive(sub *Subscription, now time.Time) bool {
	updated, err := time.Parse(time.RFC3339, sub.Updated)
	if err != nil {
		return false
	}
	return !updated.Before(now.Add(-m.timeout))
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

// loadSubscription 读取并解析一条订阅记录；不存在返回 404。
func (m *Manager) loadSubscription(id string) (*Subscription, *Http.HttpError) {
	result, err := m.db.Get(map[string]interface{}{
		DbIface.Table:   KeySubscription,
		Record.DataType: KeySubscription,
		Record.DataId:   id,
	})
	if err != nil {
		return nil, wrapDbError(fmt.Sprintf("failed to load subscription [%s]", id), err)
	}
	if len(result) == 0 {
		return nil, Http.NewHttpError(fmt.Sprintf("subscription [%s] does not exist", id), http.StatusNotFound)
	}
	data, _ := result[0][Record.Data].(map[string]interface{})
	sub := &Subscription{}
	if err := Json.CopyTo(data, sub); err != nil {
		return nil, wrapDbError(fmt.Sprintf("failed to parse subscription [%s]", id), err)
	}
	return sub, nil
}

func (m *Manager) saveSubscription(sub *Subscription) error {
	subMap, err := Json.CopyToMap(sub)
	if err != nil {
		return err
	}
	record := Record.NewRecord(KeySubscription, stateVer, sub.Id, subMap)
	return m.db.Replace(KeySubscription, map[string]interface{}{
		Record.DataType: KeySubscription,
		Record.DataId:   sub.Id,
	}, record.Map())
}

// newSubscriberId 生成 "sub-" + 16 字节随机 hex（InventoryService 模块零外部依赖，不用 uuid）。
func newSubscriberId() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "sub-" + hex.EncodeToString(buf), nil
}

func wrapDbError(msg string, err error) *Http.HttpError {
	return Http.NewHttpError(fmt.Sprintf("%s. Error: %s", msg, err), http.StatusInternalServerError)
}
