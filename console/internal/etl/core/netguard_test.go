package core_test

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mooncell/console/internal/etl/core"
)

func envAllow(allow bool) core.Env {
	k, v := core.HTTPAllowLoopbackEntry(allow)
	return core.MapEnv{k: v}
}

// lanIP 取一张非回环 IPv4 网卡地址（作重定向链的第一跳）。
func lanIP(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skip("无法枚举网卡")
	}
	for _, a := range addrs {
		ip, _, err := net.ParseCIDR(a.String())
		if err == nil && ip.To4() != nil && !ip.IsLoopback() {
			return ip.String()
		}
	}
	t.Skip("没有非回环 IPv4 地址")
	return ""
}

// R1-m3：HTTP 客户端默认拒绝回环/链路本地/未指定地址；
// 302 跳转目标同样受 DialContext 拦截；http_allow_loopback 放开后可达。
func TestSSRFGuard(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer target.Close()

	cli := core.GuardedHTTPClient(envAllow(false), 3*time.Second)
	if _, err := cli.Get(target.URL); err == nil || !strings.Contains(err.Error(), "禁用地址") {
		t.Fatalf("直连回环应被拒，实得 %v", err)
	}
	// http.Client 会把校验错误包进 url.Error
	if resp, err := cli.Head(target.URL); err == nil {
		resp.Body.Close()
		t.Fatal("HEAD 回环同样应被拒")
	}

	// 302 跳转：第一跳走网卡 LAN 地址（内网放行），目标回环必须在第二跳被拦
	lan := lanIP(t)
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Skipf("无法起监听：%v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	redirector := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	})}
	go redirector.Serve(ln)
	defer redirector.Close()

	if _, err := cli.Get(fmt.Sprintf("http://%s:%d/", lan, port)); err == nil || !strings.Contains(err.Error(), "禁用地址") {
		t.Fatalf("302 跳转回环应被拒，实得 %v", err)
	}

	// 放开配置后两跳都可达
	cliAllow := core.GuardedHTTPClient(envAllow(true), 3*time.Second)
	resp, err := cliAllow.Get(fmt.Sprintf("http://%s:%d/", lan, port))
	if err != nil {
		t.Fatalf("放开后应可达：%v", err)
	}
	resp.Body.Close()

	// 私网/公网目标不受影响的连通性由上面的第一跳（LAN 地址）顺带覆盖
}
