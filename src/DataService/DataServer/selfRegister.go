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

package DataServer

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"DataService/Config"
	"InventoryService/InvRecord"

	"github.com/google/uuid"
	"github.com/salesforce/UniTAO/lib/Schema"
	"github.com/salesforce/UniTAO/lib/Schema/Record"
	"github.com/salesforce/UniTAO/lib/Util/Http"
	"github.com/salesforce/UniTAO/lib/Util/Json"
)

const (
	SelfRegRetrySec = 10
)

// StartSelfRegistration 启动后台自我注册 goroutine。
// 无 Inventory 链接（Inv.Url 为空）时直接返回；ds.id/ds.instanceId 为空时生成 UUID 并回写 config。
func (srv *Server) StartSelfRegistration() {
	if !srv.data.Inventory.InvLinked() {
		srv.log.Printf("inventory not linked, skip self registration")
		return
	}
	persist := false
	if srv.config.Ds.Id == "" {
		srv.config.Ds.Id = uuid.NewString()
		persist = true
	}
	if srv.config.Ds.InstanceId == "" {
		srv.config.Ds.InstanceId = uuid.NewString()
		persist = true
	}
	if persist {
		if err := Config.Write(srv.args[CONFIG], &srv.config); err != nil {
			srv.log.Printf("failed to persist generated ds.id/ds.instanceId, Err:%s", err)
		}
	}
	go srv.selfRegister()
}

// selfRegister 先查询 Inventory 中当前注册记录：已注册且一致则跳过；
// 未注册/不一致则 PUT 注册（服务端可能分配新的 short name，读响应回写）；
// 查询或写入失败时 10s 后重试。
func (srv *Server) selfRegister() {
	for {
		registered, err := srv.checkRegistration()
		if err != nil {
			srv.log.Printf("failed to query current registration from inventory. Err:%s", err)
			time.Sleep(SelfRegRetrySec * time.Second)
			continue
		}
		if registered {
			srv.log.Printf("already registered to inventory=[%s] as ds.id=[%s], skip", srv.config.Inv.Url, srv.config.Ds.Id)
			return
		}
		recordMap := srv.buildRegisterRecord()
		resp, status, err := Http.SubmitPayload(srv.config.Inv.Url, http.MethodPut, nil, recordMap)
		if err != nil {
			srv.log.Printf("self registration failed. Code:%d, Err:%s", status, err)
			time.Sleep(SelfRegRetrySec * time.Second)
			continue
		}
		// 服务端可能因撞名分配了带后缀的新 short name（响应体为实际 record id）
		assignedId := srv.readAssignedId(resp)
		if assignedId != "" && assignedId != srv.config.Ds.Id {
			srv.log.Printf("self registration assigned new ds.id=[%s]", assignedId)
			srv.config.Ds.Id = assignedId
			if err := Config.Write(srv.args[CONFIG], &srv.config); err != nil {
				srv.log.Printf("failed to persist assigned ds.id=[%s], Err:%s", assignedId, err)
			}
		}
		srv.log.Printf("self registered to inventory=[%s] as ds.id=[%s]", srv.config.Inv.Url, srv.config.Ds.Id)
		return
	}
}

// readAssignedId 读取 PUT 响应体（Inventory 返回实际写入的 record id）。
func (srv *Server) readAssignedId(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		srv.log.Printf("failed to read registration response body. Err:%s", err)
		return ""
	}
	return strings.TrimSpace(string(body))
}

// checkRegistration 查询 Inventory 中本 DS 的 inventory 记录。
// 返回 (已注册且与当前 urls 一致, error)。记录不存在(404)视为未注册返回 (false, nil)；
// Inventory 不可达等查询错误返回 error，交由调用方重试。
func (srv *Server) checkRegistration() (bool, error) {
	queryUrl, err := Http.URLPathJoin(srv.config.Inv.Url, Schema.Inventory, srv.config.Ds.Id)
	if err != nil {
		return false, err
	}
	data, code, err := Http.GetRestData(*queryUrl)
	if err != nil {
		if code == http.StatusNotFound {
			return false, nil
		}
		return false, err
	}
	recordMap, ok := data.(map[string]interface{})
	if !ok {
		return false, fmt.Errorf("inventory record from [%s] is not a map", *queryUrl)
	}
	record, e := Record.LoadMap(recordMap)
	if e != nil {
		return false, e
	}
	dsInfo, e := InvRecord.CreateDsInfo(record.Data)
	if e != nil {
		return false, e
	}
	if dsInfo.Id != srv.config.Ds.Id {
		return false, nil
	}
	if dsInfo.InstanceId != srv.config.Ds.InstanceId {
		return false, nil
	}
	return sameUrlList(dsInfo.URL, srv.dsUrls()), nil
}

// dsUrls 返回本 DS 需要广播的可访问 URL 列表。
// 优先采用 config.Ds.Urls；为空时按 http.dns:port 推导。
func (srv *Server) dsUrls() []string {
	if len(srv.config.Ds.Urls) > 0 {
		return srv.config.Ds.Urls
	}
	return []string{fmt.Sprintf("%s://%s:%s", srv.config.Http.HttpType, srv.config.Http.DnsName, srv.Port)}
}

// buildRegisterRecord 构造完整 inventory Record 信封（__type/__id/__ver/data）。
func (srv *Server) buildRegisterRecord() map[string]interface{} {
	dsInfo := InvRecord.DataServiceInfo{
		Id:         srv.config.Ds.Id,
		InstanceId: srv.config.Ds.InstanceId,
		URL:        srv.dsUrls(),
	}
	dsMap, _ := Json.CopyToMap(dsInfo)
	record := Record.NewRecord(Schema.Inventory, InvRecord.LatestVer, srv.config.Ds.Id, dsMap)
	return record.Map()
}

// sameUrlList 判断两个 URL 列表是否一致（集合相等，忽略顺序）。
func sameUrlList(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, url := range a {
		set[url] = true
	}
	for _, url := range b {
		if !set[url] {
			return false
		}
	}
	return true
}
