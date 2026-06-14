package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
)

// 部署/还原:前端只提交 制品 + version + releaseId,不再组装 Agent 配置。
// Console 据已保存的类型化应用配置(entity kind=app)在服务端生成 Agent 请求,
// 关闭「前端可注入任意 binPath/reloadCmd 等」的信任面;releaseId 提供幂等。

// appConfig 是应用实体里部署相关的字段(前端 addApp 落库的形态)。
type appConfig struct {
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	Runner     string            `json:"runner"`
	Path       string            `json:"path"`
	Workdir    string            `json:"workdir"`
	Health     string            `json:"health"`
	Interp     string            `json:"interp"`
	Jvm        string            `json:"jvm"`
	User       string            `json:"user"`
	AgentID    string            `json:"agentId"`
	BackupKeep float64           `json:"backupKeep"`
	Env        map[string]string `json:"env"`
}

// buildAgentConfig 据已存应用配置 + 本次 version + 制品 sha256 生成下发给 Agent 的部署配置 JSON。
// 返回 (配置 JSON, 目标 agentId)。binPath 取 path 首段(static 的 path 可能含 " → release")。
func buildAgentConfig(raw json.RawMessage, version, expectedSha256 string) ([]byte, string, error) {
	var app appConfig
	if err := json.Unmarshal(raw, &app); err != nil {
		return nil, "", err
	}
	binPath := app.Path
	if f := strings.Fields(app.Path); len(f) > 0 {
		binPath = f[0]
	}
	keep := int(app.BackupKeep)
	if keep <= 0 {
		keep = 5
	}
	cfg := map[string]any{
		"name": app.Name, "type": app.Type, "runner": app.Runner,
		"interpreter": app.Interp,
		"binPath":     binPath, "workdir": app.Workdir, "user": app.User,
		"health":         httpHealthURL(app.Health),
		"version":        version,
		"expectedSha256": expectedSha256,
		"backupKeep":     keep,
	}
	// jvm 字段按类型映射:java 是 JVM 参数,其余是启动参数。
	if app.Type == "java-jar" {
		cfg["jvmArgs"] = app.Jvm
	} else {
		cfg["args"] = app.Jvm
	}
	if len(app.Env) > 0 {
		cfg["env"] = app.Env
	}
	b, err := json.Marshal(cfg)
	return b, app.AgentID, err
}

// appRouting 据已存应用配置(authoritative)解析目标 Agent 客户端与 runner,
// 供日志/状态等接口服务端派生路由与 runner,不再信任前端传的 ?agent / ?runner。
// 应用未落库时退回配置内置 Agent(兼容尚未真机化的演示应用)。
func (a *api) appRouting(id string) (*agentClient, string) {
	raw, ok := a.store.getEntity("app", id)
	if !ok {
		return a.agent, ""
	}
	var app appConfig
	json.Unmarshal(raw, &app)
	return a.resolveAgentByID(app.AgentID), app.Runner
}

// httpHealthURL 仅放行 http(s) 健康检查 URL;其它(如「端口探活 :8080」)视为未配置。
func httpHealthURL(h string) string {
	if strings.HasPrefix(h, "http://") || strings.HasPrefix(h, "https://") {
		return h
	}
	return ""
}

// buildDeployBody 流式构造发给 Agent 的 multipart(config 字段 + artifact 文件),不整文件缓存进内存。
func buildDeployBody(configJSON []byte, file io.Reader) (*io.PipeReader, string) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	ct := mw.FormDataContentType()
	go func() {
		var err error
		defer func() { mw.Close(); pw.CloseWithError(err) }()
		if err = mw.WriteField("config", string(configJSON)); err != nil {
			return
		}
		fw, e := mw.CreateFormFile("artifact", "artifact")
		if e != nil {
			err = e
			return
		}
		_, err = io.Copy(fw, file)
	}()
	return pr, ct
}

// agentDeployStream 服务端部署:读已存应用配置 + 制品 + version/releaseId,生成 Agent 请求并透传 SSE。
func (a *api) agentDeployStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	defer r.Body.Close()
	if err := r.ParseMultipartForm(256 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "表单解析失败"})
		return
	}
	version := r.FormValue("version")
	releaseID := r.FormValue("releaseId")
	sha := r.FormValue("sha256")

	// 幂等:同 releaseId 已成功部署则直接返回缓存结果,不重复执行。
	if releaseID != "" {
		if res, ok := a.store.getDeploy(releaseID); ok && res == "success" {
			a.sseIdempotent(w, "部署", res, version)
			return
		}
	}

	appRaw, ok := a.store.getEntity("app", id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "应用不存在,无法部署"})
		return
	}
	cfgJSON, agentID, err := buildAgentConfig(appRaw, version, sha)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "生成部署配置失败"})
		return
	}
	cl := a.resolveAgentByID(agentID)
	if a.unknownAgent(w, cl) {
		return
	}
	file, _, err := r.FormFile("artifact")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 artifact 制品"})
		return
	}
	defer file.Close()

	body, ct := buildDeployBody(cfgJSON, file)
	resp, perr := cl.postStream("/api/apps/"+id+"/deploy/stream", ct, body)
	a.streamAndAudit(w, r, resp, perr, "部署", id, releaseID)
}

// agentRestoreStream 服务端还原:读已存应用配置生成 Agent 请求(前端只提交 backup + version + releaseId)。
func (a *api) agentRestoreStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	defer r.Body.Close()
	var req struct {
		Backup    string `json:"backup"`
		Version   string `json:"version"`
		ReleaseID string `json:"releaseId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	if releaseID := req.ReleaseID; releaseID != "" {
		if res, ok := a.store.getDeploy(releaseID); ok && res == "success" {
			a.sseIdempotent(w, "还原", res, req.Version)
			return
		}
	}
	appRaw, ok := a.store.getEntity("app", id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "应用不存在,无法还原"})
		return
	}
	cfgJSON, agentID, err := buildAgentConfig(appRaw, req.Version, "") // 还原用 Agent 本地备份制品,无需上传 sha 校验
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "生成还原配置失败"})
		return
	}
	cl := a.resolveAgentByID(agentID)
	if a.unknownAgent(w, cl) {
		return
	}
	agentBody, _ := json.Marshal(map[string]any{"config": json.RawMessage(cfgJSON), "backup": req.Backup})
	resp, perr := cl.postStream("/api/apps/"+id+"/restore/stream", "application/json", bytes.NewReader(agentBody))
	a.streamAndAudit(w, r, resp, perr, "还原", id, req.ReleaseID)
}

// sseIdempotent 对已成功的 releaseId 直接回一个 SSE done(幂等跳过,不重复部署)。
func (a *api) sseIdempotent(w http.ResponseWriter, action, result, version string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"result": result, "version": version, "idempotent": true})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	emit := func(event string, payload any) {
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}
	emit("step", map[string]any{"name": "幂等跳过", "ok": true, "logs": []string{"该 release 已成功" + action + ",跳过重复执行"}})
	emit("done", map[string]any{"result": result, "version": version, "idempotent": true})
}
