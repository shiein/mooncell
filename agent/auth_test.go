package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTokenAuthRejectsEmptyToken 验证空 token 配置时一律 401,杜绝「空 Bearer 绕过鉴权」。
func TestTokenAuthRejectsEmptyToken(t *testing.T) {
	called := false
	h := func(http.ResponseWriter, *http.Request) { called = true }

	a := &agent{cfg: &Config{Security: SecurityConfig{Token: ""}}}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	req.Header.Set("Authorization", "Bearer ")
	a.tokenAuth(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("空 token 配置应拒绝请求,got status %d", rr.Code)
	}
	if called {
		t.Fatalf("空 token 配置不应进入下游 handler")
	}
}

// TestTokenAuthRejectsMissingBearer 验证无 Authorization 头时 401。
func TestTokenAuthRejectsMissingBearer(t *testing.T) {
	called := false
	h := func(http.ResponseWriter, *http.Request) { called = true }

	a := &agent{cfg: &Config{Security: SecurityConfig{Token: "secret"}}}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	a.tokenAuth(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("无 Bearer 头应拒绝,got status %d", rr.Code)
	}
	if called {
		t.Fatalf("无 Bearer 头不应进入下游 handler")
	}
}

// TestTokenAuthAcceptsCorrectToken 验证正确 token 放行。
func TestTokenAuthAcceptsCorrectToken(t *testing.T) {
	called := false
	h := func(http.ResponseWriter, *http.Request) { called = true }

	a := &agent{cfg: &Config{Security: SecurityConfig{Token: "secret"}}}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	req.Header.Set("Authorization", "Bearer secret")
	a.tokenAuth(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("正确 token 应放行,got status %d", rr.Code)
	}
	if !called {
		t.Fatalf("正确 token 应进入下游 handler")
	}
}
