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
)

// WithUser 将用户名与角色注入请求 context。
func WithUser(r *http.Request, username, role string) *http.Request {
	ctx := context.WithValue(r.Context(), keyUser, username)
	ctx = context.WithValue(ctx, keyRole, role)
	return r.WithContext(ctx)
}

func userFromCtx(r *http.Request) (string, string, bool) {
	user, ok1 := r.Context().Value(keyUser).(string)
	role, ok2 := r.Context().Value(keyRole).(string)
	if !ok1 || !ok2 || user == "" {
		return "", "", false
	}
	return user, role, true
}

func isAdmin(role string) bool { return role == "admin" }
