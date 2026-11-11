//go:build etl_live && etl_bench && (linux || darwin)

package bench

import (
	"runtime"
	"syscall"
)

// rssMB 返回进程峰值常驻内存（getrusage ru_maxrss；Linux 为 KB，macOS 为字节）。
// 单调不减：某时刻读到的即「截至当时的峰值」，不需要轮询采样。
func rssMB() float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return float64(ru.Maxrss) / (1 << 20)
	}
	return float64(ru.Maxrss) / (1 << 10)
}
