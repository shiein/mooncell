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
	// 默认关闭客户端拼参：走二进制协议预编译绑定，避免扩大转义边界依赖。
	cfg.InterpolateParams = false
	if r.SSLMode == "require" {
		cfg.TLSConfig = "true"
	} else {
		cfg.TLSConfig = "false"
	}
	return cfg.FormatDSN()
}

// dmSchema 达梦连接后 SET SCHEMA 的目标：默认 schema > database_name > 用户名。
// 达梦无独立 catalog（库）层，用户模式与 schema 对应（见驱动 README）。
func dmSchema(r DataResource) string {
	if s := strings.TrimSpace(r.DefaultSchema); s != "" {
		return s
	}
	if s := strings.TrimSpace(r.DatabaseName); s != "" {
		return s
	}
	return strings.TrimSpace(r.Username)
}

// quoteDMSchemaIdent 为达梦 SET SCHEMA 生成安全标识符。
// 官方驱动实现为：exec("set schema " + schema) 无自动加引号，
// ADMIN 等保留字会报 -2007 语法分析错误，必须双引号包裹。
func quoteDMSchemaIdent(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	// 已是双引号标识符则不再包一层
	if len(name) >= 2 && name[0] == '"' && name[len(name)-1] == '"' {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// dmDSN 构建达梦 DM 的 DSN：dm://user:pass@host:port?schema=…。
//
// 注意：官方驱动 parseDSN 对 query 值不做 URL 解码（直接 Split 赋值），
// 因此 schema 不能走 url.Values.Encode（会把 "ADMIN" 编成 %22ADMIN%22，
// 最终执行 set schema %22ADMIN%22 触发 -2007）。schema 段须原样拼接；
// 标识符用双引号包裹以兼容 ADMIN 等保留字。
func dmDSN(r DataResource, password string) string {
	u := &url.URL{
		Scheme: "dm",
		User:   url.UserPassword(r.Username, password),
		Host:   fmt.Sprintf("%s:%d", r.Host, r.Port),
	}
	base := u.String()
	schema := quoteDMSchemaIdent(dmSchema(r))
	if schema == "" {
		return base
	}
	return base + "?schema=" + schema
}

// DriverName 返回 dbType 对应的 database/sql 驱动注册名。
func DriverName(dbType string) string {
	return dbType // 驱动名与 dbType 常量值一致
}

// BoundSchema 返回适配器绑定用的默认 schema/owner。
// MySQL 常等同 database；达梦优先 default_schema / database_name / 用户名。
func BoundSchema(r DataResource) string {
	if r.DBType == DriverDM {
		return dmSchema(r)
	}
	if s := strings.TrimSpace(r.DefaultSchema); s != "" {
		return s
	}
	return strings.TrimSpace(r.DatabaseName)
}
