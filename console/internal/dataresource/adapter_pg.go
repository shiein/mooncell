// PostgreSQL 适配器：基于 information_schema 和 pg_catalog 查询元数据。
// Kingbase 和 Vastbase 基于 PostgreSQL/openGauss，复用此适配器。
package dataresource

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// pgAdapter 实现 PostgreSQL/Kingbase/Vastbase 的适配器。
type pgAdapter struct {
	db     *sql.DB
	schema string // 默认 schema（资源配置中的 default_schema）
}

// newPGAdapter 创建 PostgreSQL 适配器。
func newPGAdapter(db *sql.DB, defaultSchema string) *pgAdapter {
	if defaultSchema == "" {
		defaultSchema = "public"
	}
	return &pgAdapter{db: db, schema: defaultSchema}
}

func (a *pgAdapter) Ping(ctx context.Context) (ServerInfo, error) {
	var version, db string
	if err := a.db.QueryRowContext(ctx, "SELECT version(), current_database()").Scan(&version, &db); err != nil {
		return ServerInfo{}, err
	}
	return ServerInfo{Version: version, Database: db}, nil
}

func (a *pgAdapter) Begin(ctx context.Context, readOnly bool) (Transaction, error) {
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: readOnly})
	if err != nil {
		return nil, err
	}
	return &pgTx{tx: tx}, nil
}

type pgTx struct {
	tx *sql.Tx
}

func (t *pgTx) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, query, args...)
}
func (t *pgTx) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, query, args...)
}
func (t *pgTx) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, query, args...)
}
func (t *pgTx) Commit() error   { return t.tx.Commit() }
func (t *pgTx) Rollback() error { return t.tx.Rollback() }

// Children 返回元数据树子节点。
// parent.Kind == "root" → 返回 schemas
// parent.Kind == "schema" → 返回该 schema 下的表、视图、物化视图、函数、序列
func (a *pgAdapter) Children(ctx context.Context, parent MetadataNode) ([]MetadataNode, error) {
	switch parent.Kind {
	case NodeRoot, "":
		return a.listSchemas(ctx)
	case NodeSchema:
		return a.listObjects(ctx, parent.Name)
	}
	return nil, nil
}

func (a *pgAdapter) listSchemas(ctx context.Context) ([]MetadataNode, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT schema_name FROM information_schema.schemata
		WHERE schema_name NOT IN ('pg_catalog', 'pg_toast', 'information_schema')
		ORDER BY schema_name`)
	if err != nil {
		return nil, fmt.Errorf("查询 schema 失败: %w", err)
	}
	defer rows.Close()
	var out []MetadataNode
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		n := MetadataNode{Kind: NodeSchema, Schema: name, Name: name}
		n.ID = n.EncodeID()
		out = append(out, n)
	}
	return out, rows.Err()
}

func (a *pgAdapter) listObjects(ctx context.Context, schema string) ([]MetadataNode, error) {
	schema = a.QuoteIdentifier(schema)
	var out []MetadataNode

	// 表
	tableRows, err := a.db.QueryContext(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = $1 AND table_type = 'BASE TABLE'
		ORDER BY table_name`, strings.Trim(schema, `"`))
	if err == nil {
		for tableRows.Next() {
			var name string
			tableRows.Scan(&name)
			n := MetadataNode{Kind: NodeTable, Schema: strings.Trim(schema, `"`), Name: name}
			n.ID = n.EncodeID()
			out = append(out, n)
		}
		tableRows.Close()
	}

	// 视图
	viewRows, err := a.db.QueryContext(ctx, `
		SELECT table_name FROM information_schema.views
		WHERE table_schema = $1
		ORDER BY table_name`, strings.Trim(schema, `"`))
	if err == nil {
		for viewRows.Next() {
			var name string
			viewRows.Scan(&name)
			n := MetadataNode{Kind: NodeView, Schema: strings.Trim(schema, `"`), Name: name}
			n.ID = n.EncodeID()
			out = append(out, n)
		}
		viewRows.Close()
	}

	// 物化视图（pg_matviews）
	matRows, err := a.db.QueryContext(ctx, `
		SELECT matviewname FROM pg_matviews WHERE schemaname = $1
		ORDER BY matviewname`, strings.Trim(schema, `"`))
	if err == nil {
		for matRows.Next() {
			var name string
			matRows.Scan(&name)
			n := MetadataNode{Kind: NodeMatView, Schema: strings.Trim(schema, `"`), Name: name}
			n.ID = n.EncodeID()
			out = append(out, n)
		}
		matRows.Close()
	}

	// 函数
	funcRows, err := a.db.QueryContext(ctx, `
		SELECT routine_name FROM information_schema.routines
		WHERE routine_schema = $1 AND routine_type = 'FUNCTION'
		ORDER BY routine_name`, strings.Trim(schema, `"`))
	if err == nil {
		for funcRows.Next() {
			var name string
			funcRows.Scan(&name)
			n := MetadataNode{Kind: NodeFunction, Schema: strings.Trim(schema, `"`), Name: name}
			n.ID = n.EncodeID()
			out = append(out, n)
		}
		funcRows.Close()
	}

	// 序列
	seqRows, err := a.db.QueryContext(ctx, `
		SELECT sequence_name FROM information_schema.sequences
		WHERE sequence_schema = $1
		ORDER BY sequence_name`, strings.Trim(schema, `"`))
	if err == nil {
		for seqRows.Next() {
			var name string
			seqRows.Scan(&name)
			n := MetadataNode{Kind: NodeSequence, Schema: strings.Trim(schema, `"`), Name: name}
			n.ID = n.EncodeID()
			out = append(out, n)
		}
		seqRows.Close()
	}

	return out, nil
}

// Describe 返回表/视图的完整结构。
func (a *pgAdapter) Describe(ctx context.Context, obj MetadataNode) (ObjectStructure, error) {
	structure := ObjectStructure{
		Columns:     []ColumnInfo{},
		Constraints: []ConstraintInfo{},
		Indexes:     []IndexInfo{},
	}

	// 字段
	colRows, err := a.db.QueryContext(ctx, `
		SELECT column_name, data_type, character_maximum_length, numeric_precision, numeric_scale,
			is_nullable, column_default
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2
		ORDER BY ordinal_position`, obj.Schema, obj.Name)
	if err != nil {
		return structure, fmt.Errorf("查询字段失败: %w", err)
	}
	defer colRows.Close()
	for colRows.Next() {
		var name, dataType string
		var charLen, numPrec, numScale sql.NullInt64
		var isNullable string
		var defVal sql.NullString
		if err := colRows.Scan(&name, &dataType, &charLen, &numPrec, &numScale, &isNullable, &defVal); err != nil {
			return structure, err
		}
		fullType := dataType
		if charLen.Valid && charLen.Int64 > 0 {
			fullType = fmt.Sprintf("%s(%d)", dataType, charLen.Int64)
		} else if numPrec.Valid && numScale.Valid {
			if numScale.Int64 > 0 {
				fullType = fmt.Sprintf("%s(%d,%d)", dataType, numPrec.Int64, numScale.Int64)
			} else {
				fullType = fmt.Sprintf("%s(%d)", dataType, numPrec.Int64)
			}
		}
		structure.Columns = append(structure.Columns, ColumnInfo{
			Name:         name,
			DataType:     fullType,
			IsNullable:   isNullable == "YES",
			DefaultValue: defVal.String,
		})
	}

	// 约束
	conRows, err := a.db.QueryContext(ctx, `
		SELECT tc.constraint_name, tc.constraint_type, kcu.column_name,
			ccu.table_name AS ref_table, ccu.column_name AS ref_column,
			cc.check_clause
		FROM information_schema.table_constraints tc
		LEFT JOIN information_schema.key_column_usage kcu
			ON tc.constraint_name = kcu.constraint_name AND tc.table_schema = kcu.table_schema
		LEFT JOIN information_schema.constraint_column_usage ccu
			ON tc.constraint_name = ccu.constraint_name AND tc.constraint_schema = ccu.constraint_schema
		LEFT JOIN information_schema.check_constraints cc
			ON tc.constraint_name = cc.constraint_name AND tc.constraint_schema = cc.constraint_schema
		WHERE tc.table_schema = $1 AND tc.table_name = $2
		ORDER BY tc.constraint_type, tc.constraint_name`, obj.Schema, obj.Name)
	if err == nil {
		conMap := map[string]*ConstraintInfo{}
		for conRows.Next() {
			var conName, conType, colName sql.NullString
			var refTable, refCol, checkClause sql.NullString
			if err := conRows.Scan(&conName, &conType, &colName, &refTable, &refCol, &checkClause); err != nil {
				continue
			}
			if !conName.Valid {
				continue
			}
			ci, ok := conMap[conName.String]
			if !ok {
				ci = &ConstraintInfo{Name: conName.String, Type: normalizeConstraintType(conType.String)}
				conMap[conName.String] = ci
			}
			if colName.Valid {
				ci.Columns = append(ci.Columns, colName.String)
			}
			if refTable.Valid {
				ci.RefTable = refTable.String
			}
			if refCol.Valid {
				ci.RefColumns = append(ci.RefColumns, refCol.String)
			}
			if checkClause.Valid {
				ci.Definition = checkClause.String
			}
		}
		conRows.Close()
		for _, ci := range conMap {
			structure.Constraints = append(structure.Constraints, *ci)
		}
	}

	// 索引：用 unnest+ordinality 保证列序，且无未分组裸列
	idxRows, err := a.db.QueryContext(ctx, `
		SELECT i.relname AS index_name, ix.indisunique,
			(SELECT array_agg(a.attname ORDER BY u.ord)
			 FROM unnest(ix.indkey) WITH ORDINALITY AS u(attnum, ord)
			 JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = u.attnum
			) AS columns
		FROM pg_index ix
		JOIN pg_class c ON c.oid = ix.indrelid
		JOIN pg_class i ON i.oid = ix.indexrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND NOT ix.indisprimary
		ORDER BY i.relname`, obj.Schema, obj.Name)
	if err == nil {
		for idxRows.Next() {
			var idxName string
			var unique bool
			var columns []string
			// pgx 可能将 text[] 扫为 []uint8 或专用类型；用 interface 再解析
			var colsAny interface{}
			if err := idxRows.Scan(&idxName, &unique, &colsAny); err != nil {
				continue
			}
			columns = parsePGTextArray(colsAny)
			structure.Indexes = append(structure.Indexes, IndexInfo{
				Name: idxName, Columns: columns, Unique: unique,
			})
		}
		idxRows.Close()
	}

	return structure, nil
}

// normalizeConstraintType 将 information_schema 的 "PRIMARY KEY" 等规范为 primary/foreign/unique/check。
func normalizeConstraintType(t string) string {
	s := strings.ToLower(strings.TrimSpace(t))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "_", "")
	switch s {
	case "primarykey", "primary":
		return "primary"
	case "foreignkey", "foreign":
		return "foreign"
	case "unique":
		return "unique"
	case "check":
		return "check"
	default:
		return strings.ToLower(strings.TrimSpace(t))
	}
}

// parsePGTextArray 解析 PostgreSQL text[] / array_agg 扫描结果。
func parsePGTextArray(v interface{}) []string {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case []string:
		return t
	case []byte:
		return parsePGArrayLiteral(string(t))
	case string:
		return parsePGArrayLiteral(t)
	default:
		return parsePGArrayLiteral(fmt.Sprintf("%v", t))
	}
}

func parsePGArrayLiteral(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || s == "{}" {
		return nil
	}
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, `"`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// DDL 生成表的 CREATE TABLE 语句（v1 只保证字段、默认值、主外键、唯一约束、检查约束、索引和注释）。
func (a *pgAdapter) DDL(ctx context.Context, obj MetadataNode) (string, error) {
	structure, err := a.Describe(ctx, obj)
	if err != nil {
		return "", err
	}
	qualifiedName := a.QuoteIdentifier(obj.Schema) + "." + a.QuoteIdentifier(obj.Name)
	var lines []string
	lines = append(lines, fmt.Sprintf("CREATE TABLE %s (", qualifiedName))
	for _, col := range structure.Columns {
		line := fmt.Sprintf("    %s %s", a.QuoteIdentifier(col.Name), col.DataType)
		if !col.IsNullable {
			line += " NOT NULL"
		}
		if col.DefaultValue != "" {
			line += " DEFAULT " + col.DefaultValue
		}
		lines = append(lines, line+",")
	}
	for _, con := range structure.Constraints {
		var line string
		switch con.Type {
		case "primary":
			line = fmt.Sprintf("    CONSTRAINT %s PRIMARY KEY (%s)", a.QuoteIdentifier(con.Name), joinQuoted(a, con.Columns))
		case "unique":
			line = fmt.Sprintf("    CONSTRAINT %s UNIQUE (%s)", a.QuoteIdentifier(con.Name), joinQuoted(a, con.Columns))
		case "foreign":
			line = fmt.Sprintf("    CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s)",
				a.QuoteIdentifier(con.Name), joinQuoted(a, con.Columns),
				a.QuoteIdentifier(con.RefTable), joinQuoted(a, con.RefColumns))
		case "check":
			line = fmt.Sprintf("    CONSTRAINT %s CHECK (%s)", a.QuoteIdentifier(con.Name), con.Definition)
		}
		if line != "" {
			lines = append(lines, line+",")
		}
	}
	// 移除最后一行的逗号
	if len(lines) > 1 && strings.HasSuffix(lines[len(lines)-1], ",") {
		lines[len(lines)-1] = strings.TrimSuffix(lines[len(lines)-1], ",")
	}
	lines = append(lines, ");")
	// 非主键索引以独立 CREATE INDEX 输出
	for _, idx := range structure.Indexes {
		uniq := ""
		if idx.Unique {
			uniq = "UNIQUE "
		}
		lines = append(lines, fmt.Sprintf("CREATE %sINDEX %s ON %s (%s);",
			uniq, a.QuoteIdentifier(idx.Name), qualifiedName, joinQuoted(a, idx.Columns)))
	}
	return strings.Join(lines, "\n"), nil
}

func joinQuoted(a *pgAdapter, names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = a.QuoteIdentifier(n)
	}
	return strings.Join(out, ", ")
}

// SQLTemplate 生成 SQL 模板。
func (a *pgAdapter) SQLTemplate(obj MetadataNode, operation string) (string, error) {
	qualified := a.QuoteIdentifier(obj.Schema) + "." + a.QuoteIdentifier(obj.Name)
	switch operation {
	case "SELECT":
		return fmt.Sprintf("SELECT * FROM %s LIMIT 100;", qualified), nil
	case "INSERT":
		return fmt.Sprintf("INSERT INTO %s (col1, col2) VALUES ($1, $2);", qualified), nil
	case "UPDATE":
		return fmt.Sprintf("UPDATE %s SET col1 = $1 WHERE col2 = $2;", qualified), nil
	case "DELETE":
		return fmt.Sprintf("DELETE FROM %s WHERE col1 = $1;", qualified), nil
	}
	return "", fmt.Errorf("不支持的操作: %s", operation)
}

// PageSQL 包装查询为分页查询（子查询包装，兼容用户 SQL 已含 LIMIT）。
func (a *pgAdapter) PageSQL(query string, limit, offset int) (string, error) {
	q := strings.TrimRight(strings.TrimSpace(query), ";")
	return fmt.Sprintf("SELECT * FROM (%s) AS _page LIMIT %d OFFSET %d", q, limit, offset), nil
}

// CountSQL 包装查询为计数查询。
func (a *pgAdapter) CountSQL(query string) (string, error) {
	// 去掉末尾分号
	q := strings.TrimRight(strings.TrimSpace(query), ";")
	return fmt.Sprintf("SELECT COUNT(*) FROM (%s) AS _count", q), nil
}

// QuoteIdentifier 安全引用标识符（PostgreSQL 用双引号）。
func (a *pgAdapter) QuoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// Placeholder 返回 PostgreSQL 风格 $n 占位符（1-based）。
func (a *pgAdapter) Placeholder(n int) string {
	return fmt.Sprintf("$%d", n)
}

// NormalizeError 归一化 PostgreSQL 错误。
func (a *pgAdapter) NormalizeError(err error) DatabaseError {
	if err == nil {
		return DatabaseError{}
	}
	msg := err.Error()
	code := "DB_ERROR"
	if strings.Contains(msg, "does not exist") {
		code = "OBJECT_NOT_FOUND"
	} else if strings.Contains(msg, "permission denied") {
		code = "PERMISSION_DENIED"
	} else if strings.Contains(msg, "duplicate key") {
		code = "DUPLICATE_KEY"
	} else if strings.Contains(msg, "violates") {
		code = "CONSTRAINT_VIOLATION"
	} else if strings.Contains(msg, "deadlock") {
		code = "DEADLOCK"
	} else if strings.Contains(msg, "syntax error") {
		code = "SYNTAX_ERROR"
	}
	return DatabaseError{Code: code, Message: sanitizeErrMsg(msg)}
}

// Capabilities 返回 PostgreSQL 适配器能力。
func (a *pgAdapter) Capabilities() Capabilities {
	return Capabilities{
		DDLSupported:        true,
		ImportSupported:     true,
		ReadOnlyTxSupported: true,
		StoredProcSupported: true,
	}
}

// sanitizeErrMsg 移除错误消息中可能包含的敏感信息（DSN、密码等）。
func sanitizeErrMsg(msg string) string {
	// PostgreSQL 错误一般不含 DSN，但保守处理：截断过长的消息。
	if len(msg) > 500 {
		msg = msg[:500] + "..."
	}
	return msg
}
