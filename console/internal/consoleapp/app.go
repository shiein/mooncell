package consoleapp

import (
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"runtime"
	"sync"
	"time"

	"mooncell/console/internal/dataresource"
)

// consoleVersion 由根 package main 的构建版本传入，供版本接口和自更新审计使用。
var consoleVersion = "dev"

// Run 启动 Console 应用。静态资源由根 package main 嵌入后传入，使应用实现可以收拢在 internal 包，
// 同时保持原有单二进制、构建命令和 -X main.consoleVersion 注入方式不变。
func Run(distFS fs.FS, version string, args []string) {
	consoleVersion = version
	// 轻量 flag:--version 打印版本;--selftest 仅证明本二进制能在本机执行 + 接受当前 config.toml
	// (供自更新前置自检)。两者都不启动服务——selftest 不绑端口、不 openDB,避免与运行中的实例冲突
	// (运行中实例持有端口与 SQLite 文件,新二进制 selftest 若绑端口/开库会与之冲突,把好包误判成坏包)。
	for _, arg := range args {
		switch arg {
		case "--version", "-v":
			fmt.Println(consoleVersion)
			return
		case "--selftest":
			// 只 loadConfig:校验配置可解析 + 通过 unsafeConsoleConfigReason 安全闸。
			// 不校验内嵌 dist 完整性(go:embed 是编译期保证,运行期再校验无收益,保持最简)。
			loadConfig("config.toml")
			fmt.Println("ok " + consoleVersion + " " + runtime.GOOS + "/" + runtime.GOARCH)
			return
		}
	}

	cfg := loadConfig("config.toml")

	// 单实例锁:双开会争 sqlite(MaxOpenConns=1),登录等请求可无限挂起(浏览器一直 pending)。
	// 自更新后若旧进程未退干净又拉起新进程,最常见症状就是「登录一直转圈」。
	lockPath := cfg.Database.Path + ".lock"
	if cfg.Database.Path == "" {
		lockPath = "mooncell.db.lock"
	}
	instanceLock, err := acquireInstanceLock(lockPath)
	if err != nil {
		log.Fatalf("[server] %v", err)
	}
	defer instanceLock.Close()

	store := openDB(cfg)
	defer store.Close()
	if removed, err := store.removeLegacyArtifacts(cfg.LegacyArtifact.Dir); err != nil {
		log.Fatalf("[db] 清理已下线制品仓库失败: %v", err)
	} else if removed > 0 {
		log.Printf("[db] 已移除旧制品仓库存量 %d 个及 artifacts 表", removed)
	}
	store.seedAdmin(cfg.Admin.Username, cfg.Admin.Password)

	maxUpload := int64(cfg.Deploy.MaxUploadMB) << 20
	if maxUpload <= 0 {
		maxUpload = 1024 << 20
	}
	cabinetMaxBytes := int64(cfg.Cabinet.MaxUploadMB) << 20
	if cabinetMaxBytes <= 0 {
		cabinetMaxBytes = 200 << 20
	}
	agentBinDir := cfg.AgentBin.Dir
	if agentBinDir == "" {
		agentBinDir = "agentbin"
	}
	a := &api{store: store, agent: newAgentClient(cfg.Agent), clients: map[string]*agentClient{}, cabinetDir: cfg.Cabinet.Dir, anonUpload: cfg.Cabinet.AnonUpload, cabinetMaxBytes: cabinetMaxBytes, agentBinDir: agentBinDir, demoSeed: cfg.Demo.Seed, maxUpload: maxUpload, uploads: map[string]*uploadSession{}, busy: map[string]int{}, appMu: map[string]*sync.Mutex{}, appEpoch: map[string]uint64{}, requireTLSAgents: cfg.Security.RequireTLSAgents}

	// 数据资源模块服务：持有 SQLite 句柄和凭据密钥。
	dataResSvc := dataresource.NewService(store.db, store.credKey)
	defer dataResSvc.Close()
	a.dataResSvc = dataResSvc
	dataResSvc.SetImportMaxMB(cfg.DataResource.ImportMaxMB)
	// 注入审计回调：复用现有审计系统，数据资源操作也写入审计日志。
	dataResSvc.SetAuditFunc(func(user, action, target, result string) {
		a.store.appendAudit(user, action, "数据资源·"+target, result)
	})

	// 文件柜过期清理 + 分块上传残留清理 + 审计保留裁剪:启动即清一次,之后每小时一次。
	go func() {
		for {
			if n := a.cleanupExpiredCabinet(); n > 0 {
				log.Printf("[cabinet] 清理过期文件 %d 个", n)
			}
			if n := a.cleanupStaleUploads(); n > 0 {
				log.Printf("[upload] 清理过期上传会话 %d 个", n)
			}
			if n := a.store.trimAudit(cfg.Audit.Keep); n > 0 {
				log.Printf("[audit] 保留最近 %d 条,清理较早审计 %d 条", cfg.Audit.Keep, n)
			}
			time.Sleep(time.Hour)
		}
	}()

	// 持续健康巡检 + Agent 指标采集(独立周期,默认 30s)。interval<=0 关闭。
	go a.runMonitor(cfg.Monitor.IntervalSeconds, cfg.Monitor.MetricsKeepHours)

	// 数据资源:手工事务超时清理(每 5 分钟检查一次,15 分钟空闲自动回滚)。
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			if n := dataResSvc.CleanupIdle(); n > 0 {
				log.Printf("[data-resource] 回滚超时手工事务 %d 个", n)
			}
		}
	}()

	// 数据资源:导入临时文件超时清理(每 10 分钟检查一次,30 分钟超时删除)。
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			dataResSvc.CleanupExpiredImports()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("POST /api/logout", a.logout)
	mux.HandleFunc("GET /api/session", a.session)

	// RBAC:
	// - admin:全量
	// - 普通用户:仅授权应用的部署/还原/启停/查看;不可新建/删除/改配置;不可访文件柜/审计/Agent/系统
	adminOnly := a.requireRole("admin")
	appOp := a.requireAppOp // 部署/还原/启停/读状态:admin 或已授权用户
	anyLogin := a.requireAuth

	// Agent 探测/预检仅 admin(非 admin 侧栏不拉真实 system;预检仅创建/改配置需要)。
	mux.HandleFunc("GET /api/agent/ping", adminOnly(a.agentProxy("/api/ping")))
	mux.HandleFunc("GET /api/agent/capabilities", adminOnly(a.agentProxy("/api/capabilities")))
	mux.HandleFunc("GET /api/agent/system", adminOnly(a.agentProxy("/api/system")))
	mux.HandleFunc("GET /api/agent/precheck", adminOnly(a.agentPrecheck))
	// 分块上传:须登录;handler 内校验 appId 授权并绑定会话(部署时复验)。
	mux.HandleFunc("POST /api/upload/start", anyLogin(a.uploadStart))
	mux.HandleFunc("PUT /api/upload/{uploadId}", anyLogin(a.uploadChunk))
	mux.HandleFunc("GET /api/upload/{uploadId}", anyLogin(a.uploadStatus))
	mux.HandleFunc("DELETE /api/upload/{uploadId}", anyLogin(a.uploadAbort))
	mux.HandleFunc("POST /api/agent/apps/{id}/deploy/stream", appOp(a.agentDeployStream))
	mux.HandleFunc("GET /api/agent/apps/{id}/status", appOp(a.agentAppStatus))
	mux.HandleFunc("POST /api/agent/apps/{id}/lifecycle", appOp(a.agentLifecycle))
	mux.HandleFunc("DELETE /api/agent/apps/{id}", adminOnly(a.agentUndeploy))
	mux.HandleFunc("DELETE /api/apps/{id}", adminOnly(a.appDelete)) // 权威删除:Agent 下线 + 删元数据 + 审计
	mux.HandleFunc("GET /api/agent/apps/{id}/backups", appOp(a.agentListBackups))
	mux.HandleFunc("POST /api/agent/apps/{id}/restore/stream", appOp(a.agentRestoreStream))
	mux.HandleFunc("GET /api/agent/apps/{id}/logs/stream", appOp(a.agentLogStream))
	mux.HandleFunc("GET /api/agent/apps/{id}/logs/download", appOp(a.agentLogDownload))
	mux.HandleFunc("GET /api/agent/apps/{id}/logs/file/stream", appOp(a.agentLogFileStream))

	// 业务数据:hydrate 按角色过滤应用;写配置/实体限 admin。
	mux.HandleFunc("POST /api/data", anyLogin(a.hydrate))
	mux.HandleFunc("GET /api/audit", adminOnly(a.listAudit))
	mux.HandleFunc("PUT /api/apps/{id}/config", adminOnly(a.putAppConfig))
	mux.HandleFunc("PUT /api/data/{kind}/{id}", adminOnly(a.putEntity))
	mux.HandleFunc("DELETE /api/data/{kind}/{id}", adminOnly(a.deleteEntity))

	// 用户管理(仅 admin)
	mux.HandleFunc("GET /api/users", adminOnly(a.listUsers))
	mux.HandleFunc("POST /api/users", adminOnly(a.createUser))
	mux.HandleFunc("PUT /api/users/{username}", adminOnly(a.updateUser))
	mux.HandleFunc("DELETE /api/users/{username}", adminOnly(a.deleteUser))

	// 数据资源管理:认证后注入用户信息到 context,handler 内做细粒度权限校验。
	drAuth := a.requireAuthDR
	mux.HandleFunc("GET /api/data-resources/drivers", drAuth(dataResSvc.ListDrivers))
	mux.HandleFunc("GET /api/data-resources", drAuth(dataResSvc.ListResources))
	mux.HandleFunc("POST /api/data-resources", drAuth(dataResSvc.CreateResource))
	mux.HandleFunc("GET /api/data-resources/{id}", drAuth(dataResSvc.GetResource))
	mux.HandleFunc("PUT /api/data-resources/{id}", drAuth(dataResSvc.UpdateResource))
	mux.HandleFunc("DELETE /api/data-resources/{id}", drAuth(dataResSvc.DeleteResource))
	mux.HandleFunc("POST /api/data-resources/test", drAuth(dataResSvc.TestConnectionHandler))
	mux.HandleFunc("POST /api/data-resources/{id}/test", drAuth(dataResSvc.TestExistingConnection))

	// 数据资源元数据:元数据树、表结构、DDL、SQL 模板。
	mux.HandleFunc("GET /api/data-resources/{id}/metadata/children", drAuth(dataResSvc.MetadataChildren))
	mux.HandleFunc("GET /api/data-resources/{id}/metadata/structure", drAuth(dataResSvc.MetadataStructure))
	mux.HandleFunc("GET /api/data-resources/{id}/metadata/ddl", drAuth(dataResSvc.MetadataDDL))
	mux.HandleFunc("POST /api/data-resources/{id}/metadata/sql-template", drAuth(dataResSvc.MetadataSQLTemplate))

	// 数据资源工作台：执行/导出仅经 workspace 契约（设计文档第三节），
	// 不再暴露无工作台状态的 /execute、/export 简化入口，避免危险确认与权限分叉。
	mux.HandleFunc("POST /api/data-resources/{id}/workspaces", drAuth(dataResSvc.CreateWorkspaceHandler))
	mux.HandleFunc("PATCH /api/data-resources/{id}/workspaces/{workspaceId}/auto-commit", drAuth(dataResSvc.PatchAutoCommit))
	mux.HandleFunc("POST /api/data-resources/{id}/workspaces/{workspaceId}/execute", drAuth(dataResSvc.ExecuteInWorkspaceHandler))
	mux.HandleFunc("POST /api/data-resources/{id}/workspaces/{workspaceId}/commit", drAuth(dataResSvc.CommitWorkspaceHandler))
	mux.HandleFunc("POST /api/data-resources/{id}/workspaces/{workspaceId}/rollback", drAuth(dataResSvc.RollbackWorkspaceHandler))
	mux.HandleFunc("POST /api/data-resources/{id}/workspaces/{workspaceId}/export", drAuth(dataResSvc.ExportFromWorkspace))
	mux.HandleFunc("DELETE /api/data-resources/{id}/workspaces/{workspaceId}", drAuth(dataResSvc.DeleteWorkspaceHandler))

	// 数据资源保存 SQL(Phase 5:个人 SQL)。
	mux.HandleFunc("GET /api/data-resources/{id}/saved-sql", drAuth(dataResSvc.ListSavedSQLHandler))
	mux.HandleFunc("POST /api/data-resources/{id}/saved-sql", drAuth(dataResSvc.CreateSavedSQLHandler))
	mux.HandleFunc("PUT /api/data-resources/{id}/saved-sql/{sqlId}", drAuth(dataResSvc.UpdateSavedSQLHandler))
	mux.HandleFunc("DELETE /api/data-resources/{id}/saved-sql/{sqlId}", drAuth(dataResSvc.DeleteSavedSQLHandler))

	// 数据资源导入(Phase 5:CSV/XLSX 导入)。
	mux.HandleFunc("POST /api/data-resources/{id}/imports/preview", drAuth(dataResSvc.ImportPreviewHandler))
	mux.HandleFunc("POST /api/data-resources/{id}/imports/{importId}/execute", drAuth(dataResSvc.ImportExecuteHandler))
	mux.HandleFunc("DELETE /api/data-resources/{id}/imports/{importId}", drAuth(dataResSvc.ImportDeleteHandler))

	// 多 Agent 管理:仅 admin
	mux.HandleFunc("GET /api/agents", adminOnly(a.listAgents))
	mux.HandleFunc("POST /api/agents", adminOnly(a.addAgent))
	mux.HandleFunc("DELETE /api/agents/{id}", adminOnly(a.deleteAgent))
	mux.HandleFunc("GET /api/agents/{id}/ping", adminOnly(a.pingAgent))
	mux.HandleFunc("GET /api/agents/{id}/metrics", adminOnly(a.listAgentMetrics))

	// Agent 自更新:仅 admin
	mux.HandleFunc("GET /api/agent-binaries", adminOnly(a.listAgentBinaries))
	mux.HandleFunc("POST /api/agent-binary", adminOnly(a.uploadAgentBinary))
	mux.HandleFunc("POST /api/agents/{id}/update", adminOnly(a.updateAgent))

	// Console 自更新:info 任意登录可读(升级后轮询确认重启);上传自更新限 admin。
	mux.HandleFunc("GET /api/console/info", anyLogin(a.consoleInfo))
	mux.HandleFunc("POST /api/console/self-update", adminOnly(a.selfUpdate))

	// 文件柜:仅 admin;公开文件凭码免登录下载。
	mux.HandleFunc("POST /api/cabinet", adminOnly(a.uploadCabinet))
	mux.HandleFunc("GET /api/cabinet/{id}/download", adminOnly(a.downloadCabinet))
	mux.HandleFunc("DELETE /api/cabinet/{id}", adminOnly(a.deleteCabinet))
	mux.HandleFunc("GET /api/pubfile/{code}", a.downloadByCode)   // 独立前缀,避免与 /api/cabinet/{id}/... 冲突
	mux.HandleFunc("GET /api/pubfile/{code}/meta", a.pubfileMeta) // 凭码校验 + 文件信息(不计下载数),供 /drop 页用
	mux.HandleFunc("POST /api/pub/cabinet", a.uploadCabinetAnon)  // 匿名上传(需 cabinet.anon_upload=true)
	mux.HandleFunc("GET /api/pub/limits", a.pubLimits)            // 公开:文件柜上限 + 匿名开关(供 /drop 客户端预检)

	// 独立免登录投递页:极简自包含 HTML,只上传 + 凭码下载,无列表(列表仅登录后 SPA 可见)。
	mux.HandleFunc("GET /drop", a.dropPage)

	// 其余路径交给嵌入的前端静态资源(单页应用,无 URL 路由)。
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		log.Fatalf("[static] 无法读取嵌入的 dist: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	addr := fmt.Sprintf("%s:%d", cfg.Server.Addr, cfg.Server.Port)
	log.Printf("Mooncell Console 运行于 http://%s", addr)
	if err := http.ListenAndServe(addr, securityHeaders(mux)); err != nil {
		log.Fatalf("[server] %v", err)
	}
}

// securityHeaders 给所有响应注入基础安全头(纵深防御,内网/对外皆生效)。
// CSP:SPA 产物为外链 JS、无内联脚本,故 script 仅同源;React 大量内联 style 属性,style 必须放开
// 'unsafe-inline'(样式注入风险远低于脚本);img 放开 data:(favicon/内联图标);连接同源(SSE/fetch)。
// /drop 自包含页含内联 <script>,由其 handler 用 per-response nonce 单独覆盖 CSP,不在此放开 inline script。
func securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; font-src 'self' data:; connect-src 'self'; " +
		"object-src 'none'; base-uri 'self'; frame-ancestors 'none'; form-action 'self'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		h.Set("Content-Security-Policy", csp)
		next.ServeHTTP(w, r)
	})
}
