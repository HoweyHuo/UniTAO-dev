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

package DataServiceTest

import (
	"DataService/Config"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeTestConfig(t *testing.T, dir string, content string) string {
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config. Error:%s", err)
	}
	return configPath
}

func loadTestConfig(t *testing.T, configPath string) (*Config.Confuguration, *Config.State) {
	config := Config.Confuguration{}
	if err := Config.Read(configPath, &config); err != nil {
		t.Fatalf("failed to read config. Error:%s", err)
	}
	state, err := Config.LoadState(configPath, &config)
	if err != nil {
		t.Fatalf("failed to load state. Error:%s", err)
	}
	return &config, state
}

func readTestState(t *testing.T, configPath string) map[string]interface{} {
	data, err := os.ReadFile(Config.StatePath(configPath))
	if err != nil {
		t.Fatalf("failed to read state file. Error:%s", err)
	}
	state := map[string]interface{}{}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("failed to parse state file. Error:%s", err)
	}
	return state
}

// 旧版本回写进 config.json 的 initialized / instanceId 首次启动时迁移到 state.json，config.json 不被改动
func TestStateMigrateFromLegacyConfig(t *testing.T) {
	content := `{"table":{"data":"Data"},"ds":{"id":"ds01","instanceId":"inst-01"},"initialized":true}`
	configPath := writeTestConfig(t, t.TempDir(), content)
	config, _ := loadTestConfig(t, configPath)
	if !config.Initialized || config.Ds.Id != "ds01" || config.Ds.InstanceId != "inst-01" {
		t.Fatalf("unexpected config after migration: %+v", config.Ds)
	}
	state := readTestState(t, configPath)
	if state["initialized"] != true || state["instanceId"] != "inst-01" || state["dsIdFrom"] != "ds01" {
		t.Fatalf("unexpected state after migration: %v", state)
	}
	if _, ok := state["dsId"]; ok {
		t.Fatalf("dsId should be omitted when same as config ds.id: %v", state)
	}
	data, _ := os.ReadFile(configPath)
	if string(data) != content {
		t.Fatalf("config.json should not be rewritten, got: %s", data)
	}
}

// state.json 优先于 config.json 中的旧字段
func TestStateOverridesConfig(t *testing.T) {
	configPath := writeTestConfig(t, t.TempDir(), `{"table":{"data":"Data"},"ds":{"id":"ds01"},"initialized":false}`)
	stateData := `{"initialized":true,"dsId":"ds01-1","dsIdFrom":"ds01","instanceId":"inst-01"}`
	if err := os.WriteFile(Config.StatePath(configPath), []byte(stateData), 0644); err != nil {
		t.Fatalf("failed to write state. Error:%s", err)
	}
	config, _ := loadTestConfig(t, configPath)
	if !config.Initialized || config.Ds.Id != "ds01-1" || config.Ds.InstanceId != "inst-01" {
		t.Fatalf("state not applied: initialized=%t, ds=%+v", config.Initialized, config.Ds)
	}
}

// config 中的 ds.id 被人工修改后，state 中旧的 ds.id 作废
func TestStateDsIdDroppedWhenConfigChanged(t *testing.T) {
	configPath := writeTestConfig(t, t.TempDir(), `{"table":{"data":"Data"},"ds":{"id":"ds02"}}`)
	stateData := `{"initialized":true,"dsId":"ds01-1","dsIdFrom":"ds01","instanceId":"inst-01"}`
	if err := os.WriteFile(Config.StatePath(configPath), []byte(stateData), 0644); err != nil {
		t.Fatalf("failed to write state. Error:%s", err)
	}
	config, state := loadTestConfig(t, configPath)
	if config.Ds.Id != "ds02" {
		t.Fatalf("config ds.id should win, got [%s]", config.Ds.Id)
	}
	if err := state.Save(config); err != nil {
		t.Fatalf("failed to save state. Error:%s", err)
	}
	saved := readTestState(t, configPath)
	if saved["dsIdFrom"] != "ds02" {
		t.Fatalf("dsIdFrom should follow config, got: %v", saved)
	}
	if _, ok := saved["dsId"]; ok {
		t.Fatalf("stale dsId should be dropped: %v", saved)
	}
}

// config 中 ds.id 为空时自动生成的 ds.id 经 state.json 跨重启保留
func TestStateKeepsGeneratedDsId(t *testing.T) {
	configPath := writeTestConfig(t, t.TempDir(), `{"table":{"data":"Data"}}`)
	config, state := loadTestConfig(t, configPath)
	config.Ds.Id = "generated-id"
	config.Ds.InstanceId = "inst-01"
	config.Initialized = true
	if err := state.Save(config); err != nil {
		t.Fatalf("failed to save state. Error:%s", err)
	}
	config, _ = loadTestConfig(t, configPath)
	if !config.Initialized || config.Ds.Id != "generated-id" || config.Ds.InstanceId != "inst-01" {
		t.Fatalf("state lost after restart: initialized=%t, ds=%+v", config.Initialized, config.Ds)
	}
}
