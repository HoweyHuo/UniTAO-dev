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

package InvRecord

import (
	"encoding/json"
	"fmt"

	"github.com/salesforce/UniTAO/lib/Schema"
	"github.com/salesforce/UniTAO/lib/Schema/Record"
	"github.com/salesforce/UniTAO/lib/Util/Http"
	"github.com/salesforce/UniTAO/lib/Util/Json"
)

const (
	LatestVer    = "0.0.1"
	SchemaRecord = `{
		"__id": "inventory",
		"__type": "schema",
		"__ver": "0.0.1",
		"data": {
			"name": "inventory",
			"description": "DataService Inventory Info",
			"version": "0.0.1",
			"properties": {
				"dsId": {
					"type": "string",
					"description": "id assigned to the Data Service, unique within the Inventory Service"
				},
				"instanceId": {
					"type": "string",
					"description": "identity of the registered vDS: the DS instance UUID for a local vDS, the source id (sha256 of the channel address, the same for a shared vDS and all its mirrors) for a shared or mirrored vDS; distinguishes a re-registration or replica of the same vDS from another one using the same dsId"
				},
				"kind": {
					"type": "string",
					"description": "kind of the registered virtual Data Service: local, shared or mirrored; empty is treated as local",
					"required": false
				},
				"url": {
					"type": "array",
					"items": {
						"type": "string"
					},
					"description": "URLs the Inventory Service can use to reach this vDS; the first reachable one is used. A shared or mirrored vDS hosted by several instances (replicas) lists all of them, ordered by sync cursor, newest first"
				},
				"lastSynctime": {
					"type": "string",
					"description": "timestamp of the last schema sync reported for this Data Service",
					"required": false
				}
			}
		}
	}`
)

const (
	KindLocal    = "local"
	KindShared   = "shared"
	KindMirrored = "mirrored"
)

type DataServiceInfo struct {
	Id           string   `json:"dsId"`
	InstanceId   string   `json:"instanceId"`
	Kind         string   `json:"kind,omitempty"`
	URL          []string `json:"url"`
	LastSyncTime string   `json:"lastSynctime,omitempty"`
	goodUrl      string
}

// Signed 判断该 vDS 是否以来源身份（SourceId）为身份（shared / mirrored）。
func (ds *DataServiceInfo) Signed() bool {
	return ds.Kind == KindShared || ds.Kind == KindMirrored
}

// EffectiveKind 返回 kind，空值视为 local。
func (ds *DataServiceInfo) EffectiveKind() string {
	if ds.Kind == "" {
		return KindLocal
	}
	return ds.Kind
}

func NewDsInfo(id string, url string) *Record.Record {
	dsInfo := DataServiceInfo{
		Id:  id,
		URL: []string{url},
	}
	dsMap, _ := Json.CopyToMap(dsInfo)
	record := Record.NewRecord(Schema.Inventory, LatestVer, id, dsMap)
	return record
}

func CreateDsInfo(payload interface{}) (*DataServiceInfo, error) {
	ds_marshalled, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal payload to bytes as json, Err:%s", err)
	}
	dsInfo := DataServiceInfo{}
	err = json.Unmarshal(ds_marshalled, &dsInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to load json payload as DataServiceInfo, Err:%s", err)
	}
	return &dsInfo, nil
}

// GetUrl 按顺序返回第一个可达的 URL（副本按 cursor 从新到旧排序）。
func (ds *DataServiceInfo) GetUrl() (string, error) {
	if ds.goodUrl == "" || !Http.SiteReachable(ds.goodUrl) {
		ds.goodUrl = ""
		for _, url := range ds.URL {
			if Http.SiteReachable(url) {
				ds.goodUrl = url
				break
			}
		}
		if ds.goodUrl == "" {
			return "", fmt.Errorf("no good url is reachable for DS=[%s]", ds.Id)
		}
	}
	return ds.goodUrl, nil
}
