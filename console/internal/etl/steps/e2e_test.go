package steps_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/pipeline"
	"mooncell/console/internal/etl/steps"
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

// withFileRoot 在测试环境注入 file_root（csv 等文件步骤的相对路径根）。
func withFileRoot(e core.Env, dir string) core.Env {
	m := e.(core.MapEnv)
	k, v := steps.FileRootEntry(dir)
	m[k] = v
	return m
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

// CSV 输入/输出：读定界文件（表头、类型转换、错误行）→ 写回文件。路径相对 file_root。
func TestCSVSteps(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE dst (id INTEGER, name TEXT, amount DECIMAL(10,2))`)
	mustExec(t, db, `CREATE TABLE bad (id INTEGER, name TEXT, amount TEXT, _error_code TEXT, _error_message TEXT, _error_field TEXT, _error_step TEXT)`)
	dir := t.TempDir()
	e = withFileRoot(e, dir)
	src := "in.csv"
	os.WriteFile(filepath.Join(dir, src), []byte("id,name,amount\n1,甲,3.40\n2,乙,7\n3,丙,不是数\n4,丁,9.99\n"), 0o644)
	outPath := "out.csv"
	def := &pipeline.Definition{
		Nodes: []pipeline.Node{
			node("csv", "csv_input", map[string]any{
				"path": src, "hasHeader": true,
				"fields": []map[string]any{
					{"name": "id", "kind": "integer"},
					{"name": "name", "kind": "string"},
					{"name": "amount", "kind": "decimal"},
				},
			}),
			node("o", "table_output", map[string]any{"connection": "db", "table": "dst"}),
			node("oe", "table_output", map[string]any{"connection": "db", "table": "bad"}),
			node("co", "csv_output", map[string]any{"path": outPath, "header": true}),
		},
		Hops: []pipeline.Hop{
			hop("csv", "main", "o"), hop("o", "main", "co"),
			{ID: "e", From: "csv", FromPort: "error", To: "oe", ToPort: "main"},
		},
	}
	res := run(t, e, def, nil)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if n := count(t, db, `SELECT count(*) FROM dst`); n != 3 {
		t.Fatalf("dst 应为 3 行，实际 %d", n)
	}
	if n := count(t, db, `SELECT count(*) FROM bad WHERE id = 3`); n != 1 {
		t.Fatalf("转换失败行应进错误输出，实际 %d", n)
	}
	data, err := os.ReadFile(filepath.Join(dir, outPath))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "id,name,amount") || !strings.Contains(text, "3.4") {
		t.Fatalf("CSV 输出内容不符：%q", text)
	}
	if lines := strings.Count(strings.TrimSpace(text), "\n"); lines != 3 {
		t.Fatalf("CSV 输出应 3 行数据，实际 %d：%q", lines, text)
	}
	if m := res.Metrics["csv"]; m.Rejected != 1 {
		t.Fatalf("度量应为 Rejected=1：%+v", m)
	}
}

// 文件根目录边界（R1-B1）：csv_input/csv_output 的 path 只解释为 file_root 下的相对路径；
// 绝对路径、.. 越界、指向根外的符号链接在编译期拒绝；编译后才出现的逃逸在运行期拦截。
func TestCSVFileRootGuard(t *testing.T) {
	e, _ := env(t)
	root := t.TempDir()
	e = withFileRoot(e, root)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.csv")
	os.WriteFile(secret, []byte("v\nTOP-SECRET\n"), 0o644)
	os.WriteFile(filepath.Join(root, "ok.csv"), []byte("v\n1\n"), 0o644)

	inDef := func(path string) *pipeline.Definition {
		return &pipeline.Definition{
			Nodes: []pipeline.Node{
				node("csv", "csv_input", map[string]any{"path": path, "hasHeader": true,
					"fields": []map[string]any{{"name": "v", "kind": "string"}}}),
				node("d", "dummy", map[string]any{}),
			},
			Hops: []pipeline.Hop{hop("csv", "main", "d")},
		}
	}
	outDef := func(path string) *pipeline.Definition {
		return &pipeline.Definition{
			Nodes: []pipeline.Node{
				node("g", "generate_rows", map[string]any{"limit": 1, "sequenceField": "n"}),
				node("o", "csv_output", map[string]any{"path": path}),
			},
			Hops: []pipeline.Hop{hop("g", "main", "o")},
		}
	}
	compileErr := func(def *pipeline.Definition) bool {
		return pipeline.Compile(context.Background(), def, pipeline.CompileOptions{Env: e}).Problems.HasErrors()
	}
	runErr := func(def *pipeline.Definition) error {
		plan := pipeline.Compile(context.Background(), def, pipeline.CompileOptions{Env: e})
		if plan.Problems.HasErrors() {
			t.Fatalf("编译失败：%+v", plan.Problems)
		}
		return pipeline.Run(context.Background(), plan,
			pipeline.RunOptions{Env: e, Log: core.NewLogger(&core.MemorySink{}, core.LevelError)}).Err
	}

	// 编译期拒绝：绝对路径 / .. 越界（输入与输出各自覆盖）
	for _, p := range []string{secret, filepath.Join(outside, "evil.csv"), "../escape.csv", "../../x.csv", "/abs.csv"} {
		if !compileErr(inDef(p)) {
			t.Fatalf("csv_input 路径 %q 应在编译期被拒", p)
		}
		if !compileErr(outDef(p)) {
			t.Fatalf("csv_output 路径 %q 应在编译期被拒", p)
		}
	}

	// 根内符号链接指向根外：文件级（输入/输出）与目录级（输入/输出）
	os.Symlink(secret, filepath.Join(root, "link.csv"))
	os.Mkdir(filepath.Join(outside, "sub"), 0o755)
	os.Symlink(outside, filepath.Join(root, "linkdir"))
	for _, p := range []string{"link.csv", "linkdir/secret.csv"} {
		if !compileErr(inDef(p)) {
			t.Fatalf("csv_input 符号链接路径 %q 应在编译期被拒", p)
		}
	}
	for _, p := range []string{"link.csv", "linkdir/new.csv"} {
		if !compileErr(outDef(p)) {
			t.Fatalf("csv_output 符号链接路径 %q 应在编译期被拒", p)
		}
	}

	// 运行期再校验：编译时路径尚不存在 → 之后出现指向根外的符号链接必须拦下
	for _, c := range []struct {
		name string
		def  *pipeline.Definition
		link string
	}{
		{"输入-运行期符号链接", inDef("late.csv"), filepath.Join(root, "late.csv")},
		{"输出-运行期符号链接", outDef("late_out.csv"), filepath.Join(root, "late_out.csv")},
	} {
		plan := pipeline.Compile(context.Background(), c.def, pipeline.CompileOptions{Env: e})
		if plan.Problems.HasErrors() {
			t.Fatalf("%s：路径不存在时编译应通过：%+v", c.name, plan.Problems)
		}
		if err := os.Symlink(secret, c.link); err != nil {
			t.Fatal(err)
		}
		res := pipeline.Run(context.Background(), plan,
			pipeline.RunOptions{Env: e, Log: core.NewLogger(&core.MemorySink{}, core.LevelError)})
		if res.Err == nil {
			t.Fatalf("%s：运行期应拒绝指向根外的符号链接", c.name)
		}
	}

	// 未配置 file_root 的环境：运行期拒绝（防误配置放开任意路径）
	noEnv, _ := env(t)
	if err := runErrNoEnv(noEnv, inDef("ok.csv")); err == nil {
		t.Fatal("无 file_root 的环境应拒绝运行文件步骤")
	}

	// 根外文件未被写出/读取
	if _, err := os.Lstat(filepath.Join(outside, "evil.csv")); !os.IsNotExist(err) {
		t.Fatal("不应在根外写出文件")
	}
	// 根内合法相对路径：输入读取、输出写入正常
	if err := runErr(inDef("ok.csv")); err != nil {
		t.Fatalf("根内相对路径读取应成功：%v", err)
	}
	if err := runErr(outDef("nodir/ok-out.csv")); err == nil {
		t.Fatal("父目录不存在时输出应失败（不自动建目录）")
	}
	if err := runErr(outDef("ok-out.csv")); err != nil {
		t.Fatalf("根内相对路径写出应成功：%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "ok-out.csv")); err != nil {
		t.Fatalf("根内输出文件应存在：%v", err)
	}
}

func runErrNoEnv(e core.Env, def *pipeline.Definition) error {
	plan := pipeline.Compile(context.Background(), def, pipeline.CompileOptions{Env: e})
	res := pipeline.Run(context.Background(), plan,
		pipeline.RunOptions{Env: e, Log: core.NewLogger(&core.MemorySink{}, core.LevelError)})
	return res.Err
}

// HTTP 输入：httptest 服务端返回分页 JSON，dataPath 拍平 + 字段点路径取值。
func TestHTTPInput(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE dst (id INTEGER, name TEXT)`)
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.RawQuery)
		var items string
		switch r.URL.Query().Get("p") {
		case "1":
			items = `[{"id":1,"n":"a"},{"id":2,"n":"b"}]`
		case "2":
			items = `[{"id":3,"n":"c"}]` // 不足 size=2 → 末页
		default:
			items = `[]`
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"rows":%s}}`, items)
	}))
	defer srv.Close()
	def := &pipeline.Definition{
		Nodes: []pipeline.Node{
			node("h", "http_input", map[string]any{
				"url": srv.URL + "/list", "dataPath": "data.rows",
				"page": map[string]any{"param": "p", "size": 2},
				"fields": []map[string]any{
					{"name": "id", "path": "id", "kind": "integer"},
					{"name": "name", "path": "n", "kind": "string"},
				},
			}),
			node("o", "table_output", map[string]any{"connection": "db", "table": "dst"}),
		},
		Hops: []pipeline.Hop{hop("h", "main", "o")},
	}
	res := run(t, e, def, nil)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if n := count(t, db, `SELECT count(*) FROM dst`); n != 3 {
		t.Fatalf("dst 应为 3 行（2 页），实际 %d", n)
	}
	if len(got) != 2 {
		t.Fatalf("应翻 2 页，实际请求 %d 次：%v", len(got), got)
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

// P3.18 / R1-B3：copiesDistribution 独立语义——单连线到多副本节点缺省轮询分发（N 行进 N 行出），
// 显式 copy 才每副本收全量；与上游端口「多连线分发方式」互不影响。
func TestNodeCopies(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE copied (seq INTEGER, w INTEGER)`)
	gen := map[string]any{"limit": 999, "sequenceField": "seq"}
	calc := map[string]any{"fields": []map[string]any{{"name": "w", "expr": "seq"}}}
	out := map[string]any{
		"connection": "db", "table": "copied", "commitSize": 128,
		"fields": []map[string]any{{"stream": "seq", "column": "seq"}, {"stream": "w", "column": "w"}},
	}

	// 缺省 distribute：行轮流分给 3 个副本，写库 999 行且 seq 无重复
	calcNode := node("calc", "calculator", calc)
	calcNode.Copies = 3
	def := &pipeline.Definition{
		Nodes: []pipeline.Node{node("gen", "generate_rows", gen), calcNode, node("out", "table_output", out)},
		Hops:  []pipeline.Hop{hop("gen", "main", "calc"), hop("calc", "main", "out")},
	}
	res := run(t, e, def, nil)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if n := count(t, db, `SELECT count(*) FROM copied`); n != 999 {
		t.Fatalf("缺省应为轮询分发、写库 999 行，实际 %d", n)
	}
	if n := count(t, db, `SELECT count(DISTINCT seq) FROM copied`); n != 999 {
		t.Fatalf("seq 不应重复，实际 %d 个不同值", n)
	}
	if m := res.Metrics["calc"]; m.Read != 999 {
		t.Fatalf("calc 汇总读取应为 999，实际 %+v", m)
	}

	// 显式 copy：每个副本收全量 → 写库 3×999
	mustExec(t, db, `DELETE FROM copied`)
	def.Nodes[1].CopiesDistribution = core.DistCopy
	res = run(t, e, def, nil)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if n := count(t, db, `SELECT count(*) FROM copied`); n != 999*3 {
		t.Fatalf("copy 模式每副本应收全量（3×999），实际 %d", n)
	}

	// 上游端口多连线时的 copy/distribute 语义独立于副本分发：
	// gen 复制发给 calc(3副本,缺省轮询) 与另一个 dummy 分支，calc 一侧仍按行切分。
	mustExec(t, db, `DELETE FROM copied`)
	calcNode.CopiesDistribution = ""
	genNode := node("gen", "generate_rows", gen)
	genNode.Distribution = map[string]core.Distribution{"main": core.DistCopy}
	def = &pipeline.Definition{
		Nodes: []pipeline.Node{genNode, calcNode, node("out", "table_output", out), node("sink", "dummy", map[string]any{})},
		Hops:  []pipeline.Hop{hop("gen", "main", "calc"), hop("gen", "main", "sink"), hop("calc", "main", "out")},
	}
	res = run(t, e, def, nil)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if n := count(t, db, `SELECT count(*) FROM copied`); n != 999 {
		t.Fatalf("多连线 copy 下 calc 各副本仍应合计只写 999 行，实际 %d", n)
	}
}

// P3.18 副本编译校验：源步骤/越界拒绝，有状态步骤告警。
func TestNodeCopiesValidation(t *testing.T) {
	e, _ := env(t)
	gen := map[string]any{"limit": 10, "sequenceField": "seq"}

	// 源步骤不可副本
	src := node("gen", "generate_rows", gen)
	src.Copies = 2
	def := &pipeline.Definition{Nodes: []pipeline.Node{src}}
	plan := pipeline.Compile(context.Background(), def, pipeline.CompileOptions{Env: e})
	if !plan.Problems.HasErrors() {
		t.Fatal("源步骤设 copies>1 应编译报错")
	}

	// 越界
	bad := node("calc", "calculator", map[string]any{"fields": []map[string]any{{"name": "w", "expr": "seq"}}})
	bad.Copies = 99
	def = &pipeline.Definition{
		Nodes: []pipeline.Node{node("gen", "generate_rows", gen), bad},
		Hops:  []pipeline.Hop{hop("gen", "main", "calc")},
	}
	plan = pipeline.Compile(context.Background(), def, pipeline.CompileOptions{Env: e})
	if !plan.Problems.HasErrors() {
		t.Fatal("copies 越界应编译报错")
	}

	// 有状态步骤副本 → 告警但可运行
	st := node("uniq", "unique_hash", map[string]any{"keys": []string{"seq"}})
	st.Copies = 2
	def = &pipeline.Definition{
		Nodes: []pipeline.Node{node("gen", "generate_rows", gen), st},
		Hops:  []pipeline.Hop{hop("gen", "main", "uniq")},
	}
	plan = pipeline.Compile(context.Background(), def, pipeline.CompileOptions{Env: e})
	if plan.Problems.HasErrors() {
		t.Fatalf("有状态副本只应告警：%+v", plan.Problems)
	}
	found := false
	for _, p := range plan.Problems {
		if p.Code == "copies_stateful" {
			found = true
		}
	}
	if !found {
		t.Fatal("应给出 copies_stateful 告警")
	}
}

// R1-B3 副本编译告警：按键写步骤多副本竞态、多副本上游打乱有序输入。
func TestNodeCopiesWarnings(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE wtbl (id INTEGER, v INTEGER)`)

	warnCodes := func(def *pipeline.Definition) map[string]bool {
		plan := pipeline.Compile(context.Background(), def, pipeline.CompileOptions{Env: e})
		if plan.Problems.HasErrors() {
			t.Fatalf("告警用例不应产生 error：%+v", plan.Problems)
		}
		codes := map[string]bool{}
		for _, p := range plan.Problems {
			if p.Severity == core.SeverityWarning {
				codes[p.Code] = true
			}
		}
		return codes
	}
	gen := map[string]any{"limit": 10, "sequenceField": "seq"}

	// 1) copies>1 的按键写步骤 → copies_write_race
	for _, typ := range []struct {
		id  string
		cfg map[string]any
	}{
		{"insert_update", map[string]any{"connection": "db", "table": "wtbl", "keys": []map[string]any{{"stream": "seq", "column": "id"}}, "fields": []map[string]any{{"stream": "seq", "column": "v"}}}},
		{"update", map[string]any{"connection": "db", "table": "wtbl", "keys": []map[string]any{{"stream": "seq", "column": "id"}}, "fields": []map[string]any{{"stream": "seq", "column": "v"}}}},
		{"delete", map[string]any{"connection": "db", "table": "wtbl", "keys": []map[string]any{{"stream": "seq", "column": "id"}}}},
		{"db_lookup", map[string]any{"connection": "db", "table": "wtbl", "keys": []map[string]any{{"stream": "seq", "column": "id"}}, "returns": []map[string]any{{"column": "v", "name": "v"}}}},
	} {
		n := node("w", typ.id, typ.cfg)
		n.Copies = 2
		def := &pipeline.Definition{
			Nodes: []pipeline.Node{node("gen", "generate_rows", gen), n},
			Hops:  []pipeline.Hop{hop("gen", "main", "w")},
		}
		if codes := warnCodes(def); !codes["copies_write_race"] {
			t.Fatalf("%s 设 2 副本应给 copies_write_race 告警，实际 %+v", typ.id, codes)
		}
	}

	// 2) copies>1 下游存在要求有序输入的步骤 → copies_order
	calc := node("calc", "calculator", map[string]any{"fields": []map[string]any{{"name": "w", "expr": "seq"}}})
	calc.Copies = 2
	for _, ordered := range []struct {
		id  string
		cfg map[string]any
	}{
		{"unique_sorted", map[string]any{"keys": []string{"seq"}}},
		{"merge_rows", nil}, // merge_rows 需要两个输入，跳过直连用例
	} {
		if ordered.cfg == nil {
			continue
		}
		def := &pipeline.Definition{
			Nodes: []pipeline.Node{node("gen", "generate_rows", gen), calc, node("o", ordered.id, ordered.cfg)},
			Hops:  []pipeline.Hop{hop("gen", "main", "calc"), hop("calc", "main", "o")},
		}
		if codes := warnCodes(def); !codes["copies_order"] {
			t.Fatalf("多副本上游→%s 应给 copies_order 告警，实际 %+v", ordered.id, codes)
		}
	}

	// 中间隔 sort_rows 后告警解除（排序重建全序）
	def := &pipeline.Definition{
		Nodes: []pipeline.Node{
			node("gen", "generate_rows", gen), calc,
			node("srt", "sort_rows", map[string]any{"fields": []map[string]any{{"name": "seq"}}}),
			node("o", "unique_sorted", map[string]any{"keys": []string{"seq"}}),
		},
		Hops: []pipeline.Hop{hop("gen", "main", "calc"), hop("calc", "main", "srt"), hop("srt", "main", "o")},
	}
	if codes := warnCodes(def); codes["copies_order"] {
		t.Fatalf("sort_rows 重建全序后不应再有 copies_order 告警，实际 %+v", codes)
	}
}

// R1-B2 下推优化器：表输入→过滤→字段选择 折叠为源端 SQL；默认关闭，
// 开启后只允许可证等价子集，字面量全部走绑定参数。
func TestPushdown(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE src (id INTEGER, name TEXT, grp INTEGER)`)
	mustExec(t, db, `INSERT INTO src VALUES (1,'a',1),(2,'b',1),(3,'c',2),(4,'d',NULL)`)
	mustExec(t, db, `CREATE TABLE dst (id INTEGER, label TEXT)`)

	ti := map[string]any{"connection": "db", "sql": "SELECT id, name, grp FROM src"}
	sel := map[string]any{"fields": []map[string]any{{"source": "id"}, {"source": "name", "name": "label"}}}
	out := map[string]any{
		"connection": "db", "table": "dst", "commitSize": 10,
		"fields": []map[string]any{{"stream": "id", "column": "id"}, {"stream": "label", "column": "label"}},
	}
	mk := func(cond string) *pipeline.Definition {
		return &pipeline.Definition{
			Nodes: []pipeline.Node{
				node("ti", "table_input", ti),
				node("flt", "filter_rows", map[string]any{"condition": cond}),
				node("sel", "select_fields", sel),
				node("out", "table_output", out),
			},
			Hops:     []pipeline.Hop{hop("ti", "main", "flt"), hop("flt", "true", "sel"), hop("sel", "main", "out")},
			Settings: pipeline.Settings{Pushdown: true},
		}
	}
	push := func(def *pipeline.Definition) (*pipeline.Definition, []pipeline.PushdownNote) {
		plan := pipeline.Compile(context.Background(), def, pipeline.CompileOptions{Env: e})
		if plan.Problems.HasErrors() {
			t.Fatalf("编译失败：%+v", plan.Problems)
		}
		return pipeline.Pushdown(context.Background(), def, plan, e)
	}
	tiCfg := func(def *pipeline.Definition) (sql string, pushArgs []any) {
		t.Helper()
		for _, n := range def.Nodes {
			if n.ID == "ti" {
				var c struct {
					SQL      string `json:"sql"`
					PushArgs []any  `json:"pushArgs"`
				}
				if err := json.Unmarshal(n.Config, &c); err != nil {
					t.Fatalf("ti config 解码失败：%v", err)
				}
				return c.SQL, c.PushArgs
			}
		}
		t.Fatal("ti 节点不存在")
		return "", nil
	}
	// 下推/不下推分别跑一遍，比较写入 dst 的 id 集合并清空
	runIDs := func(def *pipeline.Definition, vars *core.Vars) map[int64]bool {
		t.Helper()
		mustExec(t, db, `DELETE FROM dst`)
		res := run(t, e, def, vars)
		if res.Err != nil {
			t.Fatal(res.Err)
		}
		ids := map[int64]bool{}
		rows, err := db.Query(`SELECT id FROM dst`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids[id] = true
		}
		return ids
	}
	eqIDs := func(a, b map[int64]bool) bool {
		if len(a) != len(b) {
			return false
		}
		for k := range a {
			if !b[k] {
				return false
			}
		}
		return true
	}
	var noLiteral = regexp.MustCompile(`'|\d`)
	var stripIdent = regexp.MustCompile(`"[^"]*"`)

	// —— 默认关闭：旧定义/未开启定义不应被改写 ——
	defOff := mk(`grp == 2`)
	defOff.Settings = pipeline.Settings{} // 显式回到缺省（等价于 JSON 里没有 pushdown 字段）
	def2, notes := push(defOff)
	if len(notes) != 0 || def2 != defOff {
		t.Fatalf("settings.pushdown 缺省应关闭：notes=%+v", notes)
	}

	// —— 可下推：白名单条件 + 纯改名字段选择；字面量进 pushArgs ——
	def := mk(`grp == 2`)
	def2, notes = push(def)
	if len(notes) != 1 {
		t.Fatalf("应产生 1 处下推，实际 %+v", notes)
	}
	pushedSQL, pushArgs := tiCfg(def2)
	if !strings.Contains(pushedSQL, "WHERE") || !strings.Contains(pushedSQL, `"name" AS "label"`) {
		t.Fatalf("改写 SQL 不符合预期：\n%s", pushedSQL)
	}
	if noLiteral.MatchString(stripIdent.ReplaceAllString(pushedSQL, "")) {
		t.Fatalf("下推 SQL 不应含值字面量：\n%s", pushedSQL)
	}
	if len(pushArgs) != 1 || pushArgs[0] != float64(2) {
		t.Fatalf("pushArgs 应含 [2]：%v", pushArgs)
	}
	if len(def2.Nodes) != 2 {
		t.Fatalf("被并入的步骤应消失，剩余 %d 个节点", len(def2.Nodes))
	}
	if !eqIDs(runIDs(def2, nil), runIDs(def, nil)) {
		t.Fatal("grp == 2：下推与不下推写入行集合应一致")
	}

	// —— != 按引擎 NULL 语义改写（nil != 1 为真，NULL 行保留）——
	def = mk(`grp != 1`)
	def2, notes = push(def)
	if len(notes) != 1 {
		t.Fatal("grp != 1 应可下推（须带 IS NULL 保护）")
	}
	pushedSQL, _ = tiCfg(def2)
	if !strings.Contains(pushedSQL, "IS NULL") {
		t.Fatalf("!= 下推必须保留 NULL 行：\n%s", pushedSQL)
	}
	if !eqIDs(runIDs(def2, nil), runIDs(def, nil)) {
		t.Fatal("grp != 1：下推与不下推写入行集合应一致（NULL 行须保留）")
	}
	if n := count(t, db, `SELECT count(*) FROM dst`); n != 2 {
		t.Fatalf("grp != 1 应有 2 行（grp=2 与 NULL），实际 %d", n)
	}

	// —— ${} 参数与下推参数按占位符顺序绑定 ——
	defV := mk(`grp != 7`)
	defV.Params = []core.ParamDecl{
		{Name: "min", Kind: core.KindInteger, Required: true},
		{Name: "max", Kind: core.KindInteger, Required: true},
	}
	for i := range defV.Nodes {
		if defV.Nodes[i].ID == "ti" {
			defV.Nodes[i].Config, _ = json.Marshal(map[string]any{
				"connection": "db", "sql": "SELECT id, name, grp FROM src WHERE id > ${min} AND id < ${max}",
			})
		}
	}
	defV2, notes := push(defV)
	if len(notes) != 1 {
		t.Fatalf("含 ${} 的源 SQL 也应可下推：%+v", notes)
	}
	pushedSQL, pushArgs = tiCfg(defV2)
	if !strings.Contains(pushedSQL, "${min}") || !strings.Contains(pushedSQL, "${max}") {
		t.Fatalf("源 SQL 的变量引用应原样保留：\n%s", pushedSQL)
	}
	if len(pushArgs) != 1 || pushArgs[0] != float64(7) {
		t.Fatalf("pushArgs 应含 [7]：%v", pushArgs)
	}
	vars := core.NewVars(nil)
	vars.Set("min", core.Value{Kind: core.KindInteger, V: int64(2)})
	vars.Set("max", core.Value{Kind: core.KindInteger, V: int64(10)})
	got := runIDs(defV2, vars)
	// id>2 && id<10 后剩 3,4；grp!=7 恒真 → {3,4}。若参数顺序错位（7 先绑），id>7 且无 id<7 → 空集。
	if len(got) != 2 || !got[3] || !got[4] {
		t.Fatalf("${} 参数与 pushArgs 顺序错误：得到 %v", got)
	}
	if !eqIDs(got, runIDs(defV, vars)) {
		t.Fatal("含 ${} 的下推与不下推行集合应一致")
	}

	// —— 字符串条件（反例语义）一律不下推 ——
	for _, cond := range []string{
		`name == "abc"`, `name != "d"`, `name in ["a","b"]`,
		`grp == 2 and name != "d"`, `length(name) == 4`, `trim(name) == "x"`,
		`name + "x" == "abcx"`, `id > 1 || grp == 2`, `not (grp < 1 and id == 1)`,
	} {
		d := mk(cond)
		if _, notes := push(d); len(notes) != 0 {
			t.Errorf("%s 不应下推", cond)
		}
	}

	// —— 预览不下推：即使定义开启 pushdown，预览仍走原链路 ——
	defP := mk(`grp == 2`)
	pv := pipeline.Preview(context.Background(), defP, "ti", 10,
		pipeline.CompileOptions{Env: e}, pipeline.RunOptions{Env: e})
	if pv.Error != "" {
		t.Fatalf("预览失败：%s", pv.Error)
	}
	if rows := pv.Ports["main"].Rows; len(rows) != 4 {
		t.Fatalf("开启 pushdown 后预览仍应返回源端全部 4 行，实际 %d", len(rows))
	}
	pv = pipeline.Preview(context.Background(), defP, "flt", 10,
		pipeline.CompileOptions{Env: e}, pipeline.RunOptions{Env: e})
	if pv.Error != "" {
		t.Fatalf("过滤节点应在预览中存在（未被折叠）：%s", pv.Error)
	}

	// 否端口被使用 → 不下推
	defNo := mk(`grp == 2`)
	defNo.Hops[1].To = "sel"
	defNo.Hops = append(defNo.Hops, pipeline.Hop{ID: "x", From: "flt", FromPort: "false", To: "out", ToPort: "main"})
	plan := pipeline.Compile(context.Background(), defNo, pipeline.CompileOptions{Env: e})
	_, notes = pipeline.Pushdown(context.Background(), defNo, plan, e)
	if len(notes) != 0 {
		t.Fatal("否端口有连线不应下推")
	}

	// var() 不可下推 → 保留原链路
	defVr := mk(`grp == var("g")`)
	defVr.Params = append(defVr.Params, core.ParamDecl{Name: "g", Kind: core.KindInteger})
	plan = pipeline.Compile(context.Background(), defVr, pipeline.CompileOptions{Env: e})
	_, notes = pipeline.Pushdown(context.Background(), defVr, plan, e)
	if len(notes) != 0 {
		t.Fatal("含 var() 的条件不应下推")
	}

	// 表输入有多条出边 → 不改写
	defM := mk(`grp == 2`)
	defM.Hops = append(defM.Hops, pipeline.Hop{ID: "x2", From: "ti", FromPort: "main", To: "out", ToPort: "main"})
	plan = pipeline.Compile(context.Background(), defM, pipeline.CompileOptions{Env: e})
	_, notes = pipeline.Pushdown(context.Background(), defM, plan, e)
	if len(notes) != 0 {
		t.Fatal("源多分支时不应改写源 SQL")
	}
}
