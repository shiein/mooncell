package main

import (
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"
)

// consoleVersion 可在构建时用 -ldflags "-X main.consoleVersion=vX.Y.Z" 覆盖(发布打版用,与 agentVersion 对齐)。
var consoleVersion = "dev"

// 编译期把 vite 构建产物嵌入二进制。运行时从内存映像直接服务,无磁盘 IO。
// 需先 `pnpm build` 生成 dist/ 再 `go build`。
//
//go:embed all:dist
var distFS embed.FS

func main() {
	// 轻量 flag:--version 打印版本;--selftest 仅证明本二进制能在本机执行 + 接受当前 config.toml
	// (供自更新前置自检)。两者都不启动服务——selftest 不绑端口、不 openDB,避免与运行中的实例冲突
	// (运行中实例持有端口与 SQLite 文件,新二进制 selftest 若绑端口/开库会与之冲突,把好包误判成坏包)。
	for _, arg := range os.Args[1:] {
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
	a := &api{store: store, agent: newAgentClient(cfg.Agent), clients: map[string]*agentClient{}, cabinetDir: cfg.Cabinet.Dir, anonUpload: cfg.Cabinet.AnonUpload, cabinetMaxBytes: cabinetMaxBytes, agentBinDir: agentBinDir, demoSeed: cfg.Demo.Seed, maxUpload: maxUpload, uploads: map[string]*uploadSession{}, busy: map[string]int{}, appMu: map[string]*sync.Mutex{}, appEpoch: map[string]uint64{}, requireTLSAgents: cfg.Security.RequireTLSAgents}

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

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("POST /api/logout", a.logout)
	mux.HandleFunc("GET /api/session", a.session)

	// RBAC:
	// - admin:全量
	// - 普通用户:仅授权应用的部署/还原/启停/查看;不可新建/删除/改配置;不可访文件柜/审计/Agent/系统
	adminOnly := a.requireRole("admin")
	appOp := a.requireAppOp // 部署/还原/启停:admin 或已授权用户
	// 分块上传服务于部署,任意已登录可上传(部署时再校验应用授权)。
	anyLogin := a.requireAuth

	// Agent 代理(需登录):Console 持共享 token 调用本机/远端 Agent,前端只与 Console 通信。
	mux.HandleFunc("GET /api/agent/ping", anyLogin(a.agentProxy("/api/ping")))
	mux.HandleFunc("GET /api/agent/capabilities", anyLogin(a.agentProxy("/api/capabilities")))
	mux.HandleFunc("GET /api/agent/system", anyLogin(a.agentProxy("/api/system")))
	mux.HandleFunc("GET /api/agent/precheck", anyLogin(a.agentPrecheck))
	// 分块上传(断点续传):大制品先分块传到 Console,完成后用 uploadId 触发部署。
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

	// Console 自更新:仅 admin
	mux.HandleFunc("GET /api/console/info", adminOnly(a.consoleInfo))
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
