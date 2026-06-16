package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// elf 构造一个最小 ELF 头(小端),e_machine 置 m。
func elfHead(m byte) []byte {
	b := make([]byte, 20)
	copy(b, []byte{0x7f, 'E', 'L', 'F'})
	b[5] = 1 // 小端
	b[18] = m
	return b
}

func TestElfArchAndArchOf(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) string { p := filepath.Join(dir, name); os.WriteFile(p, b, 0o644); return p }

	if got := elfArch(write("amd", elfHead(0x3E))); got != "amd64" {
		t.Errorf("amd64 ELF 应识别为 amd64, got %q", got)
	}
	if got := elfArch(write("arm", elfHead(0xB7))); got != "arm64" {
		t.Errorf("arm64 ELF 应识别为 arm64, got %q", got)
	}
	if got := elfArch(write("macho", []byte{0xCF, 0xFA, 0xED, 0xFE, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})); got != "" {
		t.Errorf("非 ELF 应返回空, got %q", got)
	}
	if got := elfArch(write("386", elfHead(0x03))); got != "" {
		t.Errorf("未放开架构(386)应返回空, got %q", got)
	}

	for in, want := range map[string]string{"linux/amd64": "amd64", "linux/arm64": "arm64", "linux/386": "", "garbage": "", "linux/": ""} {
		if got := archOf(in); got != want {
			t.Errorf("archOf(%q)=%q, want %q", in, got, want)
		}
	}
}

// 上传 Agent 包:声明架构与实际 ELF 不符必须被拒绝;一致则落库并可列出。
func TestUploadAgentBinaryArchGuard(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	a := &api{store: s, agentBinDir: t.TempDir()}

	post := func(arch, version string, body []byte) int {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		mw.WriteField("arch", arch)
		mw.WriteField("version", version)
		fw, _ := mw.CreateFormFile("binary", "agent")
		fw.Write(body)
		mw.Close()
		req := httptest.NewRequest("POST", "/api/agent-binary", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		w := httptest.NewRecorder()
		a.uploadAgentBinary(w, req)
		return w.Code
	}

	// 声明 amd64 但传 arm64 ELF → 拒绝
	if c := post("amd64", "v1", elfHead(0xB7)); c != http.StatusBadRequest {
		t.Fatalf("架构不符应 400, got %d", c)
	}
	// 声明 amd64 传 amd64 ELF → 成功
	if c := post("amd64", "v1.2.3", elfHead(0x3E)); c != http.StatusOK {
		t.Fatalf("架构一致应 200, got %d", c)
	}
	// 非法架构 → 拒绝
	if c := post("mips", "v1", elfHead(0x3E)); c != http.StatusBadRequest {
		t.Fatalf("非法架构应 400, got %d", c)
	}

	// 列表应含刚上传的 amd64 v1.2.3
	w := httptest.NewRecorder()
	a.listAgentBinaries(w, httptest.NewRequest("GET", "/api/agent-binaries", nil))
	var resp struct {
		Binaries []agentBinMeta `json:"binaries"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Binaries) != 1 || resp.Binaries[0].Version != "v1.2.3" || resp.Binaries[0].Arch != "amd64" {
		t.Fatalf("列表应含 amd64 v1.2.3, got %+v", resp.Binaries)
	}
}
