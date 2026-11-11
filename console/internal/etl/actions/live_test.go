//go:build etl_live

// 真机端到端：与 SQLite 版 TestTypicalIncrementalSync 同一场景
// 「A 取 max(ts) → B 计数 → 条件 → 增量插入/更新」，验证各方言上的完整链路。
package actions_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"mooncell/console/internal/etl/actions"
	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/livetest"
	"mooncell/console/internal/etl/pipeline"
	_ "mooncell/console/internal/etl/steps"
	"mooncell/console/internal/etl/workflow"
)

func TestLiveTypicalIncrementalSync(t *testing.T) {
	for _, dialect := range []string{"mysql", "dm", "kingbase", "postgres"} {
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			liveSync(t, dialect)
		})
	}
}

// liveSync：同一库内 orders_src(B 侧) → orders(A 侧) 增量同步。
func liveSync(t *testing.T, dialect string) {
	t.Helper()
	cfg, pw := livetest.Config(t, dialect)
	cfg.Options.TimeZone = "Asia/Shanghai"
	ctx := context.Background()
	h, err := conn.Open(ctx, cfg, pw)
	if err != nil {
		t.Fatalf("连接失败：%v", err)
	}
	defer h.DB.Close()
	d := h.Dialect
	schema := d.FoldIdent(conn.EffectiveSchema(cfg, ""))
	src := conn.TableRef{Schema: schema, Name: d.FoldIdent("orders_src")}
	dst := conn.TableRef{Schema: schema, Name: d.FoldIdent("orders")}
	for _, r := range []conn.TableRef{src, dst} {
		h.DB.ExecContext(ctx, "DROP TABLE IF EXISTS "+d.Qualified(r))
	}
	t.Cleanup(func() {
		h.DB.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+d.Qualified(src))
		h.DB.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+d.Qualified(dst))
	})

	ddlA, ddlB := syncDDL(dialect)
	if _, err := h.DB.ExecContext(ctx, fmt.Sprintf(ddlB, d.Qualified(src))); err != nil {
		t.Fatalf("建 B 表失败：%v", err)
	}
	if _, err := h.DB.ExecContext(ctx, fmt.Sprintf(ddlA, d.Qualified(dst))); err != nil {
		t.Fatalf("建 A 表失败：%v", err)
	}
	// A 已有 O-1（2026-11-01）；B 有 O-1..O-12（每天 08:00）
	mustExecLive(t, h, fmt.Sprintf("INSERT INTO %s (order_no, amount, ts) VALUES ('O-1', 10, '2026-11-01 00:00:00')", d.Qualified(dst)))
	for i := 1; i <= 12; i++ {
		mustExecLive(t, h, fmt.Sprintf("INSERT INTO %s VALUES ('O-%d', %s, '2026-11-%02d 08:00:00', 'x')",
			d.Qualified(src), i, amountLit(dialect, i), i))
	}

	// 连接池 + 作业宿主
	pool := conn.NewPool(func(ctx context.Context, id string) (conn.Config, string, error) {
		if id != "live" {
			return conn.Config{}, "", fmt.Errorf("未知连接 %s", id)
		}
		return cfg, pw, nil
	})
	defer pool.Close()
	host := &liveHost{pipelines: map[string]*pipeline.Definition{}, workflows: map[string]*workflow.Definition{}}
	pk, pv := pool.EnvEntry()
	rk, rv := actions.RunnerEntry(host)
	env := core.MapEnv{pk: pv, rk: rv}
	host.env = env

	host.pipelines["sync"] = &pipeline.Definition{
		Params: []core.ParamDecl{{Name: "since", Kind: core.KindTimestamp}},
		Nodes: []pipeline.Node{
			{ID: "in", Type: "table_input", Name: "读B", Config: cfgJSON(map[string]any{
				"connection": "live", "sql": fmt.Sprintf("select order_no, amount, ts from %s where ts > ${since}", d.Qualified(src))})},
			{ID: "iu", Type: "insert_update", Name: "写A", Config: cfgJSON(map[string]any{
				"connection": "live", "schema": schema, "table": "orders", "skipUnchanged": true, "commitSize": 5,
				"keys":   []map[string]any{{"stream": keyCol(dialect, "order_no"), "column": "order_no"}},
				"fields": []map[string]any{{"stream": keyCol(dialect, "amount"), "column": "amount", "update": true}, {"stream": keyCol(dialect, "ts"), "column": "ts", "update": true}},
			})},
		},
		Hops: []pipeline.Hop{{ID: "h", From: "in", FromPort: "main", To: "iu", ToPort: "main"}},
	}

	def := &workflow.Definition{
		Nodes: []workflow.Node{
			wnode("start", "start", nil),
			wnode("max", "sql_value", map[string]any{"connection": "live",
				"sql":    fmt.Sprintf("select max(ts) as maxts from %s", d.Qualified(dst)),
				"assign": []map[string]any{{"column": colAlias(dialect, "maxTs"), "var": "maxTs", "kind": "timestamp"}}}),
			wnode("cnt", "sql_value", map[string]any{"connection": "live",
				"sql":    fmt.Sprintf("select count(*) as cnt from %s where ts > ${maxTs}", d.Qualified(src)),
				"assign": []map[string]any{{"column": colAlias(dialect, "cnt"), "var": "cnt", "kind": "integer"}}}),
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

	plan := workflow.Compile(ctx, def, workflow.CompileOptions{Env: env})
	if plan.Problems.HasErrors() {
		t.Fatalf("编译失败：%+v", plan.Problems)
	}
	res := workflow.Run(ctx, plan, workflow.RunOptions{Env: env})
	if res.Status != workflow.NodeSucceeded {
		t.Fatalf("作业应成功：%v（skip=%s done=%s）", res.Err, res.Nodes["skip"].Status, res.Nodes["done"].Status)
	}
	if res.Nodes["cnt"].Vars["cnt"].V != int64(12) {
		t.Fatalf("B 侧增量应为 12：%+v", res.Nodes["cnt"].Vars)
	}
	var n int
	mustQuery(t, h, fmt.Sprintf("SELECT count(*) FROM %s", d.Qualified(dst)), &n)
	if n != 12 {
		t.Fatalf("A 应有 12 行，实际 %d", n)
	}
	var amount string
	mustQuery(t, h, fmt.Sprintf("SELECT CAST(amount AS %s) FROM %s WHERE order_no = 'O-7'", castType(dialect), d.Qualified(dst)), &amount)
	if amount != "7.5" && amount != "7.50" {
		t.Fatalf("金额写入错误：%s", amount)
	}

	// 重跑：无增量 → 条件走 false → sync 跳过
	res = workflow.Run(ctx, workflow.Compile(ctx, def, workflow.CompileOptions{Env: env}), workflow.RunOptions{Env: env})
	if res.Nodes["cond"].Port != "false" || res.Nodes["sync"].Status != workflow.NodeSkipped {
		t.Fatalf("无增量应跳过：cond=%s sync=%s", res.Nodes["cond"].Port, res.Nodes["sync"].Status)
	}
	t.Logf("%s 典型场景通过（含重跑分支）", dialect)
}

// syncDDL 返回 (A表DDL, B表DDL)；%s 处替换为带 schema 的限定名。
func syncDDL(dialect string) (a, b string) {
	switch dialect {
	case "mysql":
		return `CREATE TABLE %s (order_no VARCHAR(32) PRIMARY KEY, amount DECIMAL(12,2), ts DATETIME)`,
			`CREATE TABLE %s (order_no VARCHAR(32), amount DECIMAL(12,2), ts DATETIME, junk VARCHAR(8), INDEX ix_o (order_no))`
	case "dm":
		return `CREATE TABLE %s (order_no VARCHAR(32) PRIMARY KEY, amount DECIMAL(12,2), ts TIMESTAMP)`,
			`CREATE TABLE %s (order_no VARCHAR(32), amount DECIMAL(12,2), ts TIMESTAMP, junk VARCHAR(8))`
	default: // postgres / kingbase
		return `CREATE TABLE %s (order_no VARCHAR(32) PRIMARY KEY, amount NUMERIC(12,2), ts TIMESTAMP)`,
			`CREATE TABLE %s (order_no VARCHAR(32), amount NUMERIC(12,2), ts TIMESTAMP, junk VARCHAR(8))`
	}
}

// keyCol 返回流字段名（DM 读出列名大写）。
func keyCol(dialect, name string) string {
	if dialect == "dm" {
		return upperASCII(name)
	}
	return name
}

func colAlias(dialect, name string) string { return keyCol(dialect, name) }

func upperASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}

func amountLit(dialect string, i int) string {
	if dialect == "dm" {
		return fmt.Sprintf("CAST('%d.50' AS DECIMAL(12,2))", i)
	}
	return fmt.Sprintf("%d.50", i)
}

func castType(dialect string) string {
	switch dialect {
	case "mysql":
		return "CHAR"
	case "dm":
		return "VARCHAR(32)"
	default:
		return "TEXT"
	}
}

func mustExecLive(t *testing.T, h *conn.Handle, q string, args ...any) {
	t.Helper()
	if _, err := h.DB.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func mustQuery(t *testing.T, h *conn.Handle, q string, out ...any) {
	t.Helper()
	if err := h.DB.QueryRowContext(context.Background(), q).Scan(out...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func cfgJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// liveHost 实现 actions.PipelineRunner：直接编译并运行内存中的转换/作业定义。
type liveHost struct {
	pipelines map[string]*pipeline.Definition
	workflows map[string]*workflow.Definition
	env       core.Env
}

func (h *liveHost) PipelineParams(ctx context.Context, id string) (string, []core.ParamDecl, error) {
	d, ok := h.pipelines[id]
	if !ok {
		return "", nil, fmt.Errorf("no pipeline %s", id)
	}
	return id, d.Params, nil
}

func (h *liveHost) RunPipeline(ctx context.Context, ec *workflow.ExecContext, id string, params map[string]core.Value) (actions.PipelineSummary, error) {
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

func (h *liveHost) WorkflowParams(ctx context.Context, id string) (string, []core.ParamDecl, error) {
	d, ok := h.workflows[id]
	if !ok {
		return "", nil, fmt.Errorf("no workflow %s", id)
	}
	return id, d.Params, nil
}

func (h *liveHost) RunWorkflow(ctx context.Context, ec *workflow.ExecContext, id string, params map[string]core.Value) (actions.WorkflowSummary, error) {
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
