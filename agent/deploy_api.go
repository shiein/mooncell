package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

func cleanupMultipart(r *http.Request) {
	if r.MultipartForm != nil {
		r.MultipartForm.RemoveAll()
	}
}

// prepareDeploy 解析部署请求公共部分:multipart(config + artifact)+ 安全边界校验 + 制品暂存。
// 成功返回 cfg、暂存路径、清理函数与 ok=true;失败已写好响应,ok=false。
func (a *agent) prepareDeploy(w http.ResponseWriter, r *http.Request) (DeployConfig, string, func(), bool) {
	var zero DeployConfig
	id := r.PathValue("id")
	// 传输层硬上限(纵深防御):超大制品会先落临时盘撑爆磁盘。ParseMultipartForm 的参数只是内存阈值。
	limit := int64(a.cfg.Deploy.MaxUploadMB) << 20
	if limit <= 0 {
		limit = 1024 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("制品超过上限 %d MB", limit>>20)})
			return zero, "", nil, false
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "表单解析失败"})
		return zero, "", nil, false
	}
	defer cleanupMultipart(r)

	var cfg DeployConfig
	if err := json.Unmarshal([]byte(r.FormValue("config")), &cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "config 解析失败"})
		return zero, "", nil, false
	}
	cfg.ID = id // 以路径为准,避免 body 与路径不一致
	if msg, ok := validIDAndRelease(cfg.ID, cfg.ReleaseID); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return zero, "", nil, false
	}

	// 安全边界:制品落盘路径必须在白名单根目录内(防穿越)。
	if !withinRoots(cfg.BinPath, a.cfg.Paths.DeployRoots) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "制品路径不在白名单内: " + cfg.BinPath})
		return zero, "", nil, false
	}

	file, _, err := r.FormFile("artifact")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 artifact 制品"})
		return zero, "", nil, false
	}
	defer file.Close()

	tmp, err := os.CreateTemp("", "mc-artifact-*")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "创建暂存失败"})
		return zero, "", nil, false
	}
	tmpPath := tmp.Name()
	if _, err := io.Copy(tmp, file); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "接收制品失败"})
		return zero, "", nil, false
	}
	tmp.Close()
	return cfg, tmpPath, func() { os.Remove(tmpPath) }, true
}

// deploy 处理 POST /api/apps/{id}/deploy(同步):执行流水线后一次性返回逐步结果与日志。
func (a *agent) deploy(w http.ResponseWriter, r *http.Request) {
	cfg, tmpPath, cleanup, ok := a.prepareDeploy(w, r)
	if !ok {
		return
	}
	defer cleanup()
	res := a.runDeployIdempotent("deploy", cfg, tmpPath, nil)
	writeJSON(w, http.StatusOK, res)
}

// deployStream 处理 POST /api/apps/{id}/deploy/stream(SSE):
// 每完成一步推送 `event: step`,结束推送 `event: done`(含整体结果),供前端实时呈现日志。
func (a *agent) deployStream(w http.ResponseWriter, r *http.Request) {
	cfg, tmpPath, cleanup, ok := a.prepareDeploy(w, r)
	if !ok {
		return
	}
	defer cleanup()
	runSSE(w, func(emit func(Step)) DeployResult { return a.runDeployIdempotent("deploy", cfg, tmpPath, emit) })
}

// sseHeader 写好 SSE 响应头并返回一个推送闭包 sse(event, payload);不支持 Flusher 时返回 ok=false。
// 部署/还原(有限流,末尾推 done)与日志(无限流)共用此骨架。
func sseHeader(w http.ResponseWriter) (func(event string, payload any), bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "服务端不支持流式响应"})
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 反代/nginx 前不缓冲
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return func(event string, payload any) {
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}, true
}

// runSSE 建立 SSE 响应头,执行 run(emit) 流水线:每步完成推 `event: step`,结束推 `event: done`。
// 部署与还原共用同一套流式骨架,差异只在传入的 run 闭包(制品来源不同)。
func runSSE(w http.ResponseWriter, run func(emit func(Step)) DeployResult) {
	sse, ok := sseHeader(w)
	if !ok {
		return
	}
	res := run(func(s Step) { sse("step", s) })
	sse("done", res)
}

// pm2NameReq 解析请求要操作的 pm2 进程名:Console 接管模式会透传 pm2Name(用户已有进程名),
// 否则用 Mooncell 托管名 deploy-<id>。供 status/lifecycle/logs 等无状态端点统一定位进程。
// 透传值须为合法名(与部署/容器名同一校验);非法则回退托管名,不把越界值喂给 pm2 argv。
func pm2NameReq(r *http.Request, id string) string {
	if n := strings.TrimSpace(r.URL.Query().Get("pm2Name")); n != "" && containerNameRe.MatchString(n) {
		return n
	}
	return unitName(id)
}

// nohupBinPathReq 取并校验 nohup 启停/状态请求的 binPath:必须在 deploy_roots 白名单内
// (binPath 决定 pidfile/spec 位置,不校验则越界 query 可让 Agent kill 任意 pid / 读任意 spec)。
func (a *agent) nohupBinPathReq(w http.ResponseWriter, r *http.Request) (string, bool) {
	bp := strings.TrimSpace(r.URL.Query().Get("binPath"))
	if bp == "" || !withinRoots(bp, a.cfg.Paths.DeployRoots) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "nohup 操作缺少合法 binPath(须在 deploy_roots 内): " + bp})
		return "", false
	}
	return bp, true
}

// nohupStatusJSON 据 binPath 读 pidfile 返回 nohup 进程状态(与 pm2/systemd 同形;含 PID 复用身份校验)。
func nohupStatusJSON(w http.ResponseWriter, id, binPath string) {
	st, ok := readNohupState(DeployConfig{BinPath: binPath})
	alive := ok && stateAlive(st)
	pid, state := "", "stopped"
	if alive {
		pid, state = fmt.Sprint(st.Pid), "online"
	}
	cpu, mem := procStats(pid)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "active": alive, "state": state, "pid": pid, "cpu": cpu, "mem": mem})
}

// appStatus 处理 GET /api/apps/{id}/status?runner=<systemd|pm2|nohup>:返回进程托管状态。
func (a *agent) appStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !requireValidID(w, id) {
		return
	}
	if r.URL.Query().Get("runner") == "nohup" {
		bp, ok := a.nohupBinPathReq(w, r)
		if !ok {
			return
		}
		nohupStatusJSON(w, id, bp)
		return
	}
	if r.URL.Query().Get("runner") == "pm2" {
		name := pm2NameReq(r, id)
		online := pm2Online(name)
		pid, _ := pm2("pid", name)
		pid = strings.TrimSpace(pid)
		state := "stopped"
		if online {
			state = "online"
		}
		cpu, mem := procStats(pid)
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "active": online, "state": state, "pid": pid, "cpu": cpu, "mem": mem})
		return
	}
	state, _ := sysctl("is-active", unitName(id))
	pid := mainPID(id)
	cpu, mem := procStats(pid)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     id,
		"active": isActive(id),
		"state":  state,
		"pid":    pid,
		"cpu":    cpu,
		"mem":    mem,
	})
}

// appLifecycle 处理 POST /api/apps/{id}/lifecycle?action=start|stop&runner=<systemd|pm2>:
// 真机启停已托管的进程(不重新部署),返回启停后的真实状态。前端不再前端伪造启停/pid。
func (a *agent) appLifecycle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !requireValidID(w, id) {
		return
	}
	action := r.URL.Query().Get("action")
	if action != "start" && action != "stop" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "action 仅支持 start|stop"})
		return
	}
	if r.URL.Query().Get("runner") == "nohup" {
		bp, ok := a.nohupBinPathReq(w, r)
		if !ok {
			return
		}
		if action == "stop" {
			nohupStop(DeployConfig{BinPath: bp})
		} else if nohupAlive(DeployConfig{BinPath: bp}) {
			// 幂等:已在运行(身份匹配)则直接返回现状,不重复 launch 出第二份进程
		} else if _, err := nohupStartFromSpec(bp); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "start 失败: " + err.Error()})
			return
		}
		nohupStatusJSON(w, id, bp)
		return
	}
	pm := r.URL.Query().Get("runner") == "pm2"
	pmName := pm2NameReq(r, id) // 接管模式=用户进程名,否则 deploy-<id>
	var out string
	var err error
	if pm {
		out, err = pm2(action, pmName)
	} else {
		out, err = sysctl(action, unitName(id))
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": action + " 失败: " + strings.TrimSpace(out+" "+err.Error())})
		return
	}
	// 返回启停后的真实托管状态(与 appStatus 同形),供前端权威刷新。
	if pm {
		online := pm2Online(pmName)
		pid, _ := pm2("pid", pmName)
		pid = strings.TrimSpace(pid)
		state := "stopped"
		if online {
			state = "online"
		}
		cpu, mem := procStats(pid)
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "active": online, "state": state, "pid": pid, "cpu": cpu, "mem": mem})
		return
	}
	state, _ := sysctl("is-active", unitName(id))
	pid := mainPID(id)
	cpu, mem := procStats(pid)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "active": isActive(id), "state": state, "pid": pid, "cpu": cpu, "mem": mem})
}

// undeploy 处理 DELETE /api/apps/{id}:停止并移除 systemd unit(保留制品与备份)。
func (a *agent) undeploy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !requireValidID(w, id) {
		return
	}
	sysctl("stop", unitName(id))
	sysctl("disable", unitName(id))
	os.Remove(unitPath(id))
	sysctl("daemon-reload")
	sysctl("reset-failed", unitName(id))
	pm2("delete", unitName(id)) // 若该应用由 pm2 托管也一并清理(无 pm2/无此进程则忽略)
	// nohup 托管:Console 传 binPath 时停掉进程并清理 pidfile/spec(无监管,不停会留孤儿进程)。
	if bp := strings.TrimSpace(r.URL.Query().Get("binPath")); bp != "" && withinRoots(bp, a.cfg.Paths.DeployRoots) {
		nohupStop(DeployConfig{BinPath: bp})
		os.Remove(nohupSpecPath(bp))
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
