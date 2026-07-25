// 服务端执行期限：防止慢查询/导出/导入占满连接池。
package dataresource

import "time"

const (
	// QueryTimeout 单次 SQL 执行（含工作台查询/写）默认上限。
	QueryTimeout = 60 * time.Second
	// CountTimeout 总数统计独立上限，超时则 totalStatus=unavailable。
	CountTimeout = 5 * time.Second
	// ExportTimeout 全量导出上限。
	ExportTimeout = 5 * time.Minute
	// ImportTimeout 导入执行上限。
	ImportTimeout = 10 * time.Minute
	// MetadataTimeout 元数据树/结构/DDL/模板查询上限。
	MetadataTimeout = 30 * time.Second
)
