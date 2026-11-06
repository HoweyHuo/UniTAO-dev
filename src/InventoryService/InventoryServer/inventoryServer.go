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
	"strings"
	"sync"
	"time"

	"Data/DbConfig"
	"Data/DbIface"
	"InventoryService/Config"
	"InventoryService/DataHandler"
	"InventoryService/DataSync"
	"InventoryService/RefRecord"

	"github.com/salesforce/UniTAO/lib/Schema"
	"github.com/salesforce/UniTAO/lib/Util"
	"github.com/salesforce/UniTAO/lib/Util/CustomLogger"
	"github.com/salesforce/UniTAO/lib/Util/Http"
	"github.com/salesforce/UniTAO/lib/Util/Json"
)

type TypeEvent struct {
	DsId     string
	DataType string
}

type Server struct {
	Port     string
	args     ServerArgs
	config   Config.ServerConfig
	data     *DataHandler.Handler
	log      *log.Logger
	syncChan chan string      // DS 注册/更新事件
	typeChan chan TypeEvent   // DS 新增/删除 data type 事件（单类型，按 DS 上实际状态合并或移除，不扫描）
	syncer   *DataSync.Syncer // referral + schema 同步器
	syncLock *sync.Mutex      // 串行化各种同步；在 Start 中创建（New 按值返回 Server，不能内嵌锁）
	// ConnectDb 建立数据库连接，测试时可替换；为 nil 时按配置连接。
	ConnectDb func(config DbConfig.DatabaseConfig, logger *log.Logger) (DbIface.Database, error)
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

// NewServer 不经命令行参数直接创建 Server（测试与嵌入使用）。
func NewServer(configPath string, port string) (*Server, error) {
	server := &Server{args: ServerArgs{config: configPath, port: port}}
	if err := Config.Read(configPath, &server.config); err != nil {
		return nil, err
	}
	server.Port = port
	if server.Port == "" {
		server.Port = server.config.Http.Port
	}
	if server.Port == "" {
		server.Port = PORT_DEFAULT
	}
	return server, nil
}

func (srv *Server) newHandler() (*DataHandler.Handler, error) {
	if srv.ConnectDb == nil {
		return DataHandler.New(srv.config.Database, srv.log)
	}
	db, err := srv.ConnectDb(srv.config.Database, srv.log)
	if err != nil {
		return nil, err
	}
	return DataHandler.NewWithDb(db, srv.log)
}

func (srv *Server) WaitForDataHandler() error {
	for {
		handler, err := srv.newHandler()
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
	if err := srv.Start(logger); err != nil {
		logger.Fatalf("failed to start Inventory Service, Err:%s", err)
	}
	http.HandleFunc("/", Http.Cors(srv.handler))
	srv.log.Printf("Data Server Listen @%s://%s:%s", srv.config.Http.HttpType, srv.config.Http.DnsName, srv.Port)
	srv.log.Fatal(http.ListenAndServe(fmt.Sprintf(":%s", srv.Port), nil))
}

// ServeHTTP 让 Server 可以直接作为 http.Handler 使用（测试）。
func (srv *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	Http.Cors(srv.handler)(w, r)
}

// Start 连接数据库并启动后台同步循环，但不监听端口。
func (srv *Server) Start(logger *log.Logger) error {
	if logger == nil {
		logger = log.Default()
	}
	srv.log = logger
	srv.syncLock = &sync.Mutex{}
	srv.log.Printf("Server Listen on PORT:%s", srv.Port)
	err := srv.WaitForDataHandler()
	if err != nil {
		return err
	}
	srv.data.PinnedSources = srv.config.PinnedSources
	// 后台 schema sync：先于 HTTP 启动，注册事件驱动优先同步新 DS
	intervalSec := srv.config.Sync.IntervalSec
	if intervalSec <= 0 {
		intervalSec = DefaultSyncIntervalSec
	}
	interval := time.Duration(intervalSec) * time.Second
	srv.syncChan = make(chan string, SyncChanCap)
	srv.data.SyncChan = srv.syncChan
	srv.typeChan = make(chan TypeEvent, SyncChanCap)
	srv.syncer = DataSync.New(srv.data, srv.log)
	go srv.syncLoop(interval)
	return nil
}

func (srv *Server) handler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		srv.handleGet(w, r)
	case http.MethodPut:
		srv.handleUpdate(w, r)
	case http.MethodDelete:
		srv.handlerDelete(w, r)
	case http.MethodPost:
		srv.handleTypeEvent(w, r)
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
		return
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
	if idx := strings.Index(urlPath, "?"); idx >= 0 {
		urlPath = urlPath[:idx]
	}
	dataType, idPath := Util.ParsePath(urlPath)
	id, nextPath := Util.ParsePath(idPath)
	if id == "" || nextPath != "" {
		err := Http.NewHttpError("invalid url for delete, expected format=[{dataType}/{dataId}]", http.StatusBadRequest)
		Http.ResponseJson(w, err, err.Status, srv.config.Http)
		return
	}
	// DELETE /inventory/{dsId}?url={url}：只移除一个副本的 URL，没有剩余 URL 时删除整条记录
	if removeUrl := r.URL.Query().Get("url"); removeUrl != "" && dataType == Schema.Inventory {
		err = srv.data.RemoveDsUrl(id, removeUrl)
		if err != nil {
			Http.ResponseJson(w, err, err.Status, srv.config.Http)
			return
		}
		Http.ResponseText(w, []byte(fmt.Sprintf("url [%s] removed from [%s/%s]", removeUrl, dataType, id)), http.StatusAccepted, srv.config.Http)
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

// handleTypeEvent 接收 DS 新增/删除 data type 事件：POST /referral，body={"dsId","dataType"}。
// 只校验路径与 body 并非阻塞入队 typeChan；referral 写入由 syncLoop 单 goroutine 执行。
func (srv *Server) handleTypeEvent(w http.ResponseWriter, r *http.Request) {
	urlPath, err := Http.GetUrl(r)
	if err != nil {
		Http.ResponseJson(w, err, err.Status, srv.config.Http)
		return
	}
	dataType, dataPath := Util.ParsePath(urlPath)
	if dataType != RefRecord.Referral || dataPath != "" {
		err := Http.NewHttpError(fmt.Sprintf("invalid url for type event, expected=[%s]", RefRecord.Referral), http.StatusBadRequest)
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
		return
	}
	var ev TypeEvent
	if ex := Json.CopyTo(payload, &ev); ex != nil {
		Http.ResponseJson(w, ex.Error(), http.StatusBadRequest, srv.config.Http)
		return
	}
	if ev.DsId == "" || ev.DataType == "" {
		Http.ResponseJson(w, "missing dsId or dataType in type event", http.StatusBadRequest, srv.config.Http)
		return
	}
	// 非阻塞发送；channel 满则丢弃（周期全量 Sync 兜底）
	select {
	case srv.typeChan <- ev:
	default:
		srv.log.Printf("type event channel full, drop event for DS=[%s] type=[%s]", ev.DsId, ev.DataType)
	}
	Http.ResponseText(w, []byte("accepted"), http.StatusAccepted, srv.config.Http)
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
		case ev := <-srv.typeChan:
			srv.runSync(fmt.Sprintf("type event %s/%s", ev.DsId, ev.DataType), func() error {
				return srv.syncer.SyncType(ev.DsId, ev.DataType)
			})
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
// SyncNow 立即做一次全量同步（测试与管理使用），与后台循环串行。
func (srv *Server) SyncNow() error {
	var result error
	srv.runSync("manual", func() error {
		result = srv.syncer.Sync()
		return result
	})
	return result
}

func (srv *Server) runSync(what string, fn func() error) {
	srv.syncLock.Lock()
	defer srv.syncLock.Unlock()
	srv.log.Printf("start %s sync", what)
	if err := fn(); err != nil {
		srv.log.Printf("%s sync failed, Error: %s", what, err)
	}
}
