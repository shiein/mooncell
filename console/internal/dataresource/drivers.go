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
	DriverPostgreSQL = "pgx"       // github.com/jackc/pgx/v5/stdlib
	DriverMySQL      = "mysql"     // github.com/go-sql-driver/mysql
	DriverDM         = "dm"        // gitee.com/chunanyong/dm
	DriverKingbase   = "kingbase"  // gitea.com/kingbase/gokb
	DriverVastbase   = "opengauss" // gitcode.com/opengauss/openGauss-connector-go-pq（Vastbase G100 基于 openGauss）
)

// SupportedDrivers 返回当前二进制内置的全部数据资源驱动名（排序后稳定）。
// 供前端展示可选项、启动时日志确认。
func SupportedDrivers() []string {
	all := sql.Drivers()
	want := map[string]bool{
		DriverPostgreSQL: true,
		DriverMySQL:      true,
		DriverDM:         true,
		DriverKingbase:   true,
		DriverVastbase:   true,
	}
	out := make([]string, 0, len(want))
	for _, d := range all {
		if want[d] {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}
