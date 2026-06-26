package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// 多 Agent:Console 在配置内置一个默认 Agent(id="default"),并可注册更多远端 Agent(存 DB)。
// 代理类接口按查询参数 ?agent=<id> 路由到目标 Agent;空或 "default" 走配置内置的。

// resolveAgent 按 ?agent=<id> 解析目标 Agent 客户端;空/"default" 用配置内置;未知返回 nil。
func (a *api) resolveAgent(r *http.Request) *agentClient {
	return a.resolveAgentByID(r.URL.Query().Get("agent"))
}

// resolveAgentByID 按 Agent id 解析客户端(供服务端按应用 agentId 路由);空/"default" 用配置内置;未知返回 nil。
func (a *api) resolveAgentByID(id string) *agentClient {
	if id == "" || id == "default" {
		return a.agent
	}
	a.clientsMu.Lock()
	defer a.clientsMu.Unlock()
	if c, ok := a.clients[id]; ok {
		return c
	}
	row, ok := a.store.getAgent(id)
	if !ok {
		return nil
	}
	c := newAgentClient(AgentConfig{Addr: row.Addr, Token: row.Token})
	a.clients[id] = c
	return c
}

// listAgents 处理 GET /api/agents:默认 Agent + 注册的远端 Agent(均不含 token)。
func (a *api) listAgents(w http.ResponseWriter, r *http.Request) {
	out := []AgentRow{{ID: "default", Name: "本机 Agent", Addr: strings.TrimPrefix(a.agent.base, "http://")}}
	rows, err := a.store.listAgents()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取 Agent 失败"})
		return
	}
	out = append(out, rows...)
	writeJSON(w, http.StatusOK, map[string]any{"agents": out})
}

// addAgent 处理 POST /api/agents(admin):注册一个远端 Agent。
func (a *api) addAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name  string `json:"name"`
		Addr  string `json:"addr"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	body.Name, body.Addr = strings.TrimSpace(body.Name), strings.TrimSpace(body.Addr)
	if body.Name == "" || body.Addr == "" || body.Token == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "名称/地址/token 不能为空"})
		return
	}
	if !validAgentAddr(body.Addr) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "地址须为 host:port 形态(如 10.0.0.5:9100),不能带 scheme 或路径"})
		return
	}
	id := fmt.Sprintf("ag%d", time.Now().UnixNano())
	if err := a.store.addAgent(id, body.Name, body.Addr, body.Token); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "注册失败"})
		return
	}
	a.store.appendAudit(a.sessionUser(r), "注册 Agent", body.Name+"("+body.Addr+")", "成功")
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

// deleteAgent 处理 DELETE /api/agents/{id}(admin):移除注册的远端 Agent(default 不可删)。
func (a *api) deleteAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "default" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "内置默认 Agent 不可删除"})
		return
	}
	a.store.deleteAgent(id)
	a.clientsMu.Lock()
	delete(a.clients, id)
	a.clientsMu.Unlock()
	a.store.appendAudit(a.sessionUser(r), "移除 Agent", id, "成功")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// pingAgent 处理 GET /api/agents/{id}/ping:连通性测试(经 Console 转发到目标 Agent /api/ping)。
func (a *api) pingAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var cl *agentClient
	if id == "default" {
		cl = a.agent
	} else if row, ok := a.store.getAgent(id); ok {
		cl = newAgentClient(AgentConfig{Addr: row.Addr, Token: row.Token})
	}
	if cl == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Agent 不存在"})
		return
	}
	status, body, err := cl.get("/api/ping")
	relayAgent(w, status, body, err)
}

// validAgentAddr 校验 Agent 地址须为 host:port 形态(如 10.0.0.5:9100、host.local:9100)。
// 拒绝带 scheme(http://…)、路径(含 /)、端口缺失或非数字等畸形输入——
// newAgentClient 会拼成 "http://"+addr,误填 scheme/路径会拼出畸形 URL。
// admin-only 故 SSRF 风险低,但早拦避免后续路由/连接报错难定位。
func validAgentAddr(addr string) bool {
	if addr == "" || strings.ContainsAny(addr, "/?#") {
		return false
	}
	if strings.Contains(addr, "://") {
		return false // 误带 scheme
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" || port == "" {
		return false
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
