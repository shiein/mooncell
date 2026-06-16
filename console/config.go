package main

import (
	"errors"
	"io/fs"
	"log"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config 对应 config.toml;缺省值在 loadConfig 里给出,配置文件可只覆盖部分字段。
type Config struct {
	Server   ServerConfig   `toml:"server"`
	Database DatabaseConfig `toml:"database"`
	Session  SessionConfig  `toml:"session"`
	Admin    AdminConfig    `toml:"admin"`
	Agent    AgentConfig    `toml:"agent"`
	Cabinet  CabinetConfig  `toml:"cabinet"`
	AgentBin AgentBinConfig `toml:"agent_bin"`
	Demo     DemoConfig     `toml:"demo"`
	Deploy   DeployUpload   `toml:"deploy"`
}

// AgentBinConfig:Agent 升级包(按架构)的存储目录,默认 agentbin。
type AgentBinConfig struct {
	Dir string `toml:"dir"`
}

// DeployUpload:部署制品上传的传输层硬上限(MB)。仅内存阈值不足以防 DoS——
// 必须在传输层用 MaxBytesReader 截断,否则超大制品会先落 Console 临时盘撑爆磁盘。
type DeployUpload struct {
	MaxUploadMB int `toml:"max_upload_mb"`
}

// DemoConfig:是否把前端 INITIAL_* 种子数据写库(演示用)。生产默认关,空库即全真实。
type DemoConfig struct {
	Seed bool `toml:"seed"`
}

// CabinetConfig 文件柜:二进制落盘目录 + 是否允许匿名(免登录)上传 + 单文件上限(MB)。
type CabinetConfig struct {
	Dir         string `toml:"dir"`
	AnonUpload  bool   `toml:"anon_upload"`
	MaxUploadMB int    `toml:"max_upload_mb"` // 单文件上限(MB),默认 200;部署大制品建议调高并提示上传方
}

// AgentConfig 是 Console 连接 Agent 的地址与共享 token(单机版默认指向本机 Agent)。
type AgentConfig struct {
	Addr  string `toml:"addr"`
	Token string `toml:"token"`
}

type ServerConfig struct {
	Addr string `toml:"addr"`
	Port int    `toml:"port"`
}

type DatabaseConfig struct {
	Path string `toml:"path"`
}

type SessionConfig struct {
	TTLHours int `toml:"ttl_hours"`
}

type AdminConfig struct {
	Username string `toml:"username"`
	Password string `toml:"password"`
}

const (
	defaultAdminPassword = "1qaz@WSX"
	defaultAgentToken    = "mc_ag_change_me"
)

func loadConfig(path string) *Config {
	cfg := &Config{
		Server:   ServerConfig{Addr: "127.0.0.1", Port: 8787},
		Database: DatabaseConfig{Path: "mooncell.db"},
		Session:  SessionConfig{TTLHours: 168}, // 7 天
		Admin:    AdminConfig{Username: "admin", Password: defaultAdminPassword},
		Agent:    AgentConfig{Addr: "127.0.0.1:9100", Token: defaultAgentToken},
		Cabinet:  CabinetConfig{Dir: "cabinet", MaxUploadMB: 200},
		Deploy:   DeployUpload{MaxUploadMB: 1024}, // 1GB:容纳常见 war/dist,又有界(分块上传是更优的长期方案)
	}
	// 文件不存在 → 只允许本地回环默认配置;文件存在但解析失败(语法错误/权限等)→ 直接退出。
	// 显式对外监听时若仍使用周知默认密码/token,同样拒绝启动。
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			log.Printf("[config] 未找到 %s,使用仅本机可访问的内置默认配置", path)
		} else {
			log.Fatalf("[config] 解析 %s 失败(拒绝以默认凭据启动): %v", path, err)
		}
	}
	if reason := unsafeConsoleConfigReason(cfg); reason != "" {
		log.Fatalf("[config] 拒绝以不安全配置对外启动: %s", reason)
	}
	return cfg
}

func externalBind(addr string) bool {
	switch strings.TrimSpace(addr) {
	case "", "0.0.0.0", "::", "[::]":
		return true
	default:
		return false
	}
}

func unsafeConsoleConfigReason(cfg *Config) string {
	if !externalBind(cfg.Server.Addr) {
		return ""
	}
	if cfg.Admin.Password == defaultAdminPassword {
		return "server.addr 对外监听时不能使用默认管理员密码"
	}
	if cfg.Agent.Token == defaultAgentToken {
		return "server.addr 对外监听时不能使用默认 Agent token"
	}
	return ""
}
