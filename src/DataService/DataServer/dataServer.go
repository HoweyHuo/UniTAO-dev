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
	"Data"
	"Data/DbConfig"
	"Data/DbIface"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"DataService/Channel"
	"DataService/Common"
	"DataService/Config"
	"DataService/DataJournal"

	"github.com/salesforce/UniTAO/lib/Schema/Record"
	"github.com/salesforce/UniTAO/lib/Util"
	"github.com/salesforce/UniTAO/lib/Util/CustomLogger"
	"github.com/salesforce/UniTAO/lib/Util/Http"
	"github.com/salesforce/UniTAO/lib/Util/Thread"
)

const (
	CONFIG       = "config"
	PORT         = "port"
	PORT_DEFAULT = "8010"
)

type Server struct {
	Id         string
	Port       string
	args       map[string]string
	config     Config.Confuguration
	state      *Config.State
	db         DbIface.Database
	vdsList    []*Vds
	vdsMap     map[string]*Vds
	BackendCtl *Thread.ThreadCtrl
	logPath    string
	log        *log.Logger
	// sshIdentity 为 git 通道访问 SSH 远端用的身份（setupSSHIdentity 生成或读取）
	sshIdentity *Channel.SSHIdentity
	// ConnectDb 建立数据库连接，测试时可替换。
	ConnectDb func(config DbConfig.DatabaseConfig, logger *log.Logger) (DbIface.Database, error)
}

func New() (Server, error) {
	srv := Server{
		Port:      PORT_DEFAULT,
		args:      make(map[string]string),
		config:    Config.Confuguration{},
		logPath:   "",
		ConnectDb: Data.ConnectDb,
	}
	err := srv.init()
	if err != nil {
		return srv, err
	}
	return srv, nil
}

// NewServer 不经命令行参数直接创建 Server（测试与嵌入使用）。
func NewServer(serverId string, configPath string, port string) (*Server, error) {
	srv := &Server{
		Id:        serverId,
		Port:      PORT_DEFAULT,
		args:      map[string]string{CONFIG: configPath, PORT: port},
		ConnectDb: Data.ConnectDb,
	}
	if err := srv.loadConfig(); err != nil {
		return nil, err
	}
	return srv, nil
}

func (srv *Server) connectDb() {
	for {
		db, err := srv.ConnectDb(srv.config.Database, srv.log)
		if err == nil {
			srv.db = db
			return
		}
		srv.log.Printf("failed to connect to database, Err:%s", err)
		srv.log.Printf("retry to connect to database after 10 seconds")
		time.Sleep(10 * time.Second)
	}
}

func (srv *Server) Run() {
	logFile, logger, ex := CustomLogger.FileLoger(srv.logPath, srv.Id)
	if ex != nil {
		log.Fatalf("failed to create file logger[%s], Error: %s", srv.Id, ex)
	}
	if logFile != nil {
		defer logFile.Close()
	}
	journalLogFile, journalLogger, ex := CustomLogger.FileLoger(srv.logPath, fmt.Sprintf("%s_Journal", srv.Id))
	if ex != nil {
		log.Fatalf("failed to create file logger[%s_Journal], Error: %s", srv.Id, ex)
	}
	if journalLogFile != nil {
		defer journalLogFile.Close()
	}
	if err := srv.Start(logger, journalLogger); err != nil {
		logger.Fatalf("failed to start Data Service, Err:%s", err)
	}
	srv.RunHttp()
}

// Start 连接数据库、建立所有 vDS、启动后台任务与自我注册，但不监听端口。
func (srv *Server) Start(logger *log.Logger, journalLogger *log.Logger) error {
	if logger == nil {
		logger = log.Default()
	}
	if journalLogger == nil {
		journalLogger = logger
	}
	srv.log = logger
	srv.BackendCtl = Thread.NewThreadController(srv.log)
	if err := srv.setupSSHIdentity(); err != nil {
		return err
	}
	srv.connectDb()
	if err := srv.setupVds(journalLogger); err != nil {
		return err
	}
	srv.StartSelfRegistration()
	return nil
}

// setupSSHIdentity 读取（不存在则生成）DS 自己的 SSH 私钥，供 git 通道访问 SSH 远端，
// 并把公钥写进 state.json。在连数据库之前做：git 服务器上加公钥不必等 DS 完全启动。
func (srv *Server) setupSSHIdentity() error {
	dir := Config.SSHDir(srv.args[CONFIG], &srv.config)
	comment := fmt.Sprintf("unitao-%s", srv.Id)
	id, created, err := Channel.EnsureSSHIdentity(dir, comment)
	if err != nil {
		return fmt.Errorf("failed to set up ssh identity in [%s], Err:%s", dir, err)
	}
	if created {
		srv.log.Printf("generated ssh key [%s]", id.KeyFile)
	}
	srv.log.Printf("ssh public key for git channel (fingerprint %s): %s", id.Fingerprint, id.PublicKey)
	Channel.SetSSHIdentity(id)
	srv.sshIdentity = id
	srv.state.SetSSH(&Config.SSHState{PublicKey: id.PublicKey, Fingerprint: id.Fingerprint, KeyFile: id.KeyFile})
	return srv.state.Save(&srv.config)
}

func (srv *Server) RunHttp() {
	http.HandleFunc("/", Http.Cors(srv.handler))
	srv.log.Printf("Data Server Listen @%s://%s:%s", srv.config.Http.HttpType, srv.config.Http.DnsName, srv.Port)
	srv.log.Fatal(http.ListenAndServe(fmt.Sprintf(":%s", srv.Port), nil))
}

// ServeHTTP 让 Server 可以直接作为 http.Handler 使用（测试）。
func (srv *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	Http.Cors(srv.handler)(w, r)
}

func (srv *Server) runJournalHandler(vds *Vds, journalHandler *DataJournal.JournalHandler) {
	worker, err := srv.BackendCtl.AddWorker(fmt.Sprintf("journalHandler_%s", vds.Path), journalHandler.Run)
	if err != nil {
		srv.log.Fatalf("failed to create Journal Handler of vDS [%s] as backend process.", vds.Path)
	}
	vds.Journal.HandlerNotify = worker.Notify
	worker.Run()
}

func (srv *Server) init() error {
	var port string
	var configPath string
	var serverId string
	var logPath string
	flag.StringVar(&serverId, "id", "", "Data Server Id")
	flag.StringVar(&port, "port", "", "Data Server Listen Port")
	flag.StringVar(&configPath, "config", "", "Data Server Configuration JSON path")
	flag.StringVar(&logPath, "log", "", "path that hold log")
	flag.Parse()
	srv.args[PORT] = port
	if serverId == "" {
		flag.Usage()
		return fmt.Errorf("missing parameter id")
	}
	srv.Id = serverId
	if configPath == "" {
		flag.Usage()
		return fmt.Errorf("missing parameter config")
	}
	if logPath != "" {
		srv.logPath = logPath
	}
	srv.args[CONFIG] = configPath
	return srv.loadConfig()
}

func (srv *Server) loadConfig() error {
	err := Config.Read(srv.args[CONFIG], &srv.config)
	if err != nil {
		return err
	}
	srv.state, err = Config.LoadState(srv.args[CONFIG], &srv.config)
	if err != nil {
		return err
	}
	if srv.args[PORT] != "" {
		srv.Port = srv.args[PORT]
	} else if srv.config.Http.Port != "" {
		srv.Port = srv.config.Http.Port
	}
	return nil
}

func (srv *Server) responseError(w http.ResponseWriter, status int, format string, args ...interface{}) {
	err := Http.NewHttpError(fmt.Sprintf(format, args...), status)
	Http.ResponseJson(w, err, err.Status, srv.config.Http)
}

// splitVdsPath 从请求路径中取出 vDS：返回 (vDS 路径, 剩余路径, 是否为 vDS 分组根 /shared 或 /mirrored)。
func splitVdsPath(requestUrl string) (string, string, string) {
	first, rest := Util.ParsePath(requestUrl)
	switch first {
	case KindShared, KindMirrored:
		name, remain := Util.ParsePath(rest)
		if name == "" || strings.HasPrefix(name, "?") {
			return "", "", first
		}
		return fmt.Sprintf("%s/%s", first, name), remain, ""
	}
	return first, rest, ""
}

func (srv *Server) handler(w http.ResponseWriter, r *http.Request) {
	requestUrl, err := Http.GetUrl(r)
	if err != nil {
		srv.log.Printf("failed to parse request URL. Error:%s", err)
		Http.ResponseJson(w, err, err.Status, srv.config.Http)
		return
	}
	vdsPath, rest, group := splitVdsPath(requestUrl)
	if vdsPath == "" || strings.HasPrefix(vdsPath, "?") {
		// GET / 列出所有 vDS；GET /shared、/mirrored 列出该组的名字
		if r.Method != http.MethodGet {
			srv.responseError(w, http.StatusMethodNotAllowed, "method [%s] not supported on [%s]", r.Method, requestUrl)
			return
		}
		Http.ResponseJson(w, srv.listVds(group), http.StatusOK, srv.config.Http)
		return
	}
	vds, ok := srv.vdsMap[vdsPath]
	if !ok {
		srv.responseError(w, http.StatusNotFound, "vDS [%s] not found on this Data Service, available: %v", vdsPath, srv.listVds(""))
		return
	}
	dataType, idPath := Util.ParsePath(rest)
	if strings.HasPrefix(dataType, "?") {
		dataType = ""
	}
	srv.log.Printf("process request[%s] on vDS [%s] [%s/%s]", r.Method, vds.Path, dataType, idPath)
	if vds.Kind == KindMirrored && r.Method != http.MethodGet {
		srv.responseError(w, http.StatusMethodNotAllowed, "vDS [%s] is read-only", vds.Path)
		return
	}
	if dataType == "" {
		switch r.Method {
		case http.MethodGet:
			srv.handleVdsRoot(w, vds)
		case http.MethodPost:
			srv.handlePost(w, r, vds, "", "")
		default:
			srv.responseError(w, http.StatusMethodNotAllowed, "method [%s] not supported on vDS root [%s]", r.Method, vds.Path)
		}
		return
	}
	if dataType == Common.KeyConfig {
		if r.Method != http.MethodGet || idPath != "" {
			srv.responseError(w, http.StatusMethodNotAllowed, "only GET [%s/%s] is supported", vds.Path, Common.KeyConfig)
			return
		}
		Http.ResponseJson(w, srv.vdsConfigInfo(vds), http.StatusOK, srv.config.Http)
		return
	}
	if dataType == Record.KeyRecord {
		srv.responseError(w, http.StatusBadRequest, "data type=[%s] is not supported", dataType)
		return
	}
	if _, ok := Common.ReadOnlyTypes[dataType]; ok && r.Method != http.MethodGet {
		srv.responseError(w, http.StatusBadRequest, "update on data type=[%s] is not supported", dataType)
		return
	}
	switch r.Method {
	case http.MethodGet:
		srv.handleGet(w, vds, dataType, idPath)
	case http.MethodPost:
		srv.handlePost(w, r, vds, dataType, idPath)
	case http.MethodDelete:
		srv.handleDelete(w, vds, dataType, idPath)
	case http.MethodPut:
		srv.handlePut(w, r, vds, dataType, idPath)
	case http.MethodPatch:
		srv.handlePatch(w, r, vds, dataType, idPath)
	default:
		srv.responseError(w, http.StatusMethodNotAllowed, "method [%s] not supported", r.Method)
	}
}

// listVds 列出 vDS 路径；group 为 shared/mirrored 时只列该组的名字。
func (srv *Server) listVds(group string) []string {
	result := []string{}
	for _, vds := range srv.vdsList {
		if group == "" {
			result = append(result, vds.Path)
			continue
		}
		if vds.Kind == group {
			result = append(result, vds.Name)
		}
	}
	sort.Strings(result)
	return result
}

func (srv *Server) handleVdsRoot(w http.ResponseWriter, vds *Vds) {
	types, err := vds.Handler.ListDataTypes()
	if err != nil {
		Http.ResponseJson(w, err, err.Status, srv.config.Http)
		return
	}
	sort.Strings(types)
	result := append([]string{Common.KeyConfig, "schema"}, types...)
	Http.ResponseJson(w, result, http.StatusOK, srv.config.Http)
}

func (srv *Server) handleGet(w http.ResponseWriter, vds *Vds, dataType string, idPath string) {
	if idPath == "" {
		srv.log.Printf("list id of [%s/%s]", vds.Path, dataType)
		idList, err := vds.Handler.List(dataType)
		if err != nil {
			Http.ResponseJson(w, err, err.Status, srv.config.Http)
			return
		}
		Http.ResponseJson(w, idList, http.StatusOK, srv.config.Http)
		return
	}
	var result interface{}
	var err *Http.HttpError
	switch dataType {
	case Common.KeyJournal:
		if vds.Journal == nil {
			srv.responseError(w, http.StatusNotFound, "vDS [%s] has no journal", vds.Path)
			return
		}
		srv.log.Printf("get Journal of type [%s]", idPath)
		result, err = vds.Journal.GetJournal(idPath)
	default:
		srv.log.Printf("get data of [%s/%s/%s]", vds.Path, dataType, idPath)
		result, err = vds.Handler.Get(dataType, idPath)
	}
	if err != nil {
		Http.ResponseJson(w, err, err.Status, srv.config.Http)
		return
	}
	Http.ResponseJson(w, result, http.StatusOK, srv.config.Http)
}

func (srv *Server) BuildRecord(vds *Vds, payload map[string]interface{}, dataType string, dataId string) (*Record.Record, *Http.HttpError) {
	if dataType == "" {
		return nil, Http.NewHttpError(fmt.Sprintf("empty data type in path. [%s/%s]=''", Record.DataType, Record.DataId), http.StatusBadRequest)
	}
	if dataId == "" {
		return nil, Http.NewHttpError(fmt.Sprintf("empty data id in path. [%s/%s]=''", Record.DataType, Record.DataId), http.StatusBadRequest)
	}
	schema, err := vds.Handler.LocalSchema(dataType, "")
	if err != nil {
		return nil, err
	}
	record := Record.NewRecord(dataType, schema.Schema.Version, dataId, payload)
	return record, nil
}

func (srv *Server) handlePost(w http.ResponseWriter, r *http.Request, vds *Vds, dataType string, dataId string) {
	reqBody, err := Http.LoadRequest(r)
	if err != nil {
		Http.ResponseJson(w, err, err.Status, srv.config.Http)
		return
	}
	payload, ok := reqBody.(map[string]interface{})
	if !ok {
		Http.ResponseJson(w, "failed to parse request into JSON object", http.StatusBadRequest, srv.config.Http)
		return
	}
	var record *Record.Record
	var ex error
	if len(r.Header.Values(Record.NotRecord)) == 0 {
		if dataType != "" {
			Http.ResponseJson(w, Http.NewHttpError(fmt.Sprintf("POST a record to the vDS root [/%s], data type is taken from [%s]", vds.Path, Record.DataType), http.StatusBadRequest), http.StatusBadRequest, srv.config.Http)
			return
		}
		record, ex = Record.LoadMap(payload)
		if ex != nil {
			Http.ResponseJson(w, Http.WrapError(ex, "failed to load payload as Record", http.StatusBadRequest), http.StatusBadRequest, srv.config.Http)
			return
		}
	} else {
		record, err = srv.BuildRecord(vds, payload, dataType, dataId)
		if err != nil {
			Http.ResponseJson(w, err, err.Status, srv.config.Http)
			return
		}
	}
	err = vds.Handler.Add(record)
	if err != nil {
		Http.ResponseJson(w, err, err.Status, srv.config.Http)
		return
	}
	Http.ResponseText(w, []byte(record.Id), http.StatusCreated, srv.config.Http)
}

func (srv *Server) handlePut(w http.ResponseWriter, r *http.Request, vds *Vds, dataType string, dataId string) {
	reqBody, err := Http.LoadRequest(r)
	if err != nil {
		Http.ResponseJson(w, err, err.Status, srv.config.Http)
		return
	}
	payload, ok := reqBody.(map[string]interface{})
	if !ok {
		Http.ResponseJson(w, "failed to parse request into JSON object", http.StatusBadRequest, srv.config.Http)
		return
	}
	var record *Record.Record
	var ex error
	if len(r.Header.Values(Record.NotRecord)) == 0 {
		record, ex = Record.LoadMap(payload)
		if ex != nil {
			Http.ResponseJson(w, Http.WrapError(ex, "failed to load payload as Record", http.StatusBadRequest), http.StatusBadRequest, srv.config.Http)
			return
		}
	} else {
		record, err = srv.BuildRecord(vds, payload, dataType, dataId)
		if err != nil {
			Http.ResponseJson(w, err, err.Status, srv.config.Http)
			return
		}
	}
	err = vds.Handler.Set(dataType, dataId, record)
	if err != nil {
		Http.ResponseJson(w, err, err.Status, srv.config.Http)
		return
	}
	Http.ResponseText(w, []byte(record.Id), http.StatusCreated, srv.config.Http)
}

func (srv *Server) handleDelete(w http.ResponseWriter, vds *Vds, dataType string, dataId string) {
	err := vds.Handler.Delete(dataType, dataId)
	if err != nil {
		Http.ResponseJson(w, err, err.Status, srv.config.Http)
		return
	}
	result := map[string]string{
		"result": fmt.Sprintf("item [type/id]=[%s/%s] deleted", dataType, dataId),
	}
	Http.ResponseJson(w, result, http.StatusAccepted, srv.config.Http)
}

func (srv *Server) handlePatch(w http.ResponseWriter, r *http.Request, vds *Vds, dataType string, idPath string) {
	payload, e := Http.LoadRequest(r)
	if e != nil {
		srv.log.Printf("PATCH: [%s/%s] failed to load request, Error: %s", dataType, idPath, e)
		Http.ResponseJson(w, e, e.Status, srv.config.Http)
		return
	}
	headers := Http.ParseHeaders(r)
	srv.log.Printf("PATCH [%s/%s/%s]: call handler Patch", vds.Path, dataType, idPath)
	response, e := vds.Handler.Patch(dataType, idPath, headers, payload)
	if e != nil {
		Http.ResponseJson(w, e, e.Status, srv.config.Http)
		return
	}
	Http.ResponseJson(w, response, http.StatusAccepted, srv.config.Http)
}
