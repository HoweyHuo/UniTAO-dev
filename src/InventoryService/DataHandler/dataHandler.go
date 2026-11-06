/*
************************************************************************************************************
Copyright (c) 2022 Salesforce, Inc.
All rights reserved.

UniTAO was originally created in 2022 by Shai Herzog & Yi Huo as an
Universal No-Coding Heterogeneous Infrastructure Maintenance & Inventory system that is holistically driven by open/community-developed semantic models/schemas.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published
by the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>

This copyright notice and license applies to all files in this directory or sub-directories, except when stated otherwise explicitly.
************************************************************************************************************
*/

package DataHandler

import (
	"fmt"
	"log"
	"net/http"

	"strings"

	"Data"
	"Data/DbConfig"
	"Data/DbIface"
	"InventoryService/InvRecord"
	"InventoryService/RefRecord"

	"github.com/salesforce/UniTAO/lib/Schema"
	"github.com/salesforce/UniTAO/lib/Schema/JsonKey"
	"github.com/salesforce/UniTAO/lib/Schema/Record"
	"github.com/salesforce/UniTAO/lib/Schema/SchemaDoc"
	"github.com/salesforce/UniTAO/lib/SchemaPath"
	SchemaPathData "github.com/salesforce/UniTAO/lib/SchemaPath/Data"
	"github.com/salesforce/UniTAO/lib/Util"
	"github.com/salesforce/UniTAO/lib/Util/Http"
	"github.com/salesforce/UniTAO/lib/Util/Json"
)

type Handler struct {
	log      *log.Logger
	Db       DbIface.Database
	SyncChan chan string // 非空时，inventory 记录写入成功后非阻塞投递 dsId 事件
	// PinnedSources 把共享名固定给某个公钥指纹：该名字只给这个指纹使用（见 allocateSignedDsId）。
	PinnedSources map[string]string
}

var InvTypes = map[string]bool{
	Schema.Inventory:    true,
	RefRecord.Referral:  true,
	SchemaPath.PathName: true,
}

var EditableTypes = map[string]bool{
	SchemaPath.PathName: true,
	Schema.Inventory:    true, // DS 自我注册写入 inventory 记录
}

func New(config DbConfig.DatabaseConfig, logger *log.Logger) (*Handler, error) {
	if logger == nil {
		logger = log.Default()
	}
	db, err := Data.ConnectDb(config, logger)
	if err != nil {
		return nil, err
	}
	return NewWithDb(db, logger)
}

// NewWithDb 用已建立的数据库连接创建 Handler。
func NewWithDb(db DbIface.Database, logger *log.Logger) (*Handler, error) {
	if logger == nil {
		logger = log.Default()
	}
	handler := Handler{
		log: logger,
		Db:  db,
	}
	handler.Db = db
	err := handler.init()
	if err != nil {
		return nil, err
	}
	return &handler, nil
}

func (h *Handler) Log(msg string) {
	h.log.Printf(fmt.Sprintf("InvSrvHandler: %s", msg))
}

func (h *Handler) init() error {
	tbList, err := h.Db.ListTable()
	if err != nil {
		return err
	}
	for _, name := range []string{Schema.Inventory, RefRecord.Referral} {
		tblExists := false
		for _, tbl := range tbList {
			if tbl == name {
				tblExists = true
			}
		}
		if !tblExists {
			h.Log(fmt.Sprintf("missing table=[%s], create one", name))
			err := h.Db.CreateTable(name, nil)
			if err != nil {
				err = fmt.Errorf("failed to creat table=[%s], Err:%s", name, err)
				return err
			}
		}
	}
	return nil
}

func (h *Handler) List(dataType string) ([]interface{}, *Http.HttpError) {
	if _, ok := InvTypes[dataType]; ok {
		result, err := h.InvListData(dataType)
		if err != nil {
			return nil, err
		}
		dsList := make([]interface{}, 0, len(result))
		dataKey := Record.DataId
		for _, data := range result {
			dsList = append(dsList, data[dataKey].(string))
		}
		return dsList, nil
	}
	if dataType == JsonKey.Schema {
		// we don't want to store Schema data locally in Inventory.
		// but list of dataType supported is the list of referral
		refList, err := h.List(RefRecord.Referral)
		if err != nil {
			return nil, err
		}
		// Add internal Inventory type to data type list
		for key := range InvTypes {
			refList = append(refList, key)
		}
		// record 是描述所有数据记录信封（__id/__type/__ver/data）的框架 schema。
		// 它不是 referral 类型（sync 会跳过内部类型），但 schema 端点必须能列出来。
		refList = append(refList, Record.KeyRecord)
		return refList, nil
	}
	referral, err := h.GetReferral(dataType)
	if err != nil {
		return nil, err
	}
	// 始终返回 {ds}/{id} 限定 id，不去重：不同 vDS 的同 type 同 id 是不同的 record
	idList := []interface{}{}
	anySuccess := false
	var lastErr string
	for _, ds := range referral.DsInfos {
		dsUrl, e := ds.GetUrl()
		if e != nil {
			h.Log(fmt.Sprintf("skip DS=[%s] for list of [%s]: %s", ds.Id, dataType, e.Error()))
			lastErr = e.Error()
			continue
		}
		urlPath, e := Http.URLPathJoin(dsUrl, dataType)
		if e != nil {
			h.Log(fmt.Sprintf("failed to build list url for DS=[%s]: %s", ds.Id, e.Error()))
			lastErr = e.Error()
			continue
		}
		data, code, e := Http.GetRestData(*urlPath)
		if e != nil {
			h.Log(fmt.Sprintf("failed to list [%s] from DS=[%s], url=[%s], code=%d, error: %s", dataType, ds.Id, *urlPath, code, e.Error()))
			lastErr = e.Error()
			continue
		}
		dsIds, ok := data.([]interface{})
		if !ok {
			h.Log(fmt.Sprintf("bad list result from DS=[%s], url=[%s], not an array", ds.Id, *urlPath))
			lastErr = fmt.Sprintf("list result from DS=[%s] is not an array", ds.Id)
			continue
		}
		anySuccess = true
		for _, id := range dsIds {
			idStr, ok := id.(string)
			if !ok || idStr == "" {
				continue
			}
			idList = append(idList, fmt.Sprintf("%s/%s", ds.Id, idStr))
		}
	}
	if !anySuccess {
		return nil, Http.NewHttpError(
			fmt.Sprintf("failed to list [%s] from any DataService, last error: %s", dataType, lastErr),
			http.StatusInternalServerError)
	}
	return idList, nil
}

func (h *Handler) Get(dataType string, dataPath string) (interface{}, *Http.HttpError) {
	// if we get to this function, it means dataPath is not empty string already
	dataId, nextPath := Util.ParsePath(dataPath)
	if _, ok := InvTypes[dataType]; ok || dataType == JsonKey.Schema {
		if nextPath != "" {
			return nil, Http.NewHttpError(fmt.Sprintf("path=[%s] not supported on type=[%s]", dataPath, dataType), http.StatusBadRequest)
		}
		return h.GetRecord(dataType, dataId)
	}
	referral, err := h.GetReferral(dataType)
	if err != nil {
		return nil, err
	}
	// 数据类型的查询路径始终为 {type}/{ds}/{id}：不同 vDS 的同 type 同 id 是不同的 record
	dsName, rest := Util.ParsePath(dataPath)
	var pinDs *InvRecord.DataServiceInfo
	for _, ds := range referral.DsInfos {
		if ds.Id == dsName {
			pinDs = ds
			break
		}
	}
	if pinDs == nil {
		dsIds := []string{}
		for _, ds := range referral.DsInfos {
			dsIds = append(dsIds, ds.Id)
		}
		return nil, Http.NewHttpError(fmt.Sprintf("unknown DataService=[%s] for type=[%s]; query path is [%s/{ds}/{id}], ds in %v", dsName, dataType, dataType, dsIds), http.StatusBadRequest)
	}
	restId, restNext := Util.ParsePath(rest)
	if restId == "" {
		return h.listFromDs(pinDs, dataType)
	}
	return h.getDataByPath(dataType, restId, restNext, pinDs)
}

// listFromDs 列出指定 DS 上某类型的所有 id（带 {ds}/ 前缀）。
func (h *Handler) listFromDs(ds *InvRecord.DataServiceInfo, dataType string) ([]interface{}, *Http.HttpError) {
	dsUrl, e := ds.GetUrl()
	if e != nil {
		return nil, Http.NewHttpError(e.Error(), http.StatusInternalServerError)
	}
	urlPath, e := Http.URLPathJoin(dsUrl, dataType)
	if e != nil {
		return nil, Http.WrapError(e, "failed to build list url", http.StatusInternalServerError)
	}
	data, code, e := Http.GetRestData(*urlPath)
	if e != nil {
		return nil, Http.NewHttpError(e.Error(), code)
	}
	dsIds, ok := data.([]interface{})
	if !ok {
		return nil, Http.NewHttpError(fmt.Sprintf("list result from DS=[%s] is not an array", ds.Id), http.StatusInternalServerError)
	}
	result := []interface{}{}
	for _, id := range dsIds {
		if idStr, ok := id.(string); ok && idStr != "" {
			result = append(result, fmt.Sprintf("%s/%s", ds.Id, idStr))
		}
	}
	return result, nil
}

// dsServesType 判断 dsId 是否在 dataType 的 referral 中。
func (h *Handler) dsServesType(dsId string, dataType string) bool {
	record, err := h.GetReferralRecord(dataType)
	if err != nil {
		return false
	}
	referral, e := RefRecord.LoadMap(record.Data)
	if e != nil {
		return false
	}
	for _, id := range referral.DataServices {
		if id == dsId {
			return true
		}
	}
	return false
}

func (h *Handler) GetSchemaRecord(dataType string) (*Record.Record, *Http.HttpError) {
	// record 不是 referral 类型，而是所有数据记录共有的信封框架 schema，
	// 直接由 lib 提供（与各 Data Service 初始化时导入的 record schema 一致）。
	if dataType == Record.KeyRecord {
		record, _ := Record.LoadStr(Record.Schema)
		return record, nil
	}
	if _, ok := InvTypes[dataType]; ok {
		var record *Record.Record
		switch dataType {
		case RefRecord.Referral:
			record, _ = Record.LoadStr(RefRecord.SchemaRecord)
		case Schema.Inventory:
			record, _ = Record.LoadStr(InvRecord.SchemaRecord)
		case SchemaPath.PathName:
			record, _ = Record.LoadStr(SchemaPath.PathDataSchema)
		default:
			return nil, Http.NewHttpError(fmt.Sprintf("don't know how to get schema for Inventory Type[%s]", dataType), http.StatusInternalServerError)
		}
		return record, nil
	}
	schemaName, _, ex := SchemaDoc.ParseDataType(dataType)
	if ex != nil {
		return nil, Http.WrapError(ex, fmt.Sprintf("failed to parse schema type[%s]", dataType), http.StatusBadRequest)
	}
	referral, err := h.GetReferral(schemaName)
	if err != nil {
		return nil, err
	}
	return referral.GetSchema(dataType, h.log)
}

func (h *Handler) GetRecord(dataType string, dataId string) (*Record.Record, *Http.HttpError) {
	var data interface{}
	var err *Http.HttpError
	switch dataType {
	case JsonKey.Schema:
		return h.GetSchemaRecord(dataId)
	case Schema.Inventory:
		data, err = h.GetData(Schema.Inventory, dataId)
	case RefRecord.Referral:
		referral, err := h.GetReferral(dataId)
		if err != nil {
			return nil, err
		}
		// when query referral, we also want to display schema of the dataType
		record, err := referral.GetSchema(dataId, h.log)
		if err != nil {
			return nil, err
		}
		referral.Schema = record.Data
		return referral.GetRecord(), nil
	case SchemaPath.PathName:
		data, err = h.GetData(SchemaPath.PathName, dataId)
	default:
		data, err = h.GetDataServiceData(dataType, dataId)
	}
	if err != nil {
		return nil, err
	}
	record, e := Record.LoadMap(data.(map[string]interface{}))
	if e != nil {
		return nil, Http.WrapError(e, "failed to load result data as Record", http.StatusInternalServerError)
	}
	return record, nil
}

// List Data for Inventory Tables
func (h *Handler) InvListData(dataType string) ([]map[string]interface{}, *Http.HttpError) {
	if _, ok := InvTypes[dataType]; !ok {
		return nil, Http.NewHttpError(fmt.Sprintf("[type]=[%s] is not supported", dataType), http.StatusBadRequest)
	}
	err := h.Db.CreateTable(dataType, nil)
	if err != nil {
		return nil, Http.NewHttpError(err.Error(), http.StatusInternalServerError)
	}
	args := make(map[string]interface{})
	args[DbIface.Table] = dataType
	args[Record.DataType] = dataType
	result, err := h.Db.Get(args)
	if err != nil {
		return nil, Http.NewHttpError(err.Error(), http.StatusInternalServerError)
	}
	return result, nil
}

func (h *Handler) GetDataByPath(dataType string, idPath string, nextPath string) (interface{}, *Http.HttpError) {
	return h.getDataByPath(dataType, idPath, nextPath, nil)
}

// getDataByPath 构建 SchemaPath 查询并执行。
// pinDs 非空时，基础类型记录（dt==dataType && id==idPath）钉到指定分片获取；
// schema 获取与跨类型 CMT 引用仍走 h.GetRecord 正常路由。
func (h *Handler) getDataByPath(dataType string, idPath string, nextPath string, pinDs *InvRecord.DataServiceInfo) (interface{}, *Http.HttpError) {
	conn := SchemaPathData.Connection{
		FuncRecord: h.GetRecord,
	}
	if pinDs != nil {
		baseId := idPath
		conn.FuncRecord = func(dt string, id string) (*Record.Record, *Http.HttpError) {
			if dt == dataType && id == baseId {
				return h.getRecordFromDs(pinDs, dataType, id)
			}
			// 引用按 vDS 优先解析：先在起始记录所在的 vDS 里找，找不到再全局查找
			if dt != JsonKey.Schema && h.dsServesType(pinDs.Id, dt) {
				record, err := h.getRecordFromDs(pinDs, dt, id)
				if err == nil {
					return record, nil
				}
				if err.Status != http.StatusNotFound {
					return nil, err
				}
			}
			return h.GetRecord(dt, id)
		}
	}
	dataPath := idPath
	if nextPath != "" {
		dataPath = fmt.Sprintf("%s/%s", idPath, nextPath)
	}
	query, err := SchemaPath.CreateQuery(&conn, dataType, dataPath)
	if err != nil {
		return nil, err
	}
	result, err := query.WalkValue()
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, Http.NewHttpError(fmt.Sprintf("walk SchemaPath with no value.from [path]=[%s]", dataPath), http.StatusNotFound)
	}
	return result, nil
}

// getRecordFromDs 直接从指定 DataService 获取单条记录（点查询按前缀路由用）。
func (h *Handler) getRecordFromDs(ds *InvRecord.DataServiceInfo, dataType string, dataId string) (*Record.Record, *Http.HttpError) {
	dsUrl, e := ds.GetUrl()
	if e != nil {
		return nil, Http.NewHttpError(e.Error(), http.StatusInternalServerError)
	}
	idPath, e := Http.URLPathJoin(dsUrl, dataType, dataId)
	if e != nil {
		return nil, Http.WrapError(e, "failed to build url", http.StatusInternalServerError)
	}
	data, code, e := Http.GetRestData(*idPath)
	if e != nil {
		return nil, Http.NewHttpError(e.Error(), code)
	}
	mapData, ok := data.(map[string]interface{})
	if !ok {
		return nil, Http.NewHttpError(fmt.Sprintf("data from [%s] is not a valid record map", *idPath), http.StatusInternalServerError)
	}
	record, e := Record.LoadMap(mapData)
	if e != nil {
		return nil, Http.WrapError(e, "failed to load data as Record", http.StatusInternalServerError)
	}
	return record, nil
}

func (h *Handler) GetDataServiceData(dataType string, dataId string) (interface{}, *Http.HttpError) {
	referral, err := h.GetReferral(dataType)
	if err != nil {
		return nil, err
	}
	var last404 string
	var lastReal string
	for _, ds := range referral.DsInfos {
		dsUrl, e := ds.GetUrl()
		if e != nil {
			h.Log(fmt.Sprintf("skip DS=[%s] for [%s/%s]: %s", ds.Id, dataType, dataId, e.Error()))
			lastReal = fmt.Sprintf("DS=[%s]: %s", ds.Id, e.Error())
			continue
		}
		idPath, e := Http.URLPathJoin(dsUrl, dataType, dataId)
		if e != nil {
			h.Log(fmt.Sprintf("failed to build url for DS=[%s]: %s", ds.Id, e.Error()))
			lastReal = fmt.Sprintf("DS=[%s]: %s", ds.Id, e.Error())
			continue
		}
		data, code, e := Http.GetRestData(*idPath)
		if e == nil {
			result, ok := data.(map[string]interface{})
			if !ok {
				h.Log(fmt.Sprintf("data from [%s] is not a valid record map", *idPath))
				lastReal = fmt.Sprintf("DS=[%s]: invalid record data from [%s]", ds.Id, *idPath)
				continue
			}
			return result, nil
		}
		if code == http.StatusNotFound {
			last404 = fmt.Sprintf("DS=[%s]: %s", ds.Id, e.Error())
			continue
		}
		h.Log(fmt.Sprintf("failed to get [%s/%s] from DS=[%s], url=[%s], code=%d, error: %s", dataType, dataId, ds.Id, *idPath, code, e.Error()))
		lastReal = fmt.Sprintf("DS=[%s] code=%d: %s", ds.Id, code, e.Error())
	}
	if lastReal != "" {
		return nil, Http.NewHttpError(
			fmt.Sprintf("failed to get [%s/%s] from any DataService, last error: %s", dataType, dataId, lastReal),
			http.StatusInternalServerError)
	}
	return nil, Http.NewHttpError(
		fmt.Sprintf("object of type [%s] with id [%s] not found on any DataService. %s", dataType, dataId, last404),
		http.StatusNotFound)
}

func (h *Handler) GetData(dataType string, dataId string) (interface{}, *Http.HttpError) {
	err := h.Db.CreateTable(dataType, nil)
	if err != nil {
		return nil, Http.NewHttpError(err.Error(), http.StatusInternalServerError)
	}
	args := make(map[string]interface{})
	args[DbIface.Table] = dataType
	args[Record.DataType] = dataType
	args[Record.DataId] = dataId
	recordList, err := h.Db.Get(args)
	if err != nil {
		return nil, Http.NewHttpError(err.Error(), http.StatusInternalServerError)
	}
	if len(recordList) == 0 {
		return nil, Http.NewHttpError(fmt.Sprintf("object of type “%s” with id <%s> not found", dataType, dataId), http.StatusNotFound)

	}
	return recordList[0], nil
}

func (h *Handler) GetReferralRecord(dataType string) (*Record.Record, *Http.HttpError) {
	referralData, err := h.GetData(RefRecord.Referral, dataType)
	if err != nil {
		return nil, err
	}
	referralMap, ok := referralData.(map[string]interface{})
	if !ok {
		return nil, Http.NewHttpError(fmt.Sprintf("failed to convert data of [%s] to map[string]interface{}", dataType), http.StatusInternalServerError)

	}
	record, e := Record.LoadMap(referralMap)
	if e != nil {
		return nil, Http.WrapError(e, fmt.Sprintf("failed to load referral data of [%s] as Record", dataType), http.StatusInternalServerError)
	}
	return record, nil
}

func (h *Handler) GetReferral(dataType string) (*RefRecord.ReferralData, *Http.HttpError) {
	record, err := h.GetReferralRecord(dataType)
	if err != nil {
		return nil, err
	}
	referral, e := RefRecord.LoadMap(record.Data)
	if e != nil {
		return nil, Http.NewHttpError(e.Error(), http.StatusBadRequest)
	}
	if len(referral.DataServices) == 0 {
		// 旧格式（仅 DataServiceId）尚未迁移：需运行一次 InventoryServiceAdmin sync 改写为数组
		return nil, Http.NewHttpError(
			fmt.Sprintf("referral for type=[%s] has no DataServices defined. run InventoryServiceAdmin sync to migrate", dataType),
			http.StatusInternalServerError)
	}
	// 解析所有可解析的 DataService（不做可达性探测；探测留到查询扇出时逐分片进行）
	var lastErr string
	seen := map[string]bool{}
	referral.DsInfos = []*InvRecord.DataServiceInfo{}
	for _, dsId := range referral.DataServices {
		if dsId == "" || seen[dsId] {
			continue
		}
		seen[dsId] = true
		dsRecord, err := h.GetDsRecord(dsId)
		if err != nil {
			h.Log(fmt.Sprintf("skip DS=[%s] for type=[%s]: %s", dsId, dataType, err.Error()))
			lastErr = err.Error()
			continue
		}
		dsInfo, e := InvRecord.CreateDsInfo(dsRecord.Data)
		if e != nil {
			h.Log(fmt.Sprintf("skip DS=[%s] for type=[%s]: invalid ds record, %s", dsId, dataType, e.Error()))
			lastErr = e.Error()
			continue
		}
		referral.DsInfos = append(referral.DsInfos, dsInfo)
	}
	if len(referral.DsInfos) == 0 {
		return nil, Http.NewHttpError(
			fmt.Sprintf("no resolvable DataService for type=[%s], DataServices=%v, last error: %s", dataType, referral.DataServices, lastErr),
			http.StatusInternalServerError)
	}
	return referral, nil
}

func (h *Handler) GetDsRecord(dsId string) (*Record.Record, *Http.HttpError) {
	dsInfoData, err := h.GetData(Schema.Inventory, dsId)
	if err != nil {
		return nil, err
	}
	recordMap, ok := dsInfoData.(map[string]interface{})
	if !ok {
		return nil, Http.NewHttpError(fmt.Sprintf("%s:%s invalid data. convert to map[string]interface{} failed", Schema.Inventory, dsId), http.StatusInternalServerError)

	}
	record, e := Record.LoadMap(recordMap)
	if e != nil {
		return nil, Http.NewHttpError(fmt.Sprintf("%s:%s invalid data. failed to load as Record", Schema.Inventory, dsId), http.StatusInternalServerError)
	}
	return record, nil
}

func (h *Handler) GetDsInfo(dsId string) (*InvRecord.DataServiceInfo, *Http.HttpError) {
	record, err := h.GetDsRecord(dsId)
	if err != nil {
		return nil, err
	}
	dsInfo, e := InvRecord.CreateDsInfo(record.Data)
	if e != nil {
		return nil, Http.NewHttpError(e.Error(), http.StatusInternalServerError)
	}
	return dsInfo, nil
}

func (h *Handler) PutData(data map[string]interface{}) (string, *Http.HttpError) {
	record, err := Record.LoadMap(data)
	if err != nil {
		return "", Http.WrapError(err, "payload failed to be load as Record", http.StatusBadRequest)
	}
	if err = record.ValidateEnvelope(); err != nil {
		return "", Http.WrapError(err, "invalid record", http.StatusBadRequest)
	}
	if _, ok := EditableTypes[record.Type]; !ok {
		return "", Http.NewHttpError(fmt.Sprintf("update on type=[%s] not editable", record.Type), http.StatusBadRequest)
	}
	err = h.Db.CreateTable(record.Type, nil)
	if err != nil {
		return "", Http.WrapError(err, fmt.Sprintf("failed to init table=[%s]", record.Type), http.StatusInternalServerError)
	}
	if record.Type == SchemaPath.PathName {
		_, e := SchemaPath.LoadPathDataMap(record.Data)
		if e != nil {
			return "", e
		}
	}
	if record.Type == Schema.Inventory {
		if e := h.allocateDsId(record); e != nil {
			return "", e
		}
	}
	// Replace 的 keys 是过滤条件：MongoDB 按它匹配已有文档，带上文档里没有的字段（如 DbIface.Table）
	// 就永远匹配不上，upsert 每次都插入一条重复记录。
	args := map[string]interface{}{
		Record.DataType: record.Type,
		Record.DataId:   record.Id,
	}
	err = h.Db.Replace(record.Type, args, record.Map())
	if err != nil {
		return "", Http.NewHttpError(err.Error(), http.StatusInternalServerError)
	}
	if record.Type == Schema.Inventory && h.SyncChan != nil {
		// 非阻塞发送；channel 满则丢弃（全量 Sync 会重扫所有 DS 兜底）
		select {
		case h.SyncChan <- record.Id:
		default:
			h.Log(fmt.Sprintf("sync event channel full, drop sync event for DS=[%s]", record.Id))
		}
	}
	return record.Id, nil
}

// allocateDsId 确保 inventory 记录的 id（short name）全局唯一。
// 目标 id 空闲 → 直接用；被相同 instanceId 的记录占用 → 同 DS 重注册，原地更新；
// 被其它（或 legacy 空 instanceId）记录占用 → 撞名，分配 {name}_{n} 并改写 record.Id 与 data.dsId。
func (h *Handler) allocateDsId(record *Record.Record) *Http.HttpError {
	dsInfo, e := InvRecord.CreateDsInfo(record.Data)
	if e != nil {
		return Http.NewHttpError(e.Error(), http.StatusBadRequest)
	}
	if dsInfo.Signed() {
		return h.allocateSignedDsId(record, dsInfo)
	}
	existingInfo, err := h.getDsInfoOrNil(record.Id)
	if err != nil {
		return err
	}
	if existingInfo == nil {
		return nil // 名字空闲
	}
	// 同 DS 重注册，或现有记录为 legacy（无 instanceId）由首个带 instanceId 的注册原地接管
	if dsInfo.InstanceId != "" && (existingInfo.InstanceId == "" || existingInfo.InstanceId == dsInfo.InstanceId) {
		return nil
	}
	newId, allocErr := h.nextFreeDsId(record.Id)
	if allocErr != nil {
		return allocErr
	}
	record.Id = newId
	record.Data["dsId"] = newId
	h.Log(fmt.Sprintf("DataService short name [%s] taken, allocated [%s]", dsInfo.Id, newId))
	return nil
}

// fingerprintShort 返回指纹的前 8 位十六进制。
func fingerprintShort(fingerprint string) string {
	hexPart := strings.TrimPrefix(fingerprint, "SHA256:")
	if len(hexPart) > 8 {
		return hexPart[:8]
	}
	return hexPart
}

// DerivedDsId 返回撞名时由指纹派生的名字：{name}-{指纹前 8 位}。
func DerivedDsId(name string, fingerprint string) string {
	return fmt.Sprintf("%s-%s", name, fingerprintShort(fingerprint))
}

func (h *Handler) getDsInfoOrNil(dsId string) (*InvRecord.DataServiceInfo, *Http.HttpError) {
	info, err := h.GetDsInfo(dsId)
	if err != nil {
		if err.Status == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	return info, nil
}

// allocateSignedDsId 处理 shared / mirrored vDS 的注册。身份是公钥指纹（instanceId）：
//   - 同一个指纹已经以别的名字注册过：合并进已有记录，作为副本（同一个来源不会被计入两次）；
//     shared vDS 和它自己的镜像在同一个 Inventory 里也合并，kind 保持 shared
//   - 名字空闲，或被同一指纹占用：使用这个名字；同指纹时合并 URL（副本）
//   - 名字被别的指纹（或 local vDS）占用：改用 {name}-{指纹前 8 位}；
//     若该名字被 PinnedSources 固定给当前指纹，则挤掉占用者（删除其记录，占用者重新注册时会得到派生名字）
func (h *Handler) allocateSignedDsId(record *Record.Record, dsInfo *InvRecord.DataServiceInfo) *Http.HttpError {
	fingerprint := dsInfo.InstanceId
	if fingerprint == "" {
		return Http.NewHttpError(fmt.Sprintf("%s vDS registration requires instanceId (public key fingerprint)", dsInfo.Kind), http.StatusBadRequest)
	}
	requested := record.Id
	target := ""
	allRecords, err := h.InvListData(Schema.Inventory)
	if err != nil {
		return err
	}
	for _, data := range allRecords {
		info, e := InvRecord.CreateDsInfo(data[Record.Data])
		if e != nil || !info.Signed() {
			continue
		}
		if info.InstanceId == fingerprint && info.Id != requested {
			h.Log(fmt.Sprintf("WARNING: vDS with fingerprint [%s] is already registered as [%s], merge registration of [%s] into it as a replica; check the alias configuration", fingerprint, info.Id, requested))
			target = info.Id
			break
		}
	}
	if target == "" {
		pinned, isPinned := h.PinnedSources[requested]
		existing, err := h.getDsInfoOrNil(requested)
		if err != nil {
			return err
		}
		switch {
		case isPinned && pinned != fingerprint:
			target = DerivedDsId(requested, fingerprint)
			h.Log(fmt.Sprintf("WARNING: name [%s] is pinned to [%s], register fingerprint [%s] as [%s]", requested, pinned, fingerprint, target))
		case existing == nil:
			target = requested
		case existing.Signed() && existing.InstanceId == fingerprint:
			target = requested
		case isPinned && pinned == fingerprint:
			h.Log(fmt.Sprintf("WARNING: name [%s] is pinned to [%s], evict current holder [%s]", requested, fingerprint, existing.InstanceId))
			if e := h.Db.Delete(Schema.Inventory, map[string]interface{}{Record.DataType: Schema.Inventory, Record.DataId: requested}); e != nil {
				return Http.WrapError(e, fmt.Sprintf("failed to evict inventory record [%s]", requested), http.StatusInternalServerError)
			}
			target = requested
		default:
			target = DerivedDsId(requested, fingerprint)
			h.Log(fmt.Sprintf("WARNING: name [%s] is taken by [%s], register fingerprint [%s] as [%s]", requested, existing.InstanceId, fingerprint, target))
		}
	}
	current, err := h.getDsInfoOrNil(target)
	if err != nil {
		return err
	}
	urls := dsInfo.URL
	if current != nil {
		if !current.Signed() || current.InstanceId != fingerprint {
			return Http.NewHttpError(fmt.Sprintf("dsId [%s] is taken by another vDS, cannot register fingerprint [%s]", target, fingerprint), http.StatusConflict)
		}
		urls = mergeUrls(current.URL, dsInfo.URL)
		// shared vDS 与它的镜像在同一个 Inventory 里合并成一条记录；有源头时 kind 保持 shared
		if current.EffectiveKind() == InvRecord.KindShared {
			record.Data["kind"] = InvRecord.KindShared
		}
	}
	record.Id = target
	record.Data["dsId"] = target
	record.Data["url"] = toInterfaceList(urls)
	return nil
}

// mergeUrls 保持已有顺序，追加新的 URL。
func mergeUrls(existing []string, added []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, list := range [][]string{existing, added} {
		for _, url := range list {
			if url == "" || seen[url] {
				continue
			}
			seen[url] = true
			result = append(result, url)
		}
	}
	return result
}

func toInterfaceList(list []string) []interface{} {
	result := make([]interface{}, 0, len(list))
	for _, item := range list {
		result = append(result, item)
	}
	return result
}

// RemoveDsUrl 从 inventory 记录中移除一个 URL（vDS 从某个实例上删除）；没有剩余 URL 时删除整条记录。
func (h *Handler) RemoveDsUrl(dsId string, url string) *Http.HttpError {
	info, err := h.getDsInfoOrNil(dsId)
	if err != nil {
		return err
	}
	if info == nil {
		return nil
	}
	remaining := []string{}
	for _, item := range info.URL {
		if item != url {
			remaining = append(remaining, item)
		}
	}
	if len(remaining) == len(info.URL) {
		return nil
	}
	if len(remaining) == 0 {
		return h.DeleteData(Schema.Inventory, dsId)
	}
	info.URL = remaining
	return h.ReplaceDsInfo(info)
}

// ReplaceDsInfo 覆盖写入一条 inventory 记录（不做名字分配）。
func (h *Handler) ReplaceDsInfo(info *InvRecord.DataServiceInfo) *Http.HttpError {
	dsMap, e := Json.CopyToMap(info)
	if e != nil {
		return Http.WrapError(e, "failed to copy ds info", http.StatusInternalServerError)
	}
	record := Record.NewRecord(Schema.Inventory, InvRecord.LatestVer, info.Id, dsMap)
	e = h.Db.Replace(Schema.Inventory, map[string]interface{}{
		Record.DataType: Schema.Inventory,
		Record.DataId:   info.Id,
	}, record.Map())
	if e != nil {
		return Http.NewHttpError(e.Error(), http.StatusInternalServerError)
	}
	return nil
}

// nextFreeDsId 返回 {name}_1、{name}_2… 中第一个空闲的 inventory id。
func (h *Handler) nextFreeDsId(name string) (string, *Http.HttpError) {
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s_%d", name, i)
		_, err := h.GetData(Schema.Inventory, candidate)
		if err != nil && err.Status == http.StatusNotFound {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
	}
}

func (h *Handler) DeleteData(dataType string, dataId string) *Http.HttpError {
	if dataType == "" || dataId == "" {
		return Http.NewHttpError("invalid url for delete, expected=[{dataType}/{dataId}]", http.StatusBadRequest)
	}
	if _, ok := EditableTypes[dataType]; !ok {
		return Http.NewHttpError(fmt.Sprintf("delete on type=[%s] not supported", dataType), http.StatusBadRequest)
	}
	err := h.Db.CreateTable(dataType, nil)
	if err != nil {
		return Http.WrapError(err, fmt.Sprintf("failed to init table=[%s]", dataType), http.StatusInternalServerError)
	}
	_, e := h.GetData(dataType, dataId)
	if e != nil {
		if e.Status == http.StatusNotFound {
			return nil
		}
		return e
	}
	err = h.Db.Delete(dataType, map[string]interface{}{Record.DataType: dataType, Record.DataId: dataId})
	if err != nil {
		return Http.NewHttpError(err.Error(), http.StatusInternalServerError)
	}
	return nil
}
