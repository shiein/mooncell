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
		SELECT cols.column_name, pg_catalog.format_type(attr.atttypid, attr.atttypmod),
			cols.is_nullable, cols.column_default,
			COALESCE(pg_catalog.col_description(cls.oid, attr.attnum), '')
		FROM information_schema.columns cols
		JOIN pg_catalog.pg_namespace ns ON ns.nspname = cols.table_schema
		JOIN pg_catalog.pg_class cls ON cls.relnamespace = ns.oid AND cls.relname = cols.table_name
		JOIN pg_catalog.pg_attribute attr ON attr.attrelid = cls.oid
			AND attr.attname = cols.column_name AND attr.attnum > 0 AND NOT attr.attisdropped
		WHERE cols.table_schema = $1 AND cols.table_name = $2
		ORDER BY cols.ordinal_position`, obj.Schema, obj.Name)
	if err != nil {
		return structure, fmt.Errorf("查询字段失败: %w", err)
	}
	defer colRows.Close()
	for colRows.Next() {
		var name, fullType string
		var isNullable string
		var defVal, comment sql.NullString
		if err := colRows.Scan(&name, &fullType, &isNullable, &defVal, &comment); err != nil {
			return structure, err
		}
		structure.Columns = append(structure.Columns, ColumnInfo{
			Name:         name,
			DataType:     fullType,
			IsNullable:   isNullable == "YES",
			DefaultValue: defVal.String,
			Comment:      comment.String,
		})
	}
	if err := colRows.Err(); err != nil {
		return structure, err
	}
	colRows.Close()

	// 约束：先取 pg_constraint 的权威定义，再按 conkey/confkey 的 ordinality 获取列序。
	// 避免 information_schema 两组列视图连接造成组合约束 N×N 行。
	conRows, err := a.db.QueryContext(ctx, `
		SELECT con.oid, con.conname, con.contype::text,
			pg_catalog.pg_get_constraintdef(con.oid, true),
			COALESCE(ref_ns.nspname, ''), COALESCE(ref_rel.relname, '')
		FROM pg_catalog.pg_constraint con
		JOIN pg_catalog.pg_class rel ON rel.oid = con.conrelid
		JOIN pg_catalog.pg_namespace ns ON ns.oid = rel.relnamespace
		LEFT JOIN pg_catalog.pg_class ref_rel ON ref_rel.oid = con.confrelid
		LEFT JOIN pg_catalog.pg_namespace ref_ns ON ref_ns.oid = ref_rel.relnamespace
		WHERE ns.nspname = $1 AND rel.relname = $2
		ORDER BY con.conname`, obj.Schema, obj.Name)
	if err != nil {
		return structure, fmt.Errorf("查询约束失败: %w", err)
	}
	type pgConstraintRow struct {
		oid                    int64
		name, kind, definition string
		refSchema, refTable    string
	}
	var constraints []pgConstraintRow
	for conRows.Next() {
		var row pgConstraintRow
		if err := conRows.Scan(&row.oid, &row.name, &row.kind, &row.definition, &row.refSchema, &row.refTable); err != nil {
			conRows.Close()
			return structure, err
		}
		constraints = append(constraints, row)
	}
	if err := conRows.Err(); err != nil {
		conRows.Close()
		return structure, err
	}
	conRows.Close()
	for _, row := range constraints {
		ci := ConstraintInfo{
			Name:       row.name,
			Type:       normalizeConstraintType(row.kind),
			Definition: row.definition,
		}
		ci.Columns, err = a.pgConstraintColumns(ctx, row.oid, false)
		if err != nil {
			return structure, fmt.Errorf("查询约束列失败: %w", err)
		}
		if row.refTable != "" {
			ci.RefTable = row.refTable
			if row.refSchema != "" {
				ci.RefTable = row.refSchema + "." + row.refTable
			}
			ci.RefColumns, err = a.pgConstraintColumns(ctx, row.oid, true)
			if err != nil {
				return structure, fmt.Errorf("查询外键列失败: %w", err)
			}
		}
		structure.Constraints = append(structure.Constraints, ci)
	}

	// 索引：排除约束所属索引，避免 DDL 同时输出 UNIQUE/PK 约束和同名索引。
	idxRows, err := a.db.QueryContext(ctx, `
		SELECT i.relname AS index_name, ix.indisunique,
			(SELECT array_agg(a.attname ORDER BY u.ord)
			 FROM unnest(ix.indkey) WITH ORDINALITY AS u(attnum, ord)
			 JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = u.attnum
			) AS columns,
			pg_catalog.pg_get_indexdef(ix.indexrelid)
		FROM pg_index ix
		JOIN pg_class c ON c.oid = ix.indrelid
		JOIN pg_class i ON i.oid = ix.indexrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2
			AND NOT EXISTS (
				SELECT 1 FROM pg_constraint con WHERE con.conindid = ix.indexrelid
			)
		ORDER BY i.relname`, obj.Schema, obj.Name)
	if err != nil {
		return structure, fmt.Errorf("查询索引失败: %w", err)
	}
	for idxRows.Next() {
		var idxName string
		var unique bool
		var definition string
		var columns []string
		// pgx 可能将 text[] 扫为 []uint8 或专用类型；用 interface 再解析
		var colsAny interface{}
		if err := idxRows.Scan(&idxName, &unique, &colsAny, &definition); err != nil {
			idxRows.Close()
			return structure, err
		}
		columns = parsePGTextArray(colsAny)
		structure.Indexes = append(structure.Indexes, IndexInfo{
			Name: idxName, Columns: columns, Unique: unique, Definition: definition,
		})
	}
	if err := idxRows.Err(); err != nil {
		idxRows.Close()
		return structure, err
	}
	idxRows.Close()

	return structure, nil
}

func (a *pgAdapter) pgConstraintColumns(ctx context.Context, oid int64, referenced bool) ([]string, error) {
	key := "con.conkey"
	rel := "con.conrelid"
	if referenced {
		key = "con.confkey"
		rel = "con.confrelid"
	}
	query := fmt.Sprintf(`
		SELECT attr.attname
		FROM pg_catalog.pg_constraint con
		JOIN LATERAL unnest(%s) WITH ORDINALITY AS keys(attnum, ord) ON true
		JOIN pg_catalog.pg_attribute attr ON attr.attrelid = %s AND attr.attnum = keys.attnum
		WHERE con.oid = $1
		ORDER BY keys.ord`, key, rel)
	rows, err := a.db.QueryContext(ctx, query, oid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// normalizeConstraintType 将 information_schema 的 "PRIMARY KEY" 等规范为 primary/foreign/unique/check。
func normalizeConstraintType(t string) string {
	s := strings.ToLower(strings.TrimSpace(t))
	switch s {
	case "p":
		return "primary"
	case "f":
		return "foreign"
	case "u":
		return "unique"
	case "c":
		return "check"
	}
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
	qualifiedName := a.QuoteIdentifier(obj.Schema) + "." + a.QuoteIdentifier(obj.Name)
	if obj.Kind == NodeView || obj.Kind == NodeMatView {
		var definition string
		if err := a.db.QueryRowContext(ctx,
			"SELECT pg_catalog.pg_get_viewdef($1::regclass, true)", qualifiedName).Scan(&definition); err != nil {
			return "", err
		}
		kind := "VIEW"
		if obj.Kind == NodeMatView {
			kind = "MATERIALIZED VIEW"
		}
		return fmt.Sprintf("CREATE %s %s AS\n%s;", kind, qualifiedName, strings.TrimSuffix(definition, ";")), nil
	}
	structure, err := a.Describe(ctx, obj)
	if err != nil {
		return "", err
	}
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
		if con.Definition != "" {
			line = fmt.Sprintf("    CONSTRAINT %s %s", a.QuoteIdentifier(con.Name), con.Definition)
		} else {
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
		if idx.Definition != "" {
			lines = append(lines, strings.TrimSuffix(idx.Definition, ";")+";")
			continue
		}
		uniq := ""
		if idx.Unique {
			uniq = "UNIQUE "
		}
		lines = append(lines, fmt.Sprintf("CREATE %sINDEX %s ON %s (%s);",
			uniq, a.QuoteIdentifier(idx.Name), qualifiedName, joinQuoted(a, idx.Columns)))
	}
	for _, col := range structure.Columns {
		if col.Comment == "" {
			continue
		}
		comment := strings.ReplaceAll(col.Comment, "'", "''")
		lines = append(lines, fmt.Sprintf("COMMENT ON COLUMN %s.%s IS '%s';",
			qualifiedName, a.QuoteIdentifier(col.Name), comment))
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
