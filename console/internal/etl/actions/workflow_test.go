package actions_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

// fakeHost 同时扮演连接池宿主、转换执行器与子作业执行器。
type fakeHost struct {
	env       core.MapEnv
	pipelines map[string]*pipeline.Definition
	workflows map[string]*workflow.Definition
	runs      atomic.Int32
	wfRuns    atomic.Int32
	depth     atomic.Int32
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

func (h *fakeHost) WorkflowParams(ctx context.Context, id string) (string, []core.ParamDecl, error) {
	d, ok := h.workflows[id]
	if !ok {
		return "", nil, fmt.Errorf("no workflow %s", id)
	}
	return id, d.Params, nil
}

func (h *fakeHost) RunWorkflow(ctx context.Context, ec *workflow.ExecContext, id string, params map[string]core.Value) (actions.WorkflowSummary, error) {
	if d := h.depth.Add(1); d > 8 {
		h.depth.Add(-1)
		return actions.WorkflowSummary{}, fmt.Errorf("子作业嵌套超过 8 层")
	}
	defer h.depth.Add(-1)
	h.wfRuns.Add(1)
	def, ok := h.workflows[id]
	if !ok {
		return actions.WorkflowSummary{}, fmt.Errorf("no workflow %s", id)
	}
	vars := core.NewVars(ec.Vars)
	for k, v := range params {
		vars.Set(k, v)
	}
	plan := workflow.Compile(ctx, def, workflow.CompileOptions{Env: h.env})
	if plan.Problems.HasErrors() {
		return actions.WorkflowSummary{}, fmt.Errorf("子作业编译失败：%v", plan.Problems.First())
	}
	res := workflow.Run(ctx, plan, workflow.RunOptions{Env: h.env, Vars: vars, Log: ec.Log})
	var sum actions.WorkflowSummary
	for _, ev := range res.Nodes {
		switch ev.Status {
		case workflow.NodeSucceeded, workflow.NodeReused:
			sum.Succeeded++
		case workflow.NodeFailed:
			sum.Failed++
		default:
			sum.Skipped++
		}
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
	h := &fakeHost{pipelines: map[string]*pipeline.Definition{}, workflows: map[string]*workflow.Definition{}}
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

// HTTP 作业项：提取响应字段写变量，非 2xx 走失败分支。
func TestHTTPAction(t *testing.T) {
	h, _, _ := setup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			w.WriteHeader(503)
			return
		}
		if r.Header.Get("X-Token") != "tk-9" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"total":42,"token":"abc-1"}}`)
	}))
	defer srv.Close()

	def := &workflow.Definition{
		Nodes: []workflow.Node{
			wnode("start", "start", nil),
			wnode("call", "http", map[string]any{
				"url": srv.URL + "/api?tag=${who}", "method": "GET",
				"headers": map[string]string{"X-Token": "tk-9"}, "statusVar": "code",
				"extract": []map[string]any{
					{"path": "data.total", "var": "total", "kind": "integer"},
					{"path": "data.token", "var": "tok", "kind": "string"},
				}}),
			wnode("big", "condition", map[string]any{"expr": "total > 10 && code == 200 && tok == 'abc-1'"}),
			wnode("ok", "success", nil),
			wnode("bad", "abort", map[string]any{"message": "提取结果不对"}),
		},
		Hops: []workflow.Hop{
			whop("start", "success", "call"), whop("call", "success", "big"),
			whop("big", "true", "ok"), whop("big", "false", "bad"),
		},
	}
	def.Params = []core.ParamDecl{{Name: "who", Kind: core.KindString, Default: &core.Value{Kind: core.KindString, V: "etl"}}}
	res := runWF(t, h, def)
	if res.Status != workflow.NodeSucceeded || res.Nodes["ok"].Status != workflow.NodeSucceeded {
		t.Fatalf("HTTP 提取+条件应成功：%v ok=%s", res.Err, res.Nodes["ok"].Status)
	}
	if res.Nodes["call"].Vars["total"].V != int64(42) || res.Nodes["call"].Vars["code"].V != int64(200) {
		t.Fatalf("变量提取错误：%+v", res.Nodes["call"].Vars)
	}

	// 非 2xx → 失败，失败连线处理则作业成功
	def2 := &workflow.Definition{
		Nodes: []workflow.Node{
			wnode("start", "start", nil),
			wnode("call", "http", map[string]any{"url": srv.URL + "/fail"}),
			wnode("onfail", "success", nil),
		},
		Hops: []workflow.Hop{whop("start", "success", "call"), whop("call", "failure", "onfail")},
	}
	res = runWF(t, h, def2)
	if res.Status != workflow.NodeSucceeded || res.Nodes["call"].Status != workflow.NodeFailed || res.Nodes["onfail"].Status != workflow.NodeSucceeded {
		t.Fatalf("503 应失败并被 failure 连线处理：%v call=%s onfail=%s", res.Err, res.Nodes["call"].Status, res.Nodes["onfail"].Status)
	}
}

// 子作业：参数表达式求值后传入；嵌套过深报错。
func TestSubWorkflow(t *testing.T) {
	h, a, _ := setup(t)
	exec(t, a, `CREATE TABLE log (msg TEXT)`)

	h.workflows["child"] = &workflow.Definition{
		Params: []core.ParamDecl{{Name: "n", Kind: core.KindInteger}},
		Nodes: []workflow.Node{
			wnode("start", "start", nil),
			wnode("w", "sql", map[string]any{"connection": "A", "sql": "insert into log values ('child-' || ${n})"}),
		},
		Hops: []workflow.Hop{whop("start", "success", "w")},
	}
	h.workflows["self"] = &workflow.Definition{
		Nodes: []workflow.Node{wnode("start", "start", nil), wnode("w", "workflow", map[string]any{"workflow": "self"})},
		Hops:  []workflow.Hop{whop("start", "success", "w")},
	}

	parent := &workflow.Definition{
		Nodes: []workflow.Node{
			wnode("start", "start", nil),
			wnode("sv", "set_vars", map[string]any{"assign": []map[string]any{{"var": "base", "kind": "integer", "expr": "5"}}}),
			wnode("sub", "workflow", map[string]any{"workflow": "child", "params": map[string]string{"n": "base + 2"}}),
			wnode("end", "success", nil),
		},
		Hops: []workflow.Hop{whop("start", "success", "sv"), whop("sv", "success", "sub"), whop("sub", "success", "end")},
	}
	res := runWF(t, h, parent)
	if res.Status != workflow.NodeSucceeded {
		t.Fatalf("子作业应成功：%v", res.Err)
	}
	var msg string
	if err := a.QueryRow(`SELECT msg FROM log`).Scan(&msg); err != nil || msg != "child-7" {
		t.Fatalf("子作业参数求值错误：msg=%q err=%v", msg, err)
	}

	// 自引用 → 嵌套上限失败
	loop := &workflow.Definition{
		Nodes: []workflow.Node{wnode("start", "start", nil), wnode("w", "workflow", map[string]any{"workflow": "self"})},
		Hops:  []workflow.Hop{whop("start", "success", "w")},
	}
	res = runWF(t, h, loop)
	if res.Status != workflow.NodeFailed || !strings.Contains(fmt.Sprint(res.Err), "嵌套") {
		t.Fatalf("循环子作业应失败并提示嵌套：%v", res.Err)
	}
}

// 通知：Webhook 收到模板正文与明文凭据注入的头；密文路径走注入的打开器。
func TestNotify(t *testing.T) {
	h, _, _ := setup(t)
	var gotAuth, gotBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		b, _ := io.ReadAll(r.Body)
		gotBody.Store(string(b))
	}))
	defer srv.Close()

	// 密文路径：注入假打开器
	sk, sv := actions.SecretEntry(openerFunc(func(sealed, aad string) (string, error) {
		return "Bearer " + sealed, nil
	}))
	h.env[sk] = sv

	def := &workflow.Definition{
		Nodes: []workflow.Node{
			wnode("start", "start", nil),
			wnode("sv", "set_vars", map[string]any{"assign": []map[string]any{{"var": "rid", "kind": "string", "expr": "'r1'"}}}),
			wnode("n", "notify", map[string]any{
				"url":          srv.URL + "/hook",
				"template":     `{"run":"${rid}","ok":true}`,
				"secretHeader": "Authorization", "secretCipher": "sealed-token",
			}),
		},
		Hops: []workflow.Hop{whop("start", "success", "sv"), whop("sv", "success", "n")},
	}
	res := runWF(t, h, def)
	if res.Status != workflow.NodeSucceeded {
		t.Fatalf("通知应成功：%v", res.Err)
	}
	if gotAuth.Load() != "Bearer sealed-token" {
		t.Fatalf("凭据头错误：%v", gotAuth.Load())
	}
	if body := gotBody.Load().(string); !strings.Contains(body, `"run":"r1"`) {
		t.Fatalf("正文模板未替换变量：%s", body)
	}
}

type openerFunc func(string, string) (string, error)

func (f openerFunc) Open(sealed, aad string) (string, error) { return f(sealed, aad) }
