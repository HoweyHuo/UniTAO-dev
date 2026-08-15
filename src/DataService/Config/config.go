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

package Config

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"

	"Data/DbConfig"

	"github.com/salesforce/UniTAO/lib/Util/Http"
	"github.com/salesforce/UniTAO/lib/Util/Json"
)

const (
	DATABASE = "database"
	HTTP     = "http"
)

// DsConfig 描述本 DataService 自身的身份与对外可访问地址。
// Id 为该 DataService 在 Inventory Service 中注册的标识（DsId），首次部署为空时
// 自动生成 UUID 并回写 config；InstanceId 为稳定实例标识（UUID），用于区分"同 DS
// 重注册"与"不同 DS 撞名"；Urls 为可被 Inventory Service 访问的 URL 列表。
type DsConfig struct {
	Id         string   `json:"id"`
	InstanceId string   `json:"instanceId"`
	Urls       []string `json:"urls"`
}

type Confuguration struct {
	Database    DbConfig.DatabaseConfig `json:"database"`
	DataTable   DataTableConfig         `json:"table"`
	Http        Http.Config             `json:"http"`
	Ds          DsConfig                `json:"ds"`
	Inv         InvConfig               `json:"inventory"`
	Initialized bool                    `json:"initialized"`
	ForceInit   bool                    `json:"force-init"`
	Init        InitConfig              `json:"init"`
}

type InitConfig struct {
	TablesFile string `json:"tablesFile"`
	SchemaFile string `json:"schemaFile"`
}

type DataTableConfig struct {
	Data string `json:"data"`
}

func (t *DataTableConfig) Map() map[string]interface{} {
	data, _ := Json.CopyToMap(t)
	return data
}

type InvConfig struct {
	Url string `json:"url"`
}

func Read(configPath string, config *Confuguration) error {
	jsonFile, err := os.Open(configPath)
	if err != nil {
		return fmt.Errorf("failed to open Config JSON file: [%s], err:%s", configPath, err)

	}
	defer jsonFile.Close()
	byteValue, _ := ioutil.ReadAll(jsonFile)
	json.Unmarshal([]byte(byteValue), config)
	if config.DataTable.Data == "" {
		return fmt.Errorf("missing field data in Config.DataTable")
	}
	return nil
}

func Write(configPath string, config *Confuguration) error {
	data, err := json.MarshalIndent(config, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to marshal Config JSON, err:%s", err)
	}
	err = ioutil.WriteFile(configPath, data, 0644)
	if err != nil {
		return fmt.Errorf("failed to write Config JSON file: [%s], err:%s", configPath, err)
	}
	return nil
}
