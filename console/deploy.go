package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
	Reload     bool              `json:"reload"`    // static/tomcat:部署后是否触发 reload 钩子
	LogPaths   []string          `json:"logPaths"`  // 该应用声明的日志文件路径(文件 tail 授权白名单)
	Env        map[string]string `json:"env"`
}

// reloadActionFor 按应用类型把「是否 reload」表意映射到 Agent 白名单内的固定动作名。
// 前端只能开关 bool,动作名由服务端按类型决定,前端无法注入任意动作;Agent 侧另有白名单二次校验。
func reloadActionFor(typ string, reload bool) string {
	if !reload {
		return ""
	}
	switch typ {
	case "static-nginx":
		return "nginx-reload"
	case "tomcat-war":
		return "tomcat-restart"
	}
	return ""
}

// buildAgentConfig 据已存应用配置 + 本次 version + 制品 sha256 生成下发给 Agent 的部署配置 JSON。
// 返回 (配置 JSON, 目标 agentId)。binPath 取 path 首段(static 的 path 可能含 " → release")。
func buildAgentConfig(raw json.RawMessage, version, expectedSha256, releaseID string) ([]byte, string, error) {
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
		"releaseId":      releaseID,
		"expectedSha256": expectedSha256,
		"backupKeep":     keep,
	}
	// static/tomcat 的部署后 reload 钩子:服务端按类型映射白名单动作名(空则不下发)。
	if rc := reloadActionFor(app.Type, app.Reload); rc != "" {
		cfg["reloadCmd"] = rc
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

// appDeclaresLog 校验请求的日志文件路径是否在该应用已存配置声明的 logPaths 内(精确匹配)。
// 应用不存在或路径不在声明内即拒绝——日志文件 tail 的授权边界,防越权读他应用/任意文件。
func (a *api) appDeclaresLog(id, path string) bool {
	if path == "" {
		return false
	}
	raw, ok := a.store.getEntity("app", id)
	if !ok {
		return false
	}
	var app appConfig
	if json.Unmarshal(raw, &app) != nil {
		return false
	}
	for _, p := range app.LogPaths {
		if p == path {
			return true
		}
	}
	return false
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

// sha256Reader 流式计算 reader 的 sha256(十六进制)。
func sha256Reader(r io.Reader) string {
	h := sha256.New()
	io.Copy(h, r)
	return hex.EncodeToString(h.Sum(nil))
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

	// 幂等:同(部署 + 本 app + releaseId)已成功则直接返回缓存结果,不重复执行。
	if releaseID != "" {
		if res, ok := a.store.getDeploy("deploy", id, releaseID); ok && res == "success" {
			a.sseIdempotent(w, "部署", res, version)
			return
		}
	}

	appRaw, ok := a.store.getEntity("app", id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "应用不存在,无法部署"})
		return
	}
	file, _, err := r.FormFile("artifact")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 artifact 制品"})
		return
	}
	defer file.Close()
	// 服务端权威计算制品 sha256(不信任客户端传值),保证 Console→Agent 完整性;Agent 强校验。
	sha := sha256Reader(file)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "制品读取失败"})
		return
	}

	cfgJSON, agentID, err := buildAgentConfig(appRaw, version, sha, releaseID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "生成部署配置失败"})
		return
	}
	cl := a.resolveAgentByID(agentID)
	if a.unknownAgent(w, cl) {
		return
	}

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
		if res, ok := a.store.getDeploy("restore", id, releaseID); ok && res == "success" {
			a.sseIdempotent(w, "还原", res, req.Version)
			return
		}
	}
	appRaw, ok := a.store.getEntity("app", id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "应用不存在,无法还原"})
		return
	}
	cfgJSON, agentID, err := buildAgentConfig(appRaw, req.Version, "", req.ReleaseID) // 还原用 Agent 本地备份制品,无需上传 sha 校验
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
