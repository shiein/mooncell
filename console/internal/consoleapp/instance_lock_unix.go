//go:build unix

package consoleapp

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// acquireInstanceLock 对 lockPath 加排他文件锁(非阻塞)。已有实例时立即失败,避免双开抢 sqlite。
// 锁随进程退出/Close 释放;syscall.Exec 自更新时 fd 默认 CLOEXEC,新映像会重新抢锁。
func acquireInstanceLock(lockPath string) (*os.File, error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("无法创建实例锁 %s: %w", lockPath, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("已有 Console 实例在运行(锁 %s)。请先结束旧进程再启动,否则登录会一直卡住: %w", lockPath, err)
	}
	// 写入 PID 便于运维排查(锁本身以 flock 为准,不依赖文件内容)。
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return f, nil
}
