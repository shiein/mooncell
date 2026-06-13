package main

import (
	"context"
	"io"
	"net/http"
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
		status, body, err := a.agent.get(agentPath)
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

// agentDeploy 透传部署请求(multipart:config + artifact)到 Agent /api/apps/{id}/deploy。
func (a *api) agentDeploy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	defer r.Body.Close()
	status, body, err := a.agent.post("/api/apps/"+id+"/deploy", r.Header.Get("Content-Type"), r.Body)
	relayAgent(w, status, body, err)
}

// agentDeployStream 把 Agent 的 SSE 部署流(text/event-stream)边读边 flush 透传给前端。
func (a *api) agentDeployStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	defer r.Body.Close()
	resp, err := a.agent.postStream("/api/apps/"+id+"/deploy/stream", r.Header.Get("Content-Type"), r.Body)
	a.streamAgentResp(w, resp, err)
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
	status, body, err := a.agent.get("/api/apps/" + id + "/backups")
	relayAgent(w, status, body, err)
}

// agentRestoreStream 把 Agent 的 SSE 还原流透传给前端(请求体为 JSON,无制品上传)。
func (a *api) agentRestoreStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	defer r.Body.Close()
	resp, err := a.agent.postStream("/api/apps/"+id+"/restore/stream", r.Header.Get("Content-Type"), r.Body)
	a.streamAgentResp(w, resp, err)
}

// agentLogStream 把 Agent 的应用日志 SSE 流透传给前端;用请求 context 绑定上游,前端断开即级联取消。
func (a *api) agentLogStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := "/api/apps/" + id + "/logs/stream"
	if q := r.URL.RawQuery; q != "" {
		path += "?" + q
	}
	resp, err := a.agent.getStream(r.Context(), path)
	a.streamAgentResp(w, resp, err)
}

func (a *api) agentAppStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	status, body, err := a.agent.get("/api/apps/" + id + "/status")
	relayAgent(w, status, body, err)
}

func (a *api) agentUndeploy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	status, body, err := a.agent.del("/api/apps/" + id)
	relayAgent(w, status, body, err)
}
