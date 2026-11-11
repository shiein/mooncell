package steps_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/pipeline"
)

// pdOutcome 运行定义并返回「写入的 id 有序列表」或「运行失败」。
func pdOutcome(t *testing.T, e core.Env, def *pipeline.Definition, q func(string) []int64) string {
	t.Helper()
	plan := pipeline.Compile(context.Background(), def, pipeline.CompileOptions{Env: e})
	if plan.Problems.HasErrors() {
		t.Fatalf("编译失败：%+v", plan.Problems)
	}
	res := pipeline.Run(context.Background(), plan, pipeline.RunOptions{Env: e})
	if res.Err != nil {
		return "运行失败"
	}
	ids := q("")
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return fmt.Sprint(ids)
}

// pdChain 构造「表输入 → 过滤(若干，首个之后可选接否端口) → 表输出」并开启下推。
func pdChain(src string, conds []string, falsePortOnLast bool) *pipeline.Definition {
	def := &pipeline.Definition{Settings: pipeline.Settings{Pushdown: true}}
	def.Nodes = append(def.Nodes, node("ti", "table_input", map[string]any{"connection": "db", "sql": src}))
	prev, port := "ti", "main"
	for i, c := range conds {
		id := fmt.Sprintf("f%d", i)
		def.Nodes = append(def.Nodes, node(id, "filter_rows", map[string]any{"condition": c}))
		def.Hops = append(def.Hops, hop(prev, port, id))
		prev, port = id, "true"
	}
	def.Nodes = append(def.Nodes,
		node("out", "table_output", map[string]any{"connection": "db", "table": "dst", "commitSize": 10,
			"fields": []map[string]any{{"stream": "id", "column": "id"}}}),
		node("rej", "table_output", map[string]any{"connection": "db", "table": "rej", "commitSize": 10,
			"fields": []map[string]any{{"stream": "id", "column": "id"}}}),
	)
	def.Hops = append(def.Hops, hop(prev, port, "out"))
	if falsePortOnLast {
		def.Hops = append(def.Hops, pipeline.Hop{ID: "fx", From: prev, FromPort: "false", To: "rej", ToPort: "main"})
	} else {
		def.Nodes = def.Nodes[:len(def.Nodes)-1] // 不用拒绝输出
	}
	return def
}

// TestPushdownNullOrderingResidual：可空字段上的次序比较遇到 NULL 时，行内过滤报错使运行失败；
// 下推后必须同样失败（源端只下推超集条件，过滤留在行内复核），而不是把 NULL 行静默滤掉。
// 无 NULL 数据时结果一致，且源端确实少传了行（下推仍然生效）。
func TestPushdownNullOrderingResidual(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE src (id INTEGER NOT NULL, grp INTEGER)`)
	mustExec(t, db, `CREATE TABLE dst (id INTEGER)`)
	ids := func(string) []int64 {
		rows, err := db.Query(`SELECT id FROM dst`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var id int64
			_ = rows.Scan(&id)
			out = append(out, id)
		}
		return out
	}
	reset := func() { mustExec(t, db, `DELETE FROM dst`) }
	src := `SELECT id, grp FROM src`

	for _, c := range []struct {
		data string
		cond string
	}{
		{`(1,1),(2,2),(3,NULL),(4,3)`, `grp > 1`},
		{`(1,1),(2,2),(3,NULL),(4,3)`, `grp + 1 > 2`},
		{`(1,1),(2,2),(3,NULL),(4,3)`, `not (grp > 1)`},
		{`(1,1),(2,2),(3,NULL),(4,3)`, `grp >= 1 && id > 0`},
		{`(1,1),(2,2),(3,3),(4,4)`, `grp > 2`},
		{`(1,1),(2,2),(3,3),(4,4)`, `not (grp > 2)`},
	} {
		mustExec(t, db, `DELETE FROM src`)
		mustExec(t, db, `INSERT INTO src VALUES `+c.data)
		def := pdChain(src, []string{c.cond}, false)
		reset()
		inline := pdOutcome(t, e, &pipeline.Definition{Nodes: def.Nodes, Hops: def.Hops}, ids)
		plan := pipeline.Compile(context.Background(), def, pipeline.CompileOptions{Env: e})
		pushed, notes := pipeline.Pushdown(context.Background(), def, plan, e)
		if len(notes) == 0 {
			t.Errorf("%s：应下推（超集 + 行内复核）却没有", c.cond)
			continue
		}
		if !strings.Contains(strings.Join(notes[0].From, ""), "保留行内复核") {
			t.Errorf("%s：可空字段上的次序比较应保留行内复核，notes=%+v", c.cond, notes)
		}
		keptFilter := false
		for _, n := range pushed.Nodes {
			if n.Type == "filter_rows" {
				keptFilter = true
			}
		}
		if !keptFilter {
			t.Errorf("%s：过滤步骤应保留在行内", c.cond)
		}
		reset()
		got := pdOutcome(t, e, pushed, ids)
		if got != inline {
			t.Errorf("%s（数据 %s）：不下推=%s 下推=%s", c.cond, c.data, inline, got)
		}
	}
	// 无 NULL 时源端确实筛掉了行：过滤步骤的输入行数 < 源表行数
	mustExec(t, db, `DELETE FROM src`)
	mustExec(t, db, `INSERT INTO src VALUES (1,1),(2,2),(3,3),(4,4)`)
	def := pdChain(src, []string{`grp > 2`}, false)
	plan := pipeline.Compile(context.Background(), def, pipeline.CompileOptions{Env: e})
	pushed, _ := pipeline.Pushdown(context.Background(), def, plan, e)
	reset()
	p2 := pipeline.Compile(context.Background(), pushed, pipeline.CompileOptions{Env: e})
	res := pipeline.Run(context.Background(), p2, pipeline.RunOptions{Env: e})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if in := res.Metrics["f0"].Read; in != 2 { // Read = 从上游收到的行数（Kettle 口径）
		t.Errorf("源端应只送出 2 行（grp>2），过滤实际收到 %d 行", in)
	}
}

// TestPushdownNoOrphanArgs：第二个过滤的否端口有连线时链在此结束，
// 它的字面量不能留在 pushArgs 里（否则占位符与参数个数不符，运行失败）。
func TestPushdownNoOrphanArgs(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE src (id INTEGER NOT NULL, grp INTEGER NOT NULL)`)
	mustExec(t, db, `INSERT INTO src VALUES (1,1),(2,2),(3,2),(4,3)`)
	mustExec(t, db, `CREATE TABLE dst (id INTEGER)`)
	mustExec(t, db, `CREATE TABLE rej (id INTEGER)`)
	def := pdChain(`SELECT id, grp FROM src`, []string{`id > 1`, `grp == 2`}, true)
	plan := pipeline.Compile(context.Background(), def, pipeline.CompileOptions{Env: e})
	pushed, notes := pipeline.Pushdown(context.Background(), def, plan, e)
	if len(notes) != 1 {
		t.Fatalf("首个过滤应下推，notes=%+v", notes)
	}
	// SQLite 驱动会静默忽略多余参数（PG 等则报参数个数不符），所以直接核对：
	// 改写 SQL 的占位符个数必须等于 pushArgs 个数。
	for _, n := range pushed.Nodes {
		if n.ID != "ti" {
			continue
		}
		var c struct {
			SQL      string `json:"sql"`
			PushArgs []any  `json:"pushArgs"`
		}
		if err := json.Unmarshal(n.Config, &c); err != nil {
			t.Fatal(err)
		}
		if ph := strings.Count(c.SQL, "?"); ph != len(c.PushArgs) {
			t.Fatalf("占位符 %d 个、pushArgs %d 个（孤儿参数）：%s %v", ph, len(c.PushArgs), c.SQL, c.PushArgs)
		}
	}
	p2 := pipeline.Compile(context.Background(), pushed, pipeline.CompileOptions{Env: e})
	if p2.Problems.HasErrors() {
		t.Fatalf("改写后编译失败（孤儿参数）：%+v", p2.Problems)
	}
	res := pipeline.Run(context.Background(), p2, pipeline.RunOptions{Env: e})
	if res.Err != nil {
		t.Fatalf("改写后运行失败（孤儿参数）：%v", res.Err)
	}
	if n := count(t, db, `SELECT count(*) FROM dst`); n != 2 {
		t.Errorf("是端口应写 2 行（id 2,3），实际 %d", n)
	}
	if n := count(t, db, `SELECT count(*) FROM rej`); n != 1 {
		t.Errorf("否端口应写 1 行（id 4），实际 %d", n)
	}
}

// TestPushdownBigintLiteral：超过 2^53 的整数字面量经 pushArgs 往返后必须精确，
// 否则会按 float64 舍入匹配到相邻的错误行。
func TestPushdownBigintLiteral(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE src (id INTEGER NOT NULL)`)
	mustExec(t, db, `INSERT INTO src VALUES (9007199254740992),(9007199254740993),(1790000000000000123)`)
	mustExec(t, db, `CREATE TABLE dst (id INTEGER)`)
	for _, c := range []struct {
		cond string
		want int64
	}{
		{`id == 9007199254740993`, 9007199254740993},
		{`id == 1790000000000000123`, 1790000000000000123},
	} {
		mustExec(t, db, `DELETE FROM dst`)
		def := pdChain(`SELECT id FROM src`, []string{c.cond}, false)
		plan := pipeline.Compile(context.Background(), def, pipeline.CompileOptions{Env: e})
		pushed, notes := pipeline.Pushdown(context.Background(), def, plan, e)
		if len(notes) == 0 {
			t.Fatalf("%s：应下推", c.cond)
		}
		p2 := pipeline.Compile(context.Background(), pushed, pipeline.CompileOptions{Env: e})
		if p2.Problems.HasErrors() {
			t.Fatalf("%s：%+v", c.cond, p2.Problems)
		}
		if res := pipeline.Run(context.Background(), p2, pipeline.RunOptions{Env: e}); res.Err != nil {
			t.Fatal(res.Err)
		}
		rows, err := db.Query(`SELECT id FROM dst`)
		if err != nil {
			t.Fatal(err)
		}
		var got []int64
		for rows.Next() {
			var id int64
			_ = rows.Scan(&id)
			got = append(got, id)
		}
		rows.Close()
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("%s：应只命中 %d，实际 %v", c.cond, c.want, got)
		}
	}
}
