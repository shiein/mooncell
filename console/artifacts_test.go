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

// TestArtifactRetention 覆盖自动归档的每应用滚动淘汰 + ⭐豁免 + 手动豁免。
func TestArtifactRetention(t *testing.T) {
	s := testStore(t)
	defer s.Close()

	mk := func(id string, ts int64, source, app string) {
		if err := s.addArtifact(ArtifactRow{ID: id, Name: id + ".jar", Sha256: id, Size: 1, Uploader: "u", CreatedAt: ts, AppID: app, Source: source}); err != nil {
			t.Fatalf("addArtifact %s: %v", id, err)
		}
	}
	// app "svc" 4 个自动条目(新→旧:a4>a3>a2>a1)+ 1 个手动 + 另一 app 的自动(不应受影响)。
	mk("a1", 1000, "auto", "svc")
	mk("a2", 2000, "auto", "svc")
	mk("a3", 3000, "auto", "svc")
	mk("a4", 4000, "auto", "svc")
	mk("m1", 2500, "manual", "svc")
	mk("o1", 1500, "auto", "other")

	// keep=2:svc 自动条目超出最近 2 份的应淘汰 → a2、a1。
	ev, err := s.evictableAutoArtifacts("svc", 2)
	if err != nil {
		t.Fatalf("evictableAutoArtifacts: %v", err)
	}
	got := map[string]bool{}
	for _, r := range ev {
		got[r.ID] = true
		if r.Source != "auto" {
			t.Errorf("淘汰候选不应含非 auto: %s", r.ID)
		}
	}
	if len(ev) != 2 || !got["a1"] || !got["a2"] {
		t.Fatalf("keep=2 应淘汰 a1,a2,实际 %v", got)
	}

	// ⭐ a2 后:a2 豁免,只剩 a1 可淘汰。
	if err := s.setArtifactPinned("a2", true); err != nil {
		t.Fatalf("setArtifactPinned: %v", err)
	}
	ev, _ = s.evictableAutoArtifacts("svc", 2)
	if len(ev) != 1 || ev[0].ID != "a1" {
		t.Fatalf("⭐ a2 后 keep=2 应只淘汰 a1,实际 %+v", ev)
	}

	// 手动条目永不在淘汰候选内(keep=0 也不淘汰 manual)。
	ev, _ = s.evictableAutoArtifacts("svc", 0)
	for _, r := range ev {
		if r.ID == "m1" {
			t.Fatal("手动上传条目不应被自动淘汰")
		}
	}
}
