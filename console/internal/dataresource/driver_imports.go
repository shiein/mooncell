// 驱动匿名导入：使 database/sql 注册各厂商驱动名。
// 集中在一个文件，方便构建验证和将来增删驱动。
//
// Vastbase 说明：设计文档要求官方本地 pq（replace => ./third_party/vastbase/pq，
// 驱动名 openGauss）并做与 pgx 共存及 CGO_ENABLED=0 真机认证。当前实现改用
// openGauss-connector-go-pq（可编译、驱动名 opengauss），**仅保证可编译与驱动
// 注册共存检测**，不声明已完成 Vastbase 真机认证；发布说明须标注实验性。
package dataresource

import (
	_ "github.com/jackc/pgx/v5/stdlib" // DriverPostgreSQL → "pgx"
	_ "github.com/go-sql-driver/mysql" // DriverMySQL → "mysql"
	_ "gitee.com/chunanyong/dm"        // DriverDM → "dm"
	_ "gitea.com/kingbase/gokb"        // DriverKingbase → "kingbase"
	// 实验性：见文件头 Vastbase 说明，非设计文档指定的本地 pq 包。
	_ "gitcode.com/opengauss/openGauss-connector-go-pq" // DriverVastbase → "opengauss"
)
