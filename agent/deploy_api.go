package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// prepareDeploy 解析部署请求公共部分:multipart(config + artifact)+ 安全边界校验 + 制品暂存。
// 成功返回 cfg、暂存路径、清理函数与 ok=true;失败已写好响应,ok=false。
func (a *agent) prepareDeploy(w http.ResponseWriter, r *http.Request) (DeployConfig, string, func(), bool) {
	var zero DeployConfig
	id := r.PathValue("id")
	if err := r.ParseMultipartForm(128 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "表单解析失败"})
		return zero, "", nil, false
	}

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

// appStatus 处理 GET /api/apps/{id}/status?runner=<systemd|pm2>:返回进程托管状态。
func (a *agent) appStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !requireValidID(w, id) {
		return
	}
	if r.URL.Query().Get("runner") == "pm2" {
		online := pm2Online(id)
		pid, _ := pm2("pid", unitName(id))
		state := "stopped"
		if online {
			state = "online"
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "active": online, "state": state, "pid": strings.TrimSpace(pid)})
		return
	}
	state, _ := sysctl("is-active", unitName(id))
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     id,
		"active": isActive(id),
		"state":  state,
		"pid":    mainPID(id),
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
	pm := r.URL.Query().Get("runner") == "pm2"
	var out string
	var err error
	if pm {
		out, err = pm2(action, unitName(id))
	} else {
		out, err = sysctl(action, unitName(id))
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": action + " 失败: " + strings.TrimSpace(out+" "+err.Error())})
		return
	}
	// 返回启停后的真实托管状态(与 appStatus 同形),供前端权威刷新。
	if pm {
		online := pm2Online(id)
		pid, _ := pm2("pid", unitName(id))
		state := "stopped"
		if online {
			state = "online"
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "active": online, "state": state, "pid": strings.TrimSpace(pid)})
		return
	}
	state, _ := sysctl("is-active", unitName(id))
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "active": isActive(id), "state": state, "pid": mainPID(id)})
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
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
