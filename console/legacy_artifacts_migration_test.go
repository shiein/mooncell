package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveLegacyArtifacts(t *testing.T) {
	s := testStore(t)
	defer s.Close()

	dir := t.TempDir()
	if _, err := s.db.Exec(`CREATE TABLE artifacts (id TEXT PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO artifacts (id, name) VALUES ('art1001', 'app.jar')`); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"art1001":      "tracked",
		"art2002":      "orphan",
		"art3003.part": "partial",
		"keep.txt":     "unrelated",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := s.removeLegacyArtifacts(dir)
	if err != nil {
		t.Fatalf("removeLegacyArtifacts: %v", err)
	}
	if removed != 3 {
		t.Fatalf("应删除 3 个受控命名制品文件，got %d", removed)
	}
	var tables int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='artifacts'").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatal("旧 artifacts 表应已删除")
	}
	if _, err := os.Stat(filepath.Join(dir, "keep.txt")); err != nil {
		t.Fatalf("非制品文件必须保留: %v", err)
	}

	// 幂等：表已删、制品文件已清后重复执行不报错也不重复计数。
	removed, err = s.removeLegacyArtifacts(dir)
	if err != nil || removed != 0 {
		t.Fatalf("重复迁移应为空操作，removed=%d err=%v", removed, err)
	}
}
