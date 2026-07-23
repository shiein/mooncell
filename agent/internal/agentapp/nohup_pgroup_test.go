package agentapp

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestNohupStopReapsProcessGroup 真跑验证进程组回收:被托管进程 fork 出一个后台子进程,
// nohupStop 必须连该子进程一起收(按进程组群发),而非旧行为的只杀单个父 PID 留孤儿。
func TestNohupStopReapsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "app")
	childPidFile := filepath.Join(dir, "child.pid")
	// 父进程 fork 一个 sleep 到后台并 wait:只杀父 PID 会让这个 sleep 变孤儿,按进程组杀才能收掉。
	spec := nohupSpec{
		Cmd:     fmt.Sprintf("sh -c 'sleep 120 & echo $! > %s; wait'", childPidFile),
		Workdir: dir,
		LogPath: filepath.Join(dir, "app.log"),
		PidFile: binPath + ".pid",
	}
	if _, err := nohupLaunch(spec); err != nil {
		t.Fatalf("nohupLaunch: %v", err)
	}

	// 等 fork 子进程 pid 落盘。
	var childPid int
	for i := 0; i < 50; i++ {
		if b, err := os.ReadFile(childPidFile); err == nil {
			if p, e := strconv.Atoi(strings.TrimSpace(string(b))); e == nil && p > 0 {
				childPid = p
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if childPid == 0 {
		t.Fatal("未取到 fork 子进程 pid")
	}
	if err := syscall.Kill(childPid, 0); err != nil {
		t.Fatalf("fork 子进程本应存活: %v", err)
	}

	nohupStop(DeployConfig{BinPath: binPath})

	dead := false
	for i := 0; i < 80; i++ {
		if syscall.Kill(childPid, 0) != nil {
			dead = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !dead {
		syscall.Kill(childPid, syscall.SIGKILL) // 兜底清理,别把 sleep 留给后续测试/系统
		t.Fatalf("fork 子进程未被进程组回收(仍存活)——说明只杀了单 PID")
	}
}
