package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestNohupLifecycle 真跑 nohup 托管闭环:启动→存活+pidfile(JSON 状态)+spec→停止→从规格重启→再停。
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
	pidN, _ := strconv.Atoi(pid)
	if pidN <= 0 || !pidRunning(pidN) {
		t.Fatalf("启动后进程应存活, pid=%q", pid)
	}
	if !nohupAlive(cfg) {
		t.Fatal("nohupAlive 应为 true")
	}
	if st, ok := readNohupState(cfg); !ok || st.Pid != pidN {
		t.Fatalf("pidfile 应记录 pid=%d, got %+v ok=%v", pidN, st, ok)
	}
	if _, err := os.Stat(nohupSpecPath(bin)); err != nil {
		t.Fatalf("启动规格 sidecar 应存在: %v", err)
	}

	nohupStop(cfg)
	if pidRunning(pidN) {
		t.Fatalf("stop 后进程应已退出, pid=%d", pidN)
	}
	if _, err := os.Stat(nohupPidFile(cfg)); !os.IsNotExist(err) {
		t.Fatal("stop 后 pidfile 应被清理")
	}

	// 仅凭落盘规格重启(模拟 lifecycle「启动」只带 binPath 的无状态请求)。
	pid2, err := nohupStartFromSpec(bin)
	if err != nil {
		t.Fatalf("nohupStartFromSpec: %v", err)
	}
	pid2N, _ := strconv.Atoi(pid2)
	if pid2N <= 0 || !pidRunning(pid2N) {
		t.Fatalf("从规格重启后应存活, pid=%q", pid2)
	}
	nohupStop(cfg)
	time.Sleep(100 * time.Millisecond)
	if pidRunning(pid2N) {
		t.Fatal("收尾 stop 后应已退出")
	}
}

// TestNohupCommand 校验各类型命令拼装:executable 与每个参数都经 shell 引用,分词按 shell 词法。
func TestNohupCommand(t *testing.T) {
	cmd, err := nohupCommand(DeployConfig{Type: "native-binary", BinPath: "/srv/apps/x/app", Args: "--port 9000"})
	if err != nil || cmd != `'/srv/apps/x/app' '--port' '9000'` {
		t.Fatalf("native-binary 命令拼装错误: %q err=%v", cmd, err)
	}
	// 注入防御:args 含 shell 元字符时,每个 token 被单引号包裹 → 元字符成字面参数,不被 sh -c 解释。
	cmd, err = nohupCommand(DeployConfig{Type: "native-binary", BinPath: "/app", Args: `-a 1 ; rm -rf /`})
	if err != nil || cmd != `'/app' '-a' '1' ';' 'rm' '-rf' '/'` {
		t.Fatalf("注入防御:元字符未被引用隔离: %q err=%v", cmd, err)
	}
	// 引号分组:带空格的值用引号包成单个参数,引号被剥离。
	cmd, err = nohupCommand(DeployConfig{Type: "java-jar", BinPath: "/x.jar", JvmArgs: `-Dname="a b"`, Args: ""})
	if err != nil {
		t.Skipf("java 不在 PATH,跳过 jvmArgs 引号分组用例: %v", err)
	}
	if !strings.Contains(cmd, `'-Dname=a b'`) || !strings.Contains(cmd, `-jar '/x.jar'`) {
		t.Fatalf("jvmArgs 引号分组错误: %q", cmd)
	}
}

// TestSplitArgs 校验 shell 词法分词:空白分隔、引号分组剥离、转义、空串。
func TestSplitArgs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"--port 8080", []string{"--port", "8080"}},
		{`--msg "a b" c`, []string{"--msg", "a b", "c"}},
		{`'a b'c`, []string{"a bc"}},
		{`x\ y`, []string{"x y"}},
		{`a ; b`, []string{"a", ";", "b"}},
	}
	for _, c := range cases {
		got := splitArgs(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("splitArgs(%q) = %q, want %q", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("splitArgs(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

// TestNohupStaleIdentity 证伪 PID 复用误判:同 PID 但 starttime 不符 → 视为已死(stale),不发信号。
// 依赖 /proc(linux);本机无 /proc 跳过。
func TestNohupStaleIdentity(t *testing.T) {
	self := strconv.Itoa(os.Getpid())
	if procStartTime(self) == "" {
		t.Skip("无 /proc/<pid>/stat,跳过 PID 复用身份校验")
	}
	selfN := os.Getpid()
	// 当前进程确实存活,但记录一个错误的 starttime → 必须判为非存活,否则 stop 会误杀本进程。
	if stateAlive(nohupState{Pid: selfN, StartTime: "1"}) {
		t.Fatal("starttime 不匹配应判为 stale(非存活)")
	}
	// starttime 匹配则判存活。
	if !stateAlive(nohupState{Pid: selfN, StartTime: procStartTime(self)}) {
		t.Fatal("starttime 匹配应判为存活")
	}
}
