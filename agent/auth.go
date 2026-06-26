package main

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// tokenAuth 校验 Authorization: Bearer <token>;内网环境这层够用,如有要求可叠加 mTLS。
// 用 constant-time 比较避免 token 计时侧信道。
//
// 空 token 一律拒绝:ConstantTimeCompare("","")==1,若放行则攻击者发空 Bearer 即可绕过鉴权,
// 形同无认证。配置层(unsafeAgentConfigReason)已在对外监听时拒启空 token,这里是运行期兜底,
// 覆盖「仅本机监听但 token 被清空」等误配——本机回环也不应无认证暴露部署/下线接口。
func (a *agent) tokenAuth(next http.HandlerFunc) http.HandlerFunc {
	want := []byte(a.cfg.Security.Token)
	return func(w http.ResponseWriter, r *http.Request) {
		if len(want) == 0 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Agent token 未配置,拒绝服务"})
			return
		}
		h := r.Header.Get("Authorization")
		got := strings.TrimPrefix(h, "Bearer ")
		if got == h || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "token 校验失败"})
			return
		}
		next(w, r)
	}
}
