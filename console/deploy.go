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
	"path/filepath"
	"regexp"
	"strings"
)

// nginxContainerRe 与 Agent 端 containerNameRe 保持一致:Docker 容器名/ID 形态校验,守住「reload 参数只追加合法标识符」。
var nginxContainerRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// 部署/还原:前端只提交 制品 + version + releaseId,不再组装 Agent 配置。
// Console 据已保存的类型化应用配置(entity kind=app)在服务端生成 Agent 请求,
// 关闭「前端可注入任意 binPath/reloadCmd 等」的信任面;releaseId 提供幂等。

// appTypeRunners 是各部署类型允许的 Runner(服务端校验用,须与前端 DEPLOY_TYPES 对齐)。
var appTypeRunners = map[string][]string{
	"native-binary": {"systemd", "pm2"},
	"java-jar":      {"systemd", "pm2"},
	"python":        {"systemd", "pm2"},
	"node":          {"pm2", "systemd"},
	"static-nginx":  {"软链", "无进程"},
	"tomcat-war":    {"tomcat"},
}

func strInSlice(s string, ss []string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// validateAppConfig 服务端校验应用配置:类型/Runner/路径形态/数值范围/agentId 可解析。
// 阻止经写入口写入坏配置(绕过配置页预检)。返回错误信息;ok=false 时拒绝落库。
func (a *api) validateAppConfig(raw json.RawMessage) (string, bool) {
	var app appConfig
	if err := json.Unmarshal(raw, &app); err != nil {
		return "配置 JSON 解析失败", false
	}
	if strings.TrimSpace(app.Name) == "" {
		return "应用名不能为空", false
	}
	runners, ok := appTypeRunners[app.Type]
	if !ok {
		return "未知部署类型: " + app.Type, false
	}
	if app.Runner == "" || !strInSlice(app.Runner, runners) {
		return "Runner 不属于该类型: " + app.Runner, false
	}
	if appBinPath(app) == "" {
		return "制品/目标路径不能为空", false
	}
	if app.Port < 0 || app.Port > 65535 {
		return "端口越界(0–65535)", false
	}
	if app.BackupKeep < 0 || app.BackupKeep > 100 {
		return "备份保留份数应为 0–100", false
	}
	if a.resolveAgentByID(app.AgentID) == nil {
		return "目标 Agent 不存在: " + app.AgentID, false
	}
	// static-nginx 的 Docker 容器名(可选):非空则须为合法容器名,与 Agent 端 reload 校验一致(fail-closed)。
	if c := strings.TrimSpace(app.NginxContainer); c != "" && !nginxContainerRe.MatchString(c) {
		return "nginx 容器名非法(仅字母数字与 _ . -,首字符字母数字): " + c, false
	}
	// pm2 接管进程名(可选):只在 pm2 runner 下有意义,且须为合法名(与 Agent 端校验一致,fail-closed)。
	if n := strings.TrimSpace(app.Pm2Name); n != "" {
		if app.Runner != "pm2" {
			return "pm2 接管进程名仅在 Runner=pm2 时可用", false
		}
		if !nginxContainerRe.MatchString(n) {
			return "pm2 进程名非法(仅字母数字与 _ . -,首字符字母数字): " + n, false
		}
	}
	for _, p := range app.LogPaths {
		if strings.TrimSpace(p) == "" {
			return "日志路径含空项", false
		}
	}
	return "", true
}

// putAppConfig 处理 PUT /api/apps/{id}/config:类型化应用配置写入口(校验后落库),
// 取代通用 PUT /api/data/app/{id}——杜绝绕过校验写脏配置。
func (a *api) putAppConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	defer r.Body.Close()
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少应用 id"})
		return
	}
	var m map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&m); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	m["id"] = id // 实体 id 以路径为准,防 body 改 id
	raw, err := json.Marshal(m)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "序列化失败"})
		return
	}
	if msg, ok := a.validateAppConfig(raw); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "配置校验未通过:" + msg})
		return
	}
	if err := a.store.putEntity("app", id, raw); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "写入失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// appConfig 是应用实体里部署相关的字段(前端 addApp 落库的形态)。
type appConfig struct {
	Name           string            `json:"name"`
	Type           string            `json:"type"`
	Runner         string            `json:"runner"`
	Path           string            `json:"path"`
	Workdir        string            `json:"workdir"`
	Health         string            `json:"health"`
	Port           int               `json:"port"` // 应用端口,用于"端口探活"健康检查
	Interp         string            `json:"interp"`
	Jvm            string            `json:"jvm"`
	User           string            `json:"user"`
	AgentID        string            `json:"agentId"`
	BackupKeep     float64           `json:"backupKeep"`
	Reload         bool              `json:"reload"`         // static/tomcat:部署后是否触发 reload 钩子
	NginxContainer string            `json:"nginxContainer"` // static-nginx:nginx 为 Docker 部署时的容器名(空=宿主机 nginx,走 nginx -s reload)
	Pm2Name        string            `json:"pm2Name"`        // pm2 接管模式:填已有 pm2 进程名/ID 则部署只 pm2 restart 它、不写 ecosystem;空=Mooncell 托管
	LogPaths       []string          `json:"logPaths"`       // 该应用声明的日志文件路径(文件 tail 授权白名单)
	Env            map[string]string `json:"env"`
}

// agentDeployConfig 是 Console 下发给 Agent 的配置形态。fingerprint 也从同一结构派生,
// 防止构造请求与本地幂等短路使用两套字段而漂移。
type agentDeployConfig struct {
	Name           string            `json:"name"`
	Type           string            `json:"type"`
	BinPath        string            `json:"binPath"`
	Workdir        string            `json:"workdir"`
	Runner         string            `json:"runner"`
	Interpreter    string            `json:"interpreter"`
	Args           string            `json:"args,omitempty"`
	JvmArgs        string            `json:"jvmArgs,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	User           string            `json:"user"`
	Health         string            `json:"health"`
	Version        string            `json:"version"`
	ReleaseID      string            `json:"releaseId"`
	ExpectedSha256 string            `json:"expectedSha256"`
	BackupKeep     int               `json:"backupKeep"`
	ReloadCmd      string            `json:"reloadCmd,omitempty"`
	ReloadArg      string            `json:"reloadArg,omitempty"` // reload 动作的受校验参数(如 nginx 容器名)
	Pm2Name        string            `json:"pm2Name,omitempty"`   // pm2 接管模式的已有进程名/ID(空=Mooncell 托管,用 deploy-<id>)
}

// reloadActionFor 按应用配置把「是否 reload」表意映射到 Agent 白名单内的固定动作名(+ 可选受校验参数)。
// 前端只能开关 bool / 填容器名,动作名由服务端按类型决定,前端无法注入任意动作;Agent 侧另有白名单 + 容器名二次校验。
// static-nginx 配了容器名 → 走 docker restart <容器名>(nginx 为 Docker 部署);否则宿主机 nginx -s reload。
func reloadActionFor(app appConfig) (cmd, arg string) {
	if !app.Reload {
		return "", ""
	}
	switch app.Type {
	case "static-nginx":
		if c := strings.TrimSpace(app.NginxContainer); c != "" {
			return "nginx-docker-restart", c
		}
		return "nginx-reload", ""
	case "tomcat-war":
		return "tomcat-restart", ""
	}
	return "", ""
}

// appBinPath 取应用落盘路径首段(static 的 path 可能含 " → release" 后缀)。
func appBinPath(app appConfig) string {
	if f := strings.Fields(app.Path); len(f) > 0 {
		return f[0]
	}
	return app.Path
}

// deployFingerprint 必须与 Agent releaseFingerprint 完全一致(字段、JSON key、顺序):
// 运行配置也进入指纹,避免同 releaseId 改 env/args/venv/reload/health 时误短路。
// 用于 Console 层短路前比对——指纹一致才返回缓存,否则放行给 Agent 做最终裁决。
// 部署 fpExtra 为空;还原传 "src=<backup>" 与 Agent 对齐。
func deployFingerprint(app appConfig, sha, version, fpExtra string) string {
	cfg := buildAgentDeployConfig(app, version, sha, "")
	payload := struct {
		Name           string            `json:"name"`
		Type           string            `json:"type"`
		BinPath        string            `json:"binPath"`
		Workdir        string            `json:"workdir"`
		Runner         string            `json:"runner"`
		Interpreter    string            `json:"interpreter"`
		Args           string            `json:"args"`
		JvmArgs        string            `json:"jvmArgs"`
		Env            map[string]string `json:"env,omitempty"`
		User           string            `json:"user"`
		Health         string            `json:"health"`
		Version        string            `json:"version"`
		ExpectedSha256 string            `json:"expectedSha256"`
		BackupKeep     int               `json:"backupKeep"`
		ReloadCmd      string            `json:"reloadCmd"`
		ReloadArg      string            `json:"reloadArg"`
		Pm2Name        string            `json:"pm2Name"`
		Extra          string            `json:"extra"`
	}{
		Name: cfg.Name, Type: cfg.Type, BinPath: cfg.BinPath, Workdir: cfg.Workdir, Runner: cfg.Runner,
		Interpreter: cfg.Interpreter, Args: cfg.Args, JvmArgs: cfg.JvmArgs, Env: cfg.Env, User: cfg.User,
		Health: cfg.Health, Version: cfg.Version, ExpectedSha256: cfg.ExpectedSha256,
		BackupKeep: cfg.BackupKeep, ReloadCmd: cfg.ReloadCmd, ReloadArg: cfg.ReloadArg, Pm2Name: cfg.Pm2Name, Extra: fpExtra,
	}
	b, _ := json.Marshal(payload)
	return "v2:" + string(b)
}

func buildAgentDeployConfig(app appConfig, version, expectedSha256, releaseID string) agentDeployConfig {
	keep := int(app.BackupKeep)
	if keep <= 0 {
		keep = 5
	}
	cfg := agentDeployConfig{
		Name: app.Name, Type: app.Type, Runner: app.Runner, Interpreter: app.Interp,
		BinPath: appBinPath(app), Workdir: app.Workdir, User: app.User,
		Health: healthSpec(app), Version: version, ReleaseID: releaseID,
		ExpectedSha256: expectedSha256, BackupKeep: keep,
	}
	// static/tomcat 的部署后 reload 钩子:服务端按类型映射白名单动作名(+ 可选容器名,空则不下发)。
	if rc, ra := reloadActionFor(app); rc != "" {
		cfg.ReloadCmd = rc
		cfg.ReloadArg = ra
	}
	// pm2 接管模式:仅 pm2 runner 下生效,透传已有进程名(Agent 据此 restart 而非写 ecosystem)。
	if app.Runner == "pm2" {
		cfg.Pm2Name = strings.TrimSpace(app.Pm2Name)
	}
	// jvm 字段按类型映射:java 是 JVM 参数,其余是启动参数。
	if app.Type == "java-jar" {
		cfg.JvmArgs = app.Jvm
	} else {
		cfg.Args = app.Jvm
	}
	if len(app.Env) > 0 {
		cfg.Env = app.Env
	}
	return cfg
}

// buildAgentConfig 据已存应用配置 + 本次 version + 制品 sha256 生成下发给 Agent 的部署配置 JSON。
// 返回 (配置 JSON, 目标 agentId)。binPath 取 path 首段(static 的 path 可能含 " → release")。
func buildAgentConfig(raw json.RawMessage, version, expectedSha256, releaseID string) ([]byte, string, error) {
	var app appConfig
	if err := json.Unmarshal(raw, &app); err != nil {
		return nil, "", err
	}
	cfg := buildAgentDeployConfig(app, version, expectedSha256, releaseID)
	b, err := json.Marshal(cfg)
	return b, app.AgentID, err
}

// appDeclaresLog 校验请求的日志文件路径是否在该应用已存配置声明的 logPaths 内。
//
// 信任边界(务必理解):这是给 **viewer(只读角色)** 的授权闸——viewer 不能改 logPaths
// (那是 admin/operator 的 write 权限),故只能 tail 管理员/operator 声明过的日志,符合"只读看日志"。
// 对 admin/operator 而言这不是提权:他们本就能通过部署任意制品/脚本在目标机执行代码读任意文件,
// 声明 logPath 不构成新能力。真正的穿越/越界由 Agent 端 log_roots 白名单兜底(防路径穿越)。
//
// 匹配收紧为"必须绝对路径 + 规范化比对",fail-closed:相对路径/含 ../ 一律拒绝,规范化后精确相等才放行。
func (a *api) appDeclaresLog(id, path string) bool {
	if path == "" {
		return false
	}
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return false // 拒绝相对路径 / 穿越,只接受绝对路径
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
		if filepath.Clean(p) == clean {
			return true
		}
	}
	return false
}

// appPm2Name 取应用配置里的 pm2 接管进程名(trim);非接管/读不到返回空串。
// 供 status/lifecycle/logs 代理端点向 Agent 透传,使无状态 Agent 能定位用户的已有 pm2 进程。
func (a *api) appPm2Name(id string) string {
	if raw, ok := a.store.getEntity("app", id); ok {
		var app appConfig
		if json.Unmarshal(raw, &app) == nil {
			return strings.TrimSpace(app.Pm2Name)
		}
	}
	return ""
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

// requireAppRouting 与 appRouting 不同:应用必须已落库。运行日志这类只读接口也不能对
// 不存在的 id 回退默认 Agent,否则 viewer 可构造 deploy-<id> 读取任意托管单元日志。
func (a *api) requireAppRouting(w http.ResponseWriter, id string) (*agentClient, string, bool) {
	raw, ok := a.store.getEntity("app", id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "应用不存在"})
		return nil, "", false
	}
	var app appConfig
	if json.Unmarshal(raw, &app) != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "应用配置损坏"})
		return nil, "", false
	}
	return a.resolveAgentByID(app.AgentID), app.Runner, true
}

// sha256Reader 流式计算 reader 的 sha256(十六进制)。
func sha256Reader(r io.Reader) string {
	h := sha256.New()
	io.Copy(h, r)
	return hex.EncodeToString(h.Sum(nil))
}

// healthSpec 决定下发给 Agent 的健康检查规格:
//   - 配了 http(s) URL → HTTP 探活(Agent 端 2xx/3xx 通过);
//   - 否则进程类有端口 → tcp://127.0.0.1:<port> 端口探活(UI 的"端口探活"由此真正落地);
//   - static-nginx 不做 TCP(无监听端口,由 reload + HTTP 判定),其它情况留空(退化为进程存活)。
func healthSpec(app appConfig) string {
	if strings.HasPrefix(app.Health, "http://") || strings.HasPrefix(app.Health, "https://") {
		return app.Health
	}
	if app.Type != "static-nginx" && app.Port > 0 {
		return fmt.Sprintf("tcp://127.0.0.1:%d", app.Port)
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
	// 传输层硬上限:仅 ParseMultipartForm 的内存阈值不足以防 DoS,超大制品会先落临时盘撑爆磁盘。
	r.Body = http.MaxBytesReader(w, r.Body, a.maxUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		if isMaxBytes(err) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("制品超过上限 %d MB", a.maxUpload>>20)})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "表单解析失败"})
		return
	}
	defer cleanupMultipart(r)
	version := r.FormValue("version")
	releaseID := r.FormValue("releaseId")
	uploadID := r.FormValue("uploadId")

	appRaw, ok := a.store.getEntity("app", id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "应用不存在,无法部署"})
		return
	}
	// 制品来源:uploadId(分块上传已收齐的临时文件)优先;否则取本次 multipart 的 artifact 文件。
	var file interface {
		io.Reader
		io.Seeker
		io.Closer
	}
	if uploadID != "" {
		f, ok := a.openUploadArtifact(uploadID)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "上传未完成或会话不存在"})
			return
		}
		defer a.finishUpload(uploadID) // 部署消费后删会话与临时文件
		file = f
	} else {
		f, _, err := r.FormFile("artifact")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 artifact 制品(或 uploadId)"})
			return
		}
		file = f
	}
	defer file.Close()
	// 服务端权威计算制品 sha256(不信任客户端传值),保证 Console→Agent 完整性;Agent 强校验。
	sha := sha256Reader(file)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "制品读取失败"})
		return
	}

	// 幂等:同(部署 + 本 app + releaseId)已成功**且指纹一致**才短路返回缓存。
	// 指纹含制品 sha,故换了制品复用 releaseId 不会被 Console 误短路——放行给 Agent 做最终裁决。
	var app appConfig
	json.Unmarshal(appRaw, &app)
	fp := deployFingerprint(app, sha, version, "")
	if releaseID != "" {
		if res, cfp, ok := a.store.getDeploy("deploy", id, releaseID); ok && res == "success" && cfp == fp {
			a.sseIdempotent(w, "部署", res, version)
			return
		}
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
	a.streamAndAudit(w, r, cl, resp, perr, "部署", id, releaseID, fp)
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
	appRaw, ok := a.store.getEntity("app", id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "应用不存在,无法还原"})
		return
	}
	// 幂等:同(还原 + 本 app + releaseId)已成功**且指纹一致**才短路。
	// 还原指纹含恢复源 src=<backup>,故同 releaseId 用不同备份还原不会被误短路——放行给 Agent 裁决。
	var app appConfig
	json.Unmarshal(appRaw, &app)
	fp := deployFingerprint(app, "", req.Version, "src="+req.Backup)
	if releaseID := req.ReleaseID; releaseID != "" {
		if res, cfp, ok := a.store.getDeploy("restore", id, releaseID); ok && res == "success" && cfp == fp {
			a.sseIdempotent(w, "还原", res, req.Version)
			return
		}
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
	a.streamAndAudit(w, r, cl, resp, perr, "还原", id, req.ReleaseID, fp)
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
