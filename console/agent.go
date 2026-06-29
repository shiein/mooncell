package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// agentClient 是 Console 主动调用 Agent 的瘦客户端;所有请求带共享 token。
// 内网/固定 IP 场景下 Console 单向调 Agent,不需要反向注册。
type agentClient struct {
	base   string // http://<addr>
	token  string
	http   *http.Client // 短超时:ping/system/status
	deploy *http.Client // 长超时:部署(健康检查重试 + 回滚可能耗时数十秒)
	stream *http.Client // 无超时:应用日志等长连接流,靠请求 context 取消而非超时
}

func newAgentClient(cfg AgentConfig) *agentClient {
	return &agentClient{
		base:   "http://" + cfg.Addr,
		token:  cfg.Token,
		http:   &http.Client{Timeout: 5 * time.Second},
		// 部署/下线含 Agent 端健康检查宽限(retries×interval≈30s,探活超时最坏更久)+ 失败回滚再探测,
		// 余量需覆盖最坏约 2×探活;给到 300s 防 Console 侧先于 Agent 超时(Agent 仍在跑造成状态不一致)。
		deploy: &http.Client{Timeout: 300 * time.Second},
		stream: &http.Client{Timeout: 0},
	}
}

// getStream 发起一个无超时的 GET 流式请求,绑定 ctx:前端断开 → ctx 取消 → 上游连接关闭 → Agent 端 journalctl 被杀。
func (c *agentClient) getStream(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	return c.stream.Do(req)
}

func (c *agentClient) get(path string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

// post 透传请求体到 Agent(部署用,multipart 原样转发,Console 不解析制品)。
func (c *agentClient) post(path, contentType string, body io.Reader) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, c.base+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.deploy.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(resp.Body)
	return resp.StatusCode, rb, err
}

// postStream 透传 multipart 到 Agent 并返回未关闭的响应,供调用方边读边 flush 转发(SSE)。
// 调用方负责 resp.Body.Close()。
func (c *agentClient) postStream(path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+c.token)
	return c.deploy.Do(req)
}

func (c *agentClient) del(path string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodDelete, c.base+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.deploy.Do(req) // 下线含 nohup SIGTERM 最多 5s 等待 + 停服,用长超时 client(非 5s 的 http)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(resp.Body)
	return resp.StatusCode, rb, err
}

// requireAuth 包裹需要登录态的接口:校验会话 cookie,未登录返回 401。
func (a *api) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录"})
			return
		}
		if _, ok := a.store.userByToken(c.Value); !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录"})
			return
		}
		if !isPassiveRequest(r) {
			a.store.touchSession(c.Value)
		}
		next(w, r)
	}
}

// agentProxy 把已登录用户的请求转发到 Agent 对应路径,原样回传状态码与 JSON。
// Agent 不可达时返回 502 + online:false,前端据此把 Agent 状态显示为离线。
func (a *api) agentProxy(agentPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cl := a.resolveAgent(r)
		if cl == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "目标 Agent 不存在", "online": false})
			return
		}
		status, body, err := cl.get(agentPath)
		relayAgent(w, status, body, err)
	}
}

// relayAgent 统一回传 Agent 响应;不可达时 502 + online:false。
func relayAgent(w http.ResponseWriter, status int, body []byte, err error) {
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "Agent 不可达", "detail": err.Error(), "online": false,
		})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(body)
}

// sessionUser 从会话 cookie 取已登录用户名(requireAuth 已校验合法性);取不到回 "unknown"。
func (a *api) sessionUser(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if u, ok := a.store.userByToken(c.Value); ok {
			return u
		}
	}
	return "unknown"
}

// unknownAgent 在目标 Agent 解析失败时回 404 并返回 false。
func (a *api) unknownAgent(w http.ResponseWriter, cl *agentClient) bool {
	if cl == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "目标 Agent 不存在", "online": false})
		return true
	}
	return false
}

// streamAndAudit 透传 Agent 的 SSE 流(部署/还原),同时旁路捕获末尾 done 事件,
// 据实际结果与会话操作人服务端写一条权威审计;releaseID 非空时记录结果用于幂等。
// 仅用于有限流(部署/还原),日志等无限流不可用此法。
//
// 关键:结果记账不再绑定在"浏览器→Console"这条瞬时流上。
//  1. 浏览器中途断开时,继续读尽 Agent 流(只停止向浏览器写),确保仍能拿到权威 done;
//  2. 即便 Console↔Agent 也断流没拿到 done,再向 Agent 查询权威幂等记录对账——
//     Agent 真机已完成的部署绝不会被误记为失败,也不会漏记幂等导致重试时重复部署。
// streamAndAudit 透传 Agent 的部署/还原 SSE 给浏览器,并据权威 done 结果落审计/发布/幂等记录。
// 返回最终 (result, version) 供调用方在成功后做旁路动作(如部署成功自动归档制品);早返回回空串。
func (a *api) streamAndAudit(w http.ResponseWriter, r *http.Request, cl *agentClient, resp *http.Response, err error, action, appID, releaseID, fingerprint string) (string, string) {
	user := a.sessionUser(r)
	op := "deploy"
	if action == "还原" {
		op = "restore"
	}
	if err != nil {
		a.store.appendAudit(user, action, appID, "失败·Agent不可达")
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "Agent 不可达", "detail": err.Error(), "online": false})
		return "", ""
	}
	defer resp.Body.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "不支持流式响应"})
		return "", ""
	}
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()

	// 部署/还原流体量小(数步),旁路全量缓存以解析 done。浏览器断开后不再向其写,
	// 但继续读尽 Agent 流——Agent→Console 是 LAN 服务端连接,远比浏览器可靠,
	// 且不绑 r.Context(postStream 未绑),浏览器取消不会掐断它。
	var capture bytes.Buffer
	buf := make([]byte, 4096)
	clientGone := false
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			capture.Write(buf[:n])
			if !clientGone {
				if _, werr := w.Write(buf[:n]); werr != nil {
					clientGone = true // 浏览器断开:停止写,但继续读尽 Agent 流以拿到权威 done
				} else {
					flusher.Flush()
				}
			}
		}
		if rerr != nil {
			break
		}
	}

	result, version := parseDoneResult(capture.Bytes())
	// 对账:未从流里拿到 success(可能 Console↔Agent 断流),查 Agent 权威幂等记录兜底。
	if result != "success" && releaseID != "" {
		if ares, aver, ok := a.fetchReleaseRecord(cl, op, appID, releaseID); ok && ares == "success" {
			result = ares
			if version == "" {
				version = aver
			}
		}
	}

	target := appID
	if version != "" {
		target += " " + version
	}
	a.store.appendAudit(user, action, target, auditResultText(result))
	if result != "" {
		a.store.appendRelease(appID, version, result, user) // 服务端权威发布记录(真实结果,前端不再伪造)
	}
	if releaseID != "" {
		a.store.putDeploy(op, appID, releaseID, result, fingerprint) // 幂等:按 操作+app+release 记录结果与指纹
	}
	if result == "success" || result == "rolledback" || result == "failed" {
		a.applyAppRuntimeState(appID, version, result) // 服务端权威更新应用 version/status/lastDeploy
	}
	return result, version
}

// applyAppRuntimeState 在真机部署/还原结束后,由 Console 服务端权威更新应用实体状态,
// 并清零前端曾伪造的 pid/cpu/mem(运行态由 …/status 轮询补)。前端真实操作只做即时显示、
// 不再 patch 落库,刷新后一律以服务端记录为准。三态:
//
//	success    → 切到新 version,running/static
//	rolledback → 保留旧 version(已回滚),running/static
//	failed     → status=failed,version 不变
func (a *api) applyAppRuntimeState(appID, version, result string) {
	raw, ok := a.store.getEntity("app", appID)
	if !ok {
		return
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	static := false
	if typ, _ := m["type"].(string); typ == "static-nginx" {
		static = true
	}
	switch result {
	case "success":
		if version != "" {
			m["version"] = version
		}
		m["status"] = boolStr(static, "static", "running")
	case "rolledback":
		m["status"] = boolStr(static, "static", "running") // 旧版本仍在跑,version 不动
	case "failed":
		m["status"] = "failed"
	default:
		return
	}
	m["lastDeploy"] = time.Now().UnixMilli()
	m["pid"] = nil
	m["cpu"] = "—"
	m["mem"] = "—"
	m["uptime"] = "—"
	if b, err := json.Marshal(m); err == nil {
		a.store.putEntity("app", appID, b)
	}
}

func boolStr(b bool, t, f string) string {
	if b {
		return t
	}
	return f
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// fetchReleaseRecord 向 Agent 查询某 (op,app,releaseId) 的权威幂等记录(仅成功才记录)。
// 用 cl 自带的短超时客户端(不绑浏览器请求 context),浏览器断开不影响本次对账查询。
func (a *api) fetchReleaseRecord(cl *agentClient, op, appID, releaseID string) (result, version string, ok bool) {
	if cl == nil || releaseID == "" {
		return "", "", false
	}
	status, body, err := cl.get("/api/apps/" + appID + "/release?op=" + op + "&releaseId=" + url.QueryEscape(releaseID))
	if err != nil || status != http.StatusOK {
		return "", "", false
	}
	var d struct {
		Recorded bool   `json:"recorded"`
		Result   string `json:"result"`
		Version  string `json:"version"`
	}
	if json.Unmarshal(body, &d) != nil || !d.Recorded {
		return "", "", false
	}
	return d.Result, d.Version, true
}

// parseDoneResult 从 SSE 字节流中取最后一个 `event: done` 帧的 data,解析出 result 与 version。
func parseDoneResult(b []byte) (result, version string) {
	s := string(b)
	idx := strings.LastIndex(s, "event: done")
	if idx < 0 {
		return "", ""
	}
	rest := s[idx:]
	di := strings.Index(rest, "data:")
	if di < 0 {
		return "", ""
	}
	line := rest[di+len("data:"):]
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}
	var d struct {
		Result  string `json:"result"`
		Version string `json:"version"`
	}
	json.Unmarshal([]byte(strings.TrimSpace(line)), &d)
	return d.Result, d.Version
}

// auditResultText 把流水线结果映射为审计结果文案;无 done(出错/非 SSE)记为失败。
func auditResultText(result string) string {
	switch result {
	case "success":
		return "成功"
	case "rolledback":
		return "失败·已回滚"
	case "failed":
		return "失败"
	default:
		return "失败"
	}
}

// streamAgentResp 把 Agent 的流式响应边读边 flush 透传给前端;Agent 出错(非 SSE)时原样回传 JSON 便于前端读到 error。
// 部署与还原的 SSE 透传共用此逻辑。
func (a *api) streamAgentResp(w http.ResponseWriter, resp *http.Response, err error) {
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "Agent 不可达", "detail": err.Error(), "online": false})
		return
	}
	defer resp.Body.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "不支持流式响应"})
		return
	}
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()

	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // 前端断开
			}
			flusher.Flush()
		}
		if rerr != nil {
			return
		}
	}
}

// agentListBackups 透传历史备份列表(Agent 据已存应用配置服务端派生)。
// static-nginx 的历史版本是 release 软链,改查 /releases(binPath 服务端从应用配置取)。
func (a *api) agentListBackups(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cl, _, ok := a.requireAppRouting(w, id)
	if !ok {
		return
	}
	if a.unknownAgent(w, cl) {
		return
	}
	path := "/api/apps/" + id + "/backups"
	if raw, ok := a.store.getEntity("app", id); ok {
		var app appConfig
		json.Unmarshal(raw, &app)
		if app.Type == "static-nginx" {
			binPath := app.Path
			if f := strings.Fields(app.Path); len(f) > 0 {
				binPath = f[0]
			}
			path = "/api/apps/" + id + "/releases?binPath=" + url.QueryEscape(binPath)
		}
	}
	status, body, err := cl.get(path)
	relayAgent(w, status, body, err)
}

// agentLogStream 把 Agent 的应用日志 SSE 流透传给前端;Agent 与 runner 据已存应用配置服务端派生,
// pm2 应用自动转发 runner=pm2(走 pm2 logs)。用请求 context 绑定上游,前端断开即级联取消。
func (a *api) agentLogStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cl, runner, ok := a.requireAppRouting(w, id)
	if !ok {
		return
	}
	if a.unknownAgent(w, cl) {
		return
	}
	// tail 必须是纯数字:否则前端可注入 &runner=pm2&pm2Name=x 篡改服务端派生的 runner,
	// 越权读取非本应用日志(用 url.Values 构造,杜绝拼接注入)。
	tail := strings.TrimSpace(r.URL.Query().Get("tail"))
	if _, err := strconv.Atoi(tail); err != nil || tail == "" {
		tail = "200"
	}
	q := url.Values{"tail": {tail}}
	a.addRunnerQuery(q, id, runner)
	resp, err := cl.getStream(r.Context(), "/api/apps/"+id+"/logs/stream?"+q.Encode())
	a.streamAgentResp(w, resp, err)
}

// agentLogDownload 按时间范围导出应用日志(gzip),Agent 与 runner 服务端派生;转发 Content-Disposition。
func (a *api) agentLogDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cl, runner, ok := a.requireAppRouting(w, id)
	if !ok {
		return
	}
	if a.unknownAgent(w, cl) {
		return
	}
	q := url.Values{}
	if s := r.URL.Query().Get("since"); s != "" {
		q.Set("since", s)
	}
	if u := r.URL.Query().Get("until"); u != "" {
		q.Set("until", u)
	}
	if runner == "pm2" {
		q.Set("runner", "pm2")
		if n := a.appPm2Name(id); n != "" {
			q.Set("pm2Name", n)
		}
	} else if runner == "nohup" {
		q.Set("runner", "nohup")
		q.Set("binPath", a.appBinPathOf(id))
	}
	resp, err := cl.getStream(r.Context(), "/api/apps/"+id+"/logs/download?"+q.Encode())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "Agent 不可达", "detail": err.Error()})
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		w.Header().Set("Content-Disposition", cd)
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// agentLogFileStream tail 声明的日志文件。除 Agent 端 log_roots 白名单外,Console 先校验
// 请求的 path 必须属于「该应用已声明的 logPaths」——否则已登录用户可越权 tail log_roots 下
// 任意文件(含他应用日志)。Agent 据应用 agentId 服务端路由。
func (a *api) agentLogFileStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cl, _, ok := a.requireAppRouting(w, id)
	if !ok {
		return
	}
	reqPath := r.URL.Query().Get("path")
	if !a.appDeclaresLog(id, reqPath) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "日志路径不属于该应用声明的 logPaths"})
		return
	}
	if a.unknownAgent(w, cl) {
		return
	}
	path := "/api/apps/" + id + "/logs/file/stream?path=" + url.QueryEscape(reqPath)
	if t := r.URL.Query().Get("tail"); t != "" {
		path += "&tail=" + url.QueryEscape(t)
	}
	resp, err := cl.getStream(r.Context(), path)
	a.streamAgentResp(w, resp, err)
}

// agentPrecheck 新建应用前预检(只读),按 ?agent 路由(应用尚未创建,不能据已存配置派生)。
func (a *api) agentPrecheck(w http.ResponseWriter, r *http.Request) {
	cl := a.resolveAgent(r)
	if a.unknownAgent(w, cl) {
		return
	}
	path := "/api/precheck"
	if q := r.URL.RawQuery; q != "" {
		path += "?" + q
	}
	status, body, err := cl.get(path)
	relayAgent(w, status, body, err)
}

func (a *api) agentAppStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cl, runner, ok := a.requireAppRouting(w, id)
	if !ok {
		return
	}
	if a.unknownAgent(w, cl) {
		return
	}
	status, body, err := cl.get(a.appStatusPath(id, runner))
	relayAgent(w, status, body, err)
}

// appStatusPath 构造向 Agent 查应用运行态的路径(按 runner 透传 pm2Name/binPath 以定位无状态 Agent 的进程)。
// 供 status 代理端点与健康巡检共用。
func (a *api) appStatusPath(id, runner string) string {
	path := "/api/apps/" + id + "/status"
	switch runner {
	case "pm2":
		path += "?runner=pm2"
		if n := a.appPm2Name(id); n != "" {
			path += "&pm2Name=" + url.QueryEscape(n)
		}
	case "nohup":
		path += "?runner=nohup&binPath=" + url.QueryEscape(a.appBinPathOf(id))
	}
	return path
}

// agentLifecycle 服务端启停:按已存应用配置路由 Agent + runner,真机 start/stop 并落审计。
// action 由前端传(start|stop),runner 服务端权威派生,不信任前端。
func (a *api) agentLifecycle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	action := r.URL.Query().Get("action")
	if action != "start" && action != "stop" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "action 仅支持 start|stop"})
		return
	}
	a.markBusy(id)
	defer a.unmarkBusy(id)
	cl, runner, ok := a.requireAppRouting(w, id)
	if !ok {
		return
	}
	if a.unknownAgent(w, cl) {
		return
	}
	path := "/api/apps/" + id + "/lifecycle?action=" + action
	if runner == "pm2" {
		path += "&runner=pm2"
		if n := a.appPm2Name(id); n != "" {
			path += "&pm2Name=" + url.QueryEscape(n)
		}
	} else if runner == "nohup" {
		path += "&runner=nohup&binPath=" + url.QueryEscape(a.appBinPathOf(id))
	}
	status, body, err := cl.post(path, "application/json", nil)
	user := a.sessionUser(r)
	verb := map[string]string{"start": "启动服务", "stop": "停止服务"}[action]
	if err != nil || status >= 400 {
		a.store.appendAudit(user, verb, id, "失败")
	} else {
		a.store.appendAudit(user, verb, id, "成功")
		a.applyLifecycleState(id, body) // 服务端权威更新 status/pid(据 Agent 返回的真实状态)
	}
	relayAgent(w, status, body, err)
}

// applyLifecycleState 据 Agent 启停后返回的真实状态({active,pid})服务端权威更新应用实体的
// status/pid——前端启停只做即时显示,不再 patch 落库。
func (a *api) applyLifecycleState(appID string, body []byte) {
	var st struct {
		Active bool   `json:"active"`
		Pid    string `json:"pid"`
		Cpu    string `json:"cpu"`
		Mem    string `json:"mem"`
	}
	if json.Unmarshal(body, &st) != nil {
		return
	}
	raw, ok := a.store.getEntity("app", appID)
	if !ok {
		return
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	if st.Active {
		m["status"] = "running"
		if st.Pid != "" && st.Pid != "0" {
			m["pid"] = st.Pid
		}
	} else {
		m["status"] = "stopped"
		m["pid"] = nil
	}
	m["cpu"] = orDash(st.Cpu)
	m["mem"] = orDash(st.Mem)
	if b, err := json.Marshal(m); err == nil {
		a.store.putEntity("app", appID, b)
	}
}

// addRunnerQuery 据服务端派生的 runner 往 query 注入定位参数:pm2→runner+pm2Name、nohup→runner+binPath、
// 软链(static-nginx)→binPath。集中一处,避免某条链路(此前的下线)漏传 pm2Name/binPath 导致残留。
func (a *api) addRunnerQuery(q url.Values, id, runner string) {
	switch runner {
	case "pm2":
		q.Set("runner", "pm2")
		if n := a.appPm2Name(id); n != "" {
			q.Set("pm2Name", n)
		}
	case "nohup":
		q.Set("runner", "nohup")
		q.Set("binPath", a.appBinPathOf(id))
	case "软链":
		q.Set("binPath", a.appBinPathOf(id)) // Agent 据此删除对外软链下线 web root
	}
}

// undeployPath 构造 Agent 下线路径(带 runner 定位参数)。
func (a *api) undeployPath(id, runner string) string {
	q := url.Values{}
	a.addRunnerQuery(q, id, runner)
	if e := q.Encode(); e != "" {
		return "/api/apps/" + id + "?" + e
	}
	return "/api/apps/" + id
}

func (a *api) agentUndeploy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.markBusy(id)
	defer a.unmarkBusy(id)
	// 必须是已落库应用:防止 write 用户对 Console 未跟踪的任意 deploy-<id> 单元执行下线。
	cl, runner, ok := a.requireAppRouting(w, id)
	if !ok {
		return
	}
	if a.unknownAgent(w, cl) {
		return
	}
	status, body, err := cl.del(a.undeployPath(id, runner))
	relayAgent(w, status, body, err)
}

// appDelete 处理 DELETE /api/apps/{id}:服务端权威删除——先经 Agent 下线(停服 + 清理 unit/pm2/nohup),
// 成功后才删 Console 元数据并审计。前端不能走通用 /api/data 删除(那里禁止删 app,只会"前端假删、刷新复现")。
func (a *api) appDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.markBusy(id)
	defer a.unmarkBusy(id)
	cl, runner, ok := a.requireAppRouting(w, id)
	if !ok {
		return
	}
	if a.unknownAgent(w, cl) {
		return
	}
	// 1. 下线目标机服务;失败则中止、不删元数据(否则留孤儿服务且丢失管理入口)。
	if status, body, err := cl.del(a.undeployPath(id, runner)); err != nil || status >= 300 {
		a.store.appendAudit(a.sessionUser(r), "删除应用", id, "下线失败")
		relayAgent(w, status, body, err)
		return
	}
	// 2. 删 Console 元数据。
	if err := a.store.deleteEntity("app", id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "已下线但删除元数据失败: " + err.Error()})
		return
	}
	a.store.appendAudit(a.sessionUser(r), "删除应用", id, "成功")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
