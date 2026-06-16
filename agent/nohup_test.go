package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestNohupLifecycle 真跑 nohup 托管闭环:启动→存活+pidfile+spec→停止→从规格重启→再停。
func TestNohupLifecycle(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "app")
	// 可执行脚本:exec sleep 使 nohup 捕获的 $! 与最终运行进程同 pid。
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &agent{cfg: &Config{Paths: PathsConfig{LogRoots: []string{dir}, DeployRoots: []string{dir}}}}
	cfg := DeployConfig{ID: "t", BinPath: bin, Type: "native-binary", Workdir: dir}

	pid, err := a.nohupStart(cfg)
	if err != nil {
		t.Fatalf("nohupStart: %v", err)
	}
	if pid == "" || !pidAlive(pid) {
		t.Fatalf("启动后进程应存活, pid=%q alive=%v", pid, pidAlive(pid))
	}
	if !nohupAlive(cfg) {
		t.Fatal("nohupAlive 应为 true")
	}
	if _, err := os.Stat(nohupSpecPath(bin)); err != nil {
		t.Fatalf("启动规格 sidecar 应存在: %v", err)
	}

	nohupStop(cfg)
	if pidAlive(pid) {
		t.Fatalf("stop 后进程应已退出, pid=%s", pid)
	}
	if _, err := os.Stat(nohupPidFile(cfg)); !os.IsNotExist(err) {
		t.Fatal("stop 后 pidfile 应被清理")
	}

	// 仅凭落盘规格重启(模拟 lifecycle「启动」只带 binPath 的无状态请求)。
	pid2, err := nohupStartFromSpec(bin)
	if err != nil {
		t.Fatalf("nohupStartFromSpec: %v", err)
	}
	if pid2 == "" || !pidAlive(pid2) {
		t.Fatalf("从规格重启后应存活, pid=%q", pid2)
	}
	nohupStop(cfg)
	time.Sleep(100 * time.Millisecond)
	if pidAlive(pid2) {
		t.Fatal("收尾 stop 后应已退出")
	}
}

// TestNohupCommand 校验各类型命令拼装(executable 经引用,参数原样)。
func TestNohupCommand(t *testing.T) {
	cmd, err := nohupCommand(DeployConfig{Type: "native-binary", BinPath: "/srv/apps/x/app", Args: "--port 9000"})
	if err != nil || cmd != `'/srv/apps/x/app' --port 9000` {
		t.Fatalf("native-binary 命令拼装错误: %q err=%v", cmd, err)
	}
}
