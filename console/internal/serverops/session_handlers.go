// SSH 会话创建与断开。密码仅在本请求处理期间使用，不落盘、不进日志。
package serverops

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"time"
)

// CreateSession POST /api/server-resources/{id}/sessions
// 密码错误返回 422 SSH_AUTH_FAILED，绝不返回 401。
func (s *Service) CreateSession(w http.ResponseWriter, r *http.Request) {
	user, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "未登录", false)
		return
	}
	resourceID := r.PathValue("id")
	res, err := RequireAccess(s.db, user, role, resourceID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if res.HostKeySHA256 == "" {
		// host key 未确认：禁止连接（含 admin，须先确认）。
		writeErr(w, http.StatusUnprocessableEntity, CodeHostKeyUnconfirmed, "主机指纹未确认，请管理员先探测并确认", false)
		return
	}

	// 限制请求体，避免超大 body。
	r.Body = http.MaxBytesReader(w, r.Body, maxPasswordReq)
	var body struct {
		Password string `json:"password"`
		Cols     int    `json:"cols"`
		Rows     int    `json:"rows"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "请求格式错误", false)
		return
	}
	// 密码长度：1～1024 字节；解码后立即使用，成功/失败后清空局部引用。
	pw := body.Password
	body.Password = ""
	if len(pw) < 1 || len(pw) > maxPasswordBytes {
		writeErr(w, http.StatusBadRequest, CodeValidation, "密码长度无效", false)
		return
	}
	if body.Cols <= 0 {
		body.Cols = 120
	}
	if body.Rows <= 0 {
		body.Rows = 36
	}

	clientIP := clientIPFromRequest(r)
	if err := s.checkAuthRateLimit(user, resourceID, clientIP); err != nil {
		writeAPIError(w, err)
		return
	}

	// 会话数限额。
	if s.sess.Count() >= s.cfg.MaxSessionsTotal {
		writeErr(w, http.StatusTooManyRequests, CodeSessionLimit, "全局 SSH 会话数已达上限", true)
		return
	}
	if s.sess.CountUser(user) >= s.cfg.MaxSessionsPerUser {
		writeErr(w, http.StatusTooManyRequests, CodeSessionLimit, "您的 SSH 会话数已达上限", true)
		return
	}

	// 连接前快照代际。
	rGen := s.sess.ResourceGeneration(resourceID)
	gGen := s.sess.GrantGeneration(user, resourceID)
	uGen := s.sess.UserGeneration(user)

	client, err := dialSSH(res.Host, res.Port, res.Username, pw, res.HostKeyAlgorithm, res.HostKeySHA256, s.cfg.connectTimeout())
	// 尽快放弃密码引用（Go 不保证内存清零，但不持久化、不记录）。
	pw = ""
	if err != nil {
		if ae, ok := err.(*APIError); ok && ae.Code == CodeSSHAuthFailed {
			s.recordAuthFail(user, resourceID, clientIP)
		}
		// 不把底层错误（可能含路径）直接返回。
		writeAPIError(w, err)
		s.auditLog(user, "SSH 连接", res.Name, "失败")
		return
	}

	// 握手完成后复核授权与代际（防慢连接竞态）。
	if s.sess.ResourceGeneration(resourceID) != rGen ||
		s.sess.GrantGeneration(user, resourceID) != gGen ||
		s.sess.UserGeneration(user) != uGen {
		_ = client.Close()
		writeErr(w, http.StatusConflict, CodeResourceChanged, "授权或资源配置已变更，请重试", true)
		return
	}
	// 复核数据库授权。
	if _, err := RequireAccess(s.db, user, role, resourceID); err != nil {
		_ = client.Close()
		writeAPIError(w, err)
		return
	}

	s.clearAuthFail(user, resourceID, clientIP)

	now := time.Now()
	// 绝对过期 = max session hours；idle 由 lastActivity + IdleTimeout 独立判定。
	expires := now.Add(s.cfg.maxSessionDuration())
	ctx, cancel := context.WithCancel(context.Background())
	sess := &Session{
		ID:           newID("ssh"),
		ResourceID:   resourceID,
		Username:     user,
		SSHUser:      res.Username,
		Host:         res.Host,
		Port:         res.Port,
		CreatedAt:    now,
		ExpiresAt:    expires,
		IdleTimeout:  s.cfg.idleTimeout(),
		ResourceGen:  rGen,
		UserGrantGen: gGen,
		client:       client,
		cancel:       cancel,
		ctx:          ctx,
	}
	sess.lastActivityUnix.Store(now.Unix())
	s.sess.Register(sess)
	s.auditLog(user, "SSH 连接", res.Name, "成功")

	// 主动活动：滑动 Mooncell 登录会话。
	if s.touch != nil {
		s.touch(user)
	}

	writeOK(w, SessionCreateResponse{
		SessionID:  sess.ID,
		ResourceID: resourceID,
		ExpiresAt:  expires.UnixMilli(),
	})
}

// DeleteSession DELETE /api/server-resources/{id}/sessions/{sid}
func (s *Service) DeleteSession(w http.ResponseWriter, r *http.Request) {
	user, _, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "未登录", false)
		return
	}
	resourceID := r.PathValue("id")
	sessionID := r.PathValue("sid")
	sess := s.sess.Get(sessionID, user, resourceID)
	if sess == nil {
		// 幂等：已不存在也返回 ok。
		writeOK(w, map[string]bool{"ok": true})
		return
	}
	name := resourceID
	if res, found, _ := getResource(s.db, resourceID); found {
		name = res.Name
	}
	sess.Close()
	s.auditLog(user, "SSH 断开", name, "成功")
	writeOK(w, map[string]bool{"ok": true})
}

func clientIPFromRequest(r *http.Request) string {
	// 不盲目信任 X-Forwarded-For；取 RemoteAddr。
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
