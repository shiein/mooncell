package main

import (
	"errors"
	"io/fs"
	"log"
	"net"
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
	Artifact ArtifactConfig `toml:"artifact"`
	AgentBin AgentBinConfig `toml:"agent_bin"`
	Demo     DemoConfig     `toml:"demo"`
	Deploy   DeployUpload   `toml:"deploy"`
	Audit    AuditConfig    `toml:"audit"`
	Monitor  MonitorConfig  `toml:"monitor"`
}

// ArtifactConfig 制品仓库:版本化制品库的落盘目录。上传一次 → 留存 → 可部署到 N 台 Agent /
// 一键重部署任意历史制品。复用文件柜的二进制落盘 + sha256 校验地基,但无 TTL(手动管理)。
type ArtifactConfig struct {
	Dir string `toml:"dir"`
}

// MonitorConfig:部署后持续健康巡检 + Agent 资源指标留存。
// IntervalSeconds<=0 关闭巡检(回滚开关);MetricsKeepHours 是指标时序保留窗口(小时)。
type MonitorConfig struct {
	IntervalSeconds  int `toml:"interval_seconds"`
	MetricsKeepHours int `toml:"metrics_keep_hours"`
}

// AuditConfig.Keep 是审计记录的保留条数:append-only 无限增长,每小时裁剪只留最近 Keep 条,
// 更早的记录被清理(<=0 表示不裁剪)。hydrate 仅下发最近一窗,更早记录经 GET /api/audit 分页查。
type AuditConfig struct {
	Keep int `toml:"keep"`
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
	MaxUploadMB int    `toml:"max_upload_mb"` // 单文件上限(MB),默认 300;部署大制品建议调高并提示上传方
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

// SessionConfig.TTLHours 是会话「闲置超时」窗口(小时):每次有动作即滑动续期,
// 闲置满该时长自动失效。配合 session cookie(关浏览器即清),无需 redis。
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
		Session:  SessionConfig{TTLHours: 1}, // 闲置 1 小时无动作自动退出(滑动续期)
		Admin:    AdminConfig{Username: "admin", Password: defaultAdminPassword},
		Agent:    AgentConfig{Addr: "127.0.0.1:9100", Token: defaultAgentToken},
		Cabinet:  CabinetConfig{Dir: "cabinet", MaxUploadMB: 300},
		Artifact: ArtifactConfig{Dir: "artifacts"},
		Deploy:   DeployUpload{MaxUploadMB: 1024}, // 1GB:容纳常见 war/dist,又有界(分块上传是更优的长期方案)
		Audit:    AuditConfig{Keep: 5000},         // 审计保留最近 5000 条,每小时裁剪
		Monitor:  MonitorConfig{IntervalSeconds: 30, MetricsKeepHours: 24},
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

// externalBind 判定监听地址是否对外:只有 loopback 才视为本机安全。
// 反转旧逻辑(原仅认 ""/0.0.0.0/:: 为对外)——绑到 192.168.x.x/10.x.x.x 等内网具体 IP
// 同样是对外暴露,必须强制改凭据。主机名等无法证明是本机 → 保守判对外。
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

func unsafeConsoleConfigReason(cfg *Config) string {
	if !externalBind(cfg.Server.Addr) {
		return ""
	}
	if strings.TrimSpace(cfg.Admin.Password) == "" {
		return "server.addr 对外监听时管理员密码不能为空"
	}
	if cfg.Admin.Password == defaultAdminPassword {
		return "server.addr 对外监听时不能使用默认管理员密码"
	}
	if strings.TrimSpace(cfg.Agent.Token) == "" {
		return "server.addr 对外监听时 Agent token 不能为空(空 token 可被空 Bearer 绕过鉴权)"
	}
	if cfg.Agent.Token == defaultAgentToken {
		return "server.addr 对外监听时不能使用默认 Agent token"
	}
	return ""
}
