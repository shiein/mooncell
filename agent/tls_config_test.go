package main

import "testing"

// TestSecurityTLSEnabled:cert 与 key 均非空才启用 HTTPS;任一空则明文(默认,向后兼容)。
func TestSecurityTLSEnabled(t *testing.T) {
	cases := []struct {
		cert, key string
		want      bool
	}{
		{"", "", false},
		{"/etc/cert.pem", "", false},
		{"", "/etc/key.pem", false},
		{"  ", "  ", false}, // 仅空白视为未配置
		{"/etc/cert.pem", "/etc/key.pem", true},
	}
	for _, c := range cases {
		s := SecurityConfig{TLSCert: c.cert, TLSKey: c.key}
		if got := s.tlsEnabled(); got != c.want {
			t.Errorf("tlsEnabled(cert=%q,key=%q)=%v, want %v", c.cert, c.key, got, c.want)
		}
	}
}
