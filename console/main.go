package main

import (
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"time"
)

// 编译期把 vite 构建产物嵌入二进制。运行时从内存映像直接服务,无磁盘 IO。
// 需先 `pnpm build` 生成 dist/ 再 `go build`。
//
//go:embed all:dist
var distFS embed.FS

func main() {
	cfg := loadConfig("config.toml")

	store := openDB(cfg)
	defer store.Close()
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
	a := &api{store: store, agent: newAgentClient(cfg.Agent), clients: map[string]*agentClient{}, cabinetDir: cfg.Cabinet.Dir, anonUpload: cfg.Cabinet.AnonUpload, cabinetMaxBytes: cabinetMaxBytes, agentBinDir: agentBinDir, demoSeed: cfg.Demo.Seed, maxUpload: maxUpload, uploads: map[string]*uploadSession{}}

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

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("POST /api/logout", a.logout)
	mux.HandleFunc("GET /api/session", a.session)

	// RBAC:读类接口任意已登录角色可访;改类(部署/还原/下线/改数据)限 admin/operator;
	// 用户管理限 admin。viewer 只读。
	writeRoles := a.requireRole("admin", "operator")

	// Agent 代理(需登录):Console 持共享 token 调用本机/远端 Agent,前端只与 Console 通信。
	mux.HandleFunc("GET /api/agent/ping", a.requireAuth(a.agentProxy("/api/ping")))
	mux.HandleFunc("GET /api/agent/capabilities", a.requireAuth(a.agentProxy("/api/capabilities")))
	mux.HandleFunc("GET /api/agent/system", a.requireAuth(a.agentProxy("/api/system")))
	mux.HandleFunc("GET /api/agent/precheck", a.requireAuth(a.agentPrecheck))
	// 分块上传(断点续传):大制品先分块传到 Console,完成后用 uploadId 触发部署。限 write。
	mux.HandleFunc("POST /api/upload/start", writeRoles(a.uploadStart))
	mux.HandleFunc("PUT /api/upload/{uploadId}", writeRoles(a.uploadChunk))
	mux.HandleFunc("GET /api/upload/{uploadId}", writeRoles(a.uploadStatus))
	mux.HandleFunc("DELETE /api/upload/{uploadId}", writeRoles(a.uploadAbort))
	mux.HandleFunc("POST /api/agent/apps/{id}/deploy/stream", writeRoles(a.agentDeployStream))
	mux.HandleFunc("GET /api/agent/apps/{id}/status", a.requireAuth(a.agentAppStatus))
	mux.HandleFunc("POST /api/agent/apps/{id}/lifecycle", writeRoles(a.agentLifecycle))
	mux.HandleFunc("DELETE /api/agent/apps/{id}", writeRoles(a.agentUndeploy))
	mux.HandleFunc("DELETE /api/apps/{id}", writeRoles(a.appDelete)) // 权威删除:Agent 下线 + 删元数据 + 审计
	mux.HandleFunc("GET /api/agent/apps/{id}/backups", a.requireAuth(a.agentListBackups))
	mux.HandleFunc("POST /api/agent/apps/{id}/restore/stream", writeRoles(a.agentRestoreStream))
	mux.HandleFunc("GET /api/agent/apps/{id}/logs/stream", a.requireAuth(a.agentLogStream))
	mux.HandleFunc("GET /api/agent/apps/{id}/logs/download", a.requireAuth(a.agentLogDownload))
	mux.HandleFunc("GET /api/agent/apps/{id}/logs/file/stream", a.requireAuth(a.agentLogFileStream))

	// 业务数据持久化:读(hydrate)任意角色;写限 admin/operator。
	mux.HandleFunc("POST /api/data", a.requireAuth(a.hydrate))
	mux.HandleFunc("GET /api/audit", a.requireAuth(a.listAudit)) // 审计倒序分页(hydrate 只带最近一窗)
	mux.HandleFunc("PUT /api/apps/{id}/config", writeRoles(a.putAppConfig)) // 类型化应用配置写入(服务端校验)
	mux.HandleFunc("PUT /api/data/{kind}/{id}", writeRoles(a.putEntity))
	mux.HandleFunc("DELETE /api/data/{kind}/{id}", writeRoles(a.deleteEntity))

	// 用户管理(仅 admin)
	mux.HandleFunc("GET /api/users", a.requireRole("admin")(a.listUsers))
	mux.HandleFunc("POST /api/users", a.requireRole("admin")(a.createUser))
	mux.HandleFunc("DELETE /api/users/{username}", a.requireRole("admin")(a.deleteUser))

	// 多 Agent 管理:列表任意登录可见;增删限 admin;ping 任意登录可测。
	mux.HandleFunc("GET /api/agents", a.requireAuth(a.listAgents))
	mux.HandleFunc("POST /api/agents", a.requireRole("admin")(a.addAgent))
	mux.HandleFunc("DELETE /api/agents/{id}", a.requireRole("admin")(a.deleteAgent))
	mux.HandleFunc("GET /api/agents/{id}/ping", a.requireAuth(a.pingAgent))

	// Agent 自更新:升级包按架构上传/列出(列表任意登录可见,上传与推送限 admin)。
	mux.HandleFunc("GET /api/agent-binaries", a.requireAuth(a.listAgentBinaries))
	mux.HandleFunc("POST /api/agent-binary", a.requireRole("admin")(a.uploadAgentBinary))
	mux.HandleFunc("POST /api/agents/{id}/update", a.requireRole("admin")(a.updateAgent))

	// 文件柜:上传/删除限 write;按 id 下载需登录;公开文件凭码免登录下载。
	mux.HandleFunc("POST /api/cabinet", writeRoles(a.uploadCabinet))
	mux.HandleFunc("GET /api/cabinet/{id}/download", a.requireAuth(a.downloadCabinet))
	mux.HandleFunc("DELETE /api/cabinet/{id}", writeRoles(a.deleteCabinet))
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
