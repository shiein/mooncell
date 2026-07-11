package main

import "testing"

// TestEvictArtifactGuardsPinned:淘汰候选被查出后、删除前若被⭐标星,evictArtifact 必须拦下,
// 不删元数据(调用方据此保留落盘字节)。堵住 SELECT→DELETE 之间的标星 TOCTOU。
func TestEvictArtifactGuardsPinned(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	if err := s.addArtifact(ArtifactRow{ID: "art1", Name: "a", AppID: "app1", Source: "auto"}); err != nil {
		t.Fatal(err)
	}
	// 模拟候选查出后被标星。
	if err := s.setArtifactPinned("art1", true); err != nil {
		t.Fatal(err)
	}
	removed, err := s.evictArtifact("art1")
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatalf("已标星的自动制品不应被淘汰删除")
	}
	if _, ok := s.getArtifact("art1"); !ok {
		t.Fatalf("被守卫拦下的制品元数据应仍在")
	}
}

// TestEvictArtifactRemovesUnpinnedAuto:未标星的自动制品正常淘汰。
func TestEvictArtifactRemovesUnpinnedAuto(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	if err := s.addArtifact(ArtifactRow{ID: "art2", Name: "a", AppID: "app1", Source: "auto"}); err != nil {
		t.Fatal(err)
	}
	removed, err := s.evictArtifact("art2")
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatalf("未标星的自动制品应被淘汰")
	}
	if _, ok := s.getArtifact("art2"); ok {
		t.Fatalf("淘汰后元数据应已删除")
	}
}

// TestEvictArtifactSkipsManual:手动上传(source=manual)的制品不进自动淘汰,evictArtifact 不删它。
func TestEvictArtifactSkipsManual(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	if err := s.addArtifact(ArtifactRow{ID: "art3", Name: "a", AppID: "app1", Source: "manual"}); err != nil {
		t.Fatal(err)
	}
	removed, err := s.evictArtifact("art3")
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatalf("手动制品不应被自动淘汰删除")
	}
}
