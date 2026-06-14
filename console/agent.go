package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
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
		deploy: &http.Client{Timeout: 180 * time.Second},
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
	resp, err := c.http.Do(req)
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
// 据实际结果与会话操作人服务端写一条权威审计;releaseID 非空时记录部署结果用于幂等。
// 仅用于有限流(部署/还原),日志等无限流不可用此法。
func (a *api) streamAndAudit(w http.ResponseWriter, r *http.Request, resp *http.Response, err error, action, appID, releaseID string) {
	user := a.sessionUser(r)
	if err != nil {
		a.store.appendAudit(user, action, appID, "失败·Agent不可达")
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

	// 部署/还原流体量小(数步),旁路全量缓存以解析 done;边透传边捕获,前端断开即止。
	var capture bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			capture.Write(buf[:n])
			if _, werr := w.Write(buf[:n]); werr != nil {
				break // 前端断开
			}
			flusher.Flush()
		}
		if rerr != nil {
			break
		}
	}

	result, version := parseDoneResult(capture.Bytes())
	target := appID
	if version != "" {
		target += " " + version
	}
	a.store.appendAudit(user, action, target, auditResultText(result))
	if releaseID != "" {
		a.store.putDeploy(releaseID, appID, result) // 幂等:记录该 release 的最终结果
	}
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

// agentListBackups 透传 GET 历史备份列表。
func (a *api) agentListBackups(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cl := a.resolveAgent(r)
	if a.unknownAgent(w, cl) {
		return
	}
	status, body, err := cl.get("/api/apps/" + id + "/backups")
	relayAgent(w, status, body, err)
}

// agentLogStream 把 Agent 的应用日志 SSE 流透传给前端;用请求 context 绑定上游,前端断开即级联取消。
func (a *api) agentLogStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cl := a.resolveAgent(r)
	if a.unknownAgent(w, cl) {
		return
	}
	path := "/api/apps/" + id + "/logs/stream"
	if t := r.URL.Query().Get("tail"); t != "" {
		path += "?tail=" + t
	}
	resp, err := cl.getStream(r.Context(), path)
	a.streamAgentResp(w, resp, err)
}

func (a *api) agentAppStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cl := a.resolveAgent(r)
	if a.unknownAgent(w, cl) {
		return
	}
	status, body, err := cl.get("/api/apps/" + id + "/status")
	relayAgent(w, status, body, err)
}

func (a *api) agentUndeploy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cl := a.resolveAgent(r)
	if a.unknownAgent(w, cl) {
		return
	}
	status, body, err := cl.del("/api/apps/" + id)
	relayAgent(w, status, body, err)
}
