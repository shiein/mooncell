package actions_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"mooncell/console/internal/etl/actions"
	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/pipeline"
	_ "mooncell/console/internal/etl/steps"
	"mooncell/console/internal/etl/workflow"
)

// fakeHost 同时扮演连接池宿主与转换执行器。
type fakeHost struct {
	env       core.MapEnv
	pipelines map[string]*pipeline.Definition
	runs      atomic.Int32
}

func (h *fakeHost) PipelineParams(ctx context.Context, id string) (string, []core.ParamDecl, error) {
	d, ok := h.pipelines[id]
	if !ok {
		return "", nil, fmt.Errorf("no pipeline %s", id)
	}
	return id, d.Params, nil
}

func (h *fakeHost) RunPipeline(ctx context.Context, ec *workflow.ExecContext, id string, params map[string]core.Value) (actions.PipelineSummary, error) {
	h.runs.Add(1)
	def := h.pipelines[id]
	vars := core.NewVars(ec.Vars)
	for k, v := range params {
		vars.Set(k, v)
	}
	plan := pipeline.Compile(ctx, def, pipeline.CompileOptions{Env: h.env})
	res := pipeline.Run(ctx, plan, pipeline.RunOptions{Env: h.env, Vars: vars, Log: ec.Log})
	var sum actions.PipelineSummary
	for _, m := range res.Metrics {
		sum.Read += m.Input
		sum.Written += m.Output + m.Updated
	}
	return sum, res.Err
}

func setup(t *testing.T) (*fakeHost, *sql.DB, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	cfgs := map[string]conn.Config{
		"A": {ID: "A", DBType: "sqlite", Database: filepath.Join(dir, "a.db")},
		"B": {ID: "B", DBType: "sqlite", Database: filepath.Join(dir, "b.db")},
	}
	pool := conn.NewPool(func(ctx context.Context, id string) (conn.Config, string, error) {
		c, ok := cfgs[id]
		if !ok {
			return c, "", fmt.Errorf("unknown %s", id)
		}
		return c, "", nil
	})
	t.Cleanup(pool.Close)
	h := &fakeHost{pipelines: map[string]*pipeline.Definition{}}
	pk, pv := pool.EnvEntry()
	rk, rv := actions.RunnerEntry(h)
	h.env = core.MapEnv{pk: pv, rk: rv}
	a, _ := pool.Get(context.Background(), "A")
	b, _ := pool.Get(context.Background(), "B")
	return h, a.DB, b.DB
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func cfg(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func wnode(id, typ string, c any) workflow.Node {
	return workflow.Node{ID: id, Type: typ, Name: id, Config: cfg(c)}
}

func whop(from, port, to string) workflow.Hop {
	return workflow.Hop{ID: from + port + to, From: from, FromPort: port, To: to}
}

func runWF(t *testing.T, h *fakeHost, def *workflow.Definition) workflow.Result {
	t.Helper()
	plan := workflow.Compile(context.Background(), def, workflow.CompileOptions{Env: h.env})
	if plan.Problems.HasErrors() {
		t.Fatalf("作业编译失败：%+v", plan.Problems)
	}
	vars := core.NewVars(nil)
	sink := &core.MemorySink{}
	res := workflow.Run(context.Background(), plan, workflow.RunOptions{Env: h.env, Vars: vars, Log: core.NewLogger(sink, core.LevelDebug)})
	if res.Status != workflow.NodeSucceeded {
		for _, e := range sink.Snapshot() {
			t.Log(e.Level, e.NodeID, e.Message)
		}
	}
	return res
}

// 典型场景：A 取 max(ts) → B 按 ts 计数 → 超过阈值才把 B 的增量同步到 A。
func TestTypicalIncrementalSync(t *testing.T) {
	h, a, b := setup(t)
	exec(t, a, `CREATE TABLE orders (order_no TEXT PRIMARY KEY, amount DECIMAL(12,2), ts TEXT)`)
	exec(t, b, `CREATE TABLE orders (order_no TEXT, amount TEXT, ts TEXT, junk TEXT)`)
	exec(t, b, `CREATE INDEX ix ON orders(order_no)`)
	exec(t, a, `INSERT INTO orders VALUES ('O-1', 10, '2026-11-01 00:00:00')`)
	for i := 1; i <= 12; i++ {
		exec(t, b, `INSERT INTO orders VALUES (?, ?, ?, 'x')`, fmt.Sprintf("O-%d", i), fmt.Sprintf("%d.50", i), fmt.Sprintf("2026-11-%02d 08:00:00", i))
	}

	h.pipelines["sync"] = &pipeline.Definition{
		Params: []core.ParamDecl{{Name: "since", Kind: core.KindTimestamp}},
		Nodes: []pipeline.Node{
			{ID: "in", Type: "table_input", Name: "读 B", Config: cfg(map[string]any{"connection": "B", "sql": "select order_no, amount, ts from orders where ts > ${since}"})},
			{ID: "iu", Type: "insert_update", Name: "写 A", Config: cfg(map[string]any{
				"connection": "A", "table": "orders", "skipUnchanged": true, "commitSize": 5,
				"keys":   []map[string]any{{"stream": "order_no", "column": "order_no"}},
				"fields": []map[string]any{{"stream": "amount", "column": "amount", "update": true}, {"stream": "ts", "column": "ts", "update": true}},
			})},
		},
		Hops: []pipeline.Hop{{ID: "h", From: "in", FromPort: "main", To: "iu", ToPort: "main"}},
	}

	def := &workflow.Definition{
		Params: []core.ParamDecl{{Name: "threshold", Kind: core.KindInteger, Default: &core.Value{Kind: core.KindInteger, V: int64(5)}}},
		Nodes: []workflow.Node{
			wnode("start", "start", nil),
			wnode("max", "sql_value", map[string]any{"connection": "A", "sql": "select max(ts) as maxts from orders",
				"assign": []map[string]any{{"column": "maxTs", "var": "maxTs", "kind": "timestamp"}}}),
			wnode("cnt", "sql_value", map[string]any{"connection": "B", "sql": "select count(*) as cnt from orders where ts > ${maxTs}",
				"assign": []map[string]any{{"column": "cnt", "var": "cnt", "kind": "integer"}}}),
			wnode("cond", "condition", map[string]any{"expr": "cnt > 5"}),
			wnode("sync", "pipeline", map[string]any{"pipeline": "sync", "params": map[string]string{"since": "maxTs"}}),
			wnode("skip", "success", nil),
			wnode("done", "success", nil),
		},
		Hops: []workflow.Hop{
			whop("start", "success", "max"), whop("max", "success", "cnt"), whop("cnt", "success", "cond"),
			whop("cond", "true", "sync"), whop("cond", "false", "skip"), whop("sync", "success", "done"),
		},
	}
	res := runWF(t, h, def)
	if res.Status != workflow.NodeSucceeded {
		t.Fatalf("作业应成功：%v", res.Err)
	}
	if res.Nodes["cnt"].Vars["cnt"].V != int64(12) {
		t.Fatalf("B 中 ts > maxTs 的行数应为 12，实际 %+v", res.Nodes["cnt"].Vars)
	}
	if res.Nodes["skip"].Status != workflow.NodeSkipped || res.Nodes["done"].Status != workflow.NodeSucceeded {
		t.Fatalf("分支执行错误：skip=%s done=%s", res.Nodes["skip"].Status, res.Nodes["done"].Status)
	}
	var n int
	a.QueryRow(`SELECT count(*) FROM orders`).Scan(&n)
	if n != 12 {
		t.Fatalf("A 应有 12 行，实际 %d", n)
	}
	var amount string
	a.QueryRow(`SELECT CAST(amount AS TEXT) FROM orders WHERE order_no='O-7'`).Scan(&amount)
	if amount != "7.5" {
		t.Fatalf("金额写入错误：%s", amount)
	}

	// 再跑一次：A 已是最新 → cnt=0 → 走「否」分支，不运行转换
	before := h.runs.Load()
	res = runWF(t, h, def)
	if res.Nodes["cond"].Port != "false" || res.Nodes["sync"].Status != workflow.NodeSkipped || h.runs.Load() != before {
		t.Fatalf("无增量时应跳过同步：cond=%s sync=%s", res.Nodes["cond"].Port, res.Nodes["sync"].Status)
	}
}

// 失败分支 + 汇合：失败被 failure 连线处理时作业成功；汇合节点只执行一次；未激活分支跳过并传播。
func TestFailureHandlingAndJoin(t *testing.T) {
	h, a, _ := setup(t)
	exec(t, a, `CREATE TABLE log (msg TEXT)`)
	def := &workflow.Definition{
		Nodes: []workflow.Node{
			wnode("start", "start", nil),
			wnode("bad", "sql", map[string]any{"connection": "A", "sql": "insert into no_such_table values (1)"}),
			wnode("good", "sql", map[string]any{"connection": "A", "sql": "insert into log values ('ok')"}),
			wnode("onfail", "sql", map[string]any{"connection": "A", "sql": "insert into log values ('handled')"}),
			wnode("never", "sql", map[string]any{"connection": "A", "sql": "insert into log values ('never')"}),
			wnode("after_never", "sql", map[string]any{"connection": "A", "sql": "insert into log values ('after_never')"}),
			wnode("join", "sql", map[string]any{"connection": "A", "sql": "insert into log values ('join')"}),
		},
		Hops: []workflow.Hop{
			whop("start", "success", "bad"), whop("start", "success", "good"),
			whop("bad", "failure", "onfail"), whop("bad", "success", "never"), whop("never", "success", "after_never"),
			whop("onfail", "success", "join"), whop("good", "success", "join"), whop("after_never", "success", "join"),
		},
	}
	res := runWF(t, h, def)
	if res.Status != workflow.NodeSucceeded {
		t.Fatalf("失败已被处理，作业应成功：%v", res.Err)
	}
	for id, want := range map[string]string{"bad": "failed", "onfail": "succeeded", "never": "skipped", "after_never": "skipped", "join": "succeeded"} {
		if got := res.Nodes[id].Status; got != want {
			t.Errorf("%s 状态 %s，期望 %s", id, got, want)
		}
	}
	var joins int
	a.QueryRow(`SELECT count(*) FROM log WHERE msg='join'`).Scan(&joins)
	if joins != 1 {
		t.Fatalf("汇合节点应只执行一次，实际 %d 次", joins)
	}
}

// 未处理失败使作业失败并停止后续；重试次数生效；中止返回失败。
func TestUnhandledFailureRetryAndAbort(t *testing.T) {
	h, _, _ := setup(t)
	def := &workflow.Definition{
		Nodes: []workflow.Node{
			wnode("start", "start", nil),
			{ID: "bad", Type: "sql", Name: "bad", Config: cfg(map[string]any{"connection": "A", "sql": "select * from nope"}), Retry: workflow.Retry{Max: 2}},
			wnode("next", "wait", map[string]any{"seconds": 0}),
		},
		Hops: []workflow.Hop{whop("start", "success", "bad"), whop("bad", "success", "next")},
	}
	start := time.Now()
	res := runWF(t, h, def)
	if res.Status != workflow.NodeFailed || res.Nodes["bad"].Attempt != 3 || res.Nodes["next"].Status != workflow.NodeSkipped {
		t.Fatalf("应重试 2 次后失败并跳过后续：%s attempt=%d next=%s", res.Status, res.Nodes["bad"].Attempt, res.Nodes["next"].Status)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("无退避重试不应耗时")
	}

	abort := &workflow.Definition{
		Params: []core.ParamDecl{{Name: "cnt", Kind: core.KindInteger}},
		Nodes:  []workflow.Node{wnode("start", "start", nil), wnode("stop", "abort", map[string]any{"message": "数量异常：${cnt}"})},
		Hops:   []workflow.Hop{whop("start", "success", "stop")},
	}
	res = runWF(t, h, abort)
	if res.Status != workflow.NodeFailed || res.Err == nil {
		t.Fatalf("中止应使作业失败：%+v", res)
	}
}

// 编译期：缺开始、环、未声明变量、错误端口。
func TestWorkflowCompileProblems(t *testing.T) {
	h, _, _ := setup(t)
	def := &workflow.Definition{
		Nodes: []workflow.Node{
			wnode("a", "condition", map[string]any{"expr": "nope > 1"}),
			wnode("b", "wait", map[string]any{"seconds": 1}),
			wnode("c", "wait", map[string]any{"seconds": 1}),
		},
		Hops: []workflow.Hop{whop("b", "success", "c"), whop("c", "success", "b"), whop("a", "success", "b")},
	}
	plan := workflow.Compile(context.Background(), def, workflow.CompileOptions{Env: h.env})
	codes := map[string]bool{}
	for _, p := range plan.Problems {
		codes[p.Code] = true
	}
	for _, c := range []string{"start_missing", "cycle", "expr", "port"} {
		if !codes[c] {
			t.Errorf("缺少问题 %s：%+v", c, plan.Problems)
		}
	}
}
