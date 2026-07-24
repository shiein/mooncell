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

// Children: root → 绑定 schema（用户模式）；schema → 表/视图/函数/过程/序列分组。
// 达梦资源绑定「实例 + 默认 schema」，不在树中枚举实例上其它用户（避免出现无关模式如业务库中的其它账号）。
func (a *dmAdapter) Children(ctx context.Context, parent MetadataNode) ([]MetadataNode, error) {
	switch parent.Kind {
	case NodeRoot, "":
		return a.listSchemas(ctx)
	case NodeSchema:
		return objectGroupNodes(parent.Name, NodeTablesFolder, NodeViewsFolder, NodeFunctionsFolder, NodeProceduresFolder, NodeSequencesFolder), nil
	case NodeTablesFolder:
		return a.listTables(ctx, parent.Schema)
	case NodeViewsFolder:
		return a.listViews(ctx, parent.Schema)
	case NodeFunctionsFolder:
		return a.listObjectsByType(ctx, parent.Schema, "FUNCTION", NodeFunction)
	case NodeProceduresFolder:
		return a.listObjectsByType(ctx, parent.Schema, "PROCEDURE", NodeProcedure)
	case NodeSequencesFolder:
		return a.listObjectsByType(ctx, parent.Schema, "SEQUENCE", NodeSequence)
	}
	return nil, nil
}

// dmSystemOwners 达梦/Oracle 风格系统 schema（仅在未配置绑定 schema 时的兜底枚举中过滤）。
var dmSystemOwners = map[string]bool{
	"SYS": true, "SYSDBA": true, "SYSSSO": true, "SYSAUDITOR": true,
	"CTISYS": true, "SYSGEO": true, "PUBLIC": true, "OUTLN": true, "DMSERVER": true,
}

// boundSchemaName 规范化适配器绑定的 schema（去掉驱动 DSN 可能带的双引号）。
func (a *dmAdapter) boundSchemaName() string {
	s := strings.TrimSpace(a.schema)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return s
}

func (a *dmAdapter) listSchemas(ctx context.Context) ([]MetadataNode, error) {
	// 已配置默认 schema：只展示绑定模式，与「一资源一库/模式」一致
	if bound := a.boundSchemaName(); bound != "" {
		n := MetadataNode{Kind: NodeSchema, Schema: bound, Name: bound}
		n.ID = n.EncodeID()
		return []MetadataNode{n}, nil
	}
	// 未配置时兜底：当前用户（USER）
	var current string
	if err := a.db.QueryRowContext(ctx, "SELECT USER FROM DUAL").Scan(&current); err == nil && strings.TrimSpace(current) != "" {
		current = strings.TrimSpace(current)
		n := MetadataNode{Kind: NodeSchema, Schema: current, Name: current}
		n.ID = n.EncodeID()
		return []MetadataNode{n}, nil
	}
	// 再兜底：枚举非系统用户（可能权限不足）
	rows, err := a.db.QueryContext(ctx, `
		SELECT username FROM all_users ORDER BY username`)
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
		if dmSystemOwners[strings.ToUpper(name)] {
			continue
		}
		n := MetadataNode{Kind: NodeSchema, Schema: name, Name: name}
		n.ID = n.EncodeID()
		out = append(out, n)
	}
	return out, rows.Err()
}

func (a *dmAdapter) listTables(ctx context.Context, owner string) ([]MetadataNode, error) {
	var out []MetadataNode
	tableRows, err := a.db.QueryContext(ctx, `
		SELECT object_name FROM all_objects
		WHERE owner = ? AND object_type = 'TABLE'
		ORDER BY object_name`, owner)
	if err != nil {
		return nil, err
	}
	defer tableRows.Close()
	for tableRows.Next() {
		var name string
		if err := tableRows.Scan(&name); err != nil {
			return nil, err
		}
		n := MetadataNode{Kind: NodeTable, Schema: owner, Name: name}
		n.ID = n.EncodeID()
		out = append(out, n)
	}
	return out, tableRows.Err()
}

func (a *dmAdapter) listViews(ctx context.Context, owner string) ([]MetadataNode, error) {
	return a.listObjectsByType(ctx, owner, "VIEW", NodeView)
}

func (a *dmAdapter) listObjectsByType(ctx context.Context, owner, objectType string, kind MetadataNodeKind) ([]MetadataNode, error) {
	var out []MetadataNode
	rows, err := a.db.QueryContext(ctx, `
		SELECT object_name FROM all_objects
		WHERE owner = ? AND object_type = ?
		ORDER BY object_name`, owner, objectType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		n := MetadataNode{Kind: kind, Schema: owner, Name: name}
		n.ID = n.EncodeID()
		out = append(out, n)
	}
	return out, rows.Err()
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

	// 主键/唯一/外键：all_constraints + all_cons_columns（Oracle 兼容视图）
	conRows, err := a.db.QueryContext(ctx, `
		SELECT c.constraint_name, c.constraint_type, cc.column_name, cc.position
		FROM all_constraints c
		JOIN all_cons_columns cc
		  ON cc.owner = c.owner AND cc.constraint_name = c.constraint_name
		WHERE c.owner = ? AND c.table_name = ?
		  AND c.constraint_type IN ('P', 'U', 'R')
		ORDER BY c.constraint_type, c.constraint_name, cc.position`, obj.Schema, obj.Name)
	if err == nil {
		type acc struct {
			name, typ string
			cols      []string
		}
		order := []string{}
		byName := map[string]*acc{}
		for conRows.Next() {
			var name, ctype, col string
			var pos sql.NullInt64
			if err := conRows.Scan(&name, &ctype, &col, &pos); err != nil {
				continue
			}
			a2, ok := byName[name]
			if !ok {
				a2 = &acc{name: name, typ: normalizeConstraintType(ctype)}
				byName[name] = a2
				order = append(order, name)
			}
			a2.cols = append(a2.cols, col)
		}
		conRows.Close()
		for _, name := range order {
			ac := byName[name]
			structure.Constraints = append(structure.Constraints, ConstraintInfo{
				Name: ac.name, Type: ac.typ, Columns: ac.cols,
			})
		}
	}
	return structure, nil
}

func (a *dmAdapter) DDL(ctx context.Context, obj MetadataNode) (string, error) {
	// 视图：避免误生成 CREATE TABLE
	if obj.Kind == NodeView || obj.Kind == NodeMatView {
		return "", fmt.Errorf("达梦视图 DDL 导出尚未完整支持")
	}
	structure, err := a.Describe(ctx, obj)
	if err != nil {
		return "", err
	}
	qualified := a.QuoteIdentifier(obj.Schema) + "." + a.QuoteIdentifier(obj.Name)
	parts := make([]string, 0, len(structure.Columns)+1)
	for _, col := range structure.Columns {
		line := fmt.Sprintf("    %s %s", a.QuoteIdentifier(col.Name), col.DataType)
		if !col.IsNullable {
			line += " NOT NULL"
		}
		if col.DefaultValue != "" {
			line += " DEFAULT " + col.DefaultValue
		}
		parts = append(parts, line)
	}
	for _, c := range structure.Constraints {
		if len(c.Columns) == 0 {
			continue
		}
		quoted := make([]string, len(c.Columns))
		for i, col := range c.Columns {
			quoted[i] = a.QuoteIdentifier(col)
		}
		cols := strings.Join(quoted, ", ")
		switch c.Type {
		case "primary":
			parts = append(parts, fmt.Sprintf("    CONSTRAINT %s PRIMARY KEY (%s)", a.QuoteIdentifier(c.Name), cols))
		case "unique":
			parts = append(parts, fmt.Sprintf("    CONSTRAINT %s UNIQUE (%s)", a.QuoteIdentifier(c.Name), cols))
		case "foreign":
			// Describe 当前未填 RefTable/RefColumns 时跳过，避免生成不完整 FK
			if c.RefTable != "" && len(c.RefColumns) > 0 {
				refCols := make([]string, len(c.RefColumns))
				for i, col := range c.RefColumns {
					refCols[i] = a.QuoteIdentifier(col)
				}
				// RefTable 可能已含 schema.table（与 PG 适配器一致）
				refTable := c.RefTable
				if !strings.Contains(refTable, ".") {
					refTable = a.QuoteIdentifier(refTable)
				} else {
					sp := strings.SplitN(refTable, ".", 2)
					refTable = a.QuoteIdentifier(sp[0]) + "." + a.QuoteIdentifier(sp[1])
				}
				parts = append(parts, fmt.Sprintf("    CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s)",
					a.QuoteIdentifier(c.Name), cols, refTable, strings.Join(refCols, ", ")))
			}
		}
	}
	// 索引与列注释尚未从系统视图导出；调用方勿假定 DDL 可完整重建表
	return fmt.Sprintf("CREATE TABLE %s (\n%s\n);", qualified, strings.Join(parts, ",\n")), nil
}

func (a *dmAdapter) SQLTemplate(obj MetadataNode, operation string) (string, error) {
	qualified := a.QuoteIdentifier(obj.Schema) + "." + a.QuoteIdentifier(obj.Name)
	switch operation {
	case "SELECT":
		// 不写 rownum/LIMIT：行数由服务端隐式分页（默认 100）
		return fmt.Sprintf("SELECT * FROM %s;", qualified), nil
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
	// 子查询包装，避免与用户 SQL 已有 LIMIT 冲突；DM 8.1+ 支持 LIMIT/OFFSET。
	q := strings.TrimRight(strings.TrimSpace(query), ";")
	return fmt.Sprintf("SELECT * FROM (%s) _page LIMIT %d OFFSET %d", q, limit, offset), nil
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
		// 视图 DDL 与完整索引/外键导出仍不完整；表级基础 DDL+PK 可用
		DDLSupported:        true,
		ImportSupported:     true,
		ReadOnlyTxSupported: true,
		StoredProcSupported: true,
	}
}
