// MySQL 适配器：基于 information_schema 查询元数据。
package dataresource

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type mysqlAdapter struct {
	db     *sql.DB
	schema string // MySQL 中 schema 即 database
}

func newMySQLAdapter(db *sql.DB, defaultSchema string) *mysqlAdapter {
	return &mysqlAdapter{db: db, schema: defaultSchema}
}

func (a *mysqlAdapter) Ping(ctx context.Context) (ServerInfo, error) {
	var version, db string
	if err := a.db.QueryRowContext(ctx, "SELECT VERSION(), DATABASE()").Scan(&version, &db); err != nil {
		return ServerInfo{}, err
	}
	return ServerInfo{Version: version, Database: db}, nil
}

func (a *mysqlAdapter) Begin(ctx context.Context, readOnly bool) (Transaction, error) {
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: readOnly})
	if err != nil {
		return nil, err
	}
	return &pgTx{tx: tx}, nil // MySQL 的 *sql.Tx 与 PG 接口一致，复用 pgTx
}

// Children: root → databases, schema(database) → tables/views/functions/procedures/triggers
func (a *mysqlAdapter) Children(ctx context.Context, parent MetadataNode) ([]MetadataNode, error) {
	switch parent.Kind {
	case NodeRoot, "":
		return a.listDatabases(ctx)
	case NodeSchema:
		return a.listTables(ctx, parent.Name)
	}
	return nil, nil
}

func (a *mysqlAdapter) listDatabases(ctx context.Context) ([]MetadataNode, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT schema_name FROM information_schema.schemata
		WHERE schema_name NOT IN ('information_schema', 'mysql', 'performance_schema', 'sys')
		ORDER BY schema_name`)
	if err != nil {
		return nil, fmt.Errorf("查询数据库失败: %w", err)
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

func (a *mysqlAdapter) listTables(ctx context.Context, dbName string) ([]MetadataNode, error) {
	var out []MetadataNode
	// 表
	tableRows, err := a.db.QueryContext(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = ? AND table_type = 'BASE TABLE'
		ORDER BY table_name`, dbName)
	if err == nil {
		for tableRows.Next() {
			var name string
			tableRows.Scan(&name)
			n := MetadataNode{Kind: NodeTable, Schema: dbName, Name: name}
			n.ID = n.EncodeID()
			out = append(out, n)
		}
		tableRows.Close()
	}
	// 视图
	viewRows, err := a.db.QueryContext(ctx, `
		SELECT table_name FROM information_schema.views
		WHERE table_schema = ?
		ORDER BY table_name`, dbName)
	if err == nil {
		for viewRows.Next() {
			var name string
			viewRows.Scan(&name)
			n := MetadataNode{Kind: NodeView, Schema: dbName, Name: name}
			n.ID = n.EncodeID()
			out = append(out, n)
		}
		viewRows.Close()
	}
	// 函数
	funcRows, err := a.db.QueryContext(ctx, `
		SELECT routine_name FROM information_schema.routines
		WHERE routine_schema = ? AND routine_type = 'FUNCTION'
		ORDER BY routine_name`, dbName)
	if err == nil {
		for funcRows.Next() {
			var name string
			funcRows.Scan(&name)
			n := MetadataNode{Kind: NodeFunction, Schema: dbName, Name: name}
			n.ID = n.EncodeID()
			out = append(out, n)
		}
		funcRows.Close()
	}
	// 存储过程
	procRows, err := a.db.QueryContext(ctx, `
		SELECT routine_name FROM information_schema.routines
		WHERE routine_schema = ? AND routine_type = 'PROCEDURE'
		ORDER BY routine_name`, dbName)
	if err == nil {
		for procRows.Next() {
			var name string
			procRows.Scan(&name)
			n := MetadataNode{Kind: NodeProcedure, Schema: dbName, Name: name}
			n.ID = n.EncodeID()
			out = append(out, n)
		}
		procRows.Close()
	}
	// 触发器
	trigRows, err := a.db.QueryContext(ctx, `
		SELECT trigger_name FROM information_schema.triggers
		WHERE trigger_schema = ?
		ORDER BY trigger_name`, dbName)
	if err == nil {
		for trigRows.Next() {
			var name string
			trigRows.Scan(&name)
			n := MetadataNode{Kind: NodeTrigger, Schema: dbName, Name: name}
			n.ID = n.EncodeID()
			out = append(out, n)
		}
		trigRows.Close()
	}
	return out, nil
}

func (a *mysqlAdapter) Describe(ctx context.Context, obj MetadataNode) (ObjectStructure, error) {
	structure := ObjectStructure{
		Columns: []ColumnInfo{}, Constraints: []ConstraintInfo{}, Indexes: []IndexInfo{},
	}
	// 字段
	colRows, err := a.db.QueryContext(ctx, `
		SELECT column_name, column_type, is_nullable, column_default, column_comment
		FROM information_schema.columns
		WHERE table_schema = ? AND table_name = ?
		ORDER BY ordinal_position`, obj.Schema, obj.Name)
	if err != nil {
		return structure, fmt.Errorf("查询字段失败: %w", err)
	}
	defer colRows.Close()
	for colRows.Next() {
		var name, dataType, isNullable string
		var defVal, comment sql.NullString
		if err := colRows.Scan(&name, &dataType, &isNullable, &defVal, &comment); err != nil {
			return structure, err
		}
		structure.Columns = append(structure.Columns, ColumnInfo{
			Name: name, DataType: dataType, IsNullable: isNullable == "YES",
			DefaultValue: defVal.String, Comment: comment.String,
		})
	}
	// 约束（主键、唯一、外键）
	conRows, err := a.db.QueryContext(ctx, `
		SELECT constraint_name, constraint_type
		FROM information_schema.table_constraints
		WHERE table_schema = ? AND table_name = ?
		ORDER BY constraint_type, constraint_name`, obj.Schema, obj.Name)
	if err == nil {
		for conRows.Next() {
			var name, conType string
			if err := conRows.Scan(&name, &conType); err != nil {
				continue
			}
			structure.Constraints = append(structure.Constraints, ConstraintInfo{
				Name: name, Type: strings.ToLower(conType),
			})
		}
		conRows.Close()
	}
	// 索引
	idxRows, err := a.db.QueryContext(ctx, `
		SELECT index_name, column_name, non_unique
		FROM information_schema.statistics
		WHERE table_schema = ? AND table_name = ?
		ORDER BY index_name, seq_in_index`, obj.Schema, obj.Name)
	if err == nil {
		idxMap := map[string]*IndexInfo{}
		for idxRows.Next() {
			var idxName, colName string
			var nonUnique int
			if err := idxRows.Scan(&idxName, &colName, &nonUnique); err != nil {
				continue
			}
			ii, ok := idxMap[idxName]
			if !ok {
				ii = &IndexInfo{Name: idxName, Unique: nonUnique == 0}
				idxMap[idxName] = ii
			}
			ii.Columns = append(ii.Columns, colName)
		}
		idxRows.Close()
		for _, ii := range idxMap {
			structure.Indexes = append(structure.Indexes, *ii)
		}
	}
	return structure, nil
}

func (a *mysqlAdapter) DDL(ctx context.Context, obj MetadataNode) (string, error) {
	// MySQL 可用 SHOW CREATE TABLE 获取完整 DDL
	var ddl sql.NullString
	err := a.db.QueryRowContext(ctx, "SHOW CREATE TABLE "+a.QuoteIdentifier(obj.Schema)+"."+a.QuoteIdentifier(obj.Name)).Scan(&ddl, &ddl)
	if err != nil {
		return "", fmt.Errorf("获取 DDL 失败: %w", err)
	}
	return ddl.String, nil
}

func (a *mysqlAdapter) SQLTemplate(obj MetadataNode, operation string) (string, error) {
	qualified := a.QuoteIdentifier(obj.Schema) + "." + a.QuoteIdentifier(obj.Name)
	switch operation {
	case "SELECT":
		return fmt.Sprintf("SELECT * FROM %s LIMIT 100;", qualified), nil
	case "INSERT":
		return fmt.Sprintf("INSERT INTO %s (col1, col2) VALUES (?, ?);", qualified), nil
	case "UPDATE":
		return fmt.Sprintf("UPDATE %s SET col1 = ? WHERE col2 = ?;", qualified), nil
	case "DELETE":
		return fmt.Sprintf("DELETE FROM %s WHERE col1 = ?;", qualified), nil
	}
	return "", fmt.Errorf("不支持的操作: %s", operation)
}

func (a *mysqlAdapter) PageSQL(query string, limit, offset int) (string, error) {
	return fmt.Sprintf("%s LIMIT %d OFFSET %d", query, limit, offset), nil
}

func (a *mysqlAdapter) CountSQL(query string) (string, error) {
	q := strings.TrimRight(strings.TrimSpace(query), ";")
	return fmt.Sprintf("SELECT COUNT(*) FROM (%s) AS _count", q), nil
}

func (a *mysqlAdapter) QuoteIdentifier(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// Placeholder 返回 MySQL 风格 ? 占位符。
func (a *mysqlAdapter) Placeholder(n int) string {
	return "?"
}

func (a *mysqlAdapter) NormalizeError(err error) DatabaseError {
	if err == nil {
		return DatabaseError{}
	}
	msg := err.Error()
	code := "DB_ERROR"
	if strings.Contains(msg, "Unknown column") || strings.Contains(msg, "doesn't exist") {
		code = "OBJECT_NOT_FOUND"
	} else if strings.Contains(msg, "Access denied") {
		code = "PERMISSION_DENIED"
	} else if strings.Contains(msg, "Duplicate entry") {
		code = "DUPLICATE_KEY"
	} else if strings.Contains(msg, "constraint") || strings.Contains(msg, "foreign key") {
		code = "CONSTRAINT_VIOLATION"
	} else if strings.Contains(msg, "Deadlock") {
		code = "DEADLOCK"
	} else if strings.Contains(msg, "syntax") {
		code = "SYNTAX_ERROR"
	}
	return DatabaseError{Code: code, Message: sanitizeErrMsg(msg)}
}

func (a *mysqlAdapter) Capabilities() Capabilities {
	return Capabilities{
		DDLSupported:        true,
		ImportSupported:     true,
		ReadOnlyTxSupported: true,
		StoredProcSupported: true,
	}
}
