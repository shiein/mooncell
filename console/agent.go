package main

import (
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
}

func newAgentClient(cfg AgentConfig) *agentClient {
	return &agentClient{
		base:   "http://" + cfg.Addr,
		token:  cfg.Token,
		http:   &http.Client{Timeout: 5 * time.Second},
		deploy: &http.Client{Timeout: 180 * time.Second},
	}
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
