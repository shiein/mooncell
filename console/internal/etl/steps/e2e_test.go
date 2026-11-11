package steps_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/pipeline"
	_ "mooncell/console/internal/etl/steps"
)

// env 为测试搭一个只含 SQLite 连接 "db" 的运行环境。
func env(t *testing.T) (core.Env, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "etl.db")
	cfg := conn.Config{ID: "db", DBType: "sqlite", Database: path}
	pool := conn.NewPool(func(ctx context.Context, id string) (conn.Config, string, error) {
		if id != "db" {
			return conn.Config{}, "", fmt.Errorf("unknown connection %s", id)
		}
		return cfg, "", nil
	})
	t.Cleanup(pool.Close)
	h, err := pool.Get(context.Background(), "db")
	if err != nil {
		t.Fatal(err)
	}
	k, v := pool.EnvEntry()
	return core.MapEnv{k: v}, h.DB
}

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func node(id, typ string, cfg any) pipeline.Node {
	b, _ := json.Marshal(cfg)
	return pipeline.Node{ID: id, Type: typ, Name: id, Config: b}
}

func hop(from, fromPort, to string) pipeline.Hop {
	return pipeline.Hop{ID: from + "-" + to, From: from, FromPort: fromPort, To: to, ToPort: "main"}
}

func run(t *testing.T, e core.Env, def *pipeline.Definition, vars *core.Vars) pipeline.Result {
	t.Helper()
	plan := pipeline.Compile(context.Background(), def, pipeline.CompileOptions{Env: e})
	if plan.Problems.HasErrors() {
		t.Fatalf("编译失败：%+v", plan.Problems)
	}
	sink := &core.MemorySink{}
	res := pipeline.Run(context.Background(), plan, pipeline.RunOptions{Env: e, Vars: vars, Log: core.NewLogger(sink, core.LevelDebug)})
	if res.Err != nil {
		for _, l := range sink.Snapshot() {
			t.Log(l.Level, l.NodeID, l.Message)
		}
	}
	return res
}

func count(t *testing.T, db *sql.DB, q string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func strp(s string) *string { return &s }

// 生成 → 表达式 → 过滤 → 唯一行 → 插入/更新；再改数据重跑，验证增量更新与「未变化跳过」。
func TestPipelineEndToEnd(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE target (id INTEGER PRIMARY KEY, bucket INTEGER, label TEXT, amount DECIMAL(10,2))`)

	gen := map[string]any{
		"limit": 1000, "sequenceField": "seq",
		"fields": []map[string]any{{"name": "price", "kind": "decimal", "value": "1.10"}},
	}
	calc := map[string]any{"fields": []map[string]any{
		{"name": "id", "expr": "seq % 300"}, // 制造重复键：1000 行 → 300 个不同 id
		{"name": "bucket", "expr": "seq % 7"},
		{"name": "label", "expr": `"b-" + toString(bucket)`},
		{"name": "amount", "expr": "price * seq"},
	}}
	filter := map[string]any{"condition": "id != 0"}
	iu := map[string]any{
		"connection": "db", "table": "target", "commitSize": 128, "skipUnchanged": true,
		"keys": []map[string]any{{"stream": "id", "column": "id"}},
		"fields": []map[string]any{
			{"stream": "bucket", "column": "bucket", "update": true},
			{"stream": "label", "column": "label", "update": true},
			{"stream": "amount", "column": "amount", "update": true},
		},
	}
	def := &pipeline.Definition{
		Nodes: []pipeline.Node{node("gen", "generate_rows", gen), node("calc", "calculator", calc),
			node("filter", "filter_rows", filter), node("uniq", "unique_hash", map[string]any{"keys": []string{"id", "bucket"}}),
			node("iu", "insert_update", iu)},
		Hops:     []pipeline.Hop{hop("gen", "main", "calc"), hop("calc", "main", "filter"), hop("filter", "true", "uniq"), hop("uniq", "main", "iu")},
		Settings: pipeline.Settings{BatchSize: 64, ChannelBatches: 2},
	}
	res := run(t, e, def, nil)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if n := count(t, db, `SELECT count(*) FROM target`); n != 299 {
		t.Fatalf("应写入 299 个不同 id，实际 %d", n)
	}
	m := res.Metrics["iu"]
	if m.Output != 299 {
		t.Fatalf("插入计数应为 299，实际 %+v", m)
	}
	// 同 id 不同 bucket 的行在批内/跨批重复 → 后者覆盖前者，属于更新
	if m.Output+m.Updated == 0 {
		t.Fatalf("度量异常：%+v", m)
	}
	// 金额保持精确：id=1 的最后一次出现是 seq=901，amount = 1.10*901 = 991.1
	var amount string
	if err := db.QueryRow(`SELECT CAST(amount AS TEXT) FROM target WHERE id = 1`).Scan(&amount); err != nil {
		t.Fatal(err)
	}
	if amount != "991.1" {
		t.Fatalf("精确小数写入错误：%s", amount)
	}

	// 原样重跑：所有行都已存在且值未变 → 不插入，更新只来自批内覆盖链（值与库中不同的中间态）
	res2 := run(t, e, def, nil)
	if res2.Err != nil {
		t.Fatal(res2.Err)
	}
	if res2.Metrics["iu"].Output != 0 {
		t.Fatalf("重跑不应再插入：%+v", res2.Metrics["iu"])
	}
	if n := count(t, db, `SELECT count(*) FROM target`); n != 299 {
		t.Fatalf("重跑后行数应不变，实际 %d", n)
	}
}

// 表输入 + 绑定参数变量 → 表输出（裁剪表），并验证类型转换失败的行进入错误输出。
func TestTableInputVarsAndErrorPort(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE src (id INTEGER, ts TEXT, qty TEXT)`)
	mustExec(t, db, `CREATE TABLE dst (id INTEGER, qty INTEGER)`)
	mustExec(t, db, `CREATE TABLE bad (id INTEGER, qty TEXT, _error_code TEXT, _error_message TEXT, _error_field TEXT, _error_step TEXT)`)
	mustExec(t, db, `INSERT INTO dst VALUES (999, 1)`) // 应被裁剪
	for i := 1; i <= 50; i++ {
		qty := fmt.Sprint(i)
		if i%10 == 0 {
			qty = "N/A" // 5 行无法转为整数
		}
		mustExec(t, db, fmt.Sprintf(`INSERT INTO src VALUES (%d, '2026-11-%02d', '%s')`, i, 1+i%28, qty))
	}
	def := &pipeline.Definition{
		Params: []core.ParamDecl{{Name: "minId", Kind: core.KindInteger}},
		Nodes: []pipeline.Node{
			node("in", "table_input", map[string]any{"connection": "db", "sql": "select id, qty from src where id > ${minId} order by id"}),
			node("sel", "select_fields", map[string]any{"fields": []map[string]any{{"source": "id"}, {"source": "qty", "kind": "integer"}}}),
			node("out", "table_output", map[string]any{"connection": "db", "table": "dst", "truncate": true, "commitSize": 7}),
			node("err", "table_output", map[string]any{"connection": "db", "table": "bad", "commitSize": 100}),
		},
		Hops: []pipeline.Hop{hop("in", "main", "sel"), hop("sel", "main", "out"), {ID: "e", From: "sel", FromPort: "error", To: "err", ToPort: "main"}},
	}
	vars := core.NewVars(nil)
	vars.Set("minId", core.NewValue(int64(20)))
	res := run(t, e, def, vars)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if n := count(t, db, `SELECT count(*) FROM dst`); n != 27 { // id 21..50 共 30 行，其中 30/40/50 三行坏
		t.Fatalf("dst 行数应为 27，实际 %d", n)
	}
	if n := count(t, db, `SELECT count(*) FROM bad WHERE _error_field = 'qty' AND _error_step = 'sel'`); n != 3 {
		t.Fatalf("错误输出应有 3 行，实际 %d", n)
	}
	if res.Metrics["sel"].Rejected != 3 || res.Metrics["in"].Input != 30 {
		t.Fatalf("度量错误：%+v / %+v", res.Metrics["sel"], res.Metrics["in"])
	}
}

// 值映射：覆盖原字段（含未匹配保留/默认值）与新字段两种形态。
func TestValueMapper(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE src (id INTEGER, status TEXT)`)
	mustExec(t, db, `CREATE TABLE dst (id INTEGER, status TEXT, label TEXT)`)
	mustExec(t, db, `INSERT INTO src VALUES (1,'A'),(2,'B'),(3,'X'),(4,NULL)`)
	def := &pipeline.Definition{
		Nodes: []pipeline.Node{
			node("in", "table_input", map[string]any{"connection": "db", "sql": "select id, status from src order by id"}),
			node("m", "value_mapper", map[string]any{
				"field":  "status",
				"target": "label",
				"mappings": []map[string]any{
					{"source": "A", "target": "激活"},
					{"source": "B", "target": "停用"},
				},
			}),
			node("out", "table_output", map[string]any{"connection": "db", "table": "dst"}),
		},
		Hops: []pipeline.Hop{hop("in", "main", "m"), hop("m", "main", "out")},
	}
	res := run(t, e, def, nil)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	var got []string
	rows, err := db.Query(`SELECT label FROM dst ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s sql.NullString
		rows.Scan(&s)
		got = append(got, s.String)
	}
	want := []string{"激活", "停用", "X", ""} // 未匹配保留原值；NULL 按空文本
	if len(got) != 4 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
		t.Fatalf("映射结果错误：%v", got)
	}
}

// 排序记录：升序复合键 + NULL 在前；大输入超内存预算时溢写外部归并，顺序与行数不变。
func TestSortRows(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE src (a INTEGER, b TEXT)`)
	mustExec(t, db, `CREATE TABLE dst (a INTEGER, b TEXT)`)
	mustExec(t, db, `INSERT INTO src VALUES (2,'x'),(NULL,'z'),(1,'b'),(1,'a'),(2,'a')`)
	def := &pipeline.Definition{
		Nodes: []pipeline.Node{
			node("in", "table_input", map[string]any{"connection": "db", "sql": "select a, b from src"}),
			node("sort", "sort_rows", map[string]any{"fields": []map[string]any{{"name": "a"}, {"name": "b", "desc": true}}}),
			node("out", "table_output", map[string]any{"connection": "db", "table": "dst"}),
		},
		Hops: []pipeline.Hop{hop("in", "main", "sort"), hop("sort", "main", "out")},
	}
	res := run(t, e, def, nil)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	var got []string
	rows, err := db.Query(`SELECT a, b FROM dst`) // 无 ORDER BY：按插入顺序读回即排序输出顺序
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a sql.NullInt64
		var b string
		rows.Scan(&a, &b)
		if a.Valid {
			got = append(got, fmt.Sprintf("%d/%s", a.Int64, b))
		} else {
			got = append(got, "NULL/"+b)
		}
	}
	rows.Close()
	want := []string{"NULL/z", "1/b", "1/a", "2/x", "2/a"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("排序结果错误：%v 期望 %v", got, want)
	}

	// 溢写路径：30000 行 + 1MB 预算 → 至少一次 spill + 归并
	mustExec(t, db, `CREATE TABLE big (n INTEGER)`)
	def2 := &pipeline.Definition{
		Nodes: []pipeline.Node{
			node("gen", "generate_rows", map[string]any{"limit": 30000, "sequenceField": "n"}),
			node("sort", "sort_rows", map[string]any{"fields": []map[string]any{{"name": "n", "desc": true}}}),
			node("out", "table_output", map[string]any{"connection": "db", "table": "big"}),
		},
		Hops:     []pipeline.Hop{hop("gen", "main", "sort"), hop("sort", "main", "out")},
		Settings: pipeline.Settings{MemoryBudgetMB: 1},
	}
	res2 := run(t, e, def2, nil)
	if res2.Err != nil {
		t.Fatal(res2.Err)
	}
	var prev, n int64 = 30001, 0 // sequenceField 生成 1..limit
	rr, err := db.Query(`SELECT n FROM big`)
	if err != nil {
		t.Fatal(err)
	}
	for rr.Next() {
		var v int64
		rr.Scan(&v)
		if v != prev-1 {
			t.Fatalf("第 %d 行应为 %d，实际 %d", n, prev-1, v)
		}
		prev, n = v, n+1
	}
	rr.Close()
	if n != 30000 || prev != 1 {
		t.Fatalf("应输出 30000 行降序至 1：n=%d prev=%d", n, prev)
	}
}

// 去除重复记录：排序流上按键/整行去重，每组保留首行。
func TestUniqueSorted(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE src (a INTEGER, b TEXT)`)
	mustExec(t, db, `CREATE TABLE dst (a INTEGER, b TEXT)`)
	mustExec(t, db, `INSERT INTO src VALUES (2,'x'),(1,'b'),(1,'a'),(2,'a'),(1,'a'),(NULL,'z'),(NULL,'z')`)
	def := &pipeline.Definition{
		Nodes: []pipeline.Node{
			node("in", "table_input", map[string]any{"connection": "db", "sql": "select a, b from src"}),
			node("sort", "sort_rows", map[string]any{"fields": []map[string]any{{"name": "a"}, {"name": "b"}}}),
			node("u", "unique_sorted", map[string]any{"keys": []string{"a", "b"}}),
			node("out", "table_output", map[string]any{"connection": "db", "table": "dst"}),
		},
		Hops: []pipeline.Hop{hop("in", "main", "sort"), hop("sort", "main", "u"), hop("u", "main", "out")},
	}
	res := run(t, e, def, nil)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if n := count(t, db, `SELECT count(*) FROM dst`); n != 5 { // (NULL,z)(1,a)(1,b)(2,a)(2,x) → 5 组
		t.Fatalf("去重后应为 5 行，实际 %d", n)
	}
	if res.Metrics["u"].Written != 5 {
		t.Fatalf("unique_sorted 写出应为 5：%+v", res.Metrics["u"])
	}
}

// 更新/删除：按键命中写目标表；未命中行进错误输出，未连接时跳过。
func TestUpdateDeleteSteps(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE src (id INTEGER, v TEXT)`)
	mustExec(t, db, `CREATE TABLE tgt (id INTEGER PRIMARY KEY, v TEXT)`)
	mustExec(t, db, `CREATE TABLE bad (id INTEGER, v TEXT, _error_code TEXT, _error_message TEXT, _error_field TEXT, _error_step TEXT)`)
	mustExec(t, db, `INSERT INTO src VALUES (1,'x'),(2,'y'),(9,'z')`)
	mustExec(t, db, `INSERT INTO tgt VALUES (1,'a'),(2,'b')`)
	def := &pipeline.Definition{
		Nodes: []pipeline.Node{
			node("in", "table_input", map[string]any{"connection": "db", "sql": "select id, v from src order by id"}),
			node("u", "update", map[string]any{
				"connection": "db", "table": "tgt",
				"keys":   []map[string]any{{"stream": "id", "column": "id"}},
				"fields": []map[string]any{{"stream": "v", "column": "v"}},
			}),
			node("err", "table_output", map[string]any{"connection": "db", "table": "bad"}),
		},
		Hops: []pipeline.Hop{
			hop("in", "main", "u"),
			{ID: "e", From: "u", FromPort: "error", To: "err", ToPort: "main"},
		},
	}
	res := run(t, e, def, nil)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	var vx, vy string
	db.QueryRow(`SELECT v FROM tgt WHERE id=1`).Scan(&vx)
	db.QueryRow(`SELECT v FROM tgt WHERE id=2`).Scan(&vy)
	if vx != "x" || vy != "y" {
		t.Fatalf("命中行应被更新为 x/y：%s/%s", vx, vy)
	}
	if n := count(t, db, `SELECT count(*) FROM bad WHERE id = 9`); n != 1 {
		t.Fatalf("未命中行 id=9 应进错误输出，实际 %d", n)
	}
	if res.Metrics["u"].Updated != 2 || res.Metrics["u"].Rejected != 1 {
		t.Fatalf("度量应为 Updated=2/Rejected=1：%+v", res.Metrics["u"])
	}

	// 删除：id=1,2 命中删除，id=9 未命中进错误输出
	mustExec(t, db, `DELETE FROM bad`)
	def.Nodes[1] = node("u", "delete", map[string]any{
		"connection": "db", "table": "tgt",
		"keys": []map[string]any{{"stream": "id", "column": "id"}},
	})
	res2 := run(t, e, def, nil)
	if res2.Err != nil {
		t.Fatal(res2.Err)
	}
	if n := count(t, db, `SELECT count(*) FROM tgt`); n != 0 {
		t.Fatalf("id=1,2 应被删除、表空，实际 %d", n)
	}
	if n := count(t, db, `SELECT count(*) FROM bad WHERE id = 9`); n != 1 {
		t.Fatalf("未命中行 id=9 应进错误输出，实际 %d", n)
	}
	if res2.Metrics["u"].Updated != 2 || res2.Metrics["u"].Rejected != 1 {
		t.Fatalf("删除度量应为 Updated=2/Rejected=1：%+v", res2.Metrics["u"])
	}
}

// 合并记录 + 数据同步：参照（目标）与对比（源）归并出标志，同步含插入/更新/删除/不变。
func TestMergeRowsSyncAfterMerge(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE old (id INTEGER, v TEXT)`) // 目标表现状（参照）
	mustExec(t, db, `CREATE TABLE new (id INTEGER, v TEXT)`) // 源侧（对比）
	mustExec(t, db, `INSERT INTO old VALUES (1,'a'),(2,'b'),(3,'c')`)
	mustExec(t, db, `INSERT INTO new VALUES (1,'a'),(2,'b2'),(4,'d')`)
	side := func(id, table string) []pipeline.Node {
		return []pipeline.Node{
			node(id+"_in", "table_input", map[string]any{"connection": "db", "sql": "select id, v from " + table}),
			node(id+"_s", "sort_rows", map[string]any{"fields": []map[string]any{{"name": "id"}}}),
		}
	}
	def := &pipeline.Definition{
		Nodes: append(append(side("r", "old"), side("c", "new")...),
			node("m", "merge_rows", map[string]any{"keys": []string{"id"}, "values": []string{"v"}}),
			node("sync", "sync_after_merge", map[string]any{
				"connection": "db", "table": "old",
				"keys": []map[string]any{{"stream": "id", "column": "id"}},
			})),
		Hops: []pipeline.Hop{
			hop("r_in", "main", "r_s"), hop("r_s", "main", "m"),
			hop("c_in", "main", "c_s"), {ID: "ci", From: "c_s", FromPort: "main", To: "m", ToPort: "info"},
			hop("m", "main", "sync"),
		},
	}
	res := run(t, e, def, nil)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	// 期望：1 不变、2 更新为 b2、3 删除、4 插入
	var v2 string
	db.QueryRow(`SELECT v FROM old WHERE id = 2`).Scan(&v2)
	checks := []struct {
		q    string
		want int64
	}{
		{`SELECT count(*) FROM old`, 3},
		{`SELECT count(*) FROM old WHERE id = 1 AND v = 'a'`, 1},
		{`SELECT count(*) FROM old WHERE id = 3`, 0},
		{`SELECT count(*) FROM old WHERE id = 4 AND v = 'd'`, 1},
	}
	for _, c := range checks {
		if n := count(t, db, c.q); n != c.want {
			t.Fatalf("校验失败 %s：实际 %d 期望 %d", c.q, n, c.want)
		}
	}
	if v2 != "b2" {
		t.Fatalf("id=2 应更新为 b2，实际 %s", v2)
	}
	m := res.Metrics["sync"]
	if m.Output != 1 || m.Updated != 2 {
		t.Fatalf("度量应为 Output=1(插入)/Updated=2(更新+删除)：%+v", m)
	}
}

// 流查询：参照流装哈希表，主流行按键查表追加字段，未命中填默认值。
func TestStreamLookup(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE src (id INTEGER, code TEXT)`)
	mustExec(t, db, `CREATE TABLE ref (code TEXT, label TEXT)`)
	mustExec(t, db, `CREATE TABLE dst (id INTEGER, code TEXT, "名" TEXT)`)
	mustExec(t, db, `INSERT INTO src VALUES (1,'a'),(2,'b'),(3,'z')`)
	mustExec(t, db, `INSERT INTO ref VALUES ('a','甲'),('b','乙'),('b','乙二')`) // 重复键保留首行
	def := &pipeline.Definition{
		Nodes: []pipeline.Node{
			node("s", "table_input", map[string]any{"connection": "db", "sql": "select id, code from src order by id"}),
			node("r", "table_input", map[string]any{"connection": "db", "sql": "select code, label from ref"}),
			node("lk", "stream_lookup", map[string]any{
				"keys":    []map[string]any{{"stream": "code", "column": "code"}},
				"returns": []map[string]any{{"column": "label", "name": "名", "default": "?"}},
			}),
			node("o", "table_output", map[string]any{"connection": "db", "table": "dst", "truncate": true}),
		},
		Hops: []pipeline.Hop{
			hop("s", "main", "lk"),
			{ID: "ri", From: "r", FromPort: "main", To: "lk", ToPort: "info"},
			hop("lk", "main", "o"),
		},
	}
	res := run(t, e, def, nil)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	checks := []struct {
		q    string
		want int64
	}{
		{`SELECT count(*) FROM dst WHERE "名" = '甲'`, 1},
		{`SELECT count(*) FROM dst WHERE "名" = '乙'`, 1},
		{`SELECT count(*) FROM dst WHERE "名" = '?'`, 1},
		{`SELECT count(*) FROM dst`, 3},
	}
	for _, c := range checks {
		if n := count(t, db, c.q); n != c.want {
			t.Fatalf("校验失败 %s：实际 %d 期望 %d", c.q, n, c.want)
		}
	}
}

// 数据库查询：逐行等值查表追加列；未命中默认值；多行命中报错时进错误输出。
func TestDBLookup(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE src (id INTEGER, code TEXT)`)
	mustExec(t, db, `CREATE TABLE dim (code TEXT, label TEXT)`)
	mustExec(t, db, `CREATE TABLE dst (id INTEGER, code TEXT, "label" TEXT, "hit" TEXT)`)
	mustExec(t, db, `CREATE TABLE dst_err (id INTEGER, code TEXT, _error_code TEXT, _error_message TEXT, _error_field TEXT, _error_step TEXT)`)
	mustExec(t, db, `INSERT INTO src VALUES (1,'a'),(2,'b'),(3,'z')`)
	mustExec(t, db, `INSERT INTO dim VALUES ('a','甲'),('b','乙'),('b','乙二')`)
	def := &pipeline.Definition{
		Nodes: []pipeline.Node{
			node("s", "table_input", map[string]any{"connection": "db", "sql": "select id, code from src order by id"}),
			node("lk", "db_lookup", map[string]any{
				"connection": "db", "table": "dim",
				"keys":    []map[string]any{{"stream": "code", "column": "code"}},
				"returns": []map[string]any{{"column": "label", "default": "?"}, {"column": "code", "name": "hit"}},
			}),
			node("o", "table_output", map[string]any{"connection": "db", "table": "dst", "truncate": true}),
			node("oe", "table_output", map[string]any{"connection": "db", "table": "dst_err", "truncate": true}),
		},
		Hops: []pipeline.Hop{
			hop("s", "main", "lk"),
			hop("lk", "main", "o"),
			{ID: "ke", From: "lk", FromPort: "error", To: "oe", ToPort: "main"},
		},
	}
	res := run(t, e, def, nil)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	// b 多行命中：默认取第一行（order by 未配置，取任意——只断言 hit=code 命中且非空）
	checks := []struct {
		q    string
		want int64
	}{
		{`SELECT count(*) FROM dst`, 3},
		{`SELECT count(*) FROM dst WHERE "hit" IS NULL AND "label" = '?'`, 1}, // z 未命中
		{`SELECT count(*) FROM dst WHERE "hit" = 'a' AND "label" = '甲'`, 1},
	}
	for _, c := range checks {
		if n := count(t, db, c.q); n != c.want {
			t.Fatalf("校验失败 %s：实际 %d 期望 %d", c.q, n, c.want)
		}
	}
	// 开启 failOnMulti 重跑：b 行进错误输出
	cfg, _ := json.Marshal(map[string]any{
		"connection": "db", "table": "dim", "failOnMulti": true,
		"keys":    []map[string]any{{"stream": "code", "column": "code"}},
		"returns": []map[string]any{{"column": "label", "default": "?"}, {"column": "code", "name": "hit"}},
	})
	def.Nodes[1].Config = cfg
	res = run(t, e, def, nil)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if n := count(t, db, `SELECT count(*) FROM dst_err`); n != 1 {
		t.Fatalf("多行命中应有 1 条错误行，实际 %d", n)
	}
	if n := count(t, db, `SELECT count(*) FROM dst`); n != 2 {
		t.Fatalf("主输出应为 2 行，实际 %d", n)
	}
}

// 表输出连接错误端口：批次内混入唯一约束冲突行 → 批量失败后退化逐行写入，
// 坏行进错误输出、好行照常提交、步骤成功；未连接错误端口则整步失败。
func TestTableOutputErrorRowDegrade(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE src (id INTEGER, qty INTEGER)`)
	mustExec(t, db, `CREATE TABLE dst (id INTEGER PRIMARY KEY, qty INTEGER)`)
	mustExec(t, db, `CREATE TABLE bad (id INTEGER, qty INTEGER, _error_code TEXT, _error_message TEXT, _error_field TEXT, _error_step TEXT)`)
	for i := 1; i <= 10; i++ {
		mustExec(t, db, fmt.Sprintf(`INSERT INTO src VALUES (%d, %d)`, i, i*10))
	}
	mustExec(t, db, `INSERT INTO dst VALUES (5, 555)`) // 与源 id=5 冲突 → 批量 INSERT 必败
	nodes := func() []pipeline.Node {
		return []pipeline.Node{
			node("in", "table_input", map[string]any{"connection": "db", "sql": "select id, qty from src order by id"}),
			node("out", "table_output", map[string]any{"connection": "db", "table": "dst", "commitSize": 20}),
			node("err", "table_output", map[string]any{"connection": "db", "table": "bad", "commitSize": 100}),
		}
	}
	def := &pipeline.Definition{
		Nodes: nodes(),
		Hops: []pipeline.Hop{
			hop("in", "main", "out"),
			{ID: "e", From: "out", FromPort: "error", To: "err", ToPort: "main"},
		},
	}
	res := run(t, e, def, nil)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if n := count(t, db, `SELECT count(*) FROM dst`); n != 10 { // 原有 id=5 + 新写入 9 行
		t.Fatalf("dst 应有 10 行（1 冲突 + 9 新），实际 %d", n)
	}
	if n := count(t, db, `SELECT count(*) FROM dst WHERE qty = 555`); n != 1 {
		t.Fatalf("原有 id=5 行不应被覆盖，实际 %d", n)
	}
	if n := count(t, db, `SELECT count(*) FROM bad WHERE id = 5 AND _error_step = 'out'`); n != 1 {
		t.Fatalf("错误输出应有 1 行（id=5），实际 %d", n)
	}
	m := res.Metrics["out"]
	if m.Rejected != 1 || m.Output != 9 {
		t.Fatalf("度量应为 Output=9/Rejected=1：%+v", m)
	}

	// 未连接错误端口：同样的冲突应使整步失败
	mustExec(t, db, `DELETE FROM dst`)
	mustExec(t, db, `INSERT INTO dst VALUES (5, 555)`)
	def2 := &pipeline.Definition{Nodes: nodes()[:2], Hops: []pipeline.Hop{hop("in", "main", "out")}}
	res2 := run(t, e, def2, nil)
	if res2.Err == nil {
		t.Fatal("未连接错误端口时冲突批应使步骤失败")
	}
	if n := count(t, db, `SELECT count(*) FROM dst`); n != 1 {
		t.Fatalf("失败批应整体回滚只剩原有 1 行，实际 %d", n)
	}
}

// 未连接错误输出时，坏行使转换失败，且失败能让上游及时停下（不死锁）。
func TestFailureCancelsUpstream(t *testing.T) {
	e, _ := env(t)
	def := &pipeline.Definition{
		Nodes: []pipeline.Node{
			node("gen", "generate_rows", map[string]any{"limit": 5_000_000, "fields": []map[string]any{{"name": "s", "kind": "string", "value": "x"}}}),
			node("sel", "select_fields", map[string]any{"fields": []map[string]any{{"source": "s", "kind": "integer"}}}),
		},
		Hops:     []pipeline.Hop{hop("gen", "main", "sel")},
		Settings: pipeline.Settings{BatchSize: 100, ChannelBatches: 1},
	}
	done := make(chan pipeline.Result, 1)
	go func() { done <- run(t, e, def, nil) }()
	select {
	case res := <-done:
		if res.Err == nil || res.FailedNode != "sel" || !strings.Contains(res.Err.Error(), "字段选择") && !strings.Contains(res.Err.Error(), "sel") {
			t.Fatalf("应由 sel 失败：%v / %s", res.Err, res.FailedNode)
		}
		if res.Metrics["gen"].Input >= 5_000_000 {
			t.Fatal("上游未被取消")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("失败后未在时限内结束（疑似死锁）")
	}
}

// 外部取消：长转换在 ctx 取消后迅速退出。
func TestExternalCancel(t *testing.T) {
	e, _ := env(t)
	def := &pipeline.Definition{
		Nodes: []pipeline.Node{
			node("gen", "generate_rows", map[string]any{"limit": 50_000_000, "sequenceField": "n"}),
			node("calc", "calculator", map[string]any{"fields": []map[string]any{{"name": "m", "expr": "n * 2"}}}),
		},
		Hops: []pipeline.Hop{hop("gen", "main", "calc")},
	}
	plan := pipeline.Compile(context.Background(), def, pipeline.CompileOptions{Env: e})
	ctx, cancel := context.WithCancel(context.Background())
	var ticks atomic.Int32
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	start := time.Now()
	res := pipeline.Run(ctx, plan, pipeline.RunOptions{Env: e, OnMetrics: func(map[string]core.MetricsSnapshot) { ticks.Add(1) }})
	if res.Err == nil {
		t.Fatal("取消后应返回错误")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("取消响应太慢：%s", time.Since(start))
	}
	if ticks.Load() == 0 {
		t.Fatal("结束时应至少回调一次度量")
	}
}

// 预览：只跑上游，输出步骤不写库，按 limit 截断。
func TestPreviewDoesNotWrite(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE dst (n INTEGER)`)
	def := &pipeline.Definition{
		Nodes: []pipeline.Node{
			node("gen", "generate_rows", map[string]any{"limit": 100000, "sequenceField": "n"}),
			node("out", "table_output", map[string]any{"connection": "db", "table": "dst", "truncate": true}),
		},
		Hops: []pipeline.Hop{hop("gen", "main", "out")},
	}
	p := pipeline.Preview(context.Background(), def, "out", 25, pipeline.CompileOptions{Env: e}, pipeline.RunOptions{Env: e})
	if p.Error != "" {
		t.Fatal(p.Error)
	}
	if got := len(p.Ports["main"].Rows); got != 25 || !p.Ports["main"].Truncated {
		t.Fatalf("预览应返回 25 行并标记截断，实际 %d / %v", got, p.Ports["main"].Truncated)
	}
	if n := count(t, db, `SELECT count(*) FROM dst`); n != 0 {
		t.Fatalf("预览不应写库，实际写入 %d", n)
	}
}

// 编译期问题：未知字段、环、端口误连、未声明变量都应定位到节点。
func TestCompileProblems(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE t (a INTEGER)`)
	def := &pipeline.Definition{
		Nodes: []pipeline.Node{
			node("a", "generate_rows", map[string]any{"limit": 1, "sequenceField": "x"}),
			node("b", "calculator", map[string]any{"fields": []map[string]any{{"name": "y", "expr": "nope + 1"}}}),
			node("c", "dummy", nil),
			node("d", "dummy", nil),
			node("in", "table_input", map[string]any{"connection": "db", "sql": "select * from t where a > ${undeclared}"}),
		},
		Hops: []pipeline.Hop{hop("a", "main", "b"), hop("c", "main", "d"), hop("d", "main", "c"), {ID: "bad", From: "a", FromPort: "true", To: "c", ToPort: "main"}},
	}
	plan := pipeline.Compile(context.Background(), def, pipeline.CompileOptions{Env: e})
	want := map[string]string{"b": "expr", "c": "cycle", "a": "port", "in": "var_undeclared"}
	got := map[string]bool{}
	for _, p := range plan.Problems {
		if want[p.NodeID] == p.Code {
			got[p.NodeID] = true
		}
	}
	for n := range want {
		if !got[n] {
			t.Errorf("缺少节点 %s 的 %s 问题；实际：%+v", n, want[n], plan.Problems)
		}
	}
}

// 无唯一约束的目标：通用路径（CASE 批量 UPDATE）更新已存在行、插入新行。
func TestInsertUpdateCaseFallback(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE t2 (k TEXT, v INTEGER, w TEXT)`)
	mustExec(t, db, `INSERT INTO t2 VALUES ('a', 1, 'x'), ('b', 2, 'y')`)

	def := &pipeline.Definition{
		Nodes: []pipeline.Node{
			node("gen", "generate_rows", map[string]any{"limit": 3, "sequenceField": "seq"}),
			node("calc", "calculator", map[string]any{"fields": []map[string]any{
				{"name": "k", "kind": "string", "expr": `decode(seq, 1, "a", 2, "c", 3, "d", "?")`},
				{"name": "v", "kind": "integer", "expr": "seq * 10"},
				{"name": "w", "kind": "string", "expr": `"w-" + toString(seq)`},
			}}),
			node("iu", "insert_update", map[string]any{
				"connection": "db", "table": "t2", "commitSize": 50,
				"keys":   []map[string]any{{"stream": "k", "column": "k"}},
				"fields": []map[string]any{{"stream": "v", "column": "v", "update": true}, {"stream": "w", "column": "w", "update": true}},
			}),
		},
		Hops: []pipeline.Hop{hop("gen", "main", "calc"), hop("calc", "main", "iu")},
	}
	res := run(t, e, def, nil)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if n := count(t, db, `SELECT count(*) FROM t2`); n != int64(4) {
		t.Fatalf("应有 4 行：%d", n)
	}
	var v int
	var w string
	if err := db.QueryRow(`SELECT v, w FROM t2 WHERE k='a'`).Scan(&v, &w); err != nil || v != 10 || w != "w-1" {
		t.Fatalf("'a' 应被 CASE UPDATE 改为 10/w-1：%v %d %s", err, v, w)
	}
	if err := db.QueryRow(`SELECT v FROM t2 WHERE k='b'`).Scan(&v); err != nil || v != 2 {
		t.Fatalf("'b' 不应被触碰：%v %d", err, v)
	}
	m := res.Metrics["iu"]
	if m.Output != 2 || m.Updated != 1 {
		t.Fatalf("度量应为 插2/更1：%+v", m)
	}
}

// 有唯一约束 + noUpdate：upsert 忽略冲突，无需查找，受影响行即实际插入数。
func TestInsertUpdateNoUpdateUpsert(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE t3 (k INTEGER PRIMARY KEY, v INTEGER)`)
	mustExec(t, db, `INSERT INTO t3 VALUES (1, 100)`)

	def := &pipeline.Definition{
		Nodes: []pipeline.Node{
			node("gen", "generate_rows", map[string]any{"limit": 3, "sequenceField": "seq"}),
			node("calc", "calculator", map[string]any{"fields": []map[string]any{
				{"name": "k", "kind": "integer", "expr": "seq"},
				{"name": "v", "kind": "integer", "expr": "seq * 10"},
			}}),
			node("iu", "insert_update", map[string]any{
				"connection": "db", "table": "t3", "noUpdate": true,
				"keys":   []map[string]any{{"stream": "k", "column": "k"}},
				"fields": []map[string]any{{"stream": "v", "column": "v", "update": true}},
			}),
		},
		Hops: []pipeline.Hop{hop("gen", "main", "calc"), hop("calc", "main", "iu")},
	}
	res := run(t, e, def, nil)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if n := count(t, db, `SELECT count(*) FROM t3`); n != int64(3) {
		t.Fatalf("应有 3 行：%d", n)
	}
	var v int
	if err := db.QueryRow(`SELECT v FROM t3 WHERE k=1`).Scan(&v); err != nil || v != 100 {
		t.Fatalf("已存在行不应被更新：%v %d", err, v)
	}
	if m := res.Metrics["iu"]; m.Output != 2 || m.Updated != 0 {
		t.Fatalf("实际插入应为 2（冲突忽略 1）：%+v", m)
	}
}
