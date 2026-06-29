package main

import (
	"errors"
	"io/fs"
	"log"
	"net"
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

// externalBind 判定监听地址是否对外:只有 loopback 才视为本机安全。
// 反转旧逻辑(原仅认 ""/0.0.0.0/:: 为对外)——绑到 192.168.x.x/10.x.x.x 等内网具体 IP
// 同样是对外暴露,必须强制改 token。主机名等无法证明是本机 → 保守判对外。
func externalBind(addr string) bool {
	s := strings.TrimSpace(addr)
	if s == "" {
		return true // 空 = 监听所有网卡
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return true // 主机名等无法证明是本机 → 保守判对外
	}
	return !ip.IsLoopback()
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
