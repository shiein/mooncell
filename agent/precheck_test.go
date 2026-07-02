package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// decodePrecheck 解析 precheck 响应里的 checks,按 label 是否含 sub 找到那条。
func findCheck(t *testing.T, body []byte, sub string) map[string]any {
	t.Helper()
	var resp struct {
		Checks []map[string]any `json:"checks"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("解析响应失败: %v (body=%s)", err, body)
	}
	for _, c := range resp.Checks {
		if label, _ := c["label"].(string); containsSub(label, sub) {
			return c
		}
	}
	return nil
}

func containsSub(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestPrecheckStaticSkipsPortCheck 验证 static-nginx 即便端口被占用(nginx 常态)也不产生 fail:
// 静态站点无独立进程,端口是 nginx 对外服务口,占用是预期的——不能因此拦住预检/保存。
func TestPrecheckStaticSkipsPortCheck(t *testing.T) {
	// 占一个真实端口,模拟 nginx 已监听。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口失败: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	portStr := itoa(port)

	a := &agent{cfg: &Config{Paths: PathsConfig{DeployRoots: []string{"/srv/apps", "/data/web"}}}}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/precheck?type=static-nginx&runner=%E8%BD%AF%E9%93%BE&port="+portStr, nil)
	a.precheck(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("预检应返回 200,got %d", rr.Code)
	}
	c := findCheck(t, rr.Body.Bytes(), "端口")
	if c == nil {
		t.Fatalf("应有一条端口相关检查项,body=%s", rr.Body.String())
	}
	if ok, _ := c["ok"].(bool); !ok {
		t.Fatalf("static-nginx 端口被占用不应判 fail(ok=false),detail=%v", c["detail"])
	}
}

// TestPrecheckProcessPortOccupiedFails 对照:进程类应用端口被占用应判 fail(占用即冲突)。
// 占通配地址 :0 与 portFree 的 net.Listen(":"+port) 同口径,确保它探测到冲突。
func TestPrecheckProcessPortOccupiedFails(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("占端口失败: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	portStr := itoa(port)

	a := &agent{cfg: &Config{Paths: PathsConfig{DeployRoots: []string{"/srv/apps"}}}}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/precheck?type=native-binary&runner=systemd&port="+portStr, nil)
	a.precheck(rr, req)

	c := findCheck(t, rr.Body.Bytes(), "端口")
	if c == nil {
		t.Fatalf("应有一条端口相关检查项,body=%s", rr.Body.String())
	}
	if ok, _ := c["ok"].(bool); ok {
		t.Fatalf("进程类应用端口被占用应判 fail,却得到 ok=true,detail=%v", c["detail"])
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
