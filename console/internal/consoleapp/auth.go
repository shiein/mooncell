package consoleapp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"mooncell/console/internal/dataresource"
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
	agentBinDir     string // Agent 升级包(按架构)的存储目录
	demoSeed        bool
	maxUpload       int64                     // 部署制品上传硬上限(字节);超出在传输层截断回 413
	uploads         map[string]*uploadSession // 分块上传会话(按 uploadId)
	uploadsMu       sync.Mutex
	busy            map[string]int // 在飞操作的应用(部署/还原/启停/下线)引用计数:健康巡检跳过,避免误判掉线。进程内状态 → Console 须单实例运行(见 README 约束)
	busyMu          sync.Mutex
	appMu           map[string]*sync.Mutex // 按 app id 的实体写锁:串行化"读实体—改字段—写回",防部署/启停/巡检/配置四链路并发丢更新
	appMuMu         sync.Mutex
	appEpoch        map[string]uint64 // 按 app id 的操作代际:每次启停/部署/还原/下线自增(markBusy 内);巡检据此丢弃陈旧回写。busyMu 保护
	draining        bool              // 自更新 draining:置位后 tryBeginOp 拒绝新操作,等在飞清零再 self-exec 重启。busyMu 保护
	requireTLSAgents bool             // 开启后拒绝注册非 loopback 明文 Agent(security.require_tls_agents)
	selfUpdateMu    sync.Mutex // Console 自更新全局串行:固定临时路径 <exe>.new 不能被并发推送互相踩
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

	token, _, err := a.store.createSession(username)
	if err != nil {
		// 常见原因:另一 Console 实例占着 sqlite 锁(双开/僵尸进程),或磁盘满。
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "创建会话失败(数据库忙或不可写)。请确认只运行一个 Console 实例后重试",
		})
		return
	}
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

// isAdmin 判定管理员。历史 operator/viewer 均视为普通用户(按 user_apps 授权)。
func isAdmin(role string) bool { return role == "admin" }

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
// "admin" 精确匹配;"user" 匹配所有非 admin(含历史 operator/viewer)。
func (a *api) requireRole(allowed ...string) func(http.HandlerFunc) http.HandlerFunc {
	allowAdmin, allowUser := false, false
	for _, role := range allowed {
		switch role {
		case "admin":
			allowAdmin = true
		case "user", "operator", "viewer":
			allowUser = true
		}
	}
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_, role, ok := a.currentUser(r)
			if !ok {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录"})
				return
			}
			okRole := (allowAdmin && isAdmin(role)) || (allowUser && !isAdmin(role))
			if !okRole {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "权限不足:需要 " + strings.Join(allowed, "/") + " 角色"})
				return
			}
			next(w, r)
		}
	}
}

// requireAppOp 要求对路径中 {id} 应用可操作:admin 全放行;普通用户须在 user_apps 授权内。
// 用于部署/还原/启停等应用级写操作。
func (a *api) requireAppOp(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, role, ok := a.currentUser(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录"})
			return
		}
		if isAdmin(role) {
			next(w, r)
			return
		}
		id := r.PathValue("id")
		if id == "" || !a.store.userHasApp(user, id) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "无权操作该应用"})
			return
		}
		next(w, r)
	}
}

// canAccessApp 读路径:admin 全放行;普通用户须授权。
func (a *api) canAccessApp(user, role, appID string) bool {
	if isAdmin(role) {
		return true
	}
	return appID != "" && a.store.userHasApp(user, appID)
}

// requireAuthDR 是数据资源模块的认证 wrapper：验证登录后注入用户/角色到 context，
// 交由 dataresource handler 做细粒度权限校验（admin 管理资源、普通用户按授权访问）。
func (a *api) requireAuthDR(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, role, ok := a.currentUser(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录"})
			return
		}
		next(w, dataresource.WithUser(r, user, role))
	}
}
