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

package Http

import (
	"net/http"
)

// Inventory 系统部署在受保护的内网中，信任内网其它系统，因此 REST API 对任意来源开放 CORS。
// 生产阶段的访问限制由白名单负责，而不是靠 CORS。
const (
	CorsAllowMethods = "GET, POST, PUT, PATCH, DELETE, OPTIONS"
	CorsMaxAge       = "86400"
)

// Cors 为每个响应附加允许任意来源的 CORS 头，并直接应答 OPTIONS 预检请求
func Cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", CorsAllowMethods)
		if reqHeaders := r.Header.Get("Access-Control-Request-Headers"); reqHeaders != "" {
			h.Set("Access-Control-Allow-Headers", reqHeaders)
		} else {
			h.Set("Access-Control-Allow-Headers", "*")
		}
		h.Set("Access-Control-Expose-Headers", "*")
		h.Set("Access-Control-Max-Age", CorsMaxAge)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}
