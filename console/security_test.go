package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// 安全头中间件:所有响应都应带基础安全头与严格 CSP(script 仅同源)。
func TestSecurityHeaders(t *testing.T) {
	h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q,期望 %q", k, got, v)
		}
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("缺少 Content-Security-Policy")
	}
	// 全局 CSP 必须严格:script 仅同源、禁 'unsafe-inline' script;style 放开 inline(React 内联 style 必需)。
	if !strings.Contains(csp, "script-src 'self';") {
		t.Errorf("CSP script-src 应仅同源: %q", csp)
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("全局 CSP 不应放开 inline script: %q", csp)
	}
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP 应禁止被嵌入: %q", csp)
	}
}

// /drop 自包含页:内联 <script> 应被注入 per-response nonce,且 CSP 用 nonce 放行(而非 'unsafe-inline')。
func TestDropPageNonce(t *testing.T) {
	a := &api{}
	rec := httptest.NewRecorder()
	a.dropPage(rec, httptest.NewRequest(http.MethodGet, "/drop", nil))

	body := rec.Body.String()
	m := regexp.MustCompile(`<script nonce="([a-f0-9]+)">`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("drop 页内联 <script> 未注入 nonce")
	}
	nonce := m[1]
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "'nonce-"+nonce+"'") {
		t.Errorf("CSP 未携带与脚本一致的 nonce: csp=%q nonce=%q", csp, nonce)
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("drop 页 CSP 不应放开 inline script(应用 nonce): %q", csp)
	}
	// 两次请求 nonce 必须不同(per-response 随机)。
	rec2 := httptest.NewRecorder()
	a.dropPage(rec2, httptest.NewRequest(http.MethodGet, "/drop", nil))
	m2 := regexp.MustCompile(`<script nonce="([a-f0-9]+)">`).FindStringSubmatch(rec2.Body.String())
	if m2 != nil && m2[1] == nonce {
		t.Error("nonce 未做到 per-response 随机(两次相同)")
	}
}
