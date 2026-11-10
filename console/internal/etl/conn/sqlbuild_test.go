package conn

import (
	"strings"
	"testing"

	"mooncell/console/internal/etl/core"
)

func mustDialect(t *testing.T, id string) Dialect {
	t.Helper()
	d, ok := Get(id)
	if !ok {
		t.Fatalf("无方言 %s", id)
	}
	return d
}

func TestUpdateCaseSQL(t *testing.T) {
	pg := mustDialect(t, "postgres")
	tab := TableRef{Schema: "public", Name: "t"}
	// 2 set 列、1 键、2 行：占位符应为 set1(k1,k2,v1,v2)=4 + set2=4 + where(k,k)=2
	q := UpdateCaseSQL(pg, tab, []string{"a", "b"}, []string{"k"}, []string{"", ""}, 2)
	want := `UPDATE "public"."t" SET "a" = CASE WHEN "k" = $1 THEN $2 WHEN "k" = $3 THEN $4 ELSE "a" END, ` +
		`"b" = CASE WHEN "k" = $5 THEN $6 WHEN "k" = $7 THEN $8 ELSE "b" END WHERE ("k" = $9) OR ("k" = $10)`
	if q != want {
		t.Errorf("CASE UPDATE 不符：\n%s\n期望：\n%s", q, want)
	}
	// 复合键 + CAST：每个 WHEN 两个键参数，THEN 带 CAST
	q = UpdateCaseSQL(pg, tab, []string{"a"}, []string{"k1", "k2"}, []string{"numeric"}, 2)
	want = `UPDATE "public"."t" SET "a" = CASE WHEN "k1" = $1 AND "k2" = $2 THEN CAST($3 AS numeric) ` +
		`WHEN "k1" = $4 AND "k2" = $5 THEN CAST($6 AS numeric) ELSE "a" END WHERE ("k1" = $7 AND "k2" = $8) OR ("k1" = $9 AND "k2" = $10)`
	if q != want {
		t.Errorf("复合键 CASE UPDATE 不符：\n%s\n期望：\n%s", q, want)
	}
	// MySQL 占位符为 ?
	my := mustDialect(t, "mysql")
	q = UpdateCaseSQL(my, TableRef{Schema: "db", Name: "t"}, []string{"a"}, []string{"k"}, []string{""}, 2)
	if n := strings.Count(q, "?"); n != 6 {
		t.Errorf("MySQL CASE UPDATE 占位符应为 6（2行×2 + 2行×1），实际 %d：%s", n, q)
	}
}

func TestUpsertSQL(t *testing.T) {
	pg := mustDialect(t, "postgres")
	tab := TableRef{Schema: "s", Name: "t"}
	q, ok := pg.UpsertSQL(tab, []string{"k", "v", "w"}, []string{"k"}, []string{"v", "w"}, 2, true)
	if !ok {
		t.Fatal("postgres 应支持 upsert")
	}
	want := `INSERT INTO "s"."t" ("k", "v", "w") VALUES ($1, $2, $3), ($4, $5, $6) ` +
		`ON CONFLICT ("k") DO UPDATE SET "v" = EXCLUDED."v", "w" = EXCLUDED."w"`
	if q != want {
		t.Errorf("PG upsert 不符：\n%s\n期望：\n%s", q, want)
	}
	// doUpdate=false → DO NOTHING
	q, _ = pg.UpsertSQL(tab, []string{"k", "v"}, []string{"k"}, nil, 1, false)
	if !strings.HasSuffix(q, `ON CONFLICT ("k") DO NOTHING`) {
		t.Errorf("忽略冲突应 DO NOTHING：%s", q)
	}
	// MySQL：VALUES(col) 引用待插值；doUpdate=false → key=key 空更新
	my := mustDialect(t, "mysql")
	q, ok = my.UpsertSQL(tab, []string{"k", "v"}, []string{"k"}, []string{"v"}, 1, true)
	if !ok || !strings.Contains(q, "ON DUPLICATE KEY UPDATE `v` = VALUES(`v`)") {
		t.Errorf("MySQL upsert 不符：%v %s", ok, q)
	}
	q, _ = my.UpsertSQL(tab, []string{"k", "v"}, []string{"k"}, nil, 1, false)
	if !strings.HasSuffix(q, "ON DUPLICATE KEY UPDATE `k` = `k`") {
		t.Errorf("MySQL 忽略冲突应为 key=key：%s", q)
	}
	// 达梦不支持 → ok=false 走 CASE 批量更新
	dmd := mustDialect(t, "dm")
	if _, ok := dmd.UpsertSQL(tab, []string{"k"}, []string{"k"}, nil, 1, true); ok {
		t.Error("达梦不应声明支持 upsert")
	}
}

func TestCaseCast(t *testing.T) {
	kb := mustDialect(t, "kingbase")
	if got := kb.CaseCast(core.Field{NativeType: "numeric(12,2)"}); got != "numeric(12,2)" {
		t.Errorf("kingbase 应 CAST 到列原生类型：%q", got)
	}
	if got := kb.CaseCast(core.Field{NativeType: ""}); got != "" {
		t.Errorf("无原生类型时不 CAST：%q", got)
	}
	for _, id := range []string{"postgres", "mysql", "dm", "sqlite"} {
		if got := mustDialect(t, id).CaseCast(core.Field{NativeType: "int"}); got != "" {
			t.Errorf("%s 无需 CAST：%q", id, got)
		}
	}
}
