package consoleapp

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.db")
	return openDB(&Config{Database: DatabaseConfig{Path: path}, Session: SessionConfig{TTLHours: 1}})
}

// TestDeleteUserLastAdminGuard:原子守卫——两个 admin 可删到只剩一个,最后一个 admin 拒删。
// 这是并发互删清零管理员的兜底:即便预检失效,SQL 内的 admin 计数条件也拦得住。
func TestDeleteUserLastAdminGuard(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	for _, u := range []string{"admin1", "admin2"} {
		if err := s.createUser(u, "pw", "admin"); err != nil {
			t.Fatalf("建 admin %s 失败: %v", u, err)
		}
	}

	// 删第一个 admin:仍剩一个,放行。
	deleted, err := s.deleteUser("admin1")
	if err != nil || !deleted {
		t.Fatalf("删 admin1 应成功,deleted=%v err=%v", deleted, err)
	}
	// 删最后一个 admin:守卫拦下。
	deleted, err = s.deleteUser("admin2")
	if err != nil {
		t.Fatalf("删 admin2 不应报错: %v", err)
	}
	if deleted {
		t.Fatalf("最后一个 admin 不应被删")
	}
	if n := s.countAdmins(); n != 1 {
		t.Fatalf("应仍剩 1 个 admin,got %d", n)
	}
}

// TestDeleteUserNonAdminAlwaysAllowed:非 admin 用户不受末位 admin 守卫影响,正常删除。
func TestDeleteUserNonAdminAlwaysAllowed(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	if err := s.createUser("boss", "pw", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.createUser("op", "pw", "operator"); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.deleteUser("op")
	if err != nil || !deleted {
		t.Fatalf("删非 admin 应成功,deleted=%v err=%v", deleted, err)
	}
}

// TestAppsReferencingAgent:按 agentId 找到引用某 Agent 的应用(名缺失回退 id),
// 供删除 Agent 前的引用拦截。
func TestAppsReferencingAgent(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	put := func(id, body string) {
		if err := s.putEntity("app", id, json.RawMessage(body)); err != nil {
			t.Fatal(err)
		}
	}
	put("a1", `{"id":"a1","name":"App One","agentId":"ag1"}`)
	put("a2", `{"id":"a2","name":"App Two","agentId":"default"}`)
	put("a3", `{"id":"a3","agentId":"ag1"}`) // 名缺失,应回退 id

	refs, err := s.appsReferencingAgent("ag1")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("ag1 应被 2 个应用引用,got %v", refs)
	}
	got := map[string]bool{refs[0]: true, refs[1]: true}
	if !got["App One"] || !got["a3"] {
		t.Fatalf("应含 App One 与回退 id a3,got %v", refs)
	}

	none, err := s.appsReferencingAgent("ag2")
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("ag2 无引用,got %v", none)
	}
}
