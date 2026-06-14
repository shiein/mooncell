package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// execStart:go-binary / python(venv 解释器)的命令生成。
func TestExecStart(t *testing.T) {
	// go-binary:直跑二进制 + 启动参数
	got, err := execStart(DeployConfig{Type: "go-binary", BinPath: "/srv/apps/x/app", Args: "--port 80"})
	if err != nil || got != "/srv/apps/x/app --port 80" {
		t.Fatalf("go-binary execStart = %q, err=%v", got, err)
	}
	// python:指定 venv 解释器时用之,不查 PATH
	got, err = execStart(DeployConfig{Type: "python", Interpreter: "/srv/apps/x/venv/bin/python", BinPath: "/srv/apps/x/app.py"})
	if err != nil || got != "/srv/apps/x/venv/bin/python /srv/apps/x/app.py" {
		t.Fatalf("python venv execStart = %q, err=%v", got, err)
	}
	// node:指定 node 路径时用之
	got, err = execStart(DeployConfig{Type: "node", Interpreter: "/usr/local/bin/node", BinPath: "/srv/apps/x/server.js"})
	if err != nil || got != "/usr/local/bin/node /srv/apps/x/server.js" {
		t.Fatalf("node execStart = %q, err=%v", got, err)
	}
}

// writePm2Eco:各类型 interpreter 正确(python 吃 venv)。
func TestWritePm2Eco(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		cfg        DeployConfig
		wantInterp string
	}{
		{DeployConfig{Type: "go-binary", BinPath: filepath.Join(dir, "a")}, "none"},
		{DeployConfig{Type: "python", BinPath: filepath.Join(dir, "b"), Interpreter: "/venv/bin/python"}, "/venv/bin/python"},
		{DeployConfig{Type: "python", BinPath: filepath.Join(dir, "c")}, "python3"},
		{DeployConfig{Type: "java-jar", BinPath: filepath.Join(dir, "d")}, "java"},
		{DeployConfig{Type: "node", BinPath: filepath.Join(dir, "e"), Interpreter: "/usr/local/bin/node"}, "/usr/local/bin/node"},
		{DeployConfig{Type: "node", BinPath: filepath.Join(dir, "f")}, "node"},
	}
	for _, c := range cases {
		path, err := writePm2Eco(c.cfg)
		if err != nil {
			t.Fatalf("writePm2Eco err: %v", err)
		}
		var eco struct {
			Apps []map[string]any `json:"apps"`
		}
		b, _ := os.ReadFile(path)
		json.Unmarshal(b, &eco)
		if got := eco.Apps[0]["interpreter"]; got != c.wantInterp {
			t.Errorf("type=%s interpreter = %v, want %v", c.cfg.Type, got, c.wantInterp)
		}
	}
}

// runReload:白名单外动作必须被拒绝且不执行;空动作跳过。
func TestRunReloadWhitelist(t *testing.T) {
	if ran, _, err := runReload(""); ran || err != nil {
		t.Fatalf("空动作应跳过: ran=%v err=%v", ran, err)
	}
	// 任意 shell 串(注入意图)必须被白名单拒绝,而不是当 sh -c 执行
	ran, _, err := runReload("rm -rf / ; curl evil")
	if !ran || err == nil || !strings.Contains(err.Error(), "disallowed") {
		t.Fatalf("白名单外动作应被拒绝: ran=%v err=%v", ran, err)
	}
	// 白名单内动作名应被识别(实际 exec 可能因环境无 nginx 失败,这里只验“被允许而非拒绝”)
	if _, _, err := runReload("nginx-reload"); err != nil && strings.Contains(err.Error(), "disallowed") {
		t.Fatalf("白名单内动作不应被判为 disallowed")
	}
}

// processHealthy:未配置 HTTP 健康检查时,进程未存活必须判失败(杜绝启动失败被判成功)。
func TestProcessHealthyGate(t *testing.T) {
	var logs []string
	if processHealthy("", false, &logs) {
		t.Fatal("无健康检查 + 进程未存活,应判失败")
	}
	logs = nil
	if !processHealthy("", true, &logs) {
		t.Fatal("无健康检查 + 进程存活,应判通过")
	}
}

// copyToTemp:还原源保护——拷贝独立副本,清理后删除。
func TestCopyToTemp(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	os.WriteFile(src, []byte("artifact-bytes"), 0644)

	tmp, cleanup, err := copyToTemp(src)
	if err != nil {
		t.Fatalf("copyToTemp err: %v", err)
	}
	if b, _ := os.ReadFile(tmp); string(b) != "artifact-bytes" {
		t.Fatalf("临时副本内容不符: %q", b)
	}
	// 删掉源后副本仍在(证明独立,免疫滚动清理)
	os.Remove(src)
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("源删除后临时副本应仍存在")
	}
	cleanup()
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("cleanup 后临时副本应被删除")
	}
}

// validateUnitFields:换行/控制字符注入必须被拒绝。
func TestValidateUnitFields(t *testing.T) {
	if err := validateUnitFields(DeployConfig{Name: "ok", BinPath: "/srv/apps/x/app", User: "root"}); err != nil {
		t.Fatalf("正常字段不应报错: %v", err)
	}
	bad := []DeployConfig{
		{Name: "x\n[Service]\nExecStartPre=/evil", BinPath: "/srv/apps/x/app"},
		{Name: "x", BinPath: "/srv/apps/x/app", User: "root\nExecStartPre=evil"},
		{Name: "x", BinPath: "/srv/apps/x/app", Env: map[string]string{"K": "v\nEnvironment=INJECT=1"}},
	}
	for i, c := range bad {
		if err := validateUnitFields(c); err == nil {
			t.Errorf("用例 %d 含换行注入,应被拒绝", i)
		}
	}
}

// processHealthy + rollback:验证空健康检查 + 进程未存活判失败的逻辑(回滚路径同样适用)。

// withinRoots:路径穿越防护。
func TestWithinRoots(t *testing.T) {
	roots := []string{"/srv/apps"}
	if !withinRoots("/srv/apps/x/app", roots) {
		t.Error("白名单内路径应通过")
	}
	if withinRoots("/srv/apps/../etc/passwd", roots) {
		t.Error("穿越路径应被拒绝")
	}
	if withinRoots("/etc/passwd", roots) {
		t.Error("白名单外路径应被拒绝")
	}
}
