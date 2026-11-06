package DataSync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"InventoryService/InvRecord"

	"github.com/salesforce/UniTAO/lib/Schema"
	"github.com/salesforce/UniTAO/lib/Schema/JsonKey"
	"github.com/salesforce/UniTAO/lib/Schema/Record"
	"github.com/salesforce/UniTAO/lib/Util"
	"github.com/salesforce/UniTAO/lib/Util/Http"
)

// 同名同版本的 schema 内容必须相同（D6）。不同 vDS 提供同一个类型时，按版本比较 schema 内容的 hash；
// 与已接受的 DS 冲突的 DS 不加入该类型的 referral，并告警。

// schemaVersionHashes 返回某 DS 上某类型所有版本（当前 + 归档）的 schema 内容 hash：version -> hash。
func (s *Syncer) schemaVersionHashes(dsId string, dataType string, schemaIds []string) (map[string]string, error) {
	ds, err := s.handler.GetDsInfo(dsId)
	if err != nil {
		return nil, fmt.Errorf("failed to get info of DataService[%s], Error: %s", dsId, err)
	}
	dsUrl, e := ds.GetUrl()
	if e != nil {
		return nil, e
	}
	if schemaIds == nil {
		schemaIds, e = s.listSchemaIds(dsUrl)
		if e != nil {
			return nil, e
		}
	}
	result := map[string]string{}
	for _, id := range schemaIds {
		base, _ := Util.ParseCustomPath(id, JsonKey.ArchivedSchemaIdDiv)
		if base != dataType {
			continue
		}
		schemaUrl, e := Http.URLPathJoin(dsUrl, JsonKey.Schema, id)
		if e != nil {
			return nil, e
		}
		data, code, e := Http.GetRestData(*schemaUrl)
		if e != nil {
			return nil, fmt.Errorf("failed to get schema [%s] from DS[%s], Code:%d, Error:%s", id, dsId, code, e)
		}
		recordMap, ok := data.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("schema [%s] from DS[%s] is not a record", id, dsId)
		}
		record, e := Record.LoadMap(recordMap)
		if e != nil {
			return nil, e
		}
		version, _ := record.Data[JsonKey.Version].(string)
		raw, e := json.Marshal(record.Data) // encoding/json 对 map 的 key 排序，结果是规范化的
		if e != nil {
			return nil, e
		}
		sum := sha256.Sum256(raw)
		result[version] = hex.EncodeToString(sum[:])
	}
	return result, nil
}

func (s *Syncer) listSchemaIds(dsUrl string) ([]string, error) {
	schemaUrl, e := Http.URLPathJoin(dsUrl, JsonKey.Schema)
	if e != nil {
		return nil, e
	}
	result, code, e := Http.GetRestData(*schemaUrl)
	if e != nil {
		return nil, fmt.Errorf("failed to Rest Data from [path]=[%s], Code:%d", *schemaUrl, code)
	}
	list, ok := result.([]interface{})
	if !ok {
		return nil, fmt.Errorf("schema list from [%s] is not an array", *schemaUrl)
	}
	ids := []string{}
	for _, item := range list {
		if id, ok := item.(string); ok {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// filterConsistent 按顺序接受 DS：与已接受的 DS 在任一共同版本上 schema 内容不同的 DS 被排除。
// schemaIds 为可选的预取 schema id 列表（dsId -> ids），没有时现取。
func (s *Syncer) filterConsistent(dataType string, dsIds []string, schemaIds map[string][]string) []string {
	if len(dsIds) <= 1 {
		return dsIds
	}
	established := map[string]string{}
	owner := map[string]string{}
	accepted := []string{}
	for _, dsId := range dsIds {
		var ids []string
		if schemaIds != nil {
			ids = schemaIds[dsId]
		}
		hashes, err := s.schemaVersionHashes(dsId, dataType, ids)
		if err != nil {
			// 取不到 schema 时不据此排除，保持原有行为（由可达性检查决定）
			s.log.Printf("[sync] failed to compare schema [%s] on DS[%s], keep it. Error: %s", dataType, dsId, err)
			accepted = append(accepted, dsId)
			continue
		}
		conflict := ""
		for version, hash := range hashes {
			if existing, ok := established[version]; ok && existing != hash {
				conflict = fmt.Sprintf("version [%s] differs from DS[%s]", version, owner[version])
				break
			}
		}
		if conflict != "" {
			s.log.Printf("[sync] WARNING: schema [%s] on DS[%s] conflicts with an accepted DataService: %s. same name and version must have the same content; DS[%s] is excluded from referral of [%s]", dataType, dsId, conflict, dsId, dataType)
			continue
		}
		for version, hash := range hashes {
			if _, ok := established[version]; !ok {
				established[version] = hash
				owner[version] = dsId
			}
		}
		accepted = append(accepted, dsId)
	}
	return accepted
}

// orderedDsIds 给出类型的 DS 的优先顺序：已在 referral 中的保持原顺序在前，新的在后。
func orderedDsIds(current []string, desired []string) []string {
	result := []string{}
	inDesired := map[string]bool{}
	for _, id := range desired {
		inDesired[id] = true
	}
	seen := map[string]bool{}
	for _, id := range current {
		if inDesired[id] && !seen[id] {
			result = append(result, id)
			seen[id] = true
		}
	}
	for _, id := range desired {
		if !seen[id] {
			result = append(result, id)
			seen[id] = true
		}
	}
	return result
}

// sortReplicaUrls 对有多个副本 URL 的签名 vDS，按各副本 /config 中的 cursor 从大到小排序 url，
// 使 GetUrl（取第一个可达的）优先落在最新的副本上。shared 源头排在最前，取不到 cursor 的副本排在最后。
func (s *Syncer) sortReplicaUrls() {
	idList, err := s.handler.List(Schema.Inventory)
	if err != nil {
		return
	}
	for _, idObj := range idList {
		dsId, _ := idObj.(string)
		info, err := s.handler.GetDsInfo(dsId)
		if err != nil || !info.Signed() || len(info.URL) < 2 {
			continue
		}
		cursors := map[string]float64{}
		for _, url := range info.URL {
			cursors[url] = -1
			configUrl, e := Http.URLPathJoin(url, "config")
			if e != nil {
				continue
			}
			data, _, e := Http.GetRestData(*configUrl)
			if e != nil {
				continue
			}
			if dataMap, ok := data.(map[string]interface{}); ok {
				if dataMap["kind"] == InvRecord.KindShared {
					cursors[url] = math.MaxFloat64
					continue
				}
				if cursor, ok := dataMap["cursor"].(float64); ok {
					cursors[url] = cursor
				}
			}
		}
		sorted := append([]string{}, info.URL...)
		sort.SliceStable(sorted, func(i, j int) bool { return cursors[sorted[i]] > cursors[sorted[j]] })
		changed := false
		for idx := range sorted {
			if sorted[idx] != info.URL[idx] {
				changed = true
				break
			}
		}
		if !changed {
			continue
		}
		info.URL = sorted
		if err := s.handler.ReplaceDsInfo(info); err != nil {
			s.log.Printf("[sync] failed to reorder replica urls of [%s], Error: %s", dsId, err)
			continue
		}
		s.log.Printf("[sync] reordered replica urls of [%s] by cursor: %v", dsId, sorted)
	}
}
