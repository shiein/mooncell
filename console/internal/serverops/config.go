// 服务器运维模块配置与默认值。
// 分块大小固定为 8 MiB（代码常量），不进入可随意调整的配置。
package serverops

import "time"

// Config 对应 config.toml 的 [server_operations] 段。
// enabled=false 时不注册业务路由、不启动清理 goroutine；表迁移仍无条件执行。
type Config struct {
	Enabled bool `toml:"enabled"`

	ConnectTimeoutSeconds int `toml:"connect_timeout_seconds"`
	IdleTimeoutMinutes    int `toml:"idle_timeout_minutes"`
	MaxSessionHours       int `toml:"max_session_hours"`
	MaxSessionsPerUser    int `toml:"max_sessions_per_user"`
	MaxSessionsTotal      int `toml:"max_sessions_total"`

	SFTPMaxUploadMB          int `toml:"sftp_max_upload_mb"`
	SFTPMaxDownloadMB        int `toml:"sftp_max_download_mb"`
	SFTPMaxTransfersPerUser  int `toml:"sftp_max_transfers_per_user"`
	SFTPMaxTransfersTotal    int `toml:"sftp_max_transfers_total"`
	TransferResumeHours      int `toml:"transfer_resume_hours"`

	ZmodemMaxTransferMB int `toml:"zmodem_max_transfer_mb"`
}

// 分块与协议层硬常量（不可由配置随意放宽）。
const (
	// ChunkSize 是 SFTP 上传单块字节数：与部署制品分块一致，便于代理与内存预算。
	ChunkSize = 8 << 20 // 8 MiB

	// 单目录列表上限，超过返回明确错误，避免浏览器卡死。
	maxDirEntries = 10000

	// WebSocket / PTY 边界。
	maxWSBinaryFrame  = 256 << 10 // 256 KiB
	maxPTYOutputQueue = 1 << 20   // 1 MiB 有界输出队列
	maxPasswordBytes  = 1024
	maxPasswordReq    = 64 << 10 // 请求体上限

	// SSH keepalive。
	defaultKeepaliveInterval = 30 * time.Second
	defaultKeepaliveMissed   = 3

	// 密码失败限速：同一 (user, resource, clientIP) 每分钟最多 5 次。
	authFailLimit   = 5
	authFailWindow  = time.Minute
)

// DefaultConfig 返回设计文档建议的默认值（功能默认关闭）。
func DefaultConfig() Config {
	return Config{
		Enabled:                  false,
		ConnectTimeoutSeconds:    15,
		IdleTimeoutMinutes:       30,
		MaxSessionHours:          8,
		MaxSessionsPerUser:       3,
		MaxSessionsTotal:         30,
		SFTPMaxUploadMB:          1024,
		SFTPMaxDownloadMB:        2048,
		SFTPMaxTransfersPerUser:  2,
		SFTPMaxTransfersTotal:    10,
		TransferResumeHours:      24,
		ZmodemMaxTransferMB:      512,
	}
}

// Normalize 填充非法/零值为安全默认。
func (c *Config) Normalize() {
	d := DefaultConfig()
	if c.ConnectTimeoutSeconds <= 0 {
		c.ConnectTimeoutSeconds = d.ConnectTimeoutSeconds
	}
	if c.IdleTimeoutMinutes <= 0 {
		c.IdleTimeoutMinutes = d.IdleTimeoutMinutes
	}
	if c.MaxSessionHours <= 0 {
		c.MaxSessionHours = d.MaxSessionHours
	}
	if c.MaxSessionsPerUser <= 0 {
		c.MaxSessionsPerUser = d.MaxSessionsPerUser
	}
	if c.MaxSessionsTotal <= 0 {
		c.MaxSessionsTotal = d.MaxSessionsTotal
	}
	if c.SFTPMaxUploadMB <= 0 {
		c.SFTPMaxUploadMB = d.SFTPMaxUploadMB
	}
	if c.SFTPMaxDownloadMB <= 0 {
		c.SFTPMaxDownloadMB = d.SFTPMaxDownloadMB
	}
	if c.SFTPMaxTransfersPerUser <= 0 {
		c.SFTPMaxTransfersPerUser = d.SFTPMaxTransfersPerUser
	}
	if c.SFTPMaxTransfersTotal <= 0 {
		c.SFTPMaxTransfersTotal = d.SFTPMaxTransfersTotal
	}
	if c.TransferResumeHours <= 0 {
		c.TransferResumeHours = d.TransferResumeHours
	}
	if c.ZmodemMaxTransferMB <= 0 {
		c.ZmodemMaxTransferMB = d.ZmodemMaxTransferMB
	}
}

func (c Config) connectTimeout() time.Duration {
	return time.Duration(c.ConnectTimeoutSeconds) * time.Second
}

func (c Config) idleTimeout() time.Duration {
	return time.Duration(c.IdleTimeoutMinutes) * time.Minute
}

func (c Config) maxSessionDuration() time.Duration {
	return time.Duration(c.MaxSessionHours) * time.Hour
}

func (c Config) transferResumeTTL() time.Duration {
	return time.Duration(c.TransferResumeHours) * time.Hour
}

func (c Config) maxUploadBytes() int64 {
	return int64(c.SFTPMaxUploadMB) << 20
}

func (c Config) maxDownloadBytes() int64 {
	return int64(c.SFTPMaxDownloadMB) << 20
}
