//go:build windows

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// acquireInstanceLock Windows 实现:LockFileEx 排他 + 立即失败,语义对齐 unix flock。
func acquireInstanceLock(lockPath string) (*os.File, error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("无法创建实例锁 %s: %w", lockPath, err)
	}
	// 锁住文件开头 1 字节即可互斥;FAIL_IMMEDIATELY = 非阻塞。
	var ol windows.Overlapped
	err = windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1, 0, // nNumberOfBytesToLockLow/High
		&ol,
	)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("已有 Console 实例在运行(锁 %s)。请先结束旧进程再启动,否则登录会一直卡住: %w", lockPath, err)
	}
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return f, nil
}
