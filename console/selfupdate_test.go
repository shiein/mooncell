package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 复用 agentupdate_test.go 的 elfHead 构造最小 ELF 头(小端,e_machine 置 m)。
// 这里再声明一个 machoHead 用于非 ELF 拒绝用例。
func machoHead() []byte {
	// Mach-O 64 小端魔数 0xFEEDFACF
	return []byte{0xCF, 0xFA, 0xED, 0xFE, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
}

// shaFile 计算文件内容的 sha256 十六进制。
func shaFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// validateConsoleSelfUpdate:sha + 架构校验链。
func TestValidateConsoleSelfUpdate(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) string { p := filepath.Join(dir, name); os.WriteFile(p, b, 0o644); return p }

	// sha 不匹配 → 拒
	amd := write("amd", elfHead(0x3E))
	if msg := validateConsoleSelfUpdate(amd, shaFile(t, amd), "deadbeef"); msg == "" || !strings.Contains(msg, "sha256") {
		t.Errorf("sha 不匹配应拒, got %q", msg)
	}
	// sha 匹配 + 无声明 sha → 仅过架构(本机架构相符时)
	if runtime.GOARCH == "amd64" {
		if msg := validateConsoleSelfUpdate(amd, shaFile(t, amd), ""); msg != "" {
			t.Errorf("amd64 ELF 在 amd64 本机应通过, got %q", msg)
		}
	}
	// 非 ELF(Mach-O)→ 拒
	if msg := validateConsoleSelfUpdate(write("macho", machoHead()), "", ""); msg == "" || !strings.Contains(msg, "架构") {
		t.Errorf("Mach-O 应拒, got %q", msg)
	}
	// 跨架构 ELF(arm64 包在 amd64 本机)→ 拒
	if runtime.GOARCH == "amd64" {
		if msg := validateConsoleSelfUpdate(write("arm", elfHead(0xB7)), "", ""); msg == "" || !strings.Contains(msg, "架构") {
			t.Errorf("arm64 ELF 在 amd64 本机应拒, got %q", msg)
		}
	}
	if runtime.GOARCH == "arm64" {
		if msg := validateConsoleSelfUpdate(write("amd", elfHead(0x3E)), "", ""); msg == "" || !strings.Contains(msg, "架构") {
			t.Errorf("amd64 ELF 在 arm64 本机应拒, got %q", msg)
		}
	}
}

// selftestBinary / binaryVersion:用 shell 脚本桩验证退出码与版本读取逻辑(不依赖 ELF)。
func TestSelftestAndVersion(t *testing.T) {
	dir := t.TempDir()
	writeScript := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755)
		return p
	}

	// selftest 成功(退出 0)
	ok := writeScript("ok.sh", "exit 0")
	if err := selftestBinary(ok); err != nil {
		t.Errorf("selftest 退出 0 应通过, got %v", err)
	}
	// selftest 失败(退出 1)
	bad := writeScript("bad.sh", "echo nope; exit 1")
	if err := selftestBinary(bad); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("selftest 退出 1 应失败并含输出, got %v", err)
	}
	// 版本读取
	ver := writeScript("ver.sh", `echo "v0.9.9"`)
	if got, err := binaryVersion(ver); err != nil || got != "v0.9.9" {
		t.Errorf("binaryVersion 应读 v0.9.9, got %q err=%v", got, err)
	}
}

// copyFile:内容 + 权限模式复制。
func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a")
	dst := filepath.Join(dir, "b")
	os.WriteFile(src, []byte("hello"), 0o644)
	if err := copyFile(src, dst, 0o755); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	b, _ := os.ReadFile(dst)
	if string(b) != "hello" {
		t.Errorf("复制内容不符: %q", b)
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o755 {
		t.Errorf("权限模式不符: %v", fi.Mode().Perm())
	}
}

// consoleInfo:返回当前版本 + os/arch。
func TestConsoleInfo(t *testing.T) {
	a := &api{}
	w := httptest.NewRecorder()
	a.consoleInfo(w, httptest.NewRequest("GET", "/api/console/info", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("info 应 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, consoleVersion) || !strings.Contains(body, runtime.GOOS+"/"+runtime.GOARCH) {
		t.Errorf("info 应含版本与 os/arch, got %s", body)
	}
}

// 并发:selfUpdateMu 已占用时第二次请求 409(TryLock 在 GOOS 闸之前,任意 OS 可测)。
func TestSelfUpdateConflict409(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	a := &api{store: s}
	a.selfUpdateMu.Lock()
	defer a.selfUpdateMu.Unlock()

	req := httptest.NewRequest("POST", "/api/console/self-update", &bytes.Buffer{})
	w := httptest.NewRecorder()
	a.selfUpdate(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("mu 占用时应 409, got %d", w.Code)
	}
}

// 非 Linux 本机:handler 在 GOOS 闸直接拒(开发机 darwin 走此分支)。
func TestSelfUpdateNonLinuxReject(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("仅非 Linux 验证 GOOS 闸")
	}
	s := testStore(t)
	defer s.Close()
	a := &api{store: s}
	req := httptest.NewRequest("POST", "/api/console/self-update", &bytes.Buffer{})
	w := httptest.NewRecorder()
	a.selfUpdate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非 Linux 应 400, got %d", w.Code)
	}
}

// Linux 本机:架构不符(Mach-O)→ 拒,保旧版,.new 被删。
func TestSelfUpdateArchGuardLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("仅 Linux 验证架构闸(需 os.Executable 写 .new)")
	}
	s := testStore(t)
	defer s.Close()
	a := &api{store: s}

	exe, _ := os.Executable()
	if resolved, e := filepath.EvalSymlinks(exe); e == nil {
		exe = resolved
	}
	newPath := exe + ".new"
	os.Remove(newPath)
	defer os.Remove(newPath)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("binary", "console")
	fw.Write(machoHead())
	mw.Close()
	req := httptest.NewRequest("POST", "/api/console/self-update", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	a.selfUpdate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("Mach-O 应 400, got %d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(newPath); err == nil {
		t.Errorf("校验失败后 .new 应被删除")
	}
}

// Linux 本机:sha256 不匹配 → 拒(用本机架构 ELF 但填错 sha)。
func TestSelfUpdateShaMismatchLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("仅 Linux 验证 sha 闸")
	}
	s := testStore(t)
	defer s.Close()
	a := &api{store: s}

	exe, _ := os.Executable()
	if resolved, e := filepath.EvalSymlinks(exe); e == nil {
		exe = resolved
	}
	newPath := exe + ".new"
	os.Remove(newPath)
	defer os.Remove(newPath)

	var eMachine byte = 0x3E
	if runtime.GOARCH == "arm64" {
		eMachine = 0xB7
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("sha256", "deadbeef")
	fw, _ := mw.CreateFormFile("binary", "console")
	fw.Write(elfHead(eMachine))
	mw.Close()
	req := httptest.NewRequest("POST", "/api/console/self-update", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	a.selfUpdate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("sha 不匹配应 400, got %d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(newPath); err == nil {
		t.Errorf("sha 校验失败后 .new 应被删除")
	}
}

// --version / --selftest flag:构建 console 二进制后实跑,验证打印版本且不监听端口。
// selftest 只 loadConfig(不绑端口/不开 DB),退出码 0。
func TestConsoleFlags(t *testing.T) {
	if testing.Short() {
		t.Skip("构建二进制较慢,-short 跳过")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "mooncell-console-test")
	// 复用已构建的 dist/(go:embed);-ldflags 注入版本号验证覆盖。
	// go test 的 CWD 即 console 包源码目录,build.Dir 留空继承。
	// 必须为宿主 GOOS/GOARCH 构建:本仓常设 GOOS=windows 交叉编译,继承环境会产出无法在本机执行的二进制
	// (exec format error),故显式钉到 runtime,保证产物能在本机跑 --version/--selftest。
	build := exec.Command("go", "build", "-ldflags", "-X main.consoleVersion=vTest1.2.3", "-o", bin, ".")
	build.Env = append(os.Environ(), "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build 失败: %v\n%s", err, out)
	}
	defer os.Remove(bin)

	// --version
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("--version 执行失败: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "vTest1.2.3" {
		t.Errorf("--version 应打印注入版本 vTest1.2.3, got %q", strings.TrimSpace(string(out)))
	}

	// --selftest:在空 CWD 跑(无 config.toml → 内置默认 127.0.0.1,安全闸通过),退出码 0 且打印 ok 行。
	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()
	cmd := exec.CommandContext(ctx2, bin, "--selftest")
	cmd.Dir = dir // 空 temp 目录,无 config.toml
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--selftest 应退出 0, got %v\n%s", err, out)
	}
	s := strings.TrimSpace(string(out))
	// loadConfig 在无 config.toml 时会 log 一行提示(到 stderr,被 CombinedOutput 捕获),ok 行在其后。
	if !strings.Contains(s, "ok vTest1.2.3 "+runtime.GOOS+"/"+runtime.GOARCH) {
		t.Errorf("--selftest 应打印 'ok <version> <goos>/<goarch>', got %q", s)
	}
	// 验证 selftest 不绑端口:上面 ctx 限定 15s,实际应秒级退出(不监听端口);若绑端口会阻塞至超时。
}
