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
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	"DataService/Config"
	"InventoryService/InvRecord"
	"InventoryService/RefRecord"

	"github.com/google/uuid"
	"github.com/salesforce/UniTAO/lib/Schema"
	"github.com/salesforce/UniTAO/lib/Schema/Record"
	"github.com/salesforce/UniTAO/lib/Util/Http"
	"github.com/salesforce/UniTAO/lib/Util/Json"
)

var (
	// SelfRegRetrySec 为注册失败后的重试间隔；SelfRegCheckSec 为注册成功后复查的间隔
	// （Inventory 记录被删除、被挤占或 URL 变化时重新注册）。测试可以调小。
	SelfRegRetrySec = 10
	SelfRegCheckSec = 60
)

func (srv *Server) invLinked() bool {
	return srv.config.Inv.Url != ""
}

// StartSelfRegistration 为每个 vDS 启动后台自我注册 goroutine。
// 无 Inventory 链接（inventory.url 为空）时直接返回；ds.id/ds.instanceId 为空时生成 UUID 并写入 state.json。
func (srv *Server) StartSelfRegistration() {
	if !srv.invLinked() {
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
		if err := srv.state.Save(&srv.config); err != nil {
			srv.log.Printf("failed to persist generated ds.id/ds.instanceId, Err:%s", err)
		}
	}
	for _, vds := range srv.vdsList {
		if vds.Kind == KindLocal {
			vds.InstanceId = srv.config.Ds.InstanceId
			vds.setInvId(srv.config.Ds.Id)
		}
		go srv.selfRegister(vds)
	}
}

// selfRegister 查询 Inventory 中该 vDS 的注册记录：已注册且一致则跳过；
// 未注册/不一致则 PUT 注册（Inventory 可能因撞名分配新的 dsId，读响应保存）。
// 之后定期复查，Inventory 记录丢失或被挤占时重新注册。
func (srv *Server) selfRegister(vds *Vds) {
	for {
		registered, err := srv.checkRegistration(vds)
		if err != nil {
			srv.log.Printf("vDS [%s]: failed to query current registration from inventory. Err:%s", vds.Path, err)
			time.Sleep(time.Duration(SelfRegRetrySec) * time.Second)
			continue
		}
		if !registered {
			if err := srv.register(vds); err != nil {
				srv.log.Printf("vDS [%s]: self registration failed. Err:%s", vds.Path, err)
				time.Sleep(time.Duration(SelfRegRetrySec) * time.Second)
				continue
			}
		}
		time.Sleep(time.Duration(SelfRegCheckSec) * time.Second)
	}
}

func (srv *Server) register(vds *Vds) error {
	recordMap := srv.buildRegisterRecord(vds)
	resp, status, err := Http.SubmitPayload(srv.config.Inv.Url, http.MethodPut, nil, recordMap)
	if err != nil {
		return fmt.Errorf("code:%d, %s", status, err)
	}
	// Inventory 可能分配了新的 dsId（响应体为实际写入的 record id）
	assignedId := srv.readAssignedId(resp)
	if status < 200 || status > 299 {
		return fmt.Errorf("inventory returned code:%d, %s", status, assignedId)
	}
	current := vds.InvId()
	if assignedId != "" && assignedId != current {
		srv.log.Printf("vDS [%s]: inventory assigned dsId=[%s] (requested [%s])", vds.Path, assignedId, current)
		vds.setInvId(assignedId)
		if vds.Kind == KindLocal {
			srv.config.Ds.Id = assignedId
			if err := srv.state.Save(&srv.config); err != nil {
				srv.log.Printf("failed to persist assigned ds.id=[%s], Err:%s", assignedId, err)
			}
		} else {
			requested := vds.RequestedId(&srv.config)
			if err := srv.state.UpdateVds(vds.Path, &srv.config, func(st *Config.VdsState) {
				st.AssignedFrom = requested
				st.AssignedId = assignedId
				if assignedId == requested {
					st.AssignedId = ""
				}
			}); err != nil {
				srv.log.Printf("failed to persist assigned dsId of vDS [%s], Err:%s", vds.Path, err)
			}
		}
	}
	srv.log.Printf("vDS [%s] registered to inventory=[%s] as dsId=[%s]", vds.Path, srv.config.Inv.Url, vds.InvId())
	return nil
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

// notifyTypeChange 在 vDS 新增或删除一个 data type 时向 Inventory 推送单类型事件：
// POST {invUrl}/referral，body={"dsId","dataType"}。best-effort：失败仅记日志，由周期全量 Sync 兜底。
func (srv *Server) notifyTypeChange(vds *Vds, dataType string) {
	if !srv.invLinked() {
		return
	}
	payload := map[string]interface{}{
		"dsId":     vds.InvId(),
		"dataType": dataType,
	}
	eventUrl, err := Http.URLPathJoin(srv.config.Inv.Url, RefRecord.Referral)
	if err != nil {
		srv.log.Printf("failed to build referral event url. Err:%s", err)
		return
	}
	resp, status, err := Http.SubmitPayload(*eventUrl, http.MethodPost, nil, payload)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		srv.log.Printf("notify type change [%s] of vDS [%s] to inventory failed. Code:%d, Err:%s", dataType, vds.Path, status, err)
		return
	}
	srv.log.Printf("notified inventory of type change [%s] as dsId=[%s], status=%d", dataType, vds.InvId(), status)
}

// unregisterUrls 从 Inventory 中移除已删除 vDS 的 URL（副本的其它 URL 保留；没有 URL 时 Inventory 删除整条记录）。
func (srv *Server) unregisterUrls(invId string, path string) {
	if !srv.invLinked() || invId == "" {
		return
	}
	for attempt := 0; attempt < 3; attempt++ {
		ok := true
		for _, base := range srv.dsUrls() {
			vdsUrl := joinVdsUrl(base, path)
			delUrl, err := Http.URLPathJoin(srv.config.Inv.Url, Schema.Inventory, invId)
			if err != nil {
				srv.log.Printf("failed to build unregister url. Err:%s", err)
				return
			}
			target := fmt.Sprintf("%s?url=%s", *delUrl, url.QueryEscape(vdsUrl))
			status, err := httpDelete(target)
			if err != nil || status >= 300 {
				srv.log.Printf("failed to unregister url [%s] of [%s] from inventory. Code:%d, Err:%v", vdsUrl, invId, status, err)
				ok = false
			}
		}
		if ok {
			srv.log.Printf("unregistered removed vDS [%s] (dsId=[%s]) from inventory", path, invId)
			return
		}
		time.Sleep(time.Duration(SelfRegRetrySec) * time.Second)
	}
}

// checkRegistration 查询 Inventory 中该 vDS 的 inventory 记录，返回是否已注册且与当前一致。
// 记录不存在(404)视为未注册；Inventory 不可达等错误返回 error，交由调用方重试。
func (srv *Server) checkRegistration(vds *Vds) (bool, error) {
	invId := vds.InvId()
	if invId == "" {
		return false, nil
	}
	queryUrl, err := Http.URLPathJoin(srv.config.Inv.Url, Schema.Inventory, invId)
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
	if dsInfo.Id != invId || dsInfo.InstanceId != vds.InstanceId {
		return false, nil
	}
	if vds.Kind == KindLocal {
		if dsInfo.EffectiveKind() != KindLocal {
			return false, nil
		}
		return sameUrlList(dsInfo.URL, srv.vdsUrls(vds)), nil
	}
	// shared 和它的镜像可能合并成同一条记录（kind 为 shared），所以只要求是签名身份
	if !dsInfo.Signed() {
		return false, nil
	}
	// shared / mirrored 的记录可能合并了其它副本的 URL，只要包含自己的 URL 即可
	return containsAll(dsInfo.URL, srv.vdsUrls(vds)), nil
}

// dsUrls 返回本实例需要广播的可访问基础 URL 列表。优先采用 config.Ds.Urls；为空时按 http.dns:port 推导。
func (srv *Server) dsUrls() []string {
	if len(srv.config.Ds.Urls) > 0 {
		return srv.config.Ds.Urls
	}
	return []string{fmt.Sprintf("%s://%s:%s", srv.config.Http.HttpType, srv.config.Http.DnsName, srv.Port)}
}

func joinVdsUrl(base string, path string) string {
	return strings.TrimSuffix(base, "/") + "/" + path
}

// vdsUrls 返回该 vDS 的 URL 列表：基础 URL + "/" + vDS 路径。
func (srv *Server) vdsUrls(vds *Vds) []string {
	result := []string{}
	for _, base := range srv.dsUrls() {
		result = append(result, joinVdsUrl(base, vds.Path))
	}
	return result
}

// buildRegisterRecord 构造该 vDS 的 inventory Record 信封（__type/__id/__ver/data）。
func (srv *Server) buildRegisterRecord(vds *Vds) map[string]interface{} {
	dsInfo := InvRecord.DataServiceInfo{
		Id:         vds.InvId(),
		InstanceId: vds.InstanceId,
		Kind:       vds.Kind,
		URL:        srv.vdsUrls(vds),
	}
	dsMap, _ := Json.CopyToMap(dsInfo)
	record := Record.NewRecord(Schema.Inventory, InvRecord.LatestVer, vds.InvId(), dsMap)
	return record.Map()
}

// sameUrlList 判断两个 URL 列表是否一致（集合相等，忽略顺序）。
func sameUrlList(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	return containsAll(a, b)
}

func containsAll(list []string, items []string) bool {
	set := make(map[string]bool, len(list))
	for _, url := range list {
		set[url] = true
	}
	for _, url := range items {
		if !set[url] {
			return false
		}
	}
	return true
}

// httpDelete 发送 DELETE 请求（Http.SubmitPayload 不支持 DELETE）。
func httpDelete(target string) (int, error) {
	req, err := http.NewRequest(http.MethodDelete, target, nil)
	if err != nil {
		return http.StatusBadRequest, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}
