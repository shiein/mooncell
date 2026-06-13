package main

import (
	"log"

	"github.com/BurntSushi/toml"
)

// Config 对应 config.toml;缺省值在 loadConfig 给出,配置文件可只覆盖部分字段。
type Config struct {
	Server   ServerConfig   `toml:"server"`
	Security SecurityConfig `toml:"security"`
	Paths    PathsConfig    `toml:"paths"`
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

func loadConfig(path string) *Config {
	cfg := &Config{
		Server:   ServerConfig{Addr: "0.0.0.0", Port: 9100},
		Security: SecurityConfig{Token: "mc_ag_change_me"},
		Paths: PathsConfig{
			DeployRoots: []string{"/srv/apps", "/data/web"},
			LogRoots:    []string{"/srv/apps", "/var/log"},
			BackupDir:   "/opt/deploy-agent/backups",
		},
	}
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		log.Printf("[config] 未能读取 %s(%v),使用内置默认配置", path, err)
	}
	return cfg
}
