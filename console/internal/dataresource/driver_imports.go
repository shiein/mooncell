// 驱动匿名导入：使 database/sql 注册各厂商驱动名。
// 集中在一个文件，方便构建验证和将来增删驱动。
// Vastbase 必须由官方本地 pq 注册 openGauss；包未随仓库提供前不导入替代驱动。
package dataresource

import (
	_ "gitea.com/kingbase/gokb"        // DriverKingbase → "kingbase"
	_ "gitee.com/chunanyong/dm"        // DriverDM → "dm"
	_ "github.com/go-sql-driver/mysql" // DriverMySQL → "mysql"
	_ "github.com/jackc/pgx/v5/stdlib" // DriverPostgreSQL → "pgx"
)
