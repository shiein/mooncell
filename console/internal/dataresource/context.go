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
)

// WithUser 将用户名和角色注入请求 context。由 consoleapp 的认证中间件调用。
func WithUser(r *http.Request, username, role string) *http.Request {
	ctx := context.WithValue(r.Context(), keyUser, username)
	ctx = context.WithValue(ctx, keyRole, role)
	return r.WithContext(ctx)
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
