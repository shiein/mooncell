package main

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
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

	// 2. 端口空闲。pm2 接管模式下端口被占用是预期的(被接管进程正在跑),
	//    只要占用者就是被接管的那个 pm2 进程(或其子进程),就不算冲突——否则用户得先停掉自己的进程才能预检过,本末倒置。
	if port := q.Get("port"); port != "" && port != "0" {
		adoptName := strings.TrimSpace(q.Get("pm2Name"))
		switch {
		case portFree(port):
			add("端口空闲 :"+port, true, "")
		case q.Get("runner") == "pm2" && containerNameRe.MatchString(adoptName):
			adopted, _ := pm2("pid", adoptName)
			adopted = strings.TrimSpace(adopted)
			owners := portOwnerPIDs(port)
			switch {
			case adopted != "" && adopted != "0" && pidMatches(adopted, owners):
				add("端口由被接管进程占用 :"+port, true, "pm2 "+adoptName+" · pid "+adopted)
			case len(owners) == 0:
				add("端口占用 :"+port, false, "已被监听,但无法确认占用进程(ss/lsof 不可用),请自行确认是否为被接管进程")
			default:
				add("端口占用 :"+port, false, "被非接管进程占用(占用 pid "+strings.Join(owners, ",")+";被接管进程 pid "+adopted+")")
			}
		default:
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

var ssPidRe = regexp.MustCompile(`pid=(\d+)`)

// portOwnerPIDs 返回正在监听该端口的进程 pid(Linux:ss 优先,退回 lsof)。
// 用于接管模式判断"端口是否被被接管进程占用"。取不到(命令缺失/无监听)返回空,调用方按未知处理。
func portOwnerPIDs(port string) []string {
	if out, err := exec.Command("ss", "-ltnHp", "sport = :"+port).Output(); err == nil {
		var pids []string
		for _, m := range ssPidRe.FindAllStringSubmatch(string(out), -1) {
			pids = append(pids, m[1])
		}
		if len(pids) > 0 {
			return pids
		}
	}
	if out, err := exec.Command("lsof", "-t", "-i", ":"+port, "-sTCP:LISTEN").Output(); err == nil {
		return strings.Fields(string(out))
	}
	return nil
}

// pidMatches 判断 target 是否就是 owners 中某 pid,或为其祖先(沿 /proc/<pid>/stat 的 ppid 上溯)。
// 接管模式下监听端口的可能是被接管进程本身,也可能是它 fork 的子进程,故连祖先链一起认。
func pidMatches(target string, owners []string) bool {
	for _, p := range owners {
		for cur, hop := p, 0; cur != "" && cur != "0" && cur != "1" && hop < 12; cur, hop = ppidOf(cur), hop+1 {
			if cur == target {
				return true
			}
		}
	}
	return false
}

// procStatFields 读 /proc/<pid>/stat 返回 comm 之后的字段切片(comm 可能含空格/括号,故从最后一个 ')' 后切)。
// f[0]=state f[1]=ppid …… f[19]=starttime(stat 第 22 字段)。非 linux / 读不到返回 nil。
func procStatFields(pid string) []string {
	b, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return nil
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return nil
	}
	return strings.Fields(s[i+1:])
}

// ppidOf 取父 pid。
func ppidOf(pid string) string {
	if f := procStatFields(pid); len(f) >= 2 {
		return f[1]
	}
	return ""
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
