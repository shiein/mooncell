package steps

import (
	"testing"

	"mooncell/console/internal/etl/conn"
)

func iuDialect(t *testing.T, id string) conn.Dialect {
	t.Helper()
	d, ok := conn.Get(id)
	if !ok {
		t.Fatalf("无方言 %s", id)
	}
	return d
}

func TestUpsertSafe(t *testing.T) {
	key := []string{"k"}
	uniqKey := conn.IndexMeta{Name: "ux_k", Unique: true, Columns: []string{"k"}}
	pkKey := conn.IndexMeta{Name: "pk", Primary: true, Columns: []string{"K"}} // 大小写不敏感
	uniqOther := conn.IndexMeta{Name: "ux_e", Unique: true, Columns: []string{"email"}}
	uniqSuperset := conn.IndexMeta{Name: "ux_ka", Unique: true, Columns: []string{"k", "a"}}
	nonuniq := conn.IndexMeta{Name: "ix_a", Columns: []string{"a"}}

	pg, my := iuDialect(t, "postgres"), iuDialect(t, "mysql")

	// 目标键有精确唯一索引 → 双方言都可 upsert
	if !upsertSafe(pg, []conn.IndexMeta{uniqKey}, key) {
		t.Error("pg：精确唯一索引应可 upsert")
	}
	if !upsertSafe(my, []conn.IndexMeta{uniqKey, nonuniq}, key) {
		t.Error("mysql：唯一索引恰为键（另有非唯一索引）应可 upsert")
	}
	// 主键也算
	if !upsertSafe(my, []conn.IndexMeta{pkKey}, key) {
		t.Error("mysql：主键覆盖键（忽略大小写）应可 upsert")
	}
	// PG 系：冲突目标显式指定，键以外的唯一索引无碍
	if !upsertSafe(pg, []conn.IndexMeta{uniqKey, uniqOther}, key) {
		t.Error("pg：存在其他唯一索引仍可 upsert（冲突目标显式）")
	}
	// MySQL：其他唯一索引会触发错误行的更新 → 回退
	if upsertSafe(my, []conn.IndexMeta{uniqKey, uniqOther}, key) {
		t.Error("mysql：存在键以外的唯一索引时应回退 CASE 路径")
	}
	// 无精确匹配（超集）→ 双方都回退
	if upsertSafe(pg, []conn.IndexMeta{uniqSuperset}, key) {
		t.Error("超集唯一索引不能作 ON CONFLICT 仲裁")
	}
	if upsertSafe(my, []conn.IndexMeta{uniqSuperset}, key) {
		t.Error("mysql：超集唯一索引不能保证键重复触发冲突")
	}
	// 无唯一索引 → false
	if upsertSafe(pg, []conn.IndexMeta{nonuniq}, key) {
		t.Error("无唯一索引应回退")
	}
}
