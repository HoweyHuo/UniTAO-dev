package VdsTest

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"Data/DbIface"

	"github.com/salesforce/UniTAO/lib/Schema/Record"
)

// MemDb 是按表隔离、线程安全的内存数据库，行为与 Mongo / DynamoDB 实现一致：
// 每张表以 (__type, __id) 为键。读写都做深拷贝，避免调用方共享底层 map。
type MemDb struct {
	lock   sync.Mutex
	tables map[string]map[string]map[string]interface{} // table -> "type\x00id" -> record
	scans  int
}

func NewMemDb() *MemDb {
	return &MemDb{tables: map[string]map[string]map[string]interface{}{}}
}

func memKey(dataType string, dataId string) string {
	return dataType + "\x00" + dataId
}

func deepCopy(data interface{}) (map[string]interface{}, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	result := map[string]interface{}{}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (db *MemDb) Name() string { return "MemDb" }

func (db *MemDb) DatabaseExists() (bool, error) {
	db.lock.Lock()
	defer db.lock.Unlock()
	return len(db.tables) > 0, nil
}

func (db *MemDb) ListTable() ([]interface{}, error) {
	db.lock.Lock()
	defer db.lock.Unlock()
	names := []string{}
	for name := range db.tables {
		names = append(names, name)
	}
	sort.Strings(names)
	result := []interface{}{}
	for _, name := range names {
		result = append(result, name)
	}
	return result, nil
}

func (db *MemDb) CreateTable(name string, data map[string]interface{}) error {
	db.lock.Lock()
	defer db.lock.Unlock()
	if _, ok := db.tables[name]; !ok {
		db.tables[name] = map[string]map[string]interface{}{}
	}
	return nil
}

func (db *MemDb) DeleteTable(name string) error {
	db.lock.Lock()
	defer db.lock.Unlock()
	delete(db.tables, name)
	return nil
}

// HasTable 供测试检查表是否存在。
func (db *MemDb) HasTable(name string) bool {
	db.lock.Lock()
	defer db.lock.Unlock()
	_, ok := db.tables[name]
	return ok
}

func (db *MemDb) table(name string) (map[string]map[string]interface{}, error) {
	tbl, ok := db.tables[name]
	if !ok {
		return nil, fmt.Errorf("table [%s] does not exists", name)
	}
	return tbl, nil
}

func (db *MemDb) Get(queryArgs map[string]interface{}) ([]map[string]interface{}, error) {
	tableName, ok := queryArgs[DbIface.Table].(string)
	if !ok {
		return nil, fmt.Errorf("missing parameter [%s] from queryArgs", DbIface.Table)
	}
	dataType, ok := queryArgs[Record.DataType].(string)
	if !ok {
		return nil, fmt.Errorf("missing parameter [%s] from queryArgs", Record.DataType)
	}
	db.lock.Lock()
	defer db.lock.Unlock()
	tbl, ok := db.tables[tableName]
	if !ok {
		return []map[string]interface{}{}, nil
	}
	result := []map[string]interface{}{}
	if dataId, ok := queryArgs[Record.DataId].(string); ok {
		if record, ok := tbl[memKey(dataType, dataId)]; ok {
			copied, err := deepCopy(record)
			if err != nil {
				return nil, err
			}
			result = append(result, copied)
		}
		return result, nil
	}
	keys := []string{}
	for key, record := range tbl {
		if record[Record.DataType] == dataType {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		copied, err := deepCopy(tbl[key])
		if err != nil {
			return nil, err
		}
		result = append(result, copied)
	}
	return result, nil
}

// Scan 实现 DbIface.Scanner：先在锁内取出键，再逐条复制回调（回调时不持锁，期间被删除的记录跳过）。
func (db *MemDb) Scan(queryArgs map[string]interface{}, fn func(record map[string]interface{}) error) error {
	tableName, ok := queryArgs[DbIface.Table].(string)
	if !ok {
		return fmt.Errorf("missing parameter [%s] from queryArgs", DbIface.Table)
	}
	dataType, ok := queryArgs[Record.DataType].(string)
	if !ok {
		return fmt.Errorf("missing parameter [%s] from queryArgs", Record.DataType)
	}
	db.lock.Lock()
	db.scans++
	keys := []string{}
	for key, record := range db.tables[tableName] {
		if record[Record.DataType] == dataType {
			keys = append(keys, key)
		}
	}
	db.lock.Unlock()
	sort.Strings(keys)
	for _, key := range keys {
		db.lock.Lock()
		record, ok := db.tables[tableName][key]
		var copied map[string]interface{}
		var err error
		if ok {
			copied, err = deepCopy(record)
		}
		db.lock.Unlock()
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := fn(copied); err != nil {
			return err
		}
	}
	return nil
}

// Scans 返回 Scan 被调用的次数（测试用）。
func (db *MemDb) Scans() int {
	db.lock.Lock()
	defer db.lock.Unlock()
	return db.scans
}

func (db *MemDb) put(tableName string, data interface{}) error {
	record, err := deepCopy(data)
	if err != nil {
		return err
	}
	dataType, _ := record[Record.DataType].(string)
	dataId, _ := record[Record.DataId].(string)
	if dataType == "" || dataId == "" {
		return fmt.Errorf("record without [%s]/[%s]", Record.DataType, Record.DataId)
	}
	tbl, err := db.table(tableName)
	if err != nil {
		return err
	}
	tbl[memKey(dataType, dataId)] = record
	return nil
}

func (db *MemDb) Create(tableName string, data interface{}) error {
	db.lock.Lock()
	defer db.lock.Unlock()
	return db.put(tableName, data)
}

// recordKeys 检查 Replace / Delete 的过滤条件。MongoDB 把 keys 原样当过滤条件，
// 多出文档里没有的字段（如 DbIface.Table）就匹配不上：Replace 会插入重复记录，Delete 什么也不删。
// MemDb 要求 keys 恰好是 {__type, __id}，否则报错，让测试暴露这类调用。
func recordKeys(op string, keys map[string]interface{}) (string, string, error) {
	dataType, _ := keys[Record.DataType].(string)
	dataId, _ := keys[Record.DataId].(string)
	if dataType == "" || dataId == "" || len(keys) != 2 {
		return "", "", fmt.Errorf("MemDb %s: keys must be exactly {%s, %s}, got %v", op, Record.DataType, Record.DataId, keys)
	}
	return dataType, dataId, nil
}

func (db *MemDb) Replace(tableName string, keys map[string]interface{}, data interface{}) error {
	db.lock.Lock()
	defer db.lock.Unlock()
	tbl, err := db.table(tableName)
	if err != nil {
		return err
	}
	dataType, dataId, err := recordKeys("Replace", keys)
	if err != nil {
		return err
	}
	record, err := deepCopy(data)
	if err != nil {
		return err
	}
	if record[Record.DataType] != dataType || record[Record.DataId] != dataId {
		return fmt.Errorf("MemDb Replace: keys %v do not match record [%v/%v]", keys, record[Record.DataType], record[Record.DataId])
	}
	delete(tbl, memKey(dataType, dataId))
	return db.put(tableName, data)
}

func (db *MemDb) Delete(tableName string, keys map[string]interface{}) error {
	db.lock.Lock()
	defer db.lock.Unlock()
	tbl, ok := db.tables[tableName]
	if !ok {
		return nil
	}
	dataType, dataId, err := recordKeys("Delete", keys)
	if err != nil {
		return err
	}
	delete(tbl, memKey(dataType, dataId))
	return nil
}

func (db *MemDb) Update(tableName string, keys map[string]interface{}, data interface{}) (map[string]interface{}, error) {
	dataType, ok := keys[Record.DataType].(string)
	if !ok {
		return nil, fmt.Errorf("missing key=[%s]", Record.DataType)
	}
	dataId, ok := keys[Record.DataId].(string)
	if !ok {
		return nil, fmt.Errorf("missing key=[%s]", Record.DataId)
	}
	queryPath, ok := keys[DbIface.PatchPath].(string)
	if !ok {
		return nil, fmt.Errorf("missing patch key=[%s]", DbIface.PatchPath)
	}
	db.lock.Lock()
	defer db.lock.Unlock()
	tbl, err := db.table(tableName)
	if err != nil {
		return nil, err
	}
	current, ok := tbl[memKey(dataType, dataId)]
	if !ok {
		return nil, fmt.Errorf("data [%s/%s] does not exists", dataType, dataId)
	}
	record, err := deepCopy(current)
	if err != nil {
		return nil, err
	}
	subData, attrPath, err := DbIface.GetDataOnPath(record, queryPath, fmt.Sprintf("%s/%s/%s", dataType, dataId, queryPath))
	if err != nil {
		return nil, err
	}
	if err := DbIface.SetPatchData(subData, attrPath, data); err != nil {
		return nil, err
	}
	tbl[memKey(dataType, dataId)] = record
	return deepCopy(record)
}

var _ DbIface.Database = (*MemDb)(nil)
