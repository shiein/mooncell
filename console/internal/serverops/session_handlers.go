// SSH 会话创建与断开。密码只进入当前 Mooncell 登录会话的内存租约，不落盘、不进日志。
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
	loginSessionID, ok := loginSessionFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "登录会话无效", false)
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
	providedPassword := body.Password != ""
	var pw []byte
	usingLease := false
	if providedPassword {
		pw = []byte(body.Password)
	} else {
		// 优先复用当前登录会话已有的活动 SSH 连接，避免重复占用会话槽。
		if existing := s.sess.FindReusable(user, resourceID, loginSessionID); existing != nil {
			existing.Touch()
			s.touchLoginSessionFromRequest(r)
			writeOK(w, SessionCreateResponse{
				SessionID: existing.ID, ResourceID: resourceID, ExpiresAt: existing.ExpiresAt.UnixMilli(),
			})
			return
		}
		var found bool
		pw, found = s.creds.Get(loginSessionID, resourceID)
		if !found {
			writeErr(w, http.StatusPreconditionRequired, CodePasswordRequired, "请输入 SSH 密码", false)
			return
		}
		usingLease = true
	}
	body.Password = ""
	if len(pw) < 1 || len(pw) > maxPasswordBytes {
		wipeBytes(pw)
		writeErr(w, http.StatusBadRequest, CodeValidation, "密码长度无效", false)
		return
	}
	defer wipeBytes(pw)
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

	// SSH 握手前原子占位；失败路径释放，成功时与注册原子转换。
	if err := s.sess.Reserve(user, s.cfg.MaxSessionsTotal, s.cfg.MaxSessionsPerUser); err != nil {
		writeAPIError(w, err)
		return
	}
	reserved := true
	defer func() {
		if reserved {
			s.sess.ReleaseReservation(user)
		}
	}()

	// 连接前快照代际。
	rGen := s.sess.ResourceGeneration(resourceID)
	gGen := s.sess.GrantGeneration(user, resourceID)
	uGen := s.sess.UserGeneration(user)
	lGen := s.sess.LoginSessionGeneration(loginSessionID)

	client, err := dialSSH(res.Host, res.Port, res.Username, string(pw), res.HostKeyAlgorithm, res.HostKeySHA256, s.cfg.connectTimeout())
	if err != nil {
		if ae, ok := err.(*APIError); ok && ae.Code == CodeSSHAuthFailed {
			if usingLease {
				s.creds.DeleteLoginSessionResource(loginSessionID, resourceID)
				wipeBytes(pw)
				writeErr(w, http.StatusPreconditionRequired, CodePasswordRequired, "已保存的 SSH 凭据失效，请重新输入密码", false)
				s.auditLog(user, "SSH 连接", res.Name, "失败")
				return
			}
			s.recordAuthFail(user, resourceID, clientIP)
		}
		wipeBytes(pw)
		// 不把底层错误（可能含路径）直接返回。
		writeAPIError(w, err)
		s.auditLog(user, "SSH 连接", res.Name, "失败")
		return
	}

	// 握手完成后复核授权与代际（防慢连接竞态）。
	if s.sess.ResourceGeneration(resourceID) != rGen ||
		s.sess.GrantGeneration(user, resourceID) != gGen ||
		s.sess.UserGeneration(user) != uGen ||
		s.sess.LoginSessionGeneration(loginSessionID) != lGen ||
		(s.valid != nil && !s.valid(loginSessionID)) {
		_ = client.Close()
		wipeBytes(pw)
		writeErr(w, http.StatusConflict, CodeResourceChanged, "授权或资源配置已变更，请重试", true)
		return
	}
	// 复核数据库授权。
	if _, err := RequireAccess(s.db, user, role, resourceID); err != nil {
		_ = client.Close()
		wipeBytes(pw)
		writeAPIError(w, err)
		return
	}

	s.clearAuthFail(user, resourceID, clientIP)

	now := time.Now()
	// 绝对过期 = max session hours；idle 由 lastActivity + IdleTimeout 独立判定。
	expires := now.Add(s.cfg.maxSessionDuration())
	ctx, cancel := context.WithCancel(context.Background())
	sess := &Session{
		ID:              newID("ssh"),
		ResourceID:      resourceID,
		Username:        user,
		LoginSessionID:  loginSessionID,
		SSHUser:         res.Username,
		Host:            res.Host,
		Port:            res.Port,
		CreatedAt:       now,
		ExpiresAt:       expires,
		IdleTimeout:     s.cfg.idleTimeout(),
		InitialCols:     body.Cols,
		InitialRows:     body.Rows,
		ResourceGen:     rGen,
		UserGrantGen:    gGen,
		LoginSessionGen: lGen,
		client:          client,
		cancel:          cancel,
		ctx:             ctx,
	}
	sess.lastActivityUnix.Store(now.Unix())
	// 先发布租约再原子注册：若此间 logout/过期/撤权发生，失效路径会清掉租约并使注册失败。
	var storedLeaseVersion uint64
	if providedPassword {
		storedLeaseVersion = s.creds.Store(user, loginSessionID, resourceID, pw)
	}
	if !s.sess.RegisterReserved(sess, uGen) {
		reserved = false // RegisterReserved 已释放占位
		cancel()
		_ = client.Close()
		s.creds.DeleteIfVersion(loginSessionID, resourceID, storedLeaseVersion)
		wipeBytes(pw)
		writeErr(w, http.StatusConflict, CodeResourceChanged, "授权、资源配置或登录会话已变更，请重试", true)
		return
	}
	reserved = false
	wipeBytes(pw)
	s.auditLog(user, "SSH 连接", res.Name, "成功")

	// 清理失败不伪装为连接失败，记录保留供下次重试。
	s.cleanupPendingTransfers(sess)

	// 主动活动：滑动 Mooncell 登录会话。
	s.touchLoginSessionFromRequest(r)

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
	sess := s.sessionFromRequest(r, sessionID, user, resourceID)
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
