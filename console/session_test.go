package main

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// TestSessionExpiry 验证会话语义:
// 1. 闲置过期(expires_at 已过)→ userByToken 失败且清理;
// 2. touchSession 滑动续期把临近过期推回 now+ttl;userByToken 自身不续期(仅校验);
// 3. 绝对最长寿命:created_at 超过 sessionAbsoluteMax → 即便未闲置也失效。
func TestSessionExpiry(t *testing.T) {
	s := openDB(&Config{
		Database: DatabaseConfig{Path: filepath.Join(t.TempDir(), "s.db")},
		Session:  SessionConfig{TTLHours: 1},
	})
	defer s.Close()

	// 1. 闲置过期。
	tok, _ := s.createSession("u")
	s.db.Exec("UPDATE sessions SET expires_at = ? WHERE token = ?", time.Now().Add(-time.Second).UnixMilli(), tok)
	if _, ok := s.userByToken(tok); ok {
		t.Fatal("闲置过期的会话应判失效")
	}

	// 2a. userByToken 不续期:临近过期的会话校验后 expires_at 不变。
	tok2, _ := s.createSession("v")
	near := time.Now().Add(30 * time.Second).UnixMilli()
	s.db.Exec("UPDATE sessions SET expires_at = ? WHERE token = ?", near, tok2)
	if _, ok := s.userByToken(tok2); !ok {
		t.Fatal("未过期会话应有效")
	}
	var exp int64
	s.db.QueryRow("SELECT expires_at FROM sessions WHERE token = ?", tok2).Scan(&exp)
	if exp != near {
		t.Fatalf("userByToken 不应续期, expires_at 变了: %d → %d", near, exp)
	}
	// 2b. touchSession 续期到约 now+1h。
	s.touchSession(tok2)
	s.db.QueryRow("SELECT expires_at FROM sessions WHERE token = ?", tok2).Scan(&exp)
	if exp < time.Now().Add(50*time.Minute).UnixMilli() {
		t.Fatalf("touchSession 应续期到约 now+1h, 剩余 %v", time.UnixMilli(exp).Sub(time.Now()))
	}

	// 3. 绝对最长寿命:created_at 推到 max 之前 → 即便 expires_at 未到也失效。
	tok3, _ := s.createSession("w")
	s.db.Exec("UPDATE sessions SET created_at = ? WHERE token = ?", time.Now().Add(-sessionAbsoluteMax-time.Minute).UnixMilli(), tok3)
	if _, ok := s.userByToken(tok3); ok {
		t.Fatal("超过绝对最长寿命的会话应失效")
	}
}

// TestPassiveRequestNoRenew 验证后台轮询路径被判为 passive(不续期),用户动作路径不是。
func TestPassiveRequestNoRenew(t *testing.T) {
	cases := []struct {
		path    string
		passive bool
	}{
		{"/api/agent/system", true},
		{"/api/agent/apps/x/status", true},
		{"/api/agent/apps/x/logs/stream", false},
		{"/api/apps/x/config", false},
		{"/api/data/app/x", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", c.path, nil)
		if got := isPassiveRequest(r); got != c.passive {
			t.Errorf("isPassiveRequest(%q) = %v, want %v", c.path, got, c.passive)
		}
	}
}
