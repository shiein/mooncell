// 请求上下文：consoleapp 认证中间件将用户信息注入 context，dataresource handlers 从中读取。
package dataresource

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

// WithSession 将用户名、角色和不可作为 Bearer 使用的登录会话标识注入 context。
func WithSession(r *http.Request, username, role, loginSessionID string) *http.Request {
	ctx := context.WithValue(r.Context(), keyUser, username)
	ctx = context.WithValue(ctx, keyRole, role)
	ctx = context.WithValue(ctx, keyLoginSession, loginSessionID)
	return r.WithContext(ctx)
}

// WithUser 保留给单元测试和模块内独立调用；生产请求统一使用 WithSession。
func WithUser(r *http.Request, username, role string) *http.Request {
	return WithSession(r, username, role, "test:"+username)
}

// userFromCtx 从请求 context 中提取用户名和角色。
func userFromCtx(r *http.Request) (string, string, bool) {
	user, ok1 := r.Context().Value(keyUser).(string)
	role, ok2 := r.Context().Value(keyRole).(string)
	if !ok1 || !ok2 {
		return "", "", false
	}
	return user, role, true
}

func loginSessionFromCtx(r *http.Request) (string, bool) {
	id, ok := r.Context().Value(keyLoginSession).(string)
	return id, ok && id != ""
}
