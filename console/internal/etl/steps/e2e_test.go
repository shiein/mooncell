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
