// 达梦 DM 适配器：DM8 兼容 Oracle/SQL 标准，元数据查询使用系统视图。
// v1 实现基础元数据查询和 SQL 执行；DDL 生成能力有限。
package dataresource

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type dmAdapter struct {
	db     *sql.DB
	schema string // DM 中 schema 即 owner
}

func newDMAdapter(db *sql.DB, defaultSchema string) *dmAdapter {
	return &dmAdapter{db: db, schema: defaultSchema}
}

func (a *dmAdapter) Ping(ctx context.Context) (ServerInfo, error) {
	var version string
	// DM 可通过 SELECT * FROM v$version 获取版本，但格式为多行。
	// 简化：使用 banner 字段。
	row := a.db.QueryRowContext(ctx, "SELECT banner FROM v$version WHERE rownum = 1")
	if err := row.Scan(&version); err != nil {
		// 降级：尝试简单查询
		var v interface{}
		if err2 := a.db.QueryRowContext(ctx, "SELECT 1").Scan(&v); err2 != nil {
			return ServerInfo{}, err
		}
		version = "DM8"
	}
	return ServerInfo{Version: version, Database: a.schema}, nil
}

func (a *dmAdapter) Begin(ctx context.Context, readOnly bool) (Transaction, error) {
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: readOnly})
	if err != nil {
		return nil, err
	}
	return &pgTx{tx: tx}, nil
}

// Children: root → schemas(owners), schema → tables/views/functions/procedures/sequences
func (a *dmAdapter) Children(ctx context.Context, parent MetadataNode) ([]MetadataNode, error) {
	switch parent.Kind {
	case NodeRoot, "":
		return a.listSchemas(ctx)
	case NodeSchema:
		return a.listObjects(ctx, parent.Name)
	}
	return nil, nil
}

func (a *dmAdapter) listSchemas(ctx context.Context) ([]MetadataNode, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT username FROM all_users ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("查询 schema 失败: %w", err)
	}
	defer rows.Close()
	var out []MetadataNode
	for rows.Next() {
		var name string
		rows.Scan(&name)
		n := MetadataNode{Kind: NodeSchema, Schema: name, Name: name}
		n.ID = n.EncodeID()
		out = append(out, n)
	}
	return out, rows.Err()
}

func (a *dmAdapter) listObjects(ctx context.Context, owner string) ([]MetadataNode, error) {
	var out []MetadataNode
	// 表
	tableRows, err := a.db.QueryContext(ctx, `
		SELECT object_name FROM all_objects
		WHERE owner = ? AND object_type = 'TABLE'
		ORDER BY object_name`, owner)
	if err == nil {
		for tableRows.Next() {
			var name string
			tableRows.Scan(&name)
			n := MetadataNode{Kind: NodeTable, Schema: owner, Name: name}
			n.ID = n.EncodeID()
			out = append(out, n)
		}
		tableRows.Close()
	}
	// 视图
	viewRows, err := a.db.QueryContext(ctx, `
		SELECT object_name FROM all_objects
		WHERE owner = ? AND object_type = 'VIEW'
		ORDER BY object_name`, owner)
	if err == nil {
		for viewRows.Next() {
			var name string
			viewRows.Scan(&name)
			n := MetadataNode{Kind: NodeView, Schema: owner, Name: name}
			n.ID = n.EncodeID()
			out = append(out, n)
		}
		viewRows.Close()
	}
	// 函数
	funcRows, err := a.db.QueryContext(ctx, `
		SELECT object_name FROM all_objects
		WHERE owner = ? AND object_type = 'FUNCTION'
		ORDER BY object_name`, owner)
	if err == nil {
		for funcRows.Next() {
			var name string
			funcRows.Scan(&name)
			n := MetadataNode{Kind: NodeFunction, Schema: owner, Name: name}
			n.ID = n.EncodeID()
			out = append(out, n)
		}
		funcRows.Close()
	}
	// 存储过程
	procRows, err := a.db.QueryContext(ctx, `
		SELECT object_name FROM all_objects
		WHERE owner = ? AND object_type = 'PROCEDURE'
		ORDER BY object_name`, owner)
	if err == nil {
		for procRows.Next() {
			var name string
			procRows.Scan(&name)
			n := MetadataNode{Kind: NodeProcedure, Schema: owner, Name: name}
			n.ID = n.EncodeID()
			out = append(out, n)
		}
		procRows.Close()
	}
	// 序列
	seqRows, err := a.db.QueryContext(ctx, `
		SELECT object_name FROM all_objects
		WHERE owner = ? AND object_type = 'SEQUENCE'
		ORDER BY object_name`, owner)
	if err == nil {
		for seqRows.Next() {
			var name string
			seqRows.Scan(&name)
			n := MetadataNode{Kind: NodeSequence, Schema: owner, Name: name}
			n.ID = n.EncodeID()
			out = append(out, n)
		}
		seqRows.Close()
	}
	return out, nil
}

func (a *dmAdapter) Describe(ctx context.Context, obj MetadataNode) (ObjectStructure, error) {
	structure := ObjectStructure{
		Columns: []ColumnInfo{}, Constraints: []ConstraintInfo{}, Indexes: []IndexInfo{},
	}
	// 字段：DM 使用 all_tab_columns
	colRows, err := a.db.QueryContext(ctx, `
		SELECT column_name, data_type, data_length, data_precision, data_scale,
			nullable, data_default
		FROM all_tab_columns
		WHERE owner = ? AND table_name = ?
		ORDER BY column_id`, obj.Schema, obj.Name)
	if err != nil {
		return structure, fmt.Errorf("查询字段失败: %w", err)
	}
	defer colRows.Close()
	for colRows.Next() {
		var name, dataType string
		var dataLen, dataPrec, dataScale sql.NullInt64
		var nullable string
		var defVal sql.NullString
		if err := colRows.Scan(&name, &dataType, &dataLen, &dataPrec, &dataScale, &nullable, &defVal); err != nil {
			return structure, err
		}
		fullType := dataType
		if dataPrec.Valid && dataScale.Valid {
			if dataScale.Int64 > 0 {
				fullType = fmt.Sprintf("%s(%d,%d)", dataType, dataPrec.Int64, dataScale.Int64)
			} else if dataPrec.Int64 > 0 {
				fullType = fmt.Sprintf("%s(%d)", dataType, dataPrec.Int64)
			}
		} else if dataLen.Valid && dataLen.Int64 > 0 && dataType == "VARCHAR2" {
			fullType = fmt.Sprintf("%s(%d)", dataType, dataLen.Int64)
		}
		structure.Columns = append(structure.Columns, ColumnInfo{
			Name: name, DataType: fullType, IsNullable: nullable == "Y",
			DefaultValue: defVal.String,
		})
	}
	return structure, nil
}

func (a *dmAdapter) DDL(ctx context.Context, obj MetadataNode) (string, error) {
	// DM 可用 DBMS_METADATA.GET_DDL，但复杂度高。v1 从 Describe 生成基础 DDL。
	structure, err := a.Describe(ctx, obj)
	if err != nil {
		return "", err
	}
	qualified := a.QuoteIdentifier(obj.Schema) + "." + a.QuoteIdentifier(obj.Name)
	var lines []string
	lines = append(lines, fmt.Sprintf("CREATE TABLE %s (", qualified))
	for i, col := range structure.Columns {
		line := fmt.Sprintf("    %s %s", a.QuoteIdentifier(col.Name), col.DataType)
		if !col.IsNullable {
			line += " NOT NULL"
		}
		if col.DefaultValue != "" {
			line += " DEFAULT " + col.DefaultValue
		}
		if i < len(structure.Columns)-1 {
			line += ","
		}
		lines = append(lines, line)
	}
	lines = append(lines, ");")
	return strings.Join(lines, "\n"), nil
}

func (a *dmAdapter) SQLTemplate(obj MetadataNode, operation string) (string, error) {
	qualified := a.QuoteIdentifier(obj.Schema) + "." + a.QuoteIdentifier(obj.Name)
	switch operation {
	case "SELECT":
		return fmt.Sprintf("SELECT * FROM %s WHERE rownum <= 100;", qualified), nil
	case "INSERT":
		return fmt.Sprintf("INSERT INTO %s (col1, col2) VALUES (?, ?);", qualified), nil
	case "UPDATE":
		return fmt.Sprintf("UPDATE %s SET col1 = ? WHERE col2 = ?;", qualified), nil
	case "DELETE":
		return fmt.Sprintf("DELETE FROM %s WHERE col1 = ?;", qualified), nil
	}
	return "", fmt.Errorf("不支持的操作: %s", operation)
}

func (a *dmAdapter) PageSQL(query string, limit, offset int) (string, error) {
	// DM 支持 LIMIT/OFFSET（8.1+），也支持 rownum。保守用 LIMIT/OFFSET。
	return fmt.Sprintf("%s LIMIT %d OFFSET %d", query, limit, offset), nil
}

func (a *dmAdapter) CountSQL(query string) (string, error) {
	q := strings.TrimRight(strings.TrimSpace(query), ";")
	return fmt.Sprintf("SELECT COUNT(*) FROM (%s) _count", q), nil
}

func (a *dmAdapter) QuoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// Placeholder 返回达梦风格 ? 占位符。
func (a *dmAdapter) Placeholder(n int) string {
	return "?"
}

func (a *dmAdapter) NormalizeError(err error) DatabaseError {
	if err == nil {
		return DatabaseError{}
	}
	msg := err.Error()
	code := "DB_ERROR"
	if strings.Contains(msg, "does not exist") || strings.Contains(msg, "ORA-00942") {
		code = "OBJECT_NOT_FOUND"
	} else if strings.Contains(msg, "insufficient privileges") {
		code = "PERMISSION_DENIED"
	} else if strings.Contains(msg, "unique constraint") {
		code = "DUPLICATE_KEY"
	} else if strings.Contains(msg, "violated") {
		code = "CONSTRAINT_VIOLATION"
	} else if strings.Contains(msg, "deadlock") {
		code = "DEADLOCK"
	} else if strings.Contains(msg, "syntax") {
		code = "SYNTAX_ERROR"
	}
	return DatabaseError{Code: code, Message: sanitizeErrMsg(msg)}
}

func (a *dmAdapter) Capabilities() Capabilities {
	return Capabilities{
		DDLSupported:        true,  // 基础 DDL 生成
		ImportSupported:     true,
		ReadOnlyTxSupported: true,
		StoredProcSupported: true,
	}
}
