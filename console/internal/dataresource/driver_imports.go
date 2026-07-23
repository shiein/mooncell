// 驱动匿名导入：使 database/sql 注册各厂商驱动名。
// 集中在一个文件，方便构建验证和将来增删驱动。
package dataresource

import (
	_ "github.com/jackc/pgx/v5/stdlib" // DriverPostgreSQL → "pgx"
	_ "github.com/go-sql-driver/mysql" // DriverMySQL → "mysql"
	_ "gitee.com/chunanyong/dm"        // DriverDM → "dm"
	_ "gitea.com/kingbase/gokb"        // DriverKingbase → "kingbase"
	_ "gitcode.com/opengauss/openGauss-connector-go-pq" // DriverVastbase → "opengauss"
)
