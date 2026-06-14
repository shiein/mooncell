package main

import (
	"errors"
	"io/fs"
	"log"

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
	Demo     DemoConfig     `toml:"demo"`
	Deploy   DeployUpload   `toml:"deploy"`
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

// CabinetConfig 文件柜:二进制落盘目录 + 是否允许匿名(免登录)上传。
type CabinetConfig struct {
	Dir        string `toml:"dir"`
	AnonUpload bool   `toml:"anon_upload"`
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

func loadConfig(path string) *Config {
	cfg := &Config{
		Server:   ServerConfig{Addr: "0.0.0.0", Port: 8787},
		Database: DatabaseConfig{Path: "mooncell.db"},
		Session:  SessionConfig{TTLHours: 168}, // 7 天
		Admin:    AdminConfig{Username: "admin", Password: "jch@9388"},
		Agent:    AgentConfig{Addr: "127.0.0.1:9100", Token: "mc_ag_change_me"},
		Cabinet:  CabinetConfig{Dir: "cabinet"},
		Deploy:   DeployUpload{MaxUploadMB: 1024}, // 1GB:容纳常见 war/dist,又有界(分块上传是更优的长期方案)
	}
	// 文件不存在 → 用内置默认(单机首跑可接受);文件存在但解析失败(语法错误/权限等)→ 直接退出,
	// 否则一个配置 typo 会把生产实例悄悄降级为周知默认凭据(admin 默认口令 / 默认 token / 0.0.0.0)。
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			log.Printf("[config] 未找到 %s,使用内置默认配置", path)
		} else {
			log.Fatalf("[config] 解析 %s 失败(拒绝以默认凭据启动): %v", path, err)
		}
	}
	return cfg
}
