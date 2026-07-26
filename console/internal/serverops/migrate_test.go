package serverops

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestMigrateIdempotent(t *testing.T) {
	db := openTestDB(t)
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaHasNoCredentialColumns(t *testing.T) {
	db := openTestDB(t)
	rows, err := db.Query(`PRAGMA table_info(server_resources)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, strings.ToLower(name))
	}
	for _, forbidden := range []string{"password", "password_cipher", "private_key", "private_key_cipher", "passphrase"} {
		for _, c := range cols {
			if c == forbidden || strings.Contains(c, "password") || strings.Contains(c, "private_key") {
				t.Fatalf("schema must not contain credential column %q (found %q)", forbidden, c)
			}
		}
	}
}

func TestResourceCRUDAndGrantFilter(t *testing.T) {
	db := openTestDB(t)
	now := int64(1_700_000_000_000)
	adminRes := ServerResource{
		ID: "srv_a", Name: "prod-01", Host: "10.0.0.1", Port: 22, Username: "ops",
		CreatedBy: "admin", CreatedAt: now, UpdatedAt: now,
	}
	if err := createResource(db, adminRes); err != nil {
		t.Fatal(err)
	}
	other := ServerResource{
		ID: "srv_b", Name: "prod-02", Host: "10.0.0.2", Port: 22, Username: "ops",
		CreatedBy: "admin", CreatedAt: now + 1, UpdatedAt: now + 1,
	}
	if err := createResource(db, other); err != nil {
		t.Fatal(err)
	}

	// 普通用户无授权 → 空列表
	list, err := VisibleResources(db, "alice", "user")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty, got %d", len(list))
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := SetUserGrantsTx(tx, "alice", []ServerResourceGrant{{ResourceID: "srv_a"}}, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	list, err = VisibleResources(db, "alice", "user")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "srv_a" {
		t.Fatalf("expected only srv_a, got %+v", list)
	}

	// 未授权 ID → RequireAccess 404
	_, err = RequireAccess(db, "alice", "user", "srv_b")
	if err == nil {
		t.Fatal("expected not found")
	}
	if ae, ok := err.(*APIError); !ok || ae.Code != CodeNotFound {
		t.Fatalf("expected NOT_FOUND, got %v", err)
	}

	// admin 可见全部
	all, err := VisibleResources(db, "admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("admin expected 2, got %d", len(all))
	}
}

func TestValidateResourceInput(t *testing.T) {
	cases := []struct {
		in   ResourceInput
		want bool
	}{
		{ResourceInput{Name: "a", Host: "10.0.0.1", Port: 22, Username: "ops"}, true},
		{ResourceInput{Name: "a", Host: "http://evil", Port: 22, Username: "ops"}, false},
		{ResourceInput{Name: "a", Host: "host/path", Port: 22, Username: "ops"}, false},
		{ResourceInput{Name: "a", Host: "ok.example.com", Port: 0, Username: "ops"}, false},
		{ResourceInput{Name: "", Host: "ok", Port: 22, Username: "ops"}, false},
	}
	for _, c := range cases {
		in := c.in
		err := validateResourceInput(&in)
		if c.want && err != nil {
			t.Fatalf("input %+v expected ok, got %v", c.in, err)
		}
		if !c.want && err == nil {
			t.Fatalf("input %+v expected error", c.in)
		}
	}
}

func TestSessionManagerGeneration(t *testing.T) {
	m := newSessionManager()
	s := &Session{
		ID: "ssh_1", ResourceID: "srv_a", Username: "alice",
		ResourceGen: 0, UserGrantGen: 0,
	}
	// 假 client 为空；Close 仍可调用
	m.Register(s)
	if m.Count() != 1 {
		t.Fatal("expected 1 session")
	}
	m.BumpGrant("alice", "srv_a")
	// 关闭后从 map 移除
	if m.Count() != 0 {
		t.Fatalf("expected 0 after bump grant, got %d", m.Count())
	}
}

func TestOverwriteColumnAndTransferInsert(t *testing.T) {
	db := openTestDB(t)
	tr := FileTransfer{
		ID: "tx1", ResourceID: "r1", Username: "u", Direction: DirectionUpload,
		RemotePath: "/tmp/a", RemoteTempPath: "/tmp/.a.part",
		ExpectedSize: 10, TransferredSize: 0, Overwrite: true,
		State: TransferUploading, CreatedAt: 1, UpdatedAt: 1, ExpiresAt: 9999999999999,
	}
	// 资源行非必须；insert 不校验 FK
	if err := insertTransfer(db, tr); err != nil {
		t.Fatal(err)
	}
	got, ok, err := getTransfer(db, "tx1")
	if err != nil || !ok {
		t.Fatalf("get: %v ok=%v", err, ok)
	}
	if !got.Overwrite {
		t.Fatal("overwrite not persisted")
	}
}

func TestSessionIdleReap(t *testing.T) {
	m := newSessionManager()
	s := &Session{
		ID: "ssh_idle", ResourceID: "srv_a", Username: "alice",
		ExpiresAt: time.Now().Add(time.Hour),
		IdleTimeout: time.Second,
	}
	s.lastActivityUnix.Store(time.Now().Add(-2 * time.Second).Unix())
	m.Register(s)
	if n := m.ReapTimedOut(); n != 1 {
		t.Fatalf("reap want 1 got %d", n)
	}
	if m.Count() != 0 {
		t.Fatal("session should be gone")
	}
}
