package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

const sessionCookie = "mc_sid"

// api 持有依赖,挂载各 HTTP handler。
type api struct {
	store           *Store
	agent           *agentClient            // 配置内的默认 Agent(id="default"/空)
	clients         map[string]*agentClient // 注册的远端 Agent 客户端缓存(按 id)
	clientsMu       sync.Mutex
	cabinetDir      string
	anonUpload      bool
	cabinetMaxBytes int64  // 文件柜单文件上限(字节),来自 cabinet.max_upload_mb(默认 300MB)
	artifactDir     string // 制品仓库(版本化制品库)的落盘目录
	agentBinDir     string // Agent 升级包(按架构)的存储目录
	demoSeed        bool
	maxUpload       int64                     // 部署制品上传硬上限(字节);超出在传输层截断回 413
	uploads         map[string]*uploadSession // 分块上传会话(按 uploadId)
	uploadsMu       sync.Mutex
	busy            map[string]int // 在飞操作的应用(部署/还原/启停/下线)引用计数:健康巡检跳过,避免误判掉线。进程内状态 → Console 须单实例运行(见 README 约束)
	busyMu          sync.Mutex
}

func randomToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (a *api) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	username := strings.TrimSpace(body.Username)
	if username == "" || body.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "用户名或密码不能为空"})
		return
	}
	if !a.store.verifyUser(username, body.Password) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "用户名或密码错误"})
		return
	}

	token, _ := a.store.createSession(username)
	// 不设 Expires/MaxAge → session cookie:浏览器关闭即清除,重开必须重新登录。
	// 服务端另有闲置超时(滑动续期,见 userByToken),双重保证。
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]string{"user": username, "role": a.store.userRole(username)})
}

func (a *api) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.store.deleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *api) session(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录"})
		return
	}
	username, ok := a.store.userByToken(c.Value)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"user": username, "role": a.store.userRole(username)})
}

var validRoles = map[string]bool{"admin": true, "operator": true, "viewer": true}

// isPassiveRequest 判断是否为后台轮询类请求(系统指标 2.5s / 应用状态 10s)。
// 这类不是"用户动作",不触发会话滑动续期——否则开着页面挂机也永不闲置超时,违背"闲置 1h 退出"。
func isPassiveRequest(r *http.Request) bool {
	p := r.URL.Path
	return p == "/api/agent/system" || strings.HasSuffix(p, "/status")
}

// currentUser 从会话 cookie 取用户名与角色;非轮询请求(=用户动作)顺带滑动续期。
func (a *api) currentUser(r *http.Request) (string, string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", "", false
	}
	u, ok := a.store.userByToken(c.Value)
	if !ok {
		return "", "", false
	}
	if !isPassiveRequest(r) {
		a.store.touchSession(c.Value)
	}
	return u, a.store.userRole(u), true
}

// requireRole 包裹需要特定角色的接口:未登录 401,角色不符 403。
func (a *api) requireRole(allowed ...string) func(http.HandlerFunc) http.HandlerFunc {
	allow := map[string]bool{}
	for _, role := range allowed {
		allow[role] = true
	}
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_, role, ok := a.currentUser(r)
			if !ok {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录"})
				return
			}
			if !allow[role] {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "权限不足:需要 " + strings.Join(allowed, "/") + " 角色"})
				return
			}
			next(w, r)
		}
	}
}
