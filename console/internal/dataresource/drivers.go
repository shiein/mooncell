// Package dataresource 实现 Mooncell 的数据资源模块：外部数据库的连接管理、元数据浏览、
// SQL 工作台、导入导出等。后端代码统一收拢在本包，不污染 consoleapp。
//
// 本文件仅负责数据库驱动的匿名导入，使 database/sql 能识别各厂商驱动名。
// 驱动选型见 docs/data-resource-design-v1.md 第一节。
package dataresource

import (
	"database/sql"
	"sort"
)

// 驱动名常量：与各驱动 sql.Register 注册名一致，供适配器按类型选择驱动。
const (
	DriverPostgreSQL = "pgx"      // github.com/jackc/pgx/v5/stdlib
	DriverMySQL      = "mysql"    // github.com/go-sql-driver/mysql
	DriverDM         = "dm"       // gitee.com/chunanyong/dm
	DriverKingbase   = "kingbase" // gitea.com/kingbase/gokb
	// 仅用于识别旧配置；官方本地 pq 未提供前不会出现在 DriverCatalog。
	DriverVastbase = "opengauss"
)

// DriverMeta 描述驱动对产品可见的元信息。
type DriverMeta struct {
	ID           string `json:"id"`           // database/sql 注册名 / dbType
	Label        string `json:"label"`        // 展示名
	DefaultPort  int    `json:"defaultPort"`  // 厂商默认端口，供前端切换类型时填充
	Experimental bool   `json:"experimental"` // true=不可在发布说明中宣称已认证支持
	// HasCatalog：是否存在独立于 schema 的「库/catalog」层。
	// false（达梦）：连接实例后仅 schema/用户模式 → 对象，两层结构。
	HasCatalog bool `json:"hasCatalog"`
}

// DefaultPort 返回驱动默认端口；未知类型返回 0。
func DefaultPort(dbType string) int {
	switch dbType {
	case DriverPostgreSQL:
		return 5432
	case DriverMySQL:
		return 3306
	case DriverDM:
		return 5236
	case DriverKingbase:
		return 54321
	case DriverVastbase:
		return 5432
	default:
		return 0
	}
}

// DriverCatalog 返回当前二进制中可创建的数据资源驱动。
func DriverCatalog() []DriverMeta {
	registered := map[string]bool{}
	for _, d := range sql.Drivers() {
		registered[d] = true
	}
	all := []DriverMeta{
		{ID: DriverPostgreSQL, Label: "PostgreSQL", DefaultPort: DefaultPort(DriverPostgreSQL), HasCatalog: true},
		{ID: DriverMySQL, Label: "MySQL", DefaultPort: DefaultPort(DriverMySQL), HasCatalog: true},
		{ID: DriverDM, Label: "达梦 DM8", DefaultPort: DefaultPort(DriverDM), HasCatalog: false},
		{ID: DriverKingbase, Label: "KingbaseES", DefaultPort: DefaultPort(DriverKingbase), HasCatalog: true},
	}
	out := make([]DriverMeta, 0, len(all))
	for _, m := range all {
		if registered[m.ID] {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// SupportedDrivers 返回当前二进制内置的全部数据资源驱动名（排序后稳定）。
func SupportedDrivers() []string {
	cat := DriverCatalog()
	out := make([]string, 0, len(cat))
	for _, m := range cat {
		out = append(out, m.ID)
	}
	return out
}

// IsExperimentalDriver 是否为未完成真机认证的实验性驱动。
func IsExperimentalDriver(dbType string) bool {
	for _, m := range DriverCatalog() {
		if m.ID == dbType {
			return m.Experimental
		}
	}
	return false
}
