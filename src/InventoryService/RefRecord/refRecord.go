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

package RefRecord

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"InventoryService/InvRecord"

	"github.com/salesforce/UniTAO/lib/Schema/JsonKey"
	"github.com/salesforce/UniTAO/lib/Schema/Record"
	"github.com/salesforce/UniTAO/lib/Util/Http"
	"github.com/salesforce/UniTAO/lib/Util/Json"
)

const (
	Referral     = "referral"
	LatestVer    = "0.0.1"
	SchemaRecord = `{
		"__id": "referral",
		"__type": "schema",
		"__ver": "0.0.1",
		"data": {
			"name": "referral",
			"description": "referral record schema",
			"version": "0.0.1",
			"properties": {
				"DataType": {
					"type": "string"
				},
				"DataServices": {
					"type": "array",
					"items": {
						"type": "string",
						"contentMediaType": "inventory/inventory"
					}
				},
				"AuthUrl": {
					"type": "string"
				},
				"AuthType": {
					"type": "string"
				}
			}
		}
	}`
)

type ReferralData struct {
	DataType     string   `json:"DataType"`
	DataServices []string `json:"DataServices"`
	AuthUrl      string   `json:"AuthUrl"`
	AuthType     string   `json:"AuthType"`
	Schema       map[string]interface{}
	DsInfos      []*InvRecord.DataServiceInfo `json:"DsInfos"`
}

func LoadMap(data map[string]interface{}) (*ReferralData, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to Marshal data for ReferralRecord. Error:%s", err)
	}
	record := ReferralData{}
	err = json.Unmarshal(raw, &record)
	if err != nil {
		return nil, fmt.Errorf("failed to parse map data for ReferralRecord. Error:%s", err)
	}
	return &record, nil
}

// GetSchema 遍历所有 DsInfos，返回第一个成功获取的 schema（各分片 schema 一致）。
func (r *ReferralData) GetSchema(dataType string, logger *log.Logger) (*Record.Record, *Http.HttpError) {
	if logger == nil {
		logger = log.Default()
	}
	if len(r.DsInfos) == 0 {
		msg := fmt.Sprintf("failed to load DsInfos for type=[%s]", dataType)
		logger.Print(msg)
		return nil, Http.NewHttpError(msg, http.StatusInternalServerError)
	}
	var lastErr string
	for _, dsInfo := range r.DsInfos {
		if dsInfo == nil {
			continue
		}
		dsUrl, err := dsInfo.GetUrl()
		if err != nil {
			logger.Printf("no good url to DS=[%s], error:%s", dsInfo.Id, err)
			lastErr = err.Error()
			continue
		}
		schemaUrl := fmt.Sprintf("%s/%s/%s", dsUrl, JsonKey.Schema, dataType)
		schemaData, code, err := Http.GetRestData(schemaUrl)
		if err != nil {
			logger.Print(err.Error())
			lastErr = fmt.Sprintf("%s (http code %d)", err.Error(), code)
			continue
		}
		schema, ok := schemaData.(map[string]interface{})
		if !ok {
			msg := fmt.Sprintf("failed to parse schema record. from path=[%s]", schemaUrl)
			logger.Print(msg)
			lastErr = msg
			continue
		}
		schemaRecord, err := Record.LoadMap(schema)
		if err != nil {
			msg := "schema from dataservice is not in Record format."
			logger.Printf("%s, Error: %s", msg, err)
			lastErr = msg
			continue
		}
		return schemaRecord, nil
	}
	return nil, Http.NewHttpError(
		fmt.Sprintf("failed to fetch schema for type=[%s] from any DataService, last error: %s", dataType, lastErr),
		http.StatusInternalServerError)
}

func (r *ReferralData) GetRecord() *Record.Record {
	rMap, _ := Json.CopyToMap(r)
	return Record.NewRecord(Referral, LatestVer, r.DataType, rMap)
}
