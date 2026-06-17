package main

import (
	"path/filepath"
	"testing"
	"time"
)

// TestSessionSlidingExpiry 验证会话闲置超时(idle timeout)的两条语义:
// 1. 闲置过期(expires_at 已过)→ userByToken 失败且清理该行;
// 2. 有动作时滑动续期 → 临近过期的会话被推回 now+ttl。
func TestSessionSlidingExpiry(t *testing.T) {
	s := openDB(&Config{
		Database: DatabaseConfig{Path: filepath.Join(t.TempDir(), "s.db")},
		Session:  SessionConfig{TTLHours: 1},
	})
	defer s.Close()

	// 闲置过期:把过期时间改到过去 → 应判失效并删除。
	tok, _ := s.createSession("u")
	s.db.Exec("UPDATE sessions SET expires_at = ? WHERE token = ?", time.Now().Add(-time.Second).UnixMilli(), tok)
	if _, ok := s.userByToken(tok); ok {
		t.Fatal("闲置过期的会话应判失效")
	}
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE token = ?", tok).Scan(&n)
	if n != 0 {
		t.Fatal("过期会话应被清理")
	}

	// 滑动续期:模拟一个临近过期(剩 30s)的会话,一次有效访问应把过期推回约 now+1h。
	tok2, _ := s.createSession("v")
	s.db.Exec("UPDATE sessions SET expires_at = ? WHERE token = ?", time.Now().Add(30*time.Second).UnixMilli(), tok2)
	if _, ok := s.userByToken(tok2); !ok {
		t.Fatal("未过期会话应有效")
	}
	var exp int64
	s.db.QueryRow("SELECT expires_at FROM sessions WHERE token = ?", tok2).Scan(&exp)
	if exp < time.Now().Add(50*time.Minute).UnixMilli() {
		t.Fatalf("有动作应滑动续期到约 now+1h,got 剩余 %v", time.UnixMilli(exp).Sub(time.Now()))
	}
}
