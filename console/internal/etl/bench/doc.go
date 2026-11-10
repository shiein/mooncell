// Package bench 是千万行级真机压测工具（非生产代码路径）：
// PG → MySQL「表输出」与「插入/更新」两组场景，记录吞吐、峰值内存与提交档影响。
//
// 实现位于 etl_live && etl_bench 构建标签之后，常规构建不含本包代码。
// 运行（需要 PG/MySQL 测试库连接，格式同 livetest）：
//
//	ETL_LIVE_POSTGRES='{...}' ETL_LIVE_MYSQL='{...}' \
//	go test -tags "etl_live etl_bench" -v ./internal/etl/bench/ -timeout 60m
//
// 可选环境变量：BENCH_ROWS（默认 10000000）、BENCH_COMMITS（默认 1000,5000,20000）。
package bench
