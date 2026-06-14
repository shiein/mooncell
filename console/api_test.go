package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	cfg := &Config{
		Database: DatabaseConfig{Path: filepath.Join(t.TempDir(), "t.db")},
		Session:  SessionConfig{TTLHours: 1},
	}
	return openDB(cfg)
}

// 审计不可伪造:通用 PUT/DELETE /api/data/{kind} 必须拒绝 kind=audit。
func TestPutDeleteEntityRejectsAudit(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	a := &api{store: s}

	for _, m := range []struct {
		name string
		call func(http.ResponseWriter, *http.Request)
		req  *http.Request
	}{
		{"PUT", a.putEntity, httptest.NewRequest("PUT", "/api/data/audit/x", strings.NewReader(`{"id":"x"}`))},
		{"DELETE", a.deleteEntity, httptest.NewRequest("DELETE", "/api/data/audit/x", nil)},
	} {
		m.req.SetPathValue("kind", "audit")
		m.req.SetPathValue("id", "x")
		w := httptest.NewRecorder()
		m.call(w, m.req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s kind=audit 应 403,得 %d", m.name, w.Code)
		}
	}

	// 非 audit 实体仍可写
	req := httptest.NewRequest("PUT", "/api/data/app/a1", strings.NewReader(`{"id":"a1"}`))
	req.SetPathValue("kind", "app")
	req.SetPathValue("id", "a1")
	w := httptest.NewRecorder()
	a.putEntity(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("kind=app 应 200,得 %d", w.Code)
	}
}

// 服务端审计追加可用,且确实落库。
func TestAppendAudit(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	if err := s.appendAudit("admin", "部署", "x v1", "成功"); err != nil {
		t.Fatalf("appendAudit err: %v", err)
	}
	got, err := s.loadEntities()
	if err != nil || len(got["audit"]) != 1 {
		t.Fatalf("审计应落库 1 条,得 %d (err=%v)", len(got["audit"]), err)
	}
}

// RBAC:operator 不能访问 admin-only;可访问 write 路由;viewer 被 write 路由拦截。
func TestRequireRole(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	s.createUser("op", "pw", "operator")
	s.createUser("vw", "pw", "viewer")
	a := &api{store: s}
	opTok, _ := s.createSession("op")
	vwTok, _ := s.createSession("vw")

	ok := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
	hit := func(h http.HandlerFunc, tok string) int {
		req := httptest.NewRequest("GET", "/x", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
		w := httptest.NewRecorder()
		h(w, req)
		return w.Code
	}

	if c := hit(a.requireRole("admin")(ok), opTok); c != http.StatusForbidden {
		t.Errorf("operator 访问 admin 路由应 403,得 %d", c)
	}
	if c := hit(a.requireRole("admin", "operator")(ok), opTok); c != http.StatusOK {
		t.Errorf("operator 访问 write 路由应 200,得 %d", c)
	}
	if c := hit(a.requireRole("admin", "operator")(ok), vwTok); c != http.StatusForbidden {
		t.Errorf("viewer 访问 write 路由应 403,得 %d", c)
	}
}
