// DSN 构建：为每种数据库生成 database/sql 可接受的连接串。
//
// 安全约束：不开放任意 DSN 或任意驱动参数；只接受设计文档定义的固定字段。
// DSN 中包含密码，仅用于 sql.Open，不得出现在日志、错误响应或审计中。
package dataresource

import (
	"fmt"
	"strings"
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

// pgDSN 构建 PostgreSQL/Kingbase/Vastbase 的 key=value DSN。
func pgDSN(r DataResource, password string) string {
	sslmode := r.SSLMode
	if sslmode == "" {
		sslmode = "disable"
	}
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		r.Host, r.Port, r.Username, password, r.DatabaseName, sslmode)
}

// mysqlDSN 构建 MySQL 的 DSN：user:pass@tcp(host:port)/dbname?params
func mysqlDSN(r DataResource, password string) string {
	tls := "false"
	if r.SSLMode == "require" {
		tls = "true"
	}
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?tls=%s&parseTime=true&interpolateParams=true",
		r.Username, password, r.Host, r.Port, r.DatabaseName, tls)
}

// dmDSN 构建达梦 DM 的 DSN：dm://user:pass@host:port
func dmDSN(r DataResource, password string) string {
	// dm 驱动 DSN 格式：dm://user:pass@host:port?schema=xxx
	scheme := "dm://"
	dsn := fmt.Sprintf("%s%s:%s@%s:%d", scheme, r.Username, password, r.Host, r.Port)
	params := []string{}
	if r.DatabaseName != "" {
		params = append(params, "schema="+r.DatabaseName)
	}
	if len(params) > 0 {
		dsn += "?" + strings.Join(params, "&")
	}
	return dsn
}

// DriverName 返回 dbType 对应的 database/sql 驱动注册名。
func DriverName(dbType string) string {
	return dbType // 驱动名与 dbType 常量值一致
}
