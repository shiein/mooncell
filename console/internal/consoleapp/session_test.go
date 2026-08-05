package consoleapp

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSessionExpiry 验证会话语义:
// 1. 闲置过期(expires_at 已过)→ userByToken 失败且清理;
// 2. touchSession 滑动续期把临近过期推回 now+ttl;userByToken 自身不续期(仅校验);
// 3. 绝对最长寿命:created_at 超过 sessionAbsoluteMax → 即便未闲置也失效。
func TestSessionExpiry(t *testing.T) {
	if sessionAbsoluteMax != 3*time.Hour {
		t.Fatalf("绝对生命周期应为 3 小时，实际 %v", sessionAbsoluteMax)
	}
	s := openDB(&Config{
		Database: DatabaseConfig{Path: filepath.Join(t.TempDir(), "s.db")},
		Session:  SessionConfig{TTLHours: 1},
	})
	defer s.Close()

	// 1. 闲置过期。
	tok, _, _ := s.createSession("u")
	s.db.Exec("UPDATE sessions SET expires_at = ? WHERE token = ?", time.Now().Add(-time.Second).UnixMilli(), tok)
	if _, ok := s.userByToken(tok); ok {
		t.Fatal("闲置过期的会话应判失效")
	}

	// 2a. userByToken 不续期:临近过期的会话校验后 expires_at 不变。
	tok2, _, _ := s.createSession("v")
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
	tok3, _, _ := s.createSession("w")
	s.db.Exec("UPDATE sessions SET created_at = ? WHERE token = ?", time.Now().Add(-sessionAbsoluteMax-time.Minute).UnixMilli(), tok3)
	if _, ok := s.userByToken(tok3); ok {
		t.Fatal("超过绝对最长寿命的会话应失效")
	}
}

func TestSessionLeaseIsolation(t *testing.T) {
	s := openDB(&Config{
		Database: DatabaseConfig{Path: filepath.Join(t.TempDir(), "lease.db")},
		Session:  SessionConfig{TTLHours: 1},
	})
	defer s.Close()

	tok1, _, _ := s.createSession("same-user")
	tok2, _, _ := s.createSession("same-user")
	near := time.Now().Add(30 * time.Second).UnixMilli()
	_, _ = s.db.Exec("UPDATE sessions SET expires_at = ? WHERE token IN (?, ?)", near, tok1, tok2)
	s.touchLeaseSession(sessionLeaseID(tok1))

	var exp1, exp2 int64
	_ = s.db.QueryRow("SELECT expires_at FROM sessions WHERE token = ?", tok1).Scan(&exp1)
	_ = s.db.QueryRow("SELECT expires_at FROM sessions WHERE token = ?", tok2).Scan(&exp2)
	if exp1 <= near {
		t.Fatal("当前登录会话应被续期")
	}
	if exp2 != near {
		t.Fatal("同账号的另一个浏览器会话不应被一并续期")
	}
}

func TestBrowserSessionCookieAndExplicitTouch(t *testing.T) {
	s := openDB(&Config{
		Database: DatabaseConfig{Path: filepath.Join(t.TempDir(), "browser.db")},
		Session:  SessionConfig{TTLHours: 1},
	})
	defer s.Close()
	s.seedAdmin("admin", "test-password")
	a := &api{store: s}

	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"admin","password":"test-password"}`))
	rec := httptest.NewRecorder()
	a.login(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected one session cookie, got %d", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != sessionCookie || cookie.MaxAge != 0 || !cookie.Expires.IsZero() {
		t.Fatalf("mc_sid 必须是浏览器 Session Cookie: %+v", cookie)
	}

	near := time.Now().Add(30 * time.Second).UnixMilli()
	_, _ = s.db.Exec("UPDATE sessions SET expires_at = ? WHERE token = ?", near, cookie.Value)
	checkReq := httptest.NewRequest(http.MethodGet, "/api/data", nil)
	checkReq.AddCookie(cookie)
	if _, _, ok := a.currentUser(checkReq); !ok {
		t.Fatal("未过期会话应有效")
	}
	var afterCheck int64
	_ = s.db.QueryRow("SELECT expires_at FROM sessions WHERE token = ?", cookie.Value).Scan(&afterCheck)
	if afterCheck != near {
		t.Fatal("普通 API 校验不得自动续期")
	}

	touchReq := httptest.NewRequest(http.MethodPost, "/api/session/touch", nil)
	touchReq.AddCookie(cookie)
	touchRec := httptest.NewRecorder()
	a.touchLoginSession(touchRec, touchReq)
	var afterTouch int64
	_ = s.db.QueryRow("SELECT expires_at FROM sessions WHERE token = ?", cookie.Value).Scan(&afterTouch)
	if touchRec.Code != http.StatusOK || afterTouch <= near {
		t.Fatalf("真实交互续期失败 status=%d expires=%d", touchRec.Code, afterTouch)
	}
}

func TestSessionLeaseIDMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE sessions (
			token TEXT PRIMARY KEY,
			username TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL
		);
		INSERT INTO sessions (token, username, created_at, expires_at) VALUES ('legacy-token', 'alice', 1, 2);
	`)
	if err != nil {
		t.Fatal(err)
	}
	_ = legacy.Close()

	s := openDB(&Config{Database: DatabaseConfig{Path: path}, Session: SessionConfig{TTLHours: 1}})
	defer s.Close()
	var leaseID string
	if err := s.db.QueryRow("SELECT lease_id FROM sessions WHERE token = ?", "legacy-token").Scan(&leaseID); err != nil {
		t.Fatal(err)
	}
	if leaseID != sessionLeaseID("legacy-token") {
		t.Fatal("旧会话必须回填不可逆 lease_id")
	}
}
