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
			// 纯检查:不创建业务目录,只探测「最近已存在祖先」是否可写(部署时再 MkdirAll)。
			dir := filepath.Dir(binPath)
			anc := dir
			for anc != "/" && anc != "." && !fileExists(anc) {
				anc = filepath.Dir(anc)
			}
			probe := filepath.Join(anc, ".mc-precheck")
			if err := os.WriteFile(probe, []byte("x"), 0644); err != nil {
				add("目标目录可写", false, anc+" 不可写: "+err.Error())
			} else {
				os.Remove(probe)
				detail := anc
				if anc != dir {
					detail = anc + "(将创建 " + dir + ")"
				}
				add("目标目录可写", true, detail)
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
	case "java-jar":
		need["java"] = "Java 运行时"
	case "tomcat-war":
		need["java"] = "Java 运行时"
		need["tomcat"] = "Tomcat 容器" // 目标机须有 Tomcat,否则换 WAR/健康检查阶段才暴露
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
