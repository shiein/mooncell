package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
)

// deploy 处理 POST /api/apps/{id}/deploy
// multipart/form-data:字段 config(DeployConfig JSON)+ artifact(制品文件)。
// 同步执行部署流水线,返回逐步结果与日志。
func (a *agent) deploy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseMultipartForm(128 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "表单解析失败"})
		return
	}

	var cfg DeployConfig
	if err := json.Unmarshal([]byte(r.FormValue("config")), &cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "config 解析失败"})
		return
	}
	cfg.ID = id // 以路径为准,避免 body 与路径不一致

	// 安全边界:制品落盘路径必须在白名单根目录内(防穿越)。
	if !withinRoots(cfg.BinPath, a.cfg.Paths.DeployRoots) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "制品路径不在白名单内: " + cfg.BinPath})
		return
	}

	file, _, err := r.FormFile("artifact")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 artifact 制品"})
		return
	}
	defer file.Close()

	tmp, err := os.CreateTemp("", "mc-artifact-*")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "创建暂存失败"})
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := io.Copy(tmp, file); err != nil {
		tmp.Close()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "接收制品失败"})
		return
	}
	tmp.Close()

	res := a.runDeploy(cfg, tmpPath)
	writeJSON(w, http.StatusOK, res)
}

// appStatus 处理 GET /api/apps/{id}/status:返回 systemd 托管状态。
func (a *agent) appStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	state, _ := sysctl("is-active", unitName(id))
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     id,
		"active": isActive(id),
		"state":  state,
		"pid":    mainPID(id),
	})
}

// undeploy 处理 DELETE /api/apps/{id}:停止并移除 systemd unit(保留制品与备份)。
func (a *agent) undeploy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sysctl("stop", unitName(id))
	sysctl("disable", unitName(id))
	os.Remove(unitPath(id))
	sysctl("daemon-reload")
	sysctl("reset-failed", unitName(id))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
