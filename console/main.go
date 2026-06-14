package main

import (
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
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

	a := &api{store: store, agent: newAgentClient(cfg.Agent)}

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
	mux.HandleFunc("POST /api/agent/apps/{id}/deploy", writeRoles(a.agentDeploy))
	mux.HandleFunc("POST /api/agent/apps/{id}/deploy/stream", writeRoles(a.agentDeployStream))
	mux.HandleFunc("GET /api/agent/apps/{id}/status", a.requireAuth(a.agentAppStatus))
	mux.HandleFunc("DELETE /api/agent/apps/{id}", writeRoles(a.agentUndeploy))
	mux.HandleFunc("GET /api/agent/apps/{id}/backups", a.requireAuth(a.agentListBackups))
	mux.HandleFunc("POST /api/agent/apps/{id}/restore/stream", writeRoles(a.agentRestoreStream))
	mux.HandleFunc("GET /api/agent/apps/{id}/logs/stream", a.requireAuth(a.agentLogStream))

	// 业务数据持久化:读(hydrate)任意角色;写限 admin/operator。
	mux.HandleFunc("POST /api/data", a.requireAuth(a.hydrate))
	mux.HandleFunc("PUT /api/data/{kind}/{id}", writeRoles(a.putEntity))
	mux.HandleFunc("DELETE /api/data/{kind}/{id}", writeRoles(a.deleteEntity))

	// 用户管理(仅 admin)
	mux.HandleFunc("GET /api/users", a.requireRole("admin")(a.listUsers))
	mux.HandleFunc("POST /api/users", a.requireRole("admin")(a.createUser))
	mux.HandleFunc("DELETE /api/users/{username}", a.requireRole("admin")(a.deleteUser))

	// 其余路径交给嵌入的前端静态资源(单页应用,无 URL 路由)。
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		log.Fatalf("[static] 无法读取嵌入的 dist: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	addr := fmt.Sprintf("%s:%d", cfg.Server.Addr, cfg.Server.Port)
	log.Printf("Mooncell Console 运行于 http://%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("[server] %v", err)
	}
}
