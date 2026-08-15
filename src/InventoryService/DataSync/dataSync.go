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

// Package DataSync 负责 Inventory Service 中 referral 与 Data Service schema 的同步。
// 逻辑由 InventoryServiceAdmin 工具迁入，供后台定时循环、注册事件触发与 CLI 复用。
package DataSync

import (
	"fmt"
	"log"
	"net/http"

	"DataService/Common"
	"InventoryService/DataHandler"
	"InventoryService/RefRecord"

	"github.com/salesforce/UniTAO/lib/Schema"
	"github.com/salesforce/UniTAO/lib/Schema/JsonKey"
	"github.com/salesforce/UniTAO/lib/Schema/Record"
	"github.com/salesforce/UniTAO/lib/Util"
	"github.com/salesforce/UniTAO/lib/Util/Http"
	"github.com/salesforce/UniTAO/lib/Util/Json"
)

// Syncer 基于 Inventory 数据层执行 referral 对账。
type Syncer struct {
	log     *log.Logger
	handler *DataHandler.Handler
}

// New 构造 Syncer；logger 为空时使用默认 logger。
func New(handler *DataHandler.Handler, logger *log.Logger) *Syncer {
	if logger == nil {
		logger = log.Default()
	}
	return &Syncer{log: logger, handler: handler}
}

// Sync 全量对账：以 Inventory 中注册的 DS 为准重算每个 data type 的 referral。
// 任一 DS 的 记录/URL/schema 获取失败即整体中止（与既有 admin 语义一致），
// 保证不会因部分失败而清掉已存在的 referral。
func (s *Syncer) Sync() error {
	s.log.Printf("[sync] start")
	idList, err := s.handler.List(Schema.Inventory)
	if err != nil {
		return fmt.Errorf("failed to list all inventorys. Error: %s", err)
	}
	s.log.Printf("[%d] Data Services to sync", len(idList))
	refTypes, ex := s.getReferralTypes()
	if ex != nil {
		s.log.Printf("failed to collect existing referral types. Error: %s", ex)
		return ex
	}
	dsTypes, ex := s.getDsTypes(idList)
	if ex != nil {
		s.log.Printf("failed to collect data types from Data Services. Error: %s", ex)
		return ex
	}
	return s.syncDataTypes(refTypes, dsTypes)
}

// SyncDs 定向同步单个新注册/更新的 DS：把该 DS 的 schema 类型 merge 进 referral，
// 只增不改不删；即使随后的全量 Sync() 因其它不可达 DS 中止，新 DS 类型也已及时注册。
func (s *Syncer) SyncDs(dsId string) error {
	s.log.Printf("[sync-ds] sync DS=[%s]", dsId)
	ds, err := s.handler.GetDsInfo(dsId)
	if err != nil {
		return fmt.Errorf("failed to get info of DataService[%s], Error: %s", dsId, err)
	}
	dsUrl, e := ds.GetUrl()
	if e != nil {
		return fmt.Errorf("failed to get URL for ds[%s], Error: %s", dsId, e)
	}
	schemaUrl, e := Http.URLPathJoin(dsUrl, JsonKey.Schema)
	if e != nil {
		return fmt.Errorf("failed to build schema url for ds[%s], Error: %s", dsId, e)
	}
	s.log.Printf("[sync-ds] DataService[%s], schema URL=[%s]", dsId, *schemaUrl)
	result, code, e := Http.GetRestData(*schemaUrl)
	if e != nil {
		return fmt.Errorf("failed to Rest Data from [path]=[%s], Code:%d", *schemaUrl, code)
	}
	typeList, ok := result.([]interface{})
	if !ok {
		return fmt.Errorf("schema list from DS[%s] is not an array", dsId)
	}
	for _, typeObj := range typeList {
		typeStr, ok := typeObj.(string)
		if !ok {
			continue
		}
		dataType, _ := Util.ParseCustomPath(typeStr, JsonKey.ArchivedSchemaIdDiv)
		if _, ok := Common.InternalTypes[dataType]; ok {
			s.log.Printf("[sync-ds] type[%s] @DS[%s] is internal type, skip", dataType, dsId)
			continue
		}
		if e := s.mergeType(dsId, dataType); e != nil {
			return e
		}
	}
	return nil
}

// mergeType 读现有 referral（不存在则新建），把 dsId 并入 DataServices 后写回。
// 仅追加、不删除其它 DS，天然幂等。
func (s *Syncer) mergeType(dsId string, dataType string) error {
	current := []string{}
	record, err := s.handler.GetReferralRecord(dataType)
	if err != nil {
		if err.Status != http.StatusNotFound {
			return fmt.Errorf("failed to get referral[%s], Error: %s", dataType, err)
		}
		s.log.Printf("[sync-ds] no existing referral for type[%s], create new", dataType)
	} else {
		referral, e := RefRecord.LoadMap(record.Data)
		if e != nil {
			s.log.Printf("[sync-ds] failed to parse referral[%s], rebuild from DS=[%s], Error: %s", dataType, dsId, e)
		} else {
			current = dedupeStrings(referral.DataServices)
		}
	}
	desired := dedupeStrings(append(current, dsId))
	if setEqual(current, desired) {
		s.log.Printf("[sync-ds] type[%s] @DS[%s] already up to date", dataType, dsId)
		return nil
	}
	return s.setType(dataType, desired)
}

// getReferralTypes 读取现有 referral 记录，得到 map[dataType][]dsId。
// 直接读原始记录（GetReferral 会做可达性探测且对旧格式报错），
// 旧格式（仅 DataServiceId）解析出空数组，交由 syncDataTypes 改写为数组。
func (s *Syncer) getReferralTypes() (map[string][]string, error) {
	typeList, err := s.handler.List(RefRecord.Referral)
	if err != nil {
		s.log.Printf("failed to get list of [%s], Error: %s", RefRecord.Referral, err)
		return nil, err
	}
	refTypes := map[string][]string{}
	for _, dataType := range typeList {
		record, err := s.handler.GetReferralRecord(dataType.(string))
		if err != nil {
			s.log.Printf("failed to get %s: [%s], Error: %s", RefRecord.Referral, dataType, err)
			continue
		}
		referral, e := RefRecord.LoadMap(record.Data)
		if e != nil {
			s.log.Printf("failed to parse %s: [%s], Error: %s", RefRecord.Referral, dataType, e)
			continue
		}
		s.log.Printf("[sync] record current Referral[%s] from DS[%v]", dataType, referral.DataServices)
		refTypes[dataType.(string)] = dedupeStrings(referral.DataServices)
	}
	return refTypes, nil
}

// getDsTypes 从每个 DS 的 /schema 端点收集其持有的 data type，得到 map[dataType][]dsId。
func (s *Syncer) getDsTypes(idList []interface{}) (map[string][]string, error) {
	dsTypes := map[string][]string{}
	for _, dsIdObj := range idList {
		dsId := dsIdObj.(string)
		ds, err := s.handler.GetDsInfo(dsId)
		if err != nil {
			s.log.Printf("failed to get info of DataService[%s], Error: %s", dsId, err)
			return nil, err
		}
		dsUrl, e := ds.GetUrl()
		if e != nil {
			s.log.Printf("failed to get URL for ds[%s], Error: %s", dsId, e)
			return nil, e
		}
		schemaUrl, e := Http.URLPathJoin(dsUrl, JsonKey.Schema)
		if e != nil {
			return nil, fmt.Errorf("failed to build schema url for ds[%s], Err:%s", dsId, e)
		}
		s.log.Printf("[sync] DataService[%s], schema URL=[%s]", dsId, *schemaUrl)
		result, code, e := Http.GetRestData(*schemaUrl)
		if e != nil {
			return nil, fmt.Errorf("failed to Rest Data from [path]=[%s], Code:%d", *schemaUrl, code)
		}
		typeList, ok := result.([]interface{})
		if !ok {
			return nil, fmt.Errorf("schema list from DS[%s] is not an array", dsId)
		}
		for _, typeObj := range typeList {
			typeStr, ok := typeObj.(string)
			if !ok {
				continue
			}
			dataType, _ := Util.ParseCustomPath(typeStr, JsonKey.ArchivedSchemaIdDiv)
			if _, ok := Common.InternalTypes[dataType]; ok {
				s.log.Printf("[sync] type[%s] @DS[%s] is internal type, skip", dataType, dsId)
				continue
			}
			if contains(dsTypes[dataType], dsId) {
				s.log.Printf("[sync] type[%s] @DS[%s] already recorded", dataType, dsId)
				continue
			}
			s.log.Printf("[sync] record type[%s] from DS[%s]", dataType, dsId)
			dsTypes[dataType] = append(dsTypes[dataType], dsId)
		}
	}
	return dsTypes, nil
}

// syncDataTypes 依据收集结果对账 referral：类型在任一 DS 上已消失则移除；
// 与期望 DS 列表不一致则重建。
func (s *Syncer) syncDataTypes(refTypes map[string][]string, dsTypes map[string][]string) error {
	for dataType := range refTypes {
		if _, ok := dsTypes[dataType]; !ok {
			s.log.Printf("[sync] data type [%s] no longer exists on any DataService. remove referral", dataType)
			if err := s.removeType(dataType); err != nil {
				s.log.Printf("remove referral [%s] failed. Error: %s", dataType, err)
				return err
			}
		}
	}
	for dataType, dsIdList := range dsTypes {
		current, exists := refTypes[dataType]
		desired := dedupeStrings(dsIdList)
		if !exists || !setEqual(current, desired) {
			s.log.Printf("[sync] set referral for type[%s] to DS[%v]", dataType, desired)
			if err := s.setType(dataType, desired); err != nil {
				s.log.Printf("set referral type [%s] failed. Error: %s", dataType, err)
				return err
			}
		}
	}
	return nil
}

// setType 全量 upsert referral 记录（DynamoDB 复合键必需 DataType + DataId）。
func (s *Syncer) setType(dataType string, dsIdList []string) error {
	referral := RefRecord.ReferralData{
		DataType:     dataType,
		DataServices: dedupeStrings(dsIdList),
	}
	referralData, _ := Json.CopyToMap(referral.GetRecord())
	keys := map[string]interface{}{
		Record.DataType: RefRecord.Referral,
		Record.DataId:   dataType,
	}
	if e := s.handler.Db.Replace(RefRecord.Referral, keys, referralData); e != nil {
		return e
	}
	s.log.Printf("[sync] referral type[%s] to DS[%v] set", dataType, referral.DataServices)
	return nil
}

// removeType 删除指定 data type 的 referral 记录。
func (s *Syncer) removeType(dataType string) error {
	keys := map[string]interface{}{
		Record.DataType: RefRecord.Referral,
		Record.DataId:   dataType,
	}
	return s.handler.Db.Delete(RefRecord.Referral, keys)
}

// dedupeStrings 去重并剔除空串，保持首次出现顺序。
func dedupeStrings(list []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, v := range list {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		result = append(result, v)
	}
	return result
}

// setEqual 无序集合相等（输入不含重复）。
func setEqual(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, v := range a {
		set[v] = true
	}
	for _, v := range b {
		if !set[v] {
			return false
		}
	}
	return true
}

// contains 线性成员判断。
func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
