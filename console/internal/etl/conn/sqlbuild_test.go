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

func TestUpdateFromSQL(t *testing.T) {
	pg := mustDialect(t, "postgres")
	tab := TableRef{Schema: "public", Name: "t"}
	// PG/KB/SQLite/DM 形态：UPDATE … SET c=m.c FROM (SELECT … UNION ALL …) m WHERE t.k=m.k
	keys := []DerivedCol{{Name: "k", Cast: "bigint"}}
	set := []DerivedCol{{Name: "a", Cast: "numeric(12,2)"}, {Name: "b"}}
	q := UpdateFromSQL(pg, tab, keys, set, 2)
	want := `UPDATE "public"."t" AS t SET "a" = m."a", "b" = m."b" FROM (` +
		`SELECT CAST($1 AS bigint) AS "k", CAST($2 AS numeric(12,2)) AS "a", $3 AS "b" ` +
		`UNION ALL SELECT $4, $5, $6) m WHERE t."k" = m."k"`
	if q != want {
		t.Errorf("PG 派生行集 UPDATE 不符：\n%s\n期望：\n%s", q, want)
	}
	// MySQL 形态：UPDATE … JOIN (SELECT … UNION ALL …) m ON … SET t.c=m.c
	my := mustDialect(t, "mysql")
	q = UpdateFromSQL(my, TableRef{Schema: "db", Name: "t"},
		[]DerivedCol{{Name: "k"}}, []DerivedCol{{Name: "a"}}, 2)
	want = "UPDATE `db`.`t` t JOIN (SELECT ? AS `k`, ? AS `a` UNION ALL SELECT ?, ?) m " +
		"ON t.`k` = m.`k` SET t.`a` = m.`a`"
	if q != want {
		t.Errorf("MySQL 派生行集 UPDATE 不符：\n%s\n期望：\n%s", q, want)
	}
	// 达梦首行 SELECT 需 FROM DUAL
	dmd := mustDialect(t, "dm")
	q = UpdateFromSQL(dmd, tab, []DerivedCol{{Name: "k"}}, []DerivedCol{{Name: "a"}}, 1)
	if !strings.Contains(q, "SELECT ? AS \"k\", ? AS \"a\" FROM DUAL") {
		t.Errorf("达梦派生行集应含 FROM DUAL：%s", q)
	}
	if !strings.Contains(q, ") m WHERE t.\"k\" = m.\"k\"") {
		t.Errorf("达梦 UPDATE...FROM 形态不符：%s", q)
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

func TestParamCast(t *testing.T) {
	// PG 系统一返回列原生类型（派生行集首行 CAST 目标）
	for _, id := range []string{"postgres", "kingbase"} {
		if got := mustDialect(t, id).ParamCast(core.Field{NativeType: "numeric(12,2)"}); got != "numeric(12,2)" {
			t.Errorf("%s 应 CAST 到列原生类型：%q", id, got)
		}
	}
	// MySQL 按逻辑类型映射 CAST 安全目标
	my := mustDialect(t, "mysql")
	cases := []struct {
		f    core.Field
		want string
	}{
		{core.Field{Kind: core.KindInteger, NativeType: "bigint"}, "SIGNED"},
		{core.Field{Kind: core.KindInteger, NativeType: "bigint unsigned"}, "UNSIGNED"},
		{core.Field{Kind: core.KindDecimal, NativeType: "decimal", Precision: 14, Scale: 2}, "DECIMAL(14,2)"},
		{core.Field{Kind: core.KindDecimal, NativeType: "decimal", Precision: -1}, "DECIMAL(65,30)"},
		{core.Field{Kind: core.KindTimestamp}, "DATETIME"},
		{core.Field{Kind: core.KindString, NativeType: "varchar(64)"}, ""},
		{core.Field{Kind: core.KindNumber}, ""},
	}
	for _, c := range cases {
		if got := my.ParamCast(c.f); got != c.want {
			t.Errorf("mysql ParamCast(%+v) = %q，期望 %q", c.f, got, c.want)
		}
	}
	// DM/SQLite 无需 CAST
	for _, id := range []string{"dm", "sqlite"} {
		if got := mustDialect(t, id).ParamCast(core.Field{Kind: core.KindInteger, NativeType: "int"}); got != "" {
			t.Errorf("%s 无需 CAST：%q", id, got)
		}
	}
}
