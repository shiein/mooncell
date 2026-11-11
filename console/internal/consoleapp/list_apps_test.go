package consoleapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// GET /api/apps 与 hydrate 同口径：非管理员只见授权应用，任何角色都拿不到 secret 明文。
func TestListAppsACLAndRedaction(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	a := &api{store: s}
	s.putEntity("app", "a1", json.RawMessage(`{"id":"a1","name":"A1","envVars":[{"name":"DB_PASS","value":"s3cr3t","secret":true}]}`))
	s.putEntity("app", "a2", json.RawMessage(`{"id":"a2","name":"A2"}`))
	s.createUser("adm", "pw", "admin")
	s.createUser("u1", "pw", "user")
	if err := s.setUserApps("u1", []string{"a2"}); err != nil {
		t.Fatal(err)
	}
	get := func(user string) []map[string]any {
		tok, _, _ := s.createSession(user)
		req := httptest.NewRequest("GET", "/api/apps", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
		w := httptest.NewRecorder()
		a.listApps(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", user, w.Code)
		}
		var out struct {
			Apps []map[string]any `json:"apps"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Apps
	}
	adm := get("adm")
	if len(adm) != 2 {
		t.Fatalf("管理员应见 2 个应用，得 %d", len(adm))
	}
	raw, _ := json.Marshal(adm)
	if json.Valid(raw) && containsStr(string(raw), "s3cr3t") {
		t.Fatalf("secret 明文泄露: %s", raw)
	}
	u := get("u1")
	if len(u) != 1 || u[0]["id"] != "a2" {
		t.Fatalf("普通用户只应见授权的 a2，得 %v", u)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
