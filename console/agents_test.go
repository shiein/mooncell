package main

import "testing"

// TestValidAgentAddr 校验 Agent 地址形态:接受 host:port,拒绝带 scheme/路径/空端口等畸形输入。
func TestValidAgentAddr(t *testing.T) {
	cases := map[string]bool{
		"10.0.0.5:9100":          true,
		"host.local:9100":        true,
		"[::1]:9100":             true,
		"127.0.0.1:9100":         true,
		"":                       false,
		"10.0.0.5":               false, // 缺端口
		":9100":                  false, // 缺 host
		"10.0.0.5:":              false, // 空端口
		"http://10.0.0.5:9100":   false, // 带 scheme
		"10.0.0.5:9100/path":     false, // 带路径
		"10.0.0.5:9100?q=1":      false, // 带 query
		"10.0.0.5:abc":           false, // 端口非数字
		"10.0.0.5:9100#frag":     false, // 带 fragment
	}
	for addr, want := range cases {
		if got := validAgentAddr(addr); got != want {
			t.Errorf("validAgentAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}
