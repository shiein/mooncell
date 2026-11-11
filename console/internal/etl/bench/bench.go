//go:build etl_live && etl_bench

package bench

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/pipeline"
	_ "mooncell/console/internal/etl/steps"
)

// Options 压测规模与提交档。
type Options struct {
	Rows    int64  // 源表行数
	Commits []int  // 输出步骤 commitSize 档
	Target  string // 目标库方言：postgres | mysql（源恒为 PG）
}

// FromEnv 读取 BENCH_ROWS（默认 1000 万）、BENCH_COMMITS（默认 1000,5000,20000）、
// BENCH_TARGET（默认 mysql；设 postgres 即同库 PG→PG 压测）。
func FromEnv() Options {
	o := Options{Rows: 10_000_000, Commits: []int{1000, 5000, 20000}, Target: "mysql"}
	if s := os.Getenv("BENCH_TARGET"); s != "" {
		o.Target = s
	}
	if s := os.Getenv("BENCH_ROWS"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
			o.Rows = n
		}
	}
	if s := os.Getenv("BENCH_COMMITS"); s != "" {
		var cs []int
		for _, p := range strings.Split(s, ",") {
			if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && n > 0 {
				cs = append(cs, n)
			}
		}
		if len(cs) > 0 {
			o.Commits = cs
		}
	}
	return o
}

// Result 是单场景×提交档的实测数据。
type Result struct {
	Scenario   string        // output | iu_insert | iu_update | iu_update_case
	Commit     int           // 输出步骤 commitSize
	Rows       int64         // 处理行数
	Duration   time.Duration // 管道端到端耗时
	RSSStartMB float64       // 场景开始时的进程峰值 RSS（ru_maxrss 单调，表示此前高水位）
	PeakRSSMB  float64       // 场景结束时的进程峰值 RSS（getrusage，真实常驻内存峰值）
}

// RowsPerSec 返回端到端吞吐。
func (r Result) RowsPerSec() int64 {
	if r.Duration <= 0 {
		return 0
	}
	return int64(float64(r.Rows) / r.Duration.Seconds())
}

// String 一行报表。
func (r Result) String() string {
	return fmt.Sprintf("%-15s commit=%-6d rows=%-9d 耗时=%-9s 吞吐=%-8d行/s 峰值RSS=%.0fMB(起点%.0f)",
		r.Scenario, r.Commit, r.Rows, r.Duration.Truncate(time.Second).String(), r.RowsPerSec(), r.PeakRSSMB, r.RSSStartMB)
}

func cfgJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// runPipeline 编译并运行「table_input → 输出步骤」两节点转换，返回度量快照。
func runPipeline(ctx context.Context, env core.Env, srcConn, sqlText string, out pipeline.Node) (map[string]core.MetricsSnapshot, error) {
	def := &pipeline.Definition{
		Settings: pipeline.Settings{BatchSize: 1000, ChannelBatches: 16},
		Nodes: []pipeline.Node{
			{ID: "in", Type: "table_input", Name: "读源", Config: cfgJSON(map[string]any{
				"connection": srcConn, "sql": sqlText})},
			out,
		},
		Hops: []pipeline.Hop{{ID: "h", From: "in", FromPort: "main", To: out.ID, ToPort: "main"}},
	}
	plan := pipeline.Compile(ctx, def, pipeline.CompileOptions{Env: env})
	if plan.Problems.HasErrors() {
		return nil, fmt.Errorf("编译失败：%+v", plan.Problems)
	}
	sink := &core.MemorySink{}
	res := pipeline.Run(ctx, plan, pipeline.RunOptions{
		Env: env, Log: core.NewLogger(sink, core.LevelDebug)})
	for _, l := range sink.Snapshot() {
		if strings.Contains(l.Message, "提交") || strings.Contains(l.Message, "查找") {
			fmt.Printf("    [%s] %s\n", time.UnixMilli(l.TS).Format("15:04:05.000"), l.Message)
		}
	}
	return res.Metrics, res.Err
}

// srcRef 源表引用（schema = 连接有效 schema）。
func srcRef(pg *conn.Handle) conn.TableRef {
	d := pg.Dialect
	return conn.TableRef{Schema: d.FoldIdent(conn.EffectiveSchema(pg.Config, "")), Name: "etl_bench_src"}
}

// srcSQL 是 PG 侧显式列清单的源查询（不走 SELECT *，不依赖 search_path）。
func srcSQL(pg *conn.Handle) string {
	return fmt.Sprintf(`SELECT id, name, amount, ts, flag FROM %s`, pg.Dialect.Qualified(srcRef(pg)))
}

// PrepareSource 在 PG 重建 etl_bench_src 并填充 rows 行（含整型/文本/精确小数/时间/布尔）。
func PrepareSource(ctx context.Context, pg *conn.Handle, rows int64) error {
	d := pg.Dialect
	ref := srcRef(pg)
	pg.DB.ExecContext(ctx, "DROP TABLE IF EXISTS "+d.Qualified(ref))
	if _, err := pg.DB.ExecContext(ctx, `CREATE TABLE `+d.Qualified(ref)+
		` (id BIGINT, name VARCHAR(64), amount NUMERIC(14,2), ts TIMESTAMP, flag BOOLEAN)`); err != nil {
		return fmt.Errorf("建源表失败：%w", err)
	}
	if _, err := pg.DB.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO %s SELECT i, 'row-' || i, (i %% 100000) / 100.0,
		 TIMESTAMP '2020-01-01' + (i || ' seconds')::interval, (i %% 2) = 0
		 FROM generate_series(1, %d) AS i`, d.Qualified(ref), rows)); err != nil {
		return fmt.Errorf("填源表 %d 行失败：%w", rows, err)
	}
	return nil
}

// targetRef 目标表引用（schema = 连接有效 schema）。
func targetRef(h *conn.Handle, name string) conn.TableRef {
	return conn.TableRef{Schema: conn.EffectiveSchema(h.Config, ""), Name: h.Dialect.FoldIdent(name)}
}

// colTypes 按目标方言给出列类型：仅覆盖 BENCH_TARGET 支持的 postgres|mysql。
func colTypes(d conn.Dialect) (dec, ts, flag string, err error) {
	switch d.ID() {
	case "mysql":
		return "DECIMAL(14,2)", "DATETIME", "TINYINT(1)", nil
	case "postgres":
		return "NUMERIC(14,2)", "TIMESTAMP", "BOOLEAN", nil
	}
	return "", "", "", fmt.Errorf("BENCH_TARGET 暂不支持方言 %s（仅 postgres|mysql）", d.ID())
}

// resetTarget 在目标库重建表；pk=true 带主键（upsert 路径），false 仅普通索引（CASE 路径）。
func resetTarget(ctx context.Context, h *conn.Handle, name string, pk bool) error {
	d := h.Dialect
	ref := targetRef(h, name)
	dec, tst, flg, err := colTypes(d)
	if err != nil {
		return err
	}
	h.DB.ExecContext(ctx, "DROP TABLE IF EXISTS "+d.Qualified(ref))
	if _, err := h.DB.ExecContext(ctx, fmt.Sprintf(
		`CREATE TABLE %s (id BIGINT, name VARCHAR(64), amount %s, ts %s, flag %s)`,
		d.Qualified(ref), dec, tst, flg)); err != nil {
		return fmt.Errorf("建目标表 %s 失败：%w", name, err)
	}
	var idx string
	if pk {
		idx = "ALTER TABLE " + d.Qualified(ref) + " ADD PRIMARY KEY (id)"
	} else {
		idx = fmt.Sprintf("CREATE INDEX %s ON %s (id)", d.Quote("ix_"+ref.Name), d.Qualified(ref))
	}
	if _, err := h.DB.ExecContext(ctx, idx); err != nil {
		return fmt.Errorf("建目标表 %s 索引失败：%w", name, err)
	}
	return nil
}

// fillTarget 用库内 SELECT 把 src 表内容复制进 dst（同库，避免再过一次管道）。
func fillTarget(ctx context.Context, h *conn.Handle, dst, src string) error {
	d := h.Dialect
	dr, sr := targetRef(h, dst), targetRef(h, src)
	if _, err := h.DB.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO %s (id, name, amount, ts, flag) SELECT id, name, amount, ts, flag FROM %s`,
		d.Qualified(dr), d.Qualified(sr))); err != nil {
		return fmt.Errorf("灌目标表失败：%w", err)
	}
	return nil
}

// markDirty 把目标 amount 加 1，使下一次插入/更新所有非 NULL 行都判定为「值有变化」。
func markDirty(ctx context.Context, h *conn.Handle, name string) error {
	_, err := h.DB.ExecContext(ctx, "UPDATE "+h.Dialect.Qualified(targetRef(h, name))+" SET amount = amount + 1")
	return err
}

func outNode(id, typ string, cfg map[string]any) pipeline.Node {
	return pipeline.Node{ID: id, Type: typ, Name: id, Config: cfgJSON(cfg)}
}

func iuCfg(dstConn, table string, commit int) map[string]any {
	return map[string]any{
		"connection": dstConn, "table": table, "commitSize": commit,
		// 测纯写入路径：不做「未变化行比对」（该比对测的是另一特性；
		// 关闭后通用路径可走 UPDATE...RETURNING 融合）。
		"skipUnchanged": false,
		"keys":          []map[string]any{{"stream": "id", "column": "id"}},
		"fields": []map[string]any{
			{"stream": "name", "column": "name", "update": true},
			{"stream": "amount", "column": "amount", "update": true},
			{"stream": "ts", "column": "ts", "update": true},
			{"stream": "flag", "column": "flag", "update": true},
		},
	}
}

// run 执行一次场景并记录真实峰值 RSS（ru_maxrss：进程高水位，单调不减）。
func run(ctx context.Context, env core.Env, srcConn, sqlText, scenario string, commit int, out pipeline.Node, rows int64) (Result, error) {
	rssStart := rssMB()
	t0 := time.Now()
	metrics, err := runPipeline(ctx, env, srcConn, sqlText, out)
	dur := time.Since(t0)
	if err != nil {
		return Result{Scenario: scenario, Commit: commit}, err
	}
	r := Result{Scenario: scenario, Commit: commit, Duration: dur, RSSStartMB: rssStart, PeakRSSMB: rssMB()}
	// Read = 步骤从上游处理行数（Output/Updated 不含「值未变化而跳过」的行）。
	r.Rows = metrics[out.ID].Read
	if r.Rows != rows {
		return r, fmt.Errorf("%s 处理行数 %d ≠ 源行数 %d", scenario, r.Rows, rows)
	}
	return r, nil
}

// Run 执行全部压测场景：表输出与插入/更新（含 upsert 与 CASE 两条路径）。
// srcConn/dstConn 是连接池中的连接 ID；pg 恒为源，dst 为目标（postgres 或 mysql）。
func Run(ctx context.Context, env core.Env, srcConn string, pg *conn.Handle, dstConn string, dst *conn.Handle, opts Options, report func(Result)) error {
	const dstT, dstCase = "etl_bench_dst", "etl_bench_dst_case"
	if err := PrepareSource(ctx, pg, opts.Rows); err != nil {
		return err
	}
	srcQ := srcSQL(pg)

	// 表输出：逐提交档测量全量插入吞吐
	for _, c := range opts.Commits {
		if err := resetTarget(ctx, dst, dstT, true); err != nil {
			return err
		}
		r, err := run(ctx, env, srcConn, srcQ, "output", c, outNode("out", "table_output",
			map[string]any{"connection": dstConn, "table": dstT, "commitSize": c}), opts.Rows)
		if err != nil {
			return err
		}
		report(r)

		// 复用已灌满的 dst：标记脏值后测插入/更新的全量更新（upsert 路径）
		if err := markDirty(ctx, dst, dstT); err != nil {
			return err
		}
		r, err = run(ctx, env, srcConn, srcQ, "iu_update", c, outNode("iu", "insert_update", iuCfg(dstConn, dstT, c)), opts.Rows)
		if err != nil {
			return err
		}
		report(r)
	}

	// 插入/更新·全量插入（中间档）
	mid := opts.Commits[len(opts.Commits)/2]
	if err := resetTarget(ctx, dst, dstT, true); err != nil {
		return err
	}
	r, err := run(ctx, env, srcConn, srcQ, "iu_insert", mid, outNode("iu", "insert_update", iuCfg(dstConn, dstT, mid)), opts.Rows)
	if err != nil {
		return err
	}
	report(r)

	// 通用 CASE 路径：目标无唯一约束（仅普通索引），全量更新（中间档）
	if err := resetTarget(ctx, dst, dstCase, false); err != nil {
		return err
	}
	if err := fillTarget(ctx, dst, dstCase, dstT); err != nil {
		return err
	}
	if err := markDirty(ctx, dst, dstCase); err != nil {
		return err
	}
	r, err = run(ctx, env, srcConn, srcQ, "iu_update_case", mid, outNode("iu", "insert_update", iuCfg(dstConn, dstCase, mid)), opts.Rows)
	if err != nil {
		return err
	}
	report(r)
	return nil
}

// VerifyCount 断言目标表行数（防漏写）。
func VerifyCount(ctx context.Context, db *sql.DB, d conn.Dialect, schema, table string, want int64) error {
	ref := conn.TableRef{Schema: schema, Name: table}
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+d.Qualified(ref)).Scan(&n); err != nil {
		return err
	}
	if n != want {
		return fmt.Errorf("%s 应有 %d 行，实际 %d", table, want, n)
	}
	return nil
}
