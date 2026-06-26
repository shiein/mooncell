package main

import (
	"errors"
	"io/fs"
	"log"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config 对应 config.toml;缺省值在 loadConfig 给出,配置文件可只覆盖部分字段。
type Config struct {
	Server   ServerConfig   `toml:"server"`
	Security SecurityConfig `toml:"security"`
	Paths    PathsConfig    `toml:"paths"`
	Deploy   DeployConfigT  `toml:"deploy"`
}

// DeployConfigT:部署制品上传的传输层硬上限(MB),纵深防御(Console 已有上限,Agent 再兜一层)。
type DeployConfigT struct {
	MaxUploadMB int `toml:"max_upload_mb"`
}

type ServerConfig struct {
	Addr string `toml:"addr"`
	Port int    `toml:"port"`
}

type SecurityConfig struct {
	// Console 添加 Agent 时录入的共享 token;所有请求需带 Authorization: Bearer <token>
	Token string `toml:"token"`
}

// PathsConfig 是 Agent 的安全边界:所有落盘 / 读日志的路径规范化后必须落在白名单根目录内(防穿越)。
type PathsConfig struct {
	DeployRoots []string `toml:"deploy_roots"` // 允许部署落盘的根目录
	LogRoots    []string `toml:"log_roots"`    // 允许读取日志的根目录
	BackupDir   string   `toml:"backup_dir"`   // 备份存储根目录
}

const defaultAgentToken = "mc_ag_change_me"

func loadConfig(path string) *Config {
	cfg := &Config{
		Server:   ServerConfig{Addr: "127.0.0.1", Port: 9100},
		Security: SecurityConfig{Token: defaultAgentToken},
		Paths: PathsConfig{
			DeployRoots: []string{"/srv/apps", "/data/web"},
			LogRoots:    []string{"/srv/apps", "/var/log"},
			BackupDir:   "/opt/deploy-agent/backups",
		},
		Deploy: DeployConfigT{MaxUploadMB: 1024},
	}
	// 文件不存在 → 只允许本地回环默认配置;文件存在但解析失败 → 直接退出。
	// 显式对外监听时若仍使用周知默认 token,同样拒绝启动。
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			log.Printf("[config] 未找到 %s,使用仅本机可访问的内置默认配置", path)
		} else {
			log.Fatalf("[config] 解析 %s 失败(拒绝以默认 token 启动): %v", path, err)
		}
	}
	if reason := unsafeAgentConfigReason(cfg); reason != "" {
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

func unsafeAgentConfigReason(cfg *Config) string {
	if !externalBind(cfg.Server.Addr) {
		return ""
	}
	if strings.TrimSpace(cfg.Security.Token) == "" {
		return "server.addr 对外监听时 Agent token 不能为空(空 token 可被空 Bearer 绕过鉴权)"
	}
	if cfg.Security.Token == defaultAgentToken {
		return "server.addr 对外监听时不能使用默认 Agent token"
	}
	return ""
}
