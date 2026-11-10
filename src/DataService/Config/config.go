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
	"regexp"

	"Data/DbConfig"
	"DataService/Channel"

	"github.com/salesforce/UniTAO/lib/Util/Http"
	"github.com/salesforce/UniTAO/lib/Util/Json"
)

const (
	DATABASE = "database"
	HTTP     = "http"
)

// DsConfig 描述本 DataService 自身的身份与对外可访问地址。
// Id 为该 DataService 在 Inventory Service 中注册的标识（DsId），首次部署为空时
// 自动生成 UUID；InstanceId 为稳定实例标识（UUID），用于区分"同 DS
// 重注册"与"不同 DS 撞名"；Urls 为可被 Inventory Service 访问的 URL 列表。
// 运行时产生的 Id / InstanceId 保存在 state.json（见 State），不回写 config。
type DsConfig struct {
	Id         string   `json:"id"`
	InstanceId string   `json:"instanceId"`
	Urls       []string `json:"urls"`
}

// Confuguration 是 config.json 的内容，DataService 只读不写。Initialized 的权威值在
// state.json，config.json 中的同名字段只在首次迁移时读取。
type Confuguration struct {
	Database    DbConfig.DatabaseConfig `json:"database"`
	DataTable   DataTableConfig         `json:"table"`
	Http        Http.Config             `json:"http"`
	Ds          DsConfig                `json:"ds"`
	Inv         InvConfig               `json:"inventory"`
	Initialized bool                    `json:"initialized"`
	ForceInit   bool                    `json:"force-init"`
	Init        InitConfig              `json:"init"`
	Vds         VdsConfig               `json:"vds"`
	SSH         SSHConfig               `json:"ssh"`
}

// SSHConfig 是 git 通道 SSH 身份的设置。Dir 为私钥（及 known_hosts）所在目录，
// 缺省为 config 同目录的 ssh/。容器部署建议指向一个 Docker 命名卷（如 /opt/UniTAO/ssh），
// 让私钥不出现在宿主机的 bind-mount 目录里，同时跨容器重建保留。
type SSHConfig struct {
	Dir string `json:"dir,omitempty"`
}

// VdsConfig 列出本实例承载的虚拟 Data Service。出现哪个 key 就启用哪个 vDS；
// 整段缺省时只启用 local。
type VdsConfig struct {
	Local    *LocalVdsConfig     `json:"local,omitempty"`
	Shared   []SharedVdsConfig   `json:"shared,omitempty"`
	Mirrored []MirroredVdsConfig `json:"mirrored,omitempty"`
}

type LocalVdsConfig struct{}

// SharedVdsConfig 是一个 shared vDS：数据全部经 Channel 共享出去（publisher）。
type SharedVdsConfig struct {
	Name               string `json:"name"`
	SigningKeyFile     string `json:"signingKeyFile"`
	PublishIntervalSec int    `json:"publishIntervalSec"`
	// SubscriberTimeoutHours：subscriber 的第一个未处理 batch 发布超过这么久，判定它死亡（默认 24）
	SubscriberTimeoutHours int `json:"subscriberTimeoutHours"`
	// CompactIntervalHours：压缩通道历史的最小间隔（默认 24，只对 git 通道有效）
	CompactIntervalHours int `json:"compactIntervalHours"`
	// HistoryReuseHours：history 发布后这么久之内，新订阅方直接拿到它的授权，不重新打包（默认 24）
	HistoryReuseHours int            `json:"historyReuseHours"`
	Channel           Channel.Config `json:"channel"`
}

// MirroredVdsConfig 是一个 mirrored vDS（subscriber）：只读，所有变化来自 Channel。
// Name 为来源的共享名；Alias 为本网使用的名字（路径与 Inventory dsId），缺省等于 Name。
type MirroredVdsConfig struct {
	Name               string         `json:"name"`
	Alias              string         `json:"alias,omitempty"`
	TrustedFingerprint string         `json:"trustedFingerprint"`
	PullIntervalSec    int            `json:"pullIntervalSec"`
	Channel            Channel.Config `json:"channel"`
}

// LocalName 返回 mirrored vDS 在本网使用的名字。
func (m MirroredVdsConfig) LocalName() string {
	if m.Alias != "" {
		return m.Alias
	}
	return m.Name
}

var vdsNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ValidVdsName 判断 vDS 名字是否合法：只允许字母、数字、下划线、连字符
// （名字会出现在 URL 路径、Inventory dsId 和表名中）。
func ValidVdsName(name string) bool {
	return vdsNamePattern.MatchString(name)
}

// Normalize 补默认值并校验 vds 段。
func (v *VdsConfig) Normalize() error {
	if v.Local == nil && len(v.Shared) == 0 && len(v.Mirrored) == 0 {
		v.Local = &LocalVdsConfig{}
	}
	seen := map[string]bool{}
	for idx := range v.Shared {
		cfg := &v.Shared[idx]
		if !ValidVdsName(cfg.Name) {
			return fmt.Errorf("invalid shared vDS name [%s], only [A-Za-z0-9_-] allowed", cfg.Name)
		}
		if seen[cfg.Name] {
			return fmt.Errorf("duplicate shared vDS name [%s]", cfg.Name)
		}
		seen[cfg.Name] = true
		if cfg.SigningKeyFile == "" {
			return fmt.Errorf("shared vDS [%s] requires [signingKeyFile]", cfg.Name)
		}
		if err := cfg.Channel.Validate(); err != nil {
			return fmt.Errorf("shared vDS [%s]: %s", cfg.Name, err)
		}
		if cfg.Channel.Encrypt == nil || cfg.Channel.Encrypt.RecipientsFile == "" {
			return fmt.Errorf("shared vDS [%s] requires [channel.encrypt.recipientsFile] (the subscriber whitelist)", cfg.Name)
		}
	}
	seen = map[string]bool{}
	for idx := range v.Mirrored {
		cfg := &v.Mirrored[idx]
		if !ValidVdsName(cfg.Name) {
			return fmt.Errorf("invalid mirrored vDS name [%s], only [A-Za-z0-9_-] allowed", cfg.Name)
		}
		if cfg.Alias != "" && !ValidVdsName(cfg.Alias) {
			return fmt.Errorf("invalid mirrored vDS alias [%s], only [A-Za-z0-9_-] allowed", cfg.Alias)
		}
		if seen[cfg.LocalName()] {
			return fmt.Errorf("duplicate mirrored vDS name [%s]", cfg.LocalName())
		}
		seen[cfg.LocalName()] = true
		if cfg.TrustedFingerprint == "" {
			return fmt.Errorf("mirrored vDS [%s] requires [trustedFingerprint]", cfg.LocalName())
		}
		if err := cfg.Channel.Validate(); err != nil {
			return fmt.Errorf("mirrored vDS [%s]: %s", cfg.LocalName(), err)
		}
		if cfg.Channel.Decrypt == nil || cfg.Channel.Decrypt.IdentityFile == "" {
			return fmt.Errorf("mirrored vDS [%s] requires [channel.decrypt.identityFile] (the subscriber identity)", cfg.LocalName())
		}
	}
	return nil
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
	return config.Vds.Normalize()
}
