//go:build etl_live && etl_bench

// 千万行级 PG→MySQL 压测入口：
//
//	ETL_LIVE_POSTGRES='{...}' ETL_LIVE_MYSQL='{...}' \
//	go test -tags "etl_live etl_bench" -v ./internal/etl/bench/ -timeout 60m
//
// 规模与提交档：BENCH_ROWS / BENCH_COMMITS（见 bench.Options）。
package bench_test

import (
	"context"
	"testing"

	"mooncell/console/internal/etl/bench"
	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/livetest"
)

// TestBench 压测入口：源恒为 PG，目标由 BENCH_TARGET=postgres|mysql 选择。
//   - BENCH_TARGET=mysql  BENCH_ROWS=10000000：PG→MySQL 表输出与插入/更新 × 三档提交
//   - BENCH_TARGET=postgres BENCH_ROWS=1000000：PG 目标库全量更新（原生 upsert + CASE 两路径）
func TestBench(t *testing.T) {
	opts := bench.FromEnv()
	env := livetest.Env(t, "postgres", opts.Target)
	pg := livetest.Open(t, "postgres")
	dst := livetest.Open(t, opts.Target)

	srcConn, dstConn := livetest.ConnID("postgres"), livetest.ConnID(opts.Target)
	t.Logf("压测规模：%d 行 × 提交档 %v（%s → %s）", opts.Rows, opts.Commits, srcConn, dstConn)

	var results []bench.Result
	err := bench.Run(context.Background(), env, srcConn, pg, dstConn, dst, opts, func(r bench.Result) {
		results = append(results, r)
		t.Log(r.String())
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bench.VerifyCount(context.Background(), dst.DB, dst.Dialect,
		conn.EffectiveSchema(dst.Config, ""), "etl_bench_dst", opts.Rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.RowsPerSec() <= 0 {
			t.Errorf("%s@commit%d 吞吐异常：%d", r.Scenario, r.Commit, r.RowsPerSec())
		}
	}
}
