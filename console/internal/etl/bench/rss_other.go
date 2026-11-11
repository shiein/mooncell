//go:build etl_live && etl_bench && !linux && !darwin

package bench

// rssMB 在非 POSIX 平台不支持，返回 0（RSS 数据点缺失不影响压测本身）。
func rssMB() float64 { return 0 }
