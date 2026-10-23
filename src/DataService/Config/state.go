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
	"path/filepath"
)

const StateFile = "state.json"

// State 保存 DataService 运行时自己产生、需要跨重启保留的值，存放在 config 同目录的
// state.json 中。DataService 只读 config.json，从不回写它。
type State struct {
	Initialized bool `json:"initialized"`
	// DsId 为自动生成或 Inventory 撞名时分配的 ds.id；与 config 中的 ds.id 相同时为空。
	DsId string `json:"dsId,omitempty"`
	// DsIdFrom 为 DsId 产生时 config 中的 ds.id。config 的 ds.id 被人工修改后两者不一致，
	// DsId 作废，以 config 为准。
	DsIdFrom   string `json:"dsIdFrom"`
	InstanceId string `json:"instanceId"`
	path       string
}

func StatePath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), StateFile)
}

// LoadState 读取 state.json 并合入 config。
// state.json 不存在时，以 config 中旧版本回写的 initialized / ds.instanceId 为初值并立即写出，
// 避免升级后 instanceId 变化，被 Inventory 判定为另一个撞名的 DS。
func LoadState(configPath string, config *Confuguration) (*State, error) {
	state := &State{path: StatePath(configPath)}
	configDsId := config.Ds.Id
	data, err := ioutil.ReadFile(state.path)
	if os.IsNotExist(err) {
		state.DsIdFrom = configDsId
		if err := state.Save(config); err != nil {
			return nil, err
		}
		return state, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read state file: [%s], err:%s", state.path, err)
	}
	if err := json.Unmarshal(data, state); err != nil {
		return nil, fmt.Errorf("failed to parse state file: [%s], err:%s", state.path, err)
	}
	config.Initialized = state.Initialized
	if state.InstanceId != "" {
		config.Ds.InstanceId = state.InstanceId
	}
	if state.DsId != "" && state.DsIdFrom == configDsId {
		config.Ds.Id = state.DsId
	}
	state.DsIdFrom = configDsId
	return state, nil
}

// Save 把 config 中的运行时值写入 state.json。
func (s *State) Save(config *Confuguration) error {
	s.Initialized = config.Initialized
	s.InstanceId = config.Ds.InstanceId
	s.DsId = ""
	if config.Ds.Id != s.DsIdFrom {
		s.DsId = config.Ds.Id
	}
	data, err := json.MarshalIndent(s, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to marshal state JSON, err:%s", err)
	}
	if err := ioutil.WriteFile(s.path, data, 0644); err != nil {
		return fmt.Errorf("failed to write state file: [%s], err:%s", s.path, err)
	}
	return nil
}
