// 适配器契约：每种数据库实现同一最小接口，统一元数据浏览和 SQL 执行行为。
//
// 设计文档第三节「适配器契约」：
//   - 动态数据库对象名只能来自已读取的元数据，由适配器安全引用。
//   - 元数据过滤值参数化绑定。
//   - 不在前端拼接 DDL、导入 SQL 或对象限定名。
//   - 一项能力不支持时由 Capabilities 明确关闭菜单，不使用空结果伪装成功。
package dataresource

import (
	"context"
	"database/sql"
	"encoding/base64"
	"strings"
)

// MetadataNodeKind 元数据节点类型。
type MetadataNodeKind string

const (
	NodeRoot             MetadataNodeKind = "root"               // 根：代表数据库本身
	NodeSchema           MetadataNodeKind = "schema"             // schema/owner
	NodeTablesFolder     MetadataNodeKind = "tables_folder"      // 虚拟分组：表
	NodeViewsFolder      MetadataNodeKind = "views_folder"       // 虚拟分组：视图（含物化视图）
	NodeFunctionsFolder  MetadataNodeKind = "functions_folder"   // 虚拟分组：函数
	NodeProceduresFolder MetadataNodeKind = "procedures_folder"  // 虚拟分组：存储过程
	NodeSequencesFolder  MetadataNodeKind = "sequences_folder"   // 虚拟分组：序列
	NodeTriggersFolder   MetadataNodeKind = "triggers_folder"    // 虚拟分组：触发器
	NodeTable            MetadataNodeKind = "table"              // 表
	NodeView             MetadataNodeKind = "view"               // 视图
	NodeMatView          MetadataNodeKind = "matview"            // 物化视图
	NodeFunction         MetadataNodeKind = "function"           // 函数
	NodeProcedure        MetadataNodeKind = "procedure"          // 存储过程
	NodeSequence         MetadataNodeKind = "sequence"           // 序列
	NodeTrigger          MetadataNodeKind = "trigger"            // 触发器
)

// objectGroupNodes 在业务 schema 下按类型分组（表/视图/函数等），系统 schema 已在 listSchemas 过滤。
func objectGroupNodes(schema string, kinds ...MetadataNodeKind) []MetadataNode {
	if len(kinds) == 0 {
		kinds = []MetadataNodeKind{NodeTablesFolder, NodeViewsFolder, NodeFunctionsFolder, NodeProceduresFolder, NodeSequencesFolder, NodeTriggersFolder}
	}
	labels := map[MetadataNodeKind]string{
		NodeTablesFolder:     "表",
		NodeViewsFolder:      "视图",
		NodeFunctionsFolder:  "函数",
		NodeProceduresFolder: "存储过程",
		NodeSequencesFolder:  "序列",
		NodeTriggersFolder:   "触发器",
	}
	out := make([]MetadataNode, 0, len(kinds))
	for _, k := range kinds {
		n := MetadataNode{Kind: k, Schema: schema, Name: labels[k]}
		if n.Name == "" {
			n.Name = string(k)
		}
		n.ID = n.EncodeID()
		out = append(out, n)
	}
	return out
}

// MetadataNode 是元数据树中的一个节点。
// ID 是服务端编码的不透明标识符，前端不可解码；服务端解码后仍重新校验。
type MetadataNode struct {
	Kind   MetadataNodeKind `json:"kind"`
	Schema string           `json:"schema,omitempty"` // 所属 schema/owner
	Name   string           `json:"name"`             // 对象名
	// ID 是编码后的节点标识，供前端引用（如 parentId/nodeId）。
	// ID 为不透明编码（base64url），服务端解码后重新校验。
	ID string `json:"id,omitempty"`
}

// EncodeID 将节点三要素编码为不透明 ID。
// 使用 base64url(kind\0schema\0name)，避免 schema/name 含 ":" 时拆分错误。
func (n MetadataNode) EncodeID() string {
	raw := string(n.Kind) + "\x00" + n.Schema + "\x00" + n.Name
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeID 从编码 ID 解析节点。服务端解码后仍须重新校验对象存在性。
// 仅接受 EncodeID 产出的 base64url(kind\0schema\0name)；nodeId 从不持久化，无需旧格式兼容。
func DecodeID(id string) (MetadataNode, bool) {
	if id == "" {
		return MetadataNode{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return MetadataNode{}, false
	}
	parts := strings.SplitN(string(b), "\x00", 3)
	if len(parts) != 3 {
		return MetadataNode{}, false
	}
	return MetadataNode{
		Kind:   MetadataNodeKind(parts[0]),
		Schema: parts[1],
		Name:   parts[2],
		ID:     id,
	}, true
}

// ColumnInfo 描述表字段。
type ColumnInfo struct {
	Name         string `json:"name"`
	DataType     string `json:"dataType"` // 完整类型（含长度、精度）
	IsNullable   bool   `json:"isNullable"`
	DefaultValue string `json:"defaultValue,omitempty"`
	Comment      string `json:"comment,omitempty"`
}

// ConstraintInfo 描述约束。
type ConstraintInfo struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"` // primary/unique/foreign/check
	Columns []string `json:"columns"`
	// 外键专用
	RefTable   string   `json:"refTable,omitempty"`
	RefColumns []string `json:"refColumns,omitempty"`
	Definition string   `json:"definition,omitempty"` // 检查约束条件等
}

// IndexInfo 描述索引。
type IndexInfo struct {
	Name       string   `json:"name"`
	Columns    []string `json:"columns"`
	Unique     bool     `json:"unique"`
	Definition string   `json:"definition,omitempty"`
}

// ObjectStructure 是表/视图的完整结构。
type ObjectStructure struct {
	Columns     []ColumnInfo     `json:"columns"`     // 字段列表
	Constraints []ConstraintInfo `json:"constraints"` // 约束
	Indexes     []IndexInfo      `json:"indexes"`     // 索引
}

// ServerInfo 是 Ping 返回的服务器信息。
type ServerInfo struct {
	Version  string `json:"version"`
	Database string `json:"database"`
}

// DatabaseError 是归一化后的数据库错误。
type DatabaseError struct {
	Code     string `json:"code"`               // 稳定错误码
	Message  string `json:"message"`            // 用户可读消息（脱敏）
	SQLState string `json:"sqlState,omitempty"` // SQLSTATE（已脱敏）
}

// Capabilities 声明适配器支持的能力，不支持的能力在前端关闭对应菜单。
type Capabilities struct {
	DDLSupported        bool `json:"ddlSupported"`        // 是否支持 DDL 导出
	ImportSupported     bool `json:"importSupported"`     // 是否支持导入
	ReadOnlyTxSupported bool `json:"readOnlyTxSupported"` // 是否支持只读事务
	StoredProcSupported bool `json:"storedProcSupported"` // 是否支持存储过程
}

// Transaction 表示一个手工事务。
type Transaction interface {
	Exec(ctx context.Context, sql string, args ...any) (sql.Result, error)
	Query(ctx context.Context, sql string, args ...any) (*sql.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) *sql.Row
	Commit() error
	Rollback() error
}

// DataSourceAdapter 是每种数据库实现的最小接口。
type DataSourceAdapter interface {
	// Ping 测试连接并返回服务器信息。
	Ping(ctx context.Context) (ServerInfo, error)
	// Begin 开启事务（readOnly=true 时为只读事务）。
	Begin(ctx context.Context, readOnly bool) (Transaction, error)

	// Children 返回父节点的子节点（元数据树按需加载）。
	Children(ctx context.Context, parent MetadataNode) ([]MetadataNode, error)
	// Describe 返回表/视图的完整结构。
	Describe(ctx context.Context, object MetadataNode) (ObjectStructure, error)
	// DDL 返回对象的 DDL 语句。
	DDL(ctx context.Context, object MetadataNode) (string, error)
	// SQLTemplate 生成 SQL 模板（SELECT/INSERT/UPDATE/DELETE）。
	SQLTemplate(object MetadataNode, operation string) (string, error)

	// PageSQL 包装查询语句为分页查询（子查询包装，避免与用户已有 LIMIT 冲突）。
	PageSQL(sql string, limit, offset int) (string, error)
	// CountSQL 包装查询语句为计数查询。
	CountSQL(sql string) (string, error)
	// QuoteIdentifier 安全引用标识符。
	QuoteIdentifier(name string) string
	// Placeholder 返回第 n 个参数占位符（1-based）。
	// PostgreSQL 系为 $n，MySQL/达梦为 ?。
	Placeholder(n int) string
	// NormalizeError 归一化数据库错误。
	NormalizeError(error) DatabaseError
	// Capabilities 返回适配器能力声明。
	Capabilities() Capabilities
}
