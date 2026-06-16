package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"
)

// agentVersion 可在构建时用 -ldflags "-X main.agentVersion=vX.Y.Z" 覆盖(发布打版用)。
var agentVersion = "v0.1.0"

// agent 持有配置与运行期状态,挂载各 HTTP handler。
type agent struct {
	cfg     *Config
	caps    []Capability
	started time.Time
	locks   sync.Map // appId → *sync.Mutex,同应用部署/还原串行
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func main() {
	// 轻量 flag:--version 打印版本;--selftest 仅证明本二进制能在本机执行(供自更新前置自检,
	// 不绑端口以免与运行中的实例冲突)。两者都不启动服务。
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--version", "-v":
			fmt.Println(agentVersion)
			return
		case "--selftest":
			fmt.Println("ok " + agentVersion + " " + runtime.GOOS + "/" + runtime.GOARCH)
			return
		}
	}

	cfg := loadConfig("config.toml")

	a := &agent{cfg: cfg, started: time.Now()}
	// 启动自检:探测目标机能力,缓存上报(能力不会在进程生命周期内变化)。
	a.caps = detectCapabilities()
	a.logCaps()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/ping", a.tokenAuth(a.ping))
	mux.HandleFunc("GET /api/capabilities", a.tokenAuth(a.capabilities))
	mux.HandleFunc("GET /api/system", a.tokenAuth(a.system))
	mux.HandleFunc("GET /api/precheck", a.tokenAuth(a.precheck)) // 新建应用前真实预检

	// 部署(native-binary / systemd Runner):上传制品 + 配置 → 备份→替换→起停→健康检查→失败回滚
	mux.HandleFunc("POST /api/apps/{id}/deploy", a.tokenAuth(a.deploy))
	mux.HandleFunc("POST /api/apps/{id}/deploy/stream", a.tokenAuth(a.deployStream)) // SSE 实时日志流
	mux.HandleFunc("GET /api/apps/{id}/status", a.tokenAuth(a.appStatus))
	mux.HandleFunc("POST /api/apps/{id}/lifecycle", a.tokenAuth(a.appLifecycle))
	mux.HandleFunc("DELETE /api/apps/{id}", a.tokenAuth(a.undeploy))

	// 一键还原:列出历史备份,用指定备份制品重跑部署流水线(还原前自动备份当前版本,失败自动回滚)
	mux.HandleFunc("GET /api/apps/{id}/backups", a.tokenAuth(a.listBackups))
	mux.HandleFunc("GET /api/apps/{id}/releases", a.tokenAuth(a.listReleases)) // static 历史 release
	mux.HandleFunc("GET /api/apps/{id}/release", a.tokenAuth(a.releaseStatus)) // 权威幂等记录(SSE 断流对账)
	mux.HandleFunc("POST /api/apps/{id}/restore", a.tokenAuth(a.restore))
	mux.HandleFunc("POST /api/apps/{id}/restore/stream", a.tokenAuth(a.restoreStream)) // SSE 实时日志流

	// 应用运行时日志:跟随 systemd journal 实时流(SSE)
	mux.HandleFunc("GET /api/apps/{id}/logs/stream", a.tokenAuth(a.logStream))
	mux.HandleFunc("GET /api/apps/{id}/logs/download", a.tokenAuth(a.logDownload))      // 时间范围导出 gzip
	mux.HandleFunc("GET /api/apps/{id}/logs/file/stream", a.tokenAuth(a.logFileStream)) // 文件日志 tail(log_roots 白名单)

	// Agent 自更新:Console 推送新二进制 → 校验架构/sha/自检 → 原子替换自身 → self-exec 就地重启
	mux.HandleFunc("POST /api/self-update", a.tokenAuth(a.selfUpdate))

	addr := fmt.Sprintf("%s:%d", cfg.Server.Addr, cfg.Server.Port)
	log.Printf("Mooncell Agent %s 运行于 http://%s", agentVersion, addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("[server] %v", err)
	}
}

func (a *agent) logCaps() {
	for _, c := range a.caps {
		state := "未检测到"
		if c.OK {
			state = c.Ver
		}
		log.Printf("[cap] %-8s %s", c.Label, state)
	}
}

// ping 连通性测试 + token 校验,Console「连通性测试」按钮调用。
func (a *agent) ping(w http.ResponseWriter, r *http.Request) {
	host, _ := os.Hostname()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"agent":   host,
		"version": agentVersion,
		"os":      runtime.GOOS + "/" + runtime.GOARCH,
		"uptime":  int(time.Since(a.started).Seconds()),
		"time":    time.Now().UnixMilli(),
	})
}

func (a *agent) capabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"capabilities": a.caps})
}

func (a *agent) system(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, readSystem(a.diskProbePath()))
}

// diskProbePath 选磁盘水位探测点:优先备份目录所在分区,不存在则退回根分区。
func (a *agent) diskProbePath() string {
	if d := a.cfg.Paths.BackupDir; d != "" {
		if _, err := os.Stat(d); err == nil {
			return d
		}
	}
	return "/"
}
