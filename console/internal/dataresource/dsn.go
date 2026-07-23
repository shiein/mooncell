// DSN 构建：为每种数据库生成 database/sql 可接受的连接串。
//
// 安全约束：不开放任意 DSN 或任意驱动参数；只接受设计文档定义的固定字段。
// DSN 中包含密码，仅用于 sql.Open，不得出现在日志、错误响应或审计中。
package dataresource

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// BuildDSN 根据资源配置和明文密码生成对应驱动的 DSN。
func BuildDSN(r DataResource, password string) string {
	switch r.DBType {
	case DriverPostgreSQL:
		return pgDSN(r, password)
	case DriverMySQL:
		return mysqlDSN(r, password)
	case DriverDM:
		return dmDSN(r, password)
	case DriverKingbase:
		return pgDSN(r, password) // Kingbase gokb 兼容 PostgreSQL DSN 格式
	case DriverVastbase:
		return pgDSN(r, password) // Vastbase openGauss 兼容 PostgreSQL DSN 格式
	default:
		return ""
	}
}

// pgQuote 按 libpq key=value 规则用单引号包裹并转义 \ 与 '。
func pgQuote(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `'`, `\'`) + "'"
}

// pgDSN 构建 PostgreSQL/Kingbase/Vastbase 的 key=value DSN。
// 用户名、密码、host、dbname 均做 libpq 引号转义，避免特殊字符破坏字段边界。
func pgDSN(r DataResource, password string) string {
	sslmode := r.SSLMode
	if sslmode == "" {
		sslmode = "disable"
	}
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		pgQuote(r.Host), r.Port, pgQuote(r.Username), pgQuote(password), pgQuote(r.DatabaseName), sslmode)
}

// mysqlDSN 构建 MySQL 的 DSN：使用驱动 Config.FormatDSN 正确转义用户名/密码。
func mysqlDSN(r DataResource, password string) string {
	cfg := mysql.NewConfig()
	cfg.User = r.Username
	cfg.Passwd = password
	cfg.Net = "tcp"
	cfg.Addr = fmt.Sprintf("%s:%d", r.Host, r.Port)
	cfg.DBName = r.DatabaseName
	cfg.ParseTime = true
	cfg.InterpolateParams = true
	if r.SSLMode == "require" {
		cfg.TLSConfig = "true"
	} else {
		cfg.TLSConfig = "false"
	}
	return cfg.FormatDSN()
}

// dmDSN 构建达梦 DM 的 DSN：dm://user:pass@host:port，URL 编码用户/密码组件。
func dmDSN(r DataResource, password string) string {
	u := &url.URL{
		Scheme: "dm",
		User:   url.UserPassword(r.Username, password),
		Host:   fmt.Sprintf("%s:%d", r.Host, r.Port),
	}
	if r.DatabaseName != "" {
		q := url.Values{}
		q.Set("schema", r.DatabaseName)
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// DriverName 返回 dbType 对应的 database/sql 驱动注册名。
func DriverName(dbType string) string {
	return dbType // 驱动名与 dbType 常量值一致
}
