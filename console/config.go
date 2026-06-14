package main

import (
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
}

// CabinetConfig 文件柜的二进制落盘目录。
type CabinetConfig struct {
	Dir string `toml:"dir"`
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
	}
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		log.Printf("[config] 未能读取 %s(%v),使用内置默认配置", path, err)
	}
	return cfg
}
