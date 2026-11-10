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

func TestBenchPGtoMySQL(t *testing.T) {
	opts := bench.FromEnv()
	env := livetest.Env(t, "postgres", "mysql")
	pg := livetest.Open(t, "postgres")
	my := livetest.Open(t, "mysql")

	srcConn, dstConn := livetest.ConnID("postgres"), livetest.ConnID("mysql")
	t.Logf("压测规模：%d 行 × 提交档 %v（%s → %s）", opts.Rows, opts.Commits, srcConn, dstConn)

	var results []bench.Result
	err := bench.Run(context.Background(), env, srcConn, pg, dstConn, my, opts, func(r bench.Result) {
		results = append(results, r)
		t.Log(r.String())
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bench.VerifyCount(context.Background(), my.DB, my.Dialect,
		conn.EffectiveSchema(my.Config, ""), "etl_bench_dst", opts.Rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.RowsPerSec() <= 0 {
			t.Errorf("%s@commit%d 吞吐异常：%d", r.Scenario, r.Commit, r.RowsPerSec())
		}
	}
}
