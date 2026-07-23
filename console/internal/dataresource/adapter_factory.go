// 适配器工厂：根据 dbType 创建对应的适配器实例。
package dataresource

import (
	"database/sql"
	"fmt"
)

// NewAdapter 根据数据库类型创建适配器。
// db 是已打开的外部数据库连接池，defaultSchema 来自资源配置。
func NewAdapter(db *sql.DB, dbType, defaultSchema string) (DataSourceAdapter, error) {
	switch dbType {
	case DriverPostgreSQL, DriverKingbase, DriverVastbase:
		return newPGAdapter(db, defaultSchema), nil
	case DriverMySQL:
		return newMySQLAdapter(db, defaultSchema), nil
	case DriverDM:
		return newDMAdapter(db, defaultSchema), nil
	default:
		return nil, fmt.Errorf("不支持的数据库类型: %s", dbType)
	}
}
