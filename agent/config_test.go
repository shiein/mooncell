package main

import (
	"strings"
	"testing"
)

func TestUnsafeAgentConfigReason(t *testing.T) {
	cfg := &Config{
		Server:   ServerConfig{Addr: "127.0.0.1"},
		Security: SecurityConfig{Token: defaultAgentToken},
	}
	if reason := unsafeAgentConfigReason(cfg); reason != "" {
		t.Fatalf("本地回环默认 token 应允许开发启动,got %q", reason)
	}

	cfg.Server.Addr = "0.0.0.0"
	if reason := unsafeAgentConfigReason(cfg); !strings.Contains(reason, "默认 Agent token") {
		t.Fatalf("对外监听 + 默认 Agent token 应被拒绝,got %q", reason)
	}

	cfg.Security.Token = "changed-token"
	if reason := unsafeAgentConfigReason(cfg); reason != "" {
		t.Fatalf("对外监听但 token 已修改应允许,got %q", reason)
	}

	// 对外监听 + 空 token 应被拒绝(空 token 可被空 Bearer 绕过鉴权)。
	cfg.Security.Token = ""
	if reason := unsafeAgentConfigReason(cfg); !strings.Contains(reason, "token 不能为空") {
		t.Fatalf("对外监听 + 空 token 应被拒绝,got %q", reason)
	}

	// 仅空格的 token 同样视为空。
	cfg.Security.Token = "   "
	if reason := unsafeAgentConfigReason(cfg); !strings.Contains(reason, "token 不能为空") {
		t.Fatalf("对外监听 + 纯空白 token 应被拒绝,got %q", reason)
	}
}
