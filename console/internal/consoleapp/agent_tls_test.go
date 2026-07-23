package consoleapp

import "testing"

func TestAgentBaseScheme(t *testing.T) {
	cases := []struct{ addr, want string }{
		{"10.0.0.5:9100", "http://10.0.0.5:9100"},    // 裸 host:port 默认 http(向后兼容)
		{"http://10.0.0.5:9100", "http://10.0.0.5:9100"},
		{"https://10.0.0.5:9100", "https://10.0.0.5:9100"}, // 显式 https 原样
	}
	for _, c := range cases {
		if got := agentBase(c.addr); got != c.want {
			t.Errorf("agentBase(%q)=%q, want %q", c.addr, got, c.want)
		}
	}
}

func TestIsPlaintextNonLoopback(t *testing.T) {
	yes := []string{"10.0.0.5:9100", "http://10.0.0.5:9100", "http://example.com:9100"}
	for _, a := range yes {
		if !isPlaintextNonLoopback(a) {
			t.Errorf("%q 应判为非 loopback 明文(须拒绝)", a)
		}
	}
	no := []string{
		"https://10.0.0.5:9100", // TLS
		"127.0.0.1:9100",        // loopback
		"http://127.0.0.1:9100", // loopback 明文可接受(不过网)
		"localhost:9100",        // loopback
		"http://[::1]:9100",     // IPv6 loopback
	}
	for _, a := range no {
		if isPlaintextNonLoopback(a) {
			t.Errorf("%q 不应判为非 loopback 明文", a)
		}
	}
}

func TestValidAgentAddrAllowsScheme(t *testing.T) {
	ok := []string{"10.0.0.5:9100", "http://10.0.0.5:9100", "https://10.0.0.5:9100", "localhost:80"}
	for _, a := range ok {
		if !validAgentAddr(a) {
			t.Errorf("%q 应为合法地址", a)
		}
	}
	bad := []string{"", "10.0.0.5", "http://10.0.0.5:9100/path", "ftp://x:1", "10.0.0.5:abc"}
	for _, a := range bad {
		if validAgentAddr(a) {
			t.Errorf("%q 应为非法地址", a)
		}
	}
}
