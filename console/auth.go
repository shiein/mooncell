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
	store      *Store
	agent      *agentClient            // 配置内的默认 Agent(id="default"/空)
	clients    map[string]*agentClient // 注册的远端 Agent 客户端缓存(按 id)
	clientsMu  sync.Mutex
	cabinetDir string
	anonUpload bool
	demoSeed   bool
	maxUpload  int64 // 部署制品上传硬上限(字节);超出在传输层截断回 413
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

	token, exp := a.store.createSession(username)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  exp,
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

// currentUser 从会话 cookie 取用户名与角色。
func (a *api) currentUser(r *http.Request) (string, string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", "", false
	}
	u, ok := a.store.userByToken(c.Value)
	if !ok {
		return "", "", false
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
