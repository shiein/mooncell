//go:build linux || darwin

package agentapp

import (
	"fmt"
	"os"
	"syscall"
)

func preserveFileOwner(path string, old os.FileInfo) error {
	st, ok := old.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if err := os.Chown(path, int(st.Uid), int(st.Gid)); err != nil {
		return fmt.Errorf("保留目标文件 owner/group 失败: %w", err)
	}
	return nil
}
