package main

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// tokenAuth 校验 Authorization: Bearer <token>;内网环境这层够用,如有要求可叠加 mTLS。
// 用 constant-time 比较避免 token 计时侧信道。
func (a *agent) tokenAuth(next http.HandlerFunc) http.HandlerFunc {
	want := []byte(a.cfg.Security.Token)
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		got := strings.TrimPrefix(h, "Bearer ")
		if got == h || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "token 校验失败"})
			return
		}
		next(w, r)
	}
}
