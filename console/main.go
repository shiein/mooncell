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

	a := &api{store: store}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("POST /api/logout", a.logout)
	mux.HandleFunc("GET /api/session", a.session)

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
