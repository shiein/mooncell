//go:build etl_live && etl_bench

package bench

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/pipeline"
	_ "mooncell/console/internal/etl/steps"
)

// Options 压测规模与提交档。
type Options struct {
	Rows    int64 // 源表行数
	Commits []int // 输出步骤 commitSize 档
}

// FromEnv 读取 BENCH_ROWS（默认 1000 万）与 BENCH_COMMITS（默认 1000,5000,20000）。
func FromEnv() Options {
	o := Options{Rows: 10_000_000, Commits: []int{1000, 5000, 20000}}
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
	Scenario  string        // output | iu_insert | iu_update | iu_update_case
	Commit    int           // 输出步骤 commitSize
	Rows      int64         // 处理行数
	Duration  time.Duration // 管道端到端耗时
	PeakSysMB float64       // 运行期间 MemStats.Sys 峰值（进程向 OS 取得内存，RSS 上界近似）
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
	return fmt.Sprintf("%-15s commit=%-6d rows=%-9d 耗时=%-9s 吞吐=%-8d行/s 峰值MemStats.Sys=%.0fMB",
		r.Scenario, r.Commit, r.Rows, r.Duration.Truncate(time.Second).String(), r.RowsPerSec(), r.PeakSysMB)
}

// sampler 周期采样 runtime.MemStats.Sys 的峰值。
type sampler struct {
	peak atomic.Int64
	done chan struct{}
}

func startSampler() *sampler {
	s := &sampler{done: make(chan struct{})}
	go func() {
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-s.done:
				return
			case <-t.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				for cur := s.peak.Load(); int64(m.Sys) > cur && !s.peak.CompareAndSwap(cur, int64(m.Sys)); cur = s.peak.Load() {
				}
			}
		}
	}()
	return s
}

func (s *sampler) stop()         { close(s.done) }
func (s *sampler) PeakMB() float64 { return float64(s.peak.Load()) / (1 << 20) }

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

// resetTarget 在 MySQL 重建目标表；pk=true 带主键（upsert 路径），false 仅普通索引（CASE 路径）。
func resetTarget(ctx context.Context, my *conn.Handle, name string, pk bool) error {
	d := my.Dialect
	ref := conn.TableRef{Schema: conn.EffectiveSchema(my.Config, ""), Name: name}
	my.DB.ExecContext(ctx, "DROP TABLE IF EXISTS "+d.Qualified(ref))
	key := "INDEX ix_id (id)"
	if pk {
		key = "PRIMARY KEY (id)"
	}
	ddl := fmt.Sprintf(`CREATE TABLE %s (id BIGINT, name VARCHAR(64), amount DECIMAL(14,2), ts DATETIME, flag TINYINT(1), %s)`,
		d.Qualified(ref), key)
	if _, err := my.DB.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("建目标表 %s 失败：%w", name, err)
	}
	return nil
}

// fillTarget 用库内 SELECT 把 src 表内容复制进 dst（同库，避免再过一次管道）。
func fillTarget(ctx context.Context, my *conn.Handle, dst, src string) error {
	d := my.Dialect
	dr := conn.TableRef{Schema: conn.EffectiveSchema(my.Config, ""), Name: dst}
	sr := conn.TableRef{Schema: conn.EffectiveSchema(my.Config, ""), Name: src}
	if _, err := my.DB.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO %s (id, name, amount, ts, flag) SELECT id, name, amount, ts, flag FROM %s`,
		d.Qualified(dr), d.Qualified(sr))); err != nil {
		return fmt.Errorf("灌目标表失败：%w", err)
	}
	return nil
}

// markDirty 把目标 amount 加 1，使下一次插入/更新所有非 NULL 行都判定为「值有变化」。
func markDirty(ctx context.Context, my *conn.Handle, name string) error {
	d := my.Dialect
	ref := conn.TableRef{Schema: conn.EffectiveSchema(my.Config, ""), Name: name}
	_, err := my.DB.ExecContext(ctx, "UPDATE "+d.Qualified(ref)+" SET amount = amount + 1")
	return err
}

func outNode(id, typ string, cfg map[string]any) pipeline.Node {
	return pipeline.Node{ID: id, Type: typ, Name: id, Config: cfgJSON(cfg)}
}

func iuCfg(dstConn, table string, commit int) map[string]any {
	return map[string]any{
		"connection": dstConn, "table": table, "commitSize": commit,
		"keys":   []map[string]any{{"stream": "id", "column": "id"}},
		"fields": []map[string]any{
			{"stream": "name", "column": "name", "update": true},
			{"stream": "amount", "column": "amount", "update": true},
			{"stream": "ts", "column": "ts", "update": true},
			{"stream": "flag", "column": "flag", "update": true},
		},
	}
}

// run 执行一次场景并采样内存。
func run(ctx context.Context, env core.Env, srcConn, sqlText, scenario string, commit int, out pipeline.Node, rows int64) (Result, error) {
	s := startSampler()
	t0 := time.Now()
	metrics, err := runPipeline(ctx, env, srcConn, sqlText, out)
	s.stop()
	dur := time.Since(t0)
	if err != nil {
		return Result{Scenario: scenario, Commit: commit}, err
	}
	r := Result{Scenario: scenario, Commit: commit, Duration: dur, PeakSysMB: s.PeakMB()}
	// Read = 步骤从上游处理行数（Output/Updated 不含「值未变化而跳过」的行）。
	r.Rows = metrics[out.ID].Read
	if r.Rows != rows {
		return r, fmt.Errorf("%s 处理行数 %d ≠ 源行数 %d", scenario, r.Rows, rows)
	}
	return r, nil
}

// Run 执行全部压测场景：表输出与插入/更新（含 upsert 与 CASE 两条路径）。
// srcConn/dstConn 是连接池中的连接 ID（PG 源 / MySQL 目标）。
func Run(ctx context.Context, env core.Env, srcConn string, pg *conn.Handle, dstConn string, my *conn.Handle, opts Options, report func(Result)) error {
	const dst, dstCase = "etl_bench_dst", "etl_bench_dst_case"
	if err := PrepareSource(ctx, pg, opts.Rows); err != nil {
		return err
	}
	srcQ := srcSQL(pg)

	// 表输出：逐提交档测量全量插入吞吐
	for _, c := range opts.Commits {
		if err := resetTarget(ctx, my, dst, true); err != nil {
			return err
		}
		r, err := run(ctx, env, srcConn, srcQ, "output", c, outNode("out", "table_output",
			map[string]any{"connection": dstConn, "table": dst, "commitSize": c}), opts.Rows)
		if err != nil {
			return err
		}
		report(r)

		// 复用已灌满的 dst：标记脏值后测插入/更新的全量更新（upsert 路径）
		if err := markDirty(ctx, my, dst); err != nil {
			return err
		}
		r, err = run(ctx, env, srcConn, srcQ, "iu_update", c, outNode("iu", "insert_update", iuCfg(dstConn, dst, c)), opts.Rows)
		if err != nil {
			return err
		}
		report(r)
	}

	// 插入/更新·全量插入（中间档）
	mid := opts.Commits[len(opts.Commits)/2]
	if err := resetTarget(ctx, my, dst, true); err != nil {
		return err
	}
	r, err := run(ctx, env, srcConn, srcQ, "iu_insert", mid, outNode("iu", "insert_update", iuCfg(dstConn, dst, mid)), opts.Rows)
	if err != nil {
		return err
	}
	report(r)

	// 通用 CASE 路径：目标无唯一约束（仅普通索引），全量更新（中间档）
	if err := resetTarget(ctx, my, dstCase, false); err != nil {
		return err
	}
	if err := fillTarget(ctx, my, dstCase, dst); err != nil {
		return err
	}
	if err := markDirty(ctx, my, dstCase); err != nil {
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
