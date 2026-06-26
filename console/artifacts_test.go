package main

import "testing"

// TestArtifactStoreRoundtrip 覆盖制品仓库 store 的增/查/列表/去重/删。
func TestArtifactStoreRoundtrip(t *testing.T) {
	s := testStore(t)
	defer s.Close()

	row := ArtifactRow{ID: "art1", Name: "app.jar", Version: "v1", Sha256: "abc", Size: 1024, Uploader: "admin", CreatedAt: 1000}
	if err := s.addArtifact(row); err != nil {
		t.Fatalf("addArtifact: %v", err)
	}

	got, ok := s.getArtifact("art1")
	if !ok {
		t.Fatal("getArtifact 应找到 art1")
	}
	if got.Name != "app.jar" || got.Sha256 != "abc" {
		t.Fatalf("getArtifact 字段不符: %+v", got)
	}

	// 按 sha 去重查询。
	if _, ok := s.artifactBySha("abc"); !ok {
		t.Fatal("artifactBySha 应命中")
	}
	if _, ok := s.artifactBySha("zzz"); ok {
		t.Fatal("artifactBySha 不应命中不存在的 sha")
	}

	// 列表(新→旧)。
	s.addArtifact(ArtifactRow{ID: "art2", Name: "b.jar", Version: "v2", Sha256: "def", Size: 512, Uploader: "u", CreatedAt: 2000})
	rows, err := s.listArtifacts()
	if err != nil || len(rows) != 2 {
		t.Fatalf("listArtifacts 应 2 条,实际 %d (err=%v)", len(rows), err)
	}
	if rows[0].ID != "art2" {
		t.Errorf("listArtifacts 应按 created_at 倒序,首条 %s", rows[0].ID)
	}

	// sha256 UNIQUE 兜底:不同 id、同 sha 的并发上传越过 dedup 检查时,DB 层必须挡住重复落条目。
	if err := s.addArtifact(ArtifactRow{ID: "art3", Name: "dup.jar", Sha256: "abc", CreatedAt: 3000}); err == nil {
		t.Error("同 sha256 重复入库应被 UNIQUE 约束拒绝")
	}

	// 删除。
	if err := s.deleteArtifact("art1"); err != nil {
		t.Fatalf("deleteArtifact: %v", err)
	}
	if _, ok := s.getArtifact("art1"); ok {
		t.Fatal("删除后 getArtifact 不应命中")
	}
}
