//go:build etl_live

// R1-B2 差分测试：对下推白名单的每种结构与验收六个反例，在 PG/MySQL/达梦/Kingbase
// 上分别跑「不下推」与「下推」，断言写入的行集合完全一致。
// 字符串语义的反例必须断言「不下推」；grp != 1（整数 != 含 NULL 行）按验收要求
// 以 NULL 保留形式 (a <> ? OR a IS NULL) 下推，断言改写后带 IS NULL 且行集一致。
package steps_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/livetest"
	"mooncell/console/internal/etl/pipeline"

	"github.com/shopspring/decimal"
)

func TestLivePushdownDifferential(t *testing.T) {
	for _, d := range []string{"postgres", "mysql", "dm", "kingbase"} {
		d := d
		t.Run(d, func(t *testing.T) { livePushdownDiff(t, d) })
	}
}

var pdDDL = map[string]string{
	"postgres": `CREATE TABLE %s (id INT, grp INT, name VARCHAR(64), price NUMERIC(10,2), d1 DATE, d2 DATE, t1 TIMESTAMP, t2 TIMESTAMP)`,
	"kingbase": `CREATE TABLE %s (id INT, grp INT, name VARCHAR(64), price NUMERIC(10,2), d1 DATE, d2 DATE, t1 TIMESTAMP, t2 TIMESTAMP)`,
	"mysql":    `CREATE TABLE %s (id INT, grp INT, name VARCHAR(64), price DECIMAL(10,2), d1 DATE, d2 DATE, t1 DATETIME, t2 DATETIME)`,
	"dm":       `CREATE TABLE %s (id INT, grp INT, name VARCHAR(64), price NUMBER(10,2), d1 DATE, d2 DATE, t1 TIMESTAMP, t2 TIMESTAMP)`,
}

func livePushdownDiff(t *testing.T, dialect string) {
	ctx := context.Background()
	e := livetest.Env(t, dialect)
	h := livetest.Open(t, dialect)
	d := h.Dialect
	srcRef := conn.TableRef{Schema: d.FoldIdent(conn.EffectiveSchema(h.Config, "")), Name: d.FoldIdent("etl_pd_src")}
	dstRef := conn.TableRef{Schema: d.FoldIdent(conn.EffectiveSchema(h.Config, "")), Name: d.FoldIdent("etl_pd_dst")}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := h.DB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, r := range []conn.TableRef{srcRef, dstRef} {
		_, _ = h.DB.ExecContext(ctx, "DROP TABLE IF EXISTS "+d.Qualified(r))
	}
	exec(fmt.Sprintf(pdDDL[dialect], d.Qualified(srcRef)))
	exec(fmt.Sprintf("CREATE TABLE %s (id INT)", d.Qualified(dstRef)))
	t.Cleanup(func() {
		_, _ = h.DB.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+d.Qualified(srcRef))
		_, _ = h.DB.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+d.Qualified(dstRef))
	})

	// 元数据 → 逻辑字段（Bind 需要）
	cols, err := d.Columns(ctx, h.DB, srcRef)
	if err != nil {
		t.Fatalf("读列元数据失败：%v", err)
	}
	fieldOf := map[string]core.Field{}
	colOrder := []string{"id", "grp", "name", "price", "d1", "d2", "t1", "t2"}
	colNames := make([]string, len(colOrder))
	for i, want := range colOrder {
		for _, c := range cols {
			if strings.EqualFold(c.Name, want) {
				fieldOf[want] = d.FieldFor(c)
				colNames[i] = c.Name
			}
		}
		if colNames[i] == "" {
			t.Fatalf("列 %s 不存在", want)
		}
	}
	day := func(m, dd int) time.Time { return time.Date(2026, time.Month(m), dd, 0, 0, 0, 0, time.UTC) }
	ts := func(hh int) time.Time { return time.Date(2026, 1, 1, hh, 0, 0, 0, time.UTC) }
	dec := func(s string) decimal.Decimal { return decimal.RequireFromString(s) }
	rows := [][]any{
		{int64(1), int64(1), "abc", dec("1.00"), day(1, 1), day(1, 1), ts(10), ts(11)},
		{int64(2), int64(1), "ABC", dec("2.50"), day(1, 2), day(1, 2), ts(10), ts(12)},
		{int64(3), int64(2), "数据集成", dec("3.75"), day(2, 1), day(1, 15), ts(11), ts(11)},
		{int64(4), nil, `a\b`, dec("4.00"), day(3, 1), day(2, 1), ts(13), ts(12)},
		{int64(5), nil, "\tx", dec("5.00"), day(4, 1), day(3, 1), ts(14), ts(13)},
		{int64(6), int64(3), "zzz", dec("6.00"), day(5, 1), day(4, 1), ts(15), ts(14)},
	}
	for _, row := range rows {
		args := make([]any, len(row))
		for i, v := range row {
			b, err := d.Bind(v, fieldOf[colOrder[i]])
			if err != nil {
				t.Fatalf("绑定 %s=%v 失败：%v", colOrder[i], v, err)
			}
			args[i] = b
		}
		exec(conn.InsertSQL(d, srcRef, colNames, 1), args...)
	}

	mk := func(cond string) *pipeline.Definition {
		marshal := func(m map[string]any) json.RawMessage { b, _ := json.Marshal(m); return b }
		return &pipeline.Definition{
			Nodes: []pipeline.Node{
				{ID: "ti", Type: "table_input", Name: "输入", Config: marshal(map[string]any{
					"connection": livetest.ConnID(dialect),
					"sql":        fmt.Sprintf("SELECT id, grp, name, price, d1, d2, t1, t2 FROM %s", d.Qualified(srcRef)),
				})},
				{ID: "flt", Type: "filter_rows", Name: "过滤", Config: marshal(map[string]any{"condition": cond})},
				{ID: "out", Type: "table_output", Name: "写出", Config: marshal(map[string]any{
					"connection": livetest.ConnID(dialect), "table": "etl_pd_dst", "commitSize": 10,
					"fields": []map[string]any{{"stream": "id", "column": "id"}},
				})},
			},
			Hops: []pipeline.Hop{
				{ID: "h1", From: "ti", FromPort: "main", To: "flt", ToPort: "main"},
				{ID: "h2", From: "flt", FromPort: "true", To: "out", ToPort: "main"},
			},
			Settings: pipeline.Settings{Pushdown: true},
		}
	}
	runDef := func(def *pipeline.Definition) []int64 {
		t.Helper()
		exec("DELETE FROM " + d.Qualified(dstRef))
		plan := pipeline.Compile(ctx, def, pipeline.CompileOptions{Env: e})
		if plan.Problems.HasErrors() {
			t.Fatalf("编译失败：%+v", plan.Problems)
		}
		if res := pipeline.Run(ctx, plan, pipeline.RunOptions{Env: e}); res.Err != nil {
			t.Fatalf("运行失败：%v", res.Err)
		}
		rs, err := h.DB.QueryContext(ctx, "SELECT id FROM "+d.Qualified(dstRef))
		if err != nil {
			t.Fatalf("读 dst 失败：%v", err)
		}
		defer rs.Close()
		var ids []int64
		for rs.Next() {
			var id int64
			if err := rs.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		return ids
	}
	pushedSQL := func(def *pipeline.Definition) string {
		t.Helper()
		for _, n := range def.Nodes {
			if n.ID == "ti" {
				var c struct {
					SQL string `json:"sql"`
				}
				_ = json.Unmarshal(n.Config, &c)
				return c.SQL
			}
		}
		t.Fatal("ti 节点不存在")
		return ""
	}

	// cond → wantPush：白名单结构逐个覆盖；false = 反例必须断言不下推。
	cases := []struct {
		cond     string
		wantPush bool
		wantNull bool // 下推 SQL 必须含 NULL 谓词（IS NULL / IS NOT NULL，NULL 语义保护）
	}{
		{`id > 2`, true, false},
		{`id >= 2 && id <= 4`, true, false},
		{`grp == 1`, true, false},
		{`grp != 1`, true, true}, // 验收反例：须以 (grp <> ? OR grp IS NULL) 形式下推
		{`grp == nil`, true, true},
		{`grp != nil`, true, true},
		{`id in [1, 3, 5]`, true, false},
		{`id not in [2]`, true, true},
		{`id == 1 || id == 3`, true, false},
		{`grp != 1 && id > 1`, true, true},
		{`not (grp != 1)`, true, false},
		{`not (id in [2, 4])`, true, true},
		{`id + 1 > 4`, true, false},
		{`price >= 2.5`, true, false},
		{`d1 >= d2`, true, false},
		{`t1 < t2`, true, false},
		{`id == grp`, true, true},
		{`id != grp`, true, true},
		// —— 验收反例（字符串语义）：必须断言不下推 ——
		{`name == "abc"`, false, false},
		{`length(name) == 4`, false, false},
		{`name == "a\\b"`, false, false},
		{`trim(name) == "x"`, false, false},
		{`name + "x" == "abcx"`, false, false},
		// —— NULL 语义陷阱：次序比较出现在 || / not 复合内 ——
		{`id > 1 || grp == 2`, false, false},
		{`not (id < 3 && grp == 1)`, false, false},
		{`grp == 2 or id < 3`, false, false},
	}
	for _, c := range cases {
		def := mk(c.cond)
		want := runDef(def)
		plan := pipeline.Compile(ctx, def, pipeline.CompileOptions{Env: e})
		def2, notes := pipeline.Pushdown(ctx, def, plan, e)
		if c.wantPush && len(notes) == 0 {
			t.Errorf("%s：应下推但没有", c.cond)
			continue
		}
		if !c.wantPush && len(notes) != 0 {
			t.Errorf("%s：反例不应下推，却被改写为：\n%s", c.cond, pushedSQL(def2))
			continue
		}
		if len(notes) == 0 {
			continue
		}
		sql := pushedSQL(def2)
		if c.wantNull && !strings.Contains(sql, "NULL") {
			t.Errorf("%s：下推 SQL 缺少 NULL 谓词的语义保护：\n%s", c.cond, sql)
		}
		p2 := pipeline.Compile(ctx, def2, pipeline.CompileOptions{Env: e})
		if p2.Problems.HasErrors() {
			t.Errorf("%s：改写后重编译失败：%+v", c.cond, p2.Problems)
			continue
		}
		got := runDef(def2)
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s：下推与不下推写入行集合不一致：不下推 %v，下推 %v\nSQL: %s", c.cond, want, got, sql)
		}
	}
}
