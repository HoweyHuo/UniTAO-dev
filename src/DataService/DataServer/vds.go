package DataServer

import (
	"fmt"
	"log"
	"sync"

	"DataService/Channel"
	"DataService/Config"
	"DataService/DataHandler"
	"DataService/DataInit"
	"DataService/DataJournal"
	"DataService/DataJournal/ProcessIface"
	"DataService/Share"
)

const (
	KindLocal    = DataHandler.KindLocal
	KindShared   = DataHandler.KindShared
	KindMirrored = DataHandler.KindMirrored
)

// Vds 是 DS 实例上承载的一个虚拟 Data Service：独立的数据表、schema、Journal、锁和读写规则，
// 在 Inventory 中注册为一个 DS。
type Vds struct {
	Kind  string
	Name  string // shared：共享名；mirrored：本网名字（alias 或来源共享名）；local 为空
	Path  string // local、shared/{name}、mirrored/{name}
	Table string

	Handler   *DataHandler.Handler
	Journal   *DataJournal.JournalLib
	Publisher *Share.Publisher
	Mirror    *Share.Mirror

	SharedConfig   *Config.SharedVdsConfig
	MirroredConfig *Config.MirroredVdsConfig

	// InstanceId 是注册到 Inventory 的身份：local 为实例 UUID，shared / mirrored 为来源公钥指纹。
	InstanceId string
	idLock     sync.Mutex
	invId      string
}

// RequestedId 返回向 Inventory 申请的 dsId（config 中的名字）。
func (v *Vds) RequestedId(config *Config.Confuguration) string {
	if v.Kind == KindLocal {
		return config.Ds.Id
	}
	return v.Name
}

func (v *Vds) InvId() string {
	v.idLock.Lock()
	defer v.idLock.Unlock()
	return v.invId
}

func (v *Vds) setInvId(id string) {
	v.idLock.Lock()
	defer v.idLock.Unlock()
	v.invId = id
}

func vdsTable(config *Config.Confuguration, kind string, name string) string {
	if kind == KindLocal {
		return config.DataTable.Data
	}
	return fmt.Sprintf("%s_%s_%s", config.DataTable.Data, kind, name)
}

// configuredVds 按 config 列出所有 vDS（尚未初始化）。
func (srv *Server) configuredVds() []*Vds {
	result := []*Vds{}
	if srv.config.Vds.Local != nil {
		result = append(result, &Vds{Kind: KindLocal, Path: KindLocal, Table: vdsTable(&srv.config, KindLocal, "")})
	}
	for idx := range srv.config.Vds.Shared {
		cfg := &srv.config.Vds.Shared[idx]
		result = append(result, &Vds{
			Kind:         KindShared,
			Name:         cfg.Name,
			Path:         fmt.Sprintf("%s/%s", KindShared, cfg.Name),
			Table:        vdsTable(&srv.config, KindShared, cfg.Name),
			SharedConfig: cfg,
		})
	}
	for idx := range srv.config.Vds.Mirrored {
		cfg := &srv.config.Vds.Mirrored[idx]
		result = append(result, &Vds{
			Kind:           KindMirrored,
			Name:           cfg.LocalName(),
			Path:           fmt.Sprintf("%s/%s", KindMirrored, cfg.LocalName()),
			Table:          vdsTable(&srv.config, KindMirrored, cfg.LocalName()),
			MirroredConfig: cfg,
			InstanceId:     cfg.TrustedFingerprint,
		})
	}
	return result
}

// setupVds 初始化所有 vDS：处理已从 config 删除的 vDS、建表导入元 schema、创建 Handler 与 Journal、
// 启动 shared 的发布器和 mirrored 的同步器。
func (srv *Server) setupVds(journalLogger *log.Logger) error {
	vdsList := srv.configuredVds()
	configured := map[string]bool{}
	for _, vds := range vdsList {
		configured[vds.Path] = true
	}
	// 旧版本只有一个 local 表，其 initialized 记在 state 顶层
	if _, ok := srv.state.Vds[KindLocal]; !ok && srv.config.Initialized {
		st := srv.state.VdsState(KindLocal)
		st.Initialized = true
		st.Table = vdsTable(&srv.config, KindLocal, "")
	}
	srv.removeDeletedVds(configured)
	if err := srv.forceInitIfNeeded(); err != nil {
		return err
	}
	srv.vdsList = []*Vds{}
	srv.vdsMap = map[string]*Vds{}
	for _, vds := range vdsList {
		if err := srv.initVds(vds, journalLogger); err != nil {
			return fmt.Errorf("failed to initialize vDS [%s], Error:%s", vds.Path, err)
		}
		srv.vdsList = append(srv.vdsList, vds)
		srv.vdsMap[vds.Path] = vds
	}
	if err := srv.state.Save(&srv.config); err != nil {
		srv.log.Printf("failed to save state, Err:%s", err)
	}
	for _, vds := range srv.vdsList {
		if vds.Publisher != nil {
			go vds.Publisher.Run()
		}
		if vds.Mirror != nil {
			go vds.Mirror.Run()
		}
	}
	return nil
}

// removeDeletedVds 处理 state 中有、config 中已删除的 shared / mirrored vDS：
// 从 Inventory 注销其 URL、删除数据表、清掉状态。local 被删除时只告警，不删数据。
func (srv *Server) removeDeletedVds(configured map[string]bool) {
	for _, path := range srv.state.VdsPaths() {
		if configured[path] {
			continue
		}
		st := srv.state.VdsState(path)
		if path == KindLocal {
			srv.log.Printf("vDS [local] is no longer configured; its table [%s] is kept", st.Table)
			continue
		}
		srv.log.Printf("vDS [%s] removed from config, drop table [%s] and unregister it before registering current vDS", path, st.Table)
		invId := st.AssignedId
		if invId == "" {
			_, invId = splitKindName(path)
		}
		// 只记录，由 StartSelfRegistration 在注册当前 vDS 之前完成注销
		if srv.invLinked() {
			for _, base := range srv.dsUrls() {
				srv.state.AddUnregister(invId, joinVdsUrl(base, path))
			}
		}
		if st.Table != "" {
			if err := srv.db.DeleteTable(st.Table); err != nil {
				srv.log.Printf("failed to drop table [%s] of removed vDS [%s], Err:%s", st.Table, path, err)
			}
		}
		srv.state.RemoveVds(path)
	}
}

func splitKindName(path string) (string, string) {
	for idx := 0; idx < len(path); idx++ {
		if path[idx] == '/' {
			return path[:idx], path[idx+1:]
		}
	}
	return path, ""
}

// forceInitIfNeeded 保持 force-init 的旧语义：local vDS 尚未初始化且 force-init 为 true 时清空整个数据库，
// 此时所有 vDS 都要重新初始化。
func (srv *Server) forceInitIfNeeded() error {
	if !srv.config.ForceInit {
		return nil
	}
	if st, ok := srv.state.Vds[KindLocal]; ok && st.Initialized {
		return nil
	}
	if err := DataInit.ResetDatabase(srv.db, srv.log); err != nil {
		return err
	}
	for _, path := range srv.state.VdsPaths() {
		srv.state.VdsState(path).Initialized = false
	}
	return nil
}

func (srv *Server) initVds(vds *Vds, journalLogger *log.Logger) error {
	st := srv.state.VdsState(vds.Path)
	vdsConfig := srv.config
	vdsConfig.DataTable.Data = vds.Table
	vdsConfig.Initialized = st.Initialized
	vdsConfig.ForceInit = false
	if !st.Initialized {
		if err := DataInit.InitDatabase(vdsConfig, srv.db, srv.log); err != nil {
			return err
		}
		st.Initialized = true
		st.Table = vds.Table
		if vds.Kind == KindLocal {
			srv.config.Initialized = true
		}
	}
	st.Table = vds.Table
	handler := DataHandler.NewWithDb(vdsConfig, srv.log, srv.db)
	handler.VdsPath = vds.Path
	handler.ReadOnly = vds.Kind == KindMirrored
	handler.RefLocalOnly = vds.Kind == KindShared
	handler.TypeChangeNotify = func(dataType string) {
		go srv.notifyTypeChange(vds, dataType)
	}
	vds.Handler = handler
	if vds.Kind != KindLocal {
		invId := vds.Name
		if st.AssignedId != "" && st.AssignedFrom == vds.Name {
			invId = st.AssignedId
		}
		vds.setInvId(invId)
	}
	switch vds.Kind {
	case KindLocal:
		vds.InstanceId = srv.config.Ds.InstanceId
		return srv.setupJournal(vds, journalLogger)
	case KindShared:
		return srv.setupShared(vds, journalLogger)
	case KindMirrored:
		return srv.setupMirrored(vds)
	}
	return nil
}

func (srv *Server) setupJournal(vds *Vds, journalLogger *log.Logger, extra ...ProcessIface.JournalProcess) error {
	journal, err := DataJournal.NewJournalLib(srv.db, vds.Table, journalLogger)
	if err != nil {
		return fmt.Errorf("failed to create Journal Library. Error: %s", err)
	}
	vds.Journal = journal
	vds.Handler.AddJournal = journal.AddJournal
	journalHandler, ex := DataJournal.NewJournalHandler(vds.Handler, journal, journalLogger, extra...)
	if ex != nil {
		return fmt.Errorf("failed to load Journal Handler. Error:%s", ex)
	}
	srv.runJournalHandler(vds, journalHandler)
	return nil
}

func (srv *Server) setupShared(vds *Vds, journalLogger *log.Logger) error {
	cfg := vds.SharedConfig
	signer, err := Channel.LoadSigningKey(cfg.SigningKeyFile)
	if err != nil {
		return err
	}
	channel, err := Channel.NewPublisher(cfg.Channel, cfg.Name, signer, srv.log)
	if err != nil {
		return err
	}
	outbox := Share.NewOutbox(srv.db, vds.Table)
	vds.Publisher = Share.NewPublisher(cfg.Name, vds.Handler, outbox, channel, signer, cfg.PublishIntervalSec, cfg.SnapshotIntervalSec, srv.log)
	vds.InstanceId = vds.Publisher.Fingerprint()
	return srv.setupJournal(vds, journalLogger, Share.NewChannelPublish(cfg.Name, outbox, journalLogger))
}

func (srv *Server) setupMirrored(vds *Vds) error {
	cfg := vds.MirroredConfig
	channel, err := Channel.NewSubscriber(cfg.Channel, cfg.Name, cfg.TrustedFingerprint, srv.log)
	if err != nil {
		return err
	}
	path := vds.Path
	loadState := func() Share.MirrorState {
		st := srv.state.VdsState(path)
		return Share.MirrorState{Loaded: st.Loaded, Cursor: st.Cursor, SourceFingerprint: st.SourceFingerprint}
	}
	saveState := func(ms Share.MirrorState) error {
		return srv.state.UpdateVds(path, &srv.config, func(st *Config.VdsState) {
			st.Loaded = ms.Loaded
			st.Cursor = ms.Cursor
			st.SourceFingerprint = ms.SourceFingerprint
		})
	}
	vds.Mirror = Share.NewMirror(cfg.Name, cfg.LocalName(), cfg.TrustedFingerprint, vds.Handler, channel, cfg.PullIntervalSec, loadState, saveState, srv.log)
	return nil
}

func channelInfo(cfg Channel.Config) map[string]interface{} {
	info := map[string]interface{}{"type": cfg.Type}
	switch cfg.Type {
	case Channel.TypeDir:
		info["path"] = cfg.Path
	case Channel.TypeGit:
		info["repo"] = Channel.RedactRepo(cfg.Repo)
		branch := cfg.Branch
		if branch == "" {
			branch = Channel.DefaultBranch
		}
		info["branch"] = branch
		info["workDir"] = cfg.WorkDir
	}
	info["encrypt"] = cfg.Encrypt != nil && cfg.Encrypt.RecipientsFile != ""
	info["decrypt"] = cfg.Decrypt != nil && cfg.Decrypt.IdentityFile != ""
	return info
}

// vdsConfigInfo 是 GET /{vDS}/config 的内容。不输出私钥、凭据和密钥内容。
func (srv *Server) vdsConfigInfo(vds *Vds) map[string]interface{} {
	info := map[string]interface{}{
		"path":        vds.Path,
		"kind":        vds.Kind,
		"readOnly":    vds.Kind == KindMirrored,
		"table":       vds.Table,
		"inventoryId": vds.InvId(),
		"instanceId":  vds.InstanceId,
		"instance": map[string]interface{}{
			"dsId":       srv.config.Ds.Id,
			"instanceId": srv.config.Ds.InstanceId,
			"urls":       srv.dsUrls(),
		},
		"urls": srv.vdsUrls(vds),
	}
	switch vds.Kind {
	case KindShared:
		info["name"] = vds.Name
		info["channel"] = channelInfo(vds.SharedConfig.Channel)
		info["signingKeyConfigured"] = vds.SharedConfig.SigningKeyFile != ""
		for key, value := range vds.Publisher.Status() {
			info[key] = value
		}
	case KindMirrored:
		info["name"] = vds.MirroredConfig.Name
		info["alias"] = vds.MirroredConfig.Alias
		info["channel"] = channelInfo(vds.MirroredConfig.Channel)
		for key, value := range vds.Mirror.Status() {
			info[key] = value
		}
	}
	return info
}

// GetVds 返回指定路径的 vDS（local、shared/{name}、mirrored/{name}），不存在时返回 nil。
func (srv *Server) GetVds(path string) *Vds {
	return srv.vdsMap[path]
}
