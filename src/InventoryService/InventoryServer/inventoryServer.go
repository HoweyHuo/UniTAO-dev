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

package InventoryServer

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"InventoryService/Config"
	"InventoryService/DataHandler"
	"InventoryService/DataSync"

	"github.com/salesforce/UniTAO/lib/Util"
	"github.com/salesforce/UniTAO/lib/Util/CustomLogger"
	"github.com/salesforce/UniTAO/lib/Util/Http"
)

type Server struct {
	Port     string
	args     ServerArgs
	config   Config.ServerConfig
	data     *DataHandler.Handler
	log      *log.Logger
	syncChan chan string      // DS 注册/更新事件
	syncer   *DataSync.Syncer // referral + schema 同步器
}

type ServerArgs struct {
	logPath string
	port    string
	config  string
}

const (
	CONFIG                 = "config"
	PORT                   = "port"
	PORT_DEFAULT           = "8003"
	DefaultSyncIntervalSec = 300 // 后台全量对账默认间隔（秒）
	SyncChanCap            = 16  // DS 注册事件缓冲大小
)

func argHandler() ServerArgs {
	args := ServerArgs{}
	var port string
	var configPath string
	var logPath string
	flag.StringVar(&port, "port", "", "Data Server Listen Port")
	flag.StringVar(&configPath, "config", "", "Data Server Configuration JSON path")
	flag.StringVar(&logPath, "log", "", "path that hold log")
	flag.Parse()
	args.port = port
	args.config = configPath
	args.logPath = logPath
	if args.config == "" {
		flag.Usage()
		log.Fatalf("missing parameter [%s]", CONFIG)
	}
	return args
}

func New() Server {
	log.Println("Create Inventory Service Instance")
	server := Server{
		args: argHandler(),
	}
	err := Config.Read(server.args.config, &server.config)
	if err != nil {
		log.Fatalf("failed to read config=[%s], Err:%s", server.args.config, err)
	}
	if server.args.port == "" {
		if server.config.Http.Port == "" {
			server.Port = PORT_DEFAULT
			return server
		}
		server.Port = server.config.Http.Port
		return server
	}
	server.Port = server.args.port
	return server
}

func (srv *Server) WaitForDataHandler() error {
	for {
		handler, err := DataHandler.New(srv.config.Database, srv.log)
		if err == nil {
			srv.data = handler
			break
		}
		srv.log.Printf("failed to initialize data layer, Err:%s", err)
		srv.log.Printf("retry to connect to database after 10 seconds")
		time.Sleep(10 * time.Second)
	}
	return nil
}

func (srv *Server) Run() {
	logFile, logger, err := CustomLogger.FileLoger(srv.args.logPath, "InventoryService")
	if err != nil {
		log.Printf("Inventory Service failed to create logger. Err: %s", err)
	}
	if logFile != nil {
		defer logFile.Close()
	}
	srv.log = logger
	srv.log.Printf("Server Listen on PORT:%s", srv.Port)
	err = srv.WaitForDataHandler()
	if err != nil {
		srv.log.Fatalf("failed to connect to database, Err:%s", err)
	}
	// 后台 schema sync：先于 HTTP 启动，注册事件驱动优先同步新 DS
	intervalSec := srv.config.Sync.IntervalSec
	if intervalSec <= 0 {
		intervalSec = DefaultSyncIntervalSec
	}
	interval := time.Duration(intervalSec) * time.Second
	srv.syncChan = make(chan string, SyncChanCap)
	srv.data.SyncChan = srv.syncChan
	srv.syncer = DataSync.New(srv.data, srv.log)
	go srv.syncLoop(interval)
	http.HandleFunc("/", srv.handler)
	srv.log.Printf("Data Server Listen @%s://%s:%s", srv.config.Http.HttpType, srv.config.Http.DnsName, srv.Port)
	srv.log.Fatal(http.ListenAndServe(fmt.Sprintf(":%s", srv.Port), nil))
}

func (srv *Server) handler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		srv.handleGet(w, r)
	case http.MethodPut:
		srv.handleUpdate(w, r)
	case http.MethodDelete:
		srv.handlerDelete(w, r)
	default:
		err := Http.NewHttpError(fmt.Sprintf("method=[%s] not supported. only support method=[%s, %s]", r.Method, http.MethodPut, http.MethodDelete), http.StatusMethodNotAllowed)
		Http.ResponseJson(w, err, err.Status, srv.config.Http)
	}
}

func (srv *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	urlPath, err := Http.GetUrl(r)
	if err != nil {
		Http.ResponseJson(w, err, err.Status, srv.config.Http)
	}
	dataType, dataPath := Util.ParsePath(urlPath)
	if dataType == "" {
		err := Http.NewHttpError("please use inventory/{type}[/{id}], dataType is empty", http.StatusBadRequest)
		Http.ResponseJson(w, err, err.Status, srv.config.Http)
		return
	}
	if dataPath == "" {
		idList, err := srv.data.List(dataType)
		if err != nil {
			Http.ResponseJson(w, err, err.Status, srv.config.Http)
			return
		}
		Http.ResponseJson(w, idList, http.StatusOK, srv.config.Http)
		return
	}
	data, err := srv.data.Get(dataType, dataPath)
	if err != nil {
		Http.ResponseJson(w, err, err.Status, srv.config.Http)
		return
	}
	Http.ResponseJson(w, data, http.StatusOK, srv.config.Http)
}

func (srv *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	urlPath, err := Http.GetUrl(r)
	if err != nil {
		Http.ResponseJson(w, err, err.Status, srv.config.Http)
	}
	if urlPath != "" && urlPath != "/" {
		// PUT method should not have any path, it should be just "" or /
		err := Http.NewHttpError(fmt.Sprintf("for PUT method, no path allowed, got [%s] instead.", urlPath), http.StatusBadRequest)
		Http.ResponseJson(w, err, err.Status, srv.config.Http)
		return
	}
	reqBody, e := Http.LoadRequest(r)
	if e != nil {
		Http.ResponseJson(w, e, e.Status, srv.config.Http)
		return
	}
	payload, ok := reqBody.(map[string]interface{})
	if !ok {
		Http.ResponseJson(w, "failed to parse request into JSON object", http.StatusBadRequest, srv.config.Http)
	}
	dataId, err := srv.data.PutData(payload)
	if err != nil {
		Http.ResponseJson(w, err, err.Status, srv.config.Http)
		return
	}
	Http.ResponseText(w, []byte(dataId), http.StatusAccepted, srv.config.Http)
}

func (srv *Server) handlerDelete(w http.ResponseWriter, r *http.Request) {
	urlPath, err := Http.GetUrl(r)
	if err != nil {
		Http.ResponseJson(w, err, err.Status, srv.config.Http)
	}
	dataType, idPath := Util.ParsePath(urlPath)
	id, nextPath := Util.ParsePath(idPath)
	if nextPath == "" {
		err := Http.NewHttpError("invalid url for delete, expected format=[{dataType}/{dataId}]", http.StatusBadRequest)
		Http.ResponseJson(w, err, err.Status, srv.config.Http)
		return
	}
	err = srv.data.DeleteData(dataType, id)
	if err != nil {
		Http.ResponseJson(w, err, err.Status, srv.config.Http)
		return
	}
	result := fmt.Sprintf("[%s/%s] deleted", dataType, id)
	Http.ResponseText(w, []byte(result), http.StatusAccepted, srv.config.Http)
}

// syncLoop 单 goroutine 串行执行同步，天然避免并发写 referral。
// select 每次迭代重建 time.After → 事件处理后周期计时自然重置，不会立刻重复全量 sync。
func (srv *Server) syncLoop(interval time.Duration) {
	srv.log.Printf("start background schema sync loop, interval=%s", interval)
	srv.runSync("startup", srv.syncer.Sync)
	for {
		select {
		case dsId := <-srv.syncChan:
			ids := srv.collectSyncEvents(dsId) // 合并突发事件、去重
			srv.log.Printf("wake sync early for DS=%v", ids)
			for _, id := range ids { // 新 DS 优先：先定向同步，再全量对账
				srv.runSync(fmt.Sprintf("new DS=%s", id), func() error { return srv.syncer.SyncDs(id) })
			}
			srv.runSync("post-event full sync", srv.syncer.Sync)
		case <-time.After(interval):
			srv.log.Printf("periodic sync")
			srv.runSync("periodic", srv.syncer.Sync)
		}
	}
}

// collectSyncEvents 排空同步通道中已缓冲的事件并去重，返回待优先同步的 DS 列表。
func (srv *Server) collectSyncEvents(first string) []string {
	seen := map[string]bool{}
	ids := []string{}
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	add(first)
drain:
	for {
		select {
		case id := <-srv.syncChan:
			add(id)
		default:
			break drain
		}
	}
	return ids
}

// runSync 执行同步并记录结果；错误仅记日志，循环不中断。
func (srv *Server) runSync(what string, fn func() error) {
	srv.log.Printf("start %s sync", what)
	if err := fn(); err != nil {
		srv.log.Printf("%s sync failed, Error: %s", what, err)
	}
}
