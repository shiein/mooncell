package main

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
)

// precheck 处理 GET /api/precheck?binPath=&port=&type=&runner= :
// 新建应用前的真实预检——目标目录在白名单内且可写、端口空闲、所需运行时/Runner 可用。
func (a *agent) precheck(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	checks := []map[string]any{}
	add := func(label string, ok bool, detail string) {
		checks = append(checks, map[string]any{"label": label, "ok": ok, "detail": detail})
	}

	// 1. 目标路径:白名单 + 目录可写
	if binPath := q.Get("binPath"); binPath != "" {
		if !withinRoots(binPath, a.cfg.Paths.DeployRoots) {
			add("目标路径在白名单内", false, binPath+" 不在 deploy_roots")
		} else {
			dir := filepath.Dir(binPath)
			os.MkdirAll(dir, 0755)
			probe := filepath.Join(dir, ".mc-precheck")
			if err := os.WriteFile(probe, []byte("x"), 0644); err != nil {
				add("目标目录可写", false, dir+" : "+err.Error())
			} else {
				os.Remove(probe)
				add("目标目录可写", true, dir)
			}
		}
	}

	// 2. 端口空闲
	if port := q.Get("port"); port != "" && port != "0" {
		if portFree(port) {
			add("端口空闲 :"+port, true, "")
		} else {
			add("端口占用 :"+port, false, "已被监听")
		}
	}

	// 3. 运行时 / Runner 能力
	need := map[string]string{} // capKey → 描述
	switch q.Get("type") {
	case "java-jar", "tomcat-war":
		need["java"] = "Java 运行时"
	case "python":
		need["python"] = "Python 运行时"
	case "node":
		need["node"] = "Node 运行时"
	}
	switch q.Get("runner") {
	case "pm2":
		need["pm2"] = "pm2"
	case "systemd", "":
		need["systemd"] = "systemd"
	}
	for key, desc := range need {
		ok, ver := a.capStatus(key)
		add(desc+" 可用", ok, ver)
	}

	writeJSON(w, http.StatusOK, map[string]any{"checks": checks})
}

// portFree 尝试在该端口监听判断是否空闲。
func portFree(port string) bool {
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// capStatus 查启动自检缓存里某能力是否可用及版本。
func (a *agent) capStatus(key string) (bool, string) {
	for _, c := range a.caps {
		if c.Key == key {
			return c.OK, c.Ver
		}
	}
	return false, "未检测到"
}
