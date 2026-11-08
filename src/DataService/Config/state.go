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
	"sync"
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
	// Vds 按 vDS 路径（local、shared/{name}、mirrored/{name}）保存各 vDS 的运行时状态。
	// 也用于启动时发现已从 config 中删除的 vDS。
	Vds map[string]*VdsState `json:"vds,omitempty"`
	// Unregister 为已从 config 删除、尚未从 Inventory 注销的 vDS URL，注销成功后移除。
	// 启动时先处理完它们再注册当前的 vDS：否则改了 alias 的 mirrored 用新名字注册时，
	// 旧记录还在，会因指纹相同被合并进旧记录，拿回旧名字。
	Unregister []UnregisterTask `json:"unregister,omitempty"`
	path       string
	lock       sync.Mutex
}

// UnregisterTask 是一个待从 Inventory 注销的 vDS URL。
type UnregisterTask struct {
	DsId string `json:"dsId"`
	Url  string `json:"url"`
}

// AddUnregister 记录一个待注销的 URL（重复的忽略）。
func (s *State) AddUnregister(dsId string, url string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	task := UnregisterTask{DsId: dsId, Url: url}
	for _, item := range s.Unregister {
		if item == task {
			return
		}
	}
	s.Unregister = append(s.Unregister, task)
}

// PendingUnregister 返回待注销 URL 的副本。
func (s *State) PendingUnregister() []UnregisterTask {
	s.lock.Lock()
	defer s.lock.Unlock()
	return append([]UnregisterTask{}, s.Unregister...)
}

// DoneUnregister 移除已处理的待注销 URL。
func (s *State) DoneUnregister(task UnregisterTask) {
	s.lock.Lock()
	defer s.lock.Unlock()
	remaining := []UnregisterTask{}
	for _, item := range s.Unregister {
		if item != task {
			remaining = append(remaining, item)
		}
	}
	s.Unregister = remaining
}

// VdsState 是单个 vDS 的运行时状态。
type VdsState struct {
	Initialized bool   `json:"initialized"`
	Table       string `json:"table"`
	// AssignedId 为 Inventory 撞名时分配的 dsId；AssignedFrom 为分配时请求的名字，
	// 请求名字（config 中的名字）改变后 AssignedId 作废。
	AssignedId   string `json:"assignedId,omitempty"`
	AssignedFrom string `json:"assignedFrom,omitempty"`
	// mirrored：当前订阅请求 id、该请求的 history 是否已加载、已应用到的最大 seq、同步到的来源公钥指纹
	Request           string `json:"request,omitempty"`
	Loaded            bool   `json:"loaded,omitempty"`
	Cursor            uint64 `json:"cursor,omitempty"`
	SourceFingerprint string `json:"sourceFingerprint,omitempty"`
}

// VdsState 返回指定 vDS 的状态，不存在时创建。
func (s *State) VdsState(path string) *VdsState {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.Vds == nil {
		s.Vds = map[string]*VdsState{}
	}
	st, ok := s.Vds[path]
	if !ok {
		st = &VdsState{}
		s.Vds[path] = st
	}
	return st
}

// VdsPaths 返回 state 中记录的所有 vDS 路径。
func (s *State) VdsPaths() []string {
	s.lock.Lock()
	defer s.lock.Unlock()
	result := []string{}
	for path := range s.Vds {
		result = append(result, path)
	}
	return result
}

// RemoveVds 删除指定 vDS 的状态。
func (s *State) RemoveVds(path string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	delete(s.Vds, path)
}

// UpdateVds 在锁内修改 vDS 状态并写回 state.json。
func (s *State) UpdateVds(path string, config *Confuguration, update func(st *VdsState)) error {
	st := s.VdsState(path)
	s.lock.Lock()
	update(st)
	s.lock.Unlock()
	return s.Save(config)
}

func StatePath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), StateFile)
}

// LoadState 读取 state.json 并合入 config。
// state.json 不存在时，以 config 中旧版本回写的 initialized / ds.instanceId 为初值并立即写出，
// 避免升级后 instanceId 变化，被 Inventory 判定为另一个撞名的 DS。
func LoadState(configPath string, config *Confuguration) (*State, error) {
	state := &State{path: StatePath(configPath), Vds: map[string]*VdsState{}}
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
	s.lock.Lock()
	defer s.lock.Unlock()
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
