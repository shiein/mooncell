package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

// 审计 hydrate 限窗 + 分页 + 保留裁剪:验证三者口径一致(都按 seq 倒序取最近)。
func TestAuditPaginationAndRetention(t *testing.T) {
	s := testStore(t)
	defer s.Close()

	// 插 12 条审计(a00..a11,seq 递增)+ 2 条非审计实体。
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("a%02d", i)
		b, _ := json.Marshal(map[string]any{"id": id, "time": int64(i), "action": "部署"})
		if err := s.putEntity("audit", id, b); err != nil {
			t.Fatalf("putEntity audit: %v", err)
		}
	}
	s.putEntity("app", "app1", json.RawMessage(`{"id":"app1"}`))
	s.putEntity("release", "r1", json.RawMessage(`{"id":"r1"}`))

	// hydrate 限窗:audit 只回最近 5 条(a07..a11),非 audit 全量。
	grouped, err := s.loadEntities(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(grouped["audit"]) != 5 {
		t.Fatalf("loadEntities(5) audit 应为 5,实际 %d", len(grouped["audit"]))
	}
	if len(grouped["app"]) != 1 || len(grouped["release"]) != 1 {
		t.Fatalf("非 audit 实体应全量保留: app=%d release=%d", len(grouped["app"]), len(grouped["release"]))
	}
	if got := idOf(t, grouped["audit"][0]); got != "a11" {
		t.Errorf("最近一条应为 a11,实际 %s", got)
	}

	// 分页:第一页(offset0 limit5)=最近 5 条,total=12;第二页接续更早。
	items, total, err := s.pageAudit(0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if total != 12 {
		t.Errorf("total 应为 12,实际 %d", total)
	}
	if len(items) != 5 || idOf(t, items[0]) != "a11" || idOf(t, items[4]) != "a07" {
		t.Errorf("第一页范围错: 首=%s 尾=%s", idOf(t, items[0]), idOf(t, items[4]))
	}
	page2, _, _ := s.pageAudit(5, 5)
	if len(page2) != 5 || idOf(t, page2[0]) != "a06" {
		t.Errorf("第二页应从 a06 起,实际 %s", idOf(t, page2[0]))
	}

	// 保留裁剪:只留最近 5 条,删除 7 条;non-audit 不受影响。
	if n := s.trimAudit(5); n != 7 {
		t.Errorf("trimAudit(5) 应删 7,实际 %d", n)
	}
	_, total2, _ := s.pageAudit(0, 100)
	if total2 != 5 {
		t.Errorf("裁剪后应剩 5,实际 %d", total2)
	}
	grouped2, _ := s.loadEntities(500)
	if len(grouped2["app"]) != 1 {
		t.Errorf("裁剪审计不应影响 app 实体")
	}
	// keep<=0 不裁剪。
	if n := s.trimAudit(0); n != 0 {
		t.Errorf("trimAudit(0) 不应删除,实际 %d", n)
	}
}

func idOf(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var m struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("解析审计 id: %v", err)
	}
	return m.ID
}
