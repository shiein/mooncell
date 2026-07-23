package consoleapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigReadsLegacyArtifactDirForRemoval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[artifact]\ndir = \"/srv/mooncell/old-artifacts\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(path)
	if cfg.LegacyArtifact.Dir != "/srv/mooncell/old-artifacts" {
		t.Fatalf("应读取旧 artifact.dir 供迁移清理，got %q", cfg.LegacyArtifact.Dir)
	}
}

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

	// 对外监听 + 空管理员密码应被拒绝(空密码不能凭空放行)。
	cfg.Admin.Password = ""
	cfg.Agent.Token = "changed-token"
	if reason := unsafeConsoleConfigReason(cfg); !strings.Contains(reason, "管理员密码不能为空") {
		t.Fatalf("对外监听 + 空管理员密码应被拒绝,got %q", reason)
	}

	// 对外监听 + 空 Agent token 应被拒绝(空 token 可被空 Bearer 绕过鉴权)。
	cfg.Admin.Password = "changed"
	cfg.Agent.Token = ""
	if reason := unsafeConsoleConfigReason(cfg); !strings.Contains(reason, "Agent token 不能为空") {
		t.Fatalf("对外监听 + 空 Agent token 应被拒绝,got %q", reason)
	}

	// 仅空格的 token 同样视为空。
	cfg.Agent.Token = "   "
	if reason := unsafeConsoleConfigReason(cfg); !strings.Contains(reason, "Agent token 不能为空") {
		t.Fatalf("对外监听 + 纯空白 token 应被拒绝,got %q", reason)
	}
}
