// Principal 注入/读取：由 consoleapp 认证中间件写入，serverops handler 读取。
// serverops 不得 import consoleapp。
package serverops

import (
	"context"
	"net/http"
)

type ctxKey int

const (
	keyUser ctxKey = iota
	keyRole
	keyLoginSession
)

// WithSession 将用户身份和精确 Mooncell 登录会话标识注入 context。
func WithSession(r *http.Request, username, role, loginSessionID string) *http.Request {
	ctx := context.WithValue(r.Context(), keyUser, username)
	ctx = context.WithValue(ctx, keyRole, role)
	ctx = context.WithValue(ctx, keyLoginSession, loginSessionID)
	return r.WithContext(ctx)
}

// WithUser 保留给模块测试；生产请求统一使用 WithSession。
func WithUser(r *http.Request, username, role string) *http.Request {
	return WithSession(r, username, role, "test:"+username)
}

func userFromCtx(r *http.Request) (string, string, bool) {
	user, ok1 := r.Context().Value(keyUser).(string)
	role, ok2 := r.Context().Value(keyRole).(string)
	if !ok1 || !ok2 || user == "" {
		return "", "", false
	}
	return user, role, true
}

func loginSessionFromCtx(r *http.Request) (string, bool) {
	id, ok := r.Context().Value(keyLoginSession).(string)
	return id, ok && id != ""
}

func (s *Service) sessionFromRequest(r *http.Request, sessionID, username, resourceID string) *Session {
	loginSessionID, ok := loginSessionFromCtx(r)
	if !ok {
		return nil
	}
	return s.sess.GetForLogin(sessionID, username, resourceID, loginSessionID)
}

func (s *Service) touchLoginSessionFromRequest(r *http.Request) {
	loginSessionID, ok := loginSessionFromCtx(r)
	if ok && s.touch != nil {
		s.touch(loginSessionID)
	}
}

func isAdmin(role string) bool { return role == "admin" }
