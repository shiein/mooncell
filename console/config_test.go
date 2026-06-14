package main

import (
	"strings"
	"testing"
)

func TestUnsafeConsoleConfigReason(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{Addr: "127.0.0.1"},
		Admin:  AdminConfig{Password: defaultAdminPassword},
		Agent:  AgentConfig{Token: defaultAgentToken},
	}
	if reason := unsafeConsoleConfigReason(cfg); reason != "" {
		t.Fatalf("本地回环默认配置应允许开发启动,got %q", reason)
	}

	cfg.Server.Addr = "0.0.0.0"
	if reason := unsafeConsoleConfigReason(cfg); !strings.Contains(reason, "默认管理员密码") {
		t.Fatalf("对外监听 + 默认管理员密码应被拒绝,got %q", reason)
	}

	cfg.Admin.Password = "changed"
	if reason := unsafeConsoleConfigReason(cfg); !strings.Contains(reason, "默认 Agent token") {
		t.Fatalf("对外监听 + 默认 Agent token 应被拒绝,got %q", reason)
	}

	cfg.Agent.Token = "changed-token"
	if reason := unsafeConsoleConfigReason(cfg); reason != "" {
		t.Fatalf("对外监听但凭据已修改应允许,got %q", reason)
	}
}
