// MySQL 适配器：基于 information_schema 查询元数据。
package dataresource

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"
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

// boundDatabase 返回本资源绑定的唯一数据库（一资源一库）。
// 优先配置的 defaultSchema/database_name，否则 DATABASE()。
func (a *mysqlAdapter) boundDatabase(ctx context.Context) (string, error) {
	if a.schema != "" {
		return a.schema, nil
	}
	var db string
	if err := a.db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&db); err != nil {
		return "", fmt.Errorf("获取当前数据库失败: %w", err)
	}
	if db == "" {
		return "", fmt.Errorf("资源未绑定数据库")
	}
	return db, nil
}

// ensureBoundSchema 拒绝跨库元数据访问。
func (a *mysqlAdapter) ensureBoundSchema(ctx context.Context, schema string) error {
	bound, err := a.boundDatabase(ctx)
	if err != nil {
		return err
	}
	if schema != "" && schema != bound {
		return fmt.Errorf("禁止访问其他数据库")
	}
	return nil
}

// Children: root → 仅绑定库；schema → 表/视图/函数/过程/触发器分组。
func (a *mysqlAdapter) Children(ctx context.Context, parent MetadataNode) ([]MetadataNode, error) {
	switch parent.Kind {
	case NodeRoot, "":
		return a.listDatabases(ctx)
	case NodeSchema:
		return objectGroupNodes(parent.Name, NodeTablesFolder, NodeViewsFolder, NodeFunctionsFolder, NodeProceduresFolder, NodeTriggersFolder), nil
	case NodeTablesFolder:
		return a.listBaseTables(ctx, parent.Schema)
	case NodeViewsFolder:
		return a.listViews(ctx, parent.Schema)
	case NodeFunctionsFolder:
		return a.listRoutines(ctx, parent.Schema, "FUNCTION", NodeFunction)
	case NodeProceduresFolder:
		return a.listRoutines(ctx, parent.Schema, "PROCEDURE", NodeProcedure)
	case NodeTriggersFolder:
		return a.listTriggers(ctx, parent.Schema)
	}
	return nil, nil
}

func (a *mysqlAdapter) listDatabases(ctx context.Context) ([]MetadataNode, error) {
	// 设计：一个数据资源只绑定一个数据库，禁止通过树切换到其他数据库
	name, err := a.boundDatabase(ctx)
	if err != nil {
		return nil, err
	}
	// 系统库本身不作为业务树根（绑定资源若误指系统库仍展示，由管理员配置约束）
	n := MetadataNode{Kind: NodeSchema, Schema: name, Name: name}
	n.ID = n.EncodeID()
	return []MetadataNode{n}, nil
}

func (a *mysqlAdapter) listBaseTables(ctx context.Context, dbName string) ([]MetadataNode, error) {
	if err := a.ensureBoundSchema(ctx, dbName); err != nil {
		return nil, err
	}
	var out []MetadataNode
	tableRows, err := a.db.QueryContext(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = ? AND table_type = 'BASE TABLE'
		ORDER BY table_name`, dbName)
	if err != nil {
		return nil, err
	}
	defer tableRows.Close()
	for tableRows.Next() {
		var name string
		if err := tableRows.Scan(&name); err != nil {
			return nil, err
		}
		n := MetadataNode{Kind: NodeTable, Schema: dbName, Name: name}
		n.ID = n.EncodeID()
		out = append(out, n)
	}
	return out, tableRows.Err()
}

func (a *mysqlAdapter) listViews(ctx context.Context, dbName string) ([]MetadataNode, error) {
	if err := a.ensureBoundSchema(ctx, dbName); err != nil {
		return nil, err
	}
	var out []MetadataNode
	viewRows, err := a.db.QueryContext(ctx, `
		SELECT table_name FROM information_schema.views
		WHERE table_schema = ?
		ORDER BY table_name`, dbName)
	if err != nil {
		return nil, err
	}
	defer viewRows.Close()
	for viewRows.Next() {
		var name string
		if err := viewRows.Scan(&name); err != nil {
			return nil, err
		}
		n := MetadataNode{Kind: NodeView, Schema: dbName, Name: name}
		n.ID = n.EncodeID()
		out = append(out, n)
	}
	return out, viewRows.Err()
}

func (a *mysqlAdapter) listRoutines(ctx context.Context, dbName, routineType string, kind MetadataNodeKind) ([]MetadataNode, error) {
	if err := a.ensureBoundSchema(ctx, dbName); err != nil {
		return nil, err
	}
	var out []MetadataNode
	rows, err := a.db.QueryContext(ctx, `
		SELECT routine_name FROM information_schema.routines
		WHERE routine_schema = ? AND routine_type = ?
		ORDER BY routine_name`, dbName, routineType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		n := MetadataNode{Kind: kind, Schema: dbName, Name: name}
		n.ID = n.EncodeID()
		out = append(out, n)
	}
	return out, rows.Err()
}

func (a *mysqlAdapter) listTriggers(ctx context.Context, dbName string) ([]MetadataNode, error) {
	if err := a.ensureBoundSchema(ctx, dbName); err != nil {
		return nil, err
	}
	var out []MetadataNode
	rows, err := a.db.QueryContext(ctx, `
		SELECT trigger_name FROM information_schema.triggers
		WHERE trigger_schema = ?
		ORDER BY trigger_name`, dbName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		n := MetadataNode{Kind: NodeTrigger, Schema: dbName, Name: name}
		n.ID = n.EncodeID()
		out = append(out, n)
	}
	return out, rows.Err()
}

func (a *mysqlAdapter) Describe(ctx context.Context, obj MetadataNode) (ObjectStructure, error) {
	structure := ObjectStructure{
		Columns: []ColumnInfo{}, Constraints: []ConstraintInfo{}, Indexes: []IndexInfo{},
	}
	if err := a.ensureBoundSchema(ctx, obj.Schema); err != nil {
		return structure, err
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
	if err := colRows.Err(); err != nil {
		return structure, fmt.Errorf("读取字段失败: %w", err)
	}
	// 约束（主键/唯一/外键）及列：key_column_usage 提供列序
	conRows, err := a.db.QueryContext(ctx, `
		SELECT tc.constraint_name, tc.constraint_type, kcu.column_name, kcu.ordinal_position
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON kcu.constraint_schema = tc.constraint_schema
		 AND kcu.constraint_name = tc.constraint_name
		 AND kcu.table_schema = tc.table_schema
		 AND kcu.table_name = tc.table_name
		WHERE tc.table_schema = ? AND tc.table_name = ?
		  AND tc.constraint_type IN ('PRIMARY KEY', 'UNIQUE', 'FOREIGN KEY')
		ORDER BY tc.constraint_type, tc.constraint_name, kcu.ordinal_position`, obj.Schema, obj.Name)
	if err != nil {
		return structure, fmt.Errorf("查询约束失败: %w", err)
	}
	type conAcc struct {
		name, typ string
		cols      []string
	}
	order := []string{}
	byName := map[string]*conAcc{}
	for conRows.Next() {
		var name, conType, col string
		var ord int
		if err := conRows.Scan(&name, &conType, &col, &ord); err != nil {
			conRows.Close()
			return structure, fmt.Errorf("读取约束失败: %w", err)
		}
		acc, ok := byName[name]
		if !ok {
			acc = &conAcc{name: name, typ: normalizeConstraintType(conType)}
			byName[name] = acc
			order = append(order, name)
		}
		acc.cols = append(acc.cols, col)
	}
	if err := conRows.Err(); err != nil {
		conRows.Close()
		return structure, fmt.Errorf("读取约束失败: %w", err)
	}
	conRows.Close()
	for _, name := range order {
		acc := byName[name]
		structure.Constraints = append(structure.Constraints, ConstraintInfo{
			Name: acc.name, Type: acc.typ, Columns: acc.cols,
		})
	}
	// 索引（MySQL 8+ 函数索引 column_name 可能为 NULL，须用 NullString）
	idxRows, err := a.db.QueryContext(ctx, `
		SELECT index_name, column_name, non_unique
		FROM information_schema.statistics
		WHERE table_schema = ? AND table_name = ?
		ORDER BY index_name, seq_in_index`, obj.Schema, obj.Name)
	if err != nil {
		return structure, fmt.Errorf("查询索引失败: %w", err)
	}
	idxMap := map[string]*IndexInfo{}
	for idxRows.Next() {
		var idxName string
		var colName sql.NullString
		var nonUnique int
		if err := idxRows.Scan(&idxName, &colName, &nonUnique); err != nil {
			idxRows.Close()
			return structure, fmt.Errorf("读取索引失败: %w", err)
		}
		ii, ok := idxMap[idxName]
		if !ok {
			ii = &IndexInfo{Name: idxName, Unique: nonUnique == 0}
			idxMap[idxName] = ii
		}
		if colName.Valid && colName.String != "" {
			ii.Columns = append(ii.Columns, colName.String)
		} else {
			// 表达式/函数索引列无物理列名
			ii.Columns = append(ii.Columns, "(expression)")
		}
	}
	if err := idxRows.Err(); err != nil {
		idxRows.Close()
		return structure, fmt.Errorf("读取索引失败: %w", err)
	}
	idxRows.Close()
	for _, ii := range idxMap {
		structure.Indexes = append(structure.Indexes, *ii)
	}
	return structure, nil
}

func (a *mysqlAdapter) DDL(ctx context.Context, obj MetadataNode) (string, error) {
	if err := a.ensureBoundSchema(ctx, obj.Schema); err != nil {
		return "", err
	}
	qual := a.QuoteIdentifier(obj.Schema) + "." + a.QuoteIdentifier(obj.Name)
	switch obj.Kind {
	case NodeView:
		// SHOW CREATE VIEW 返回 4 列：View, Create View, character_set_client, collation_connection
		var name, ddl sql.NullString
		var cs, coll sql.NullString
		err := a.db.QueryRowContext(ctx, "SHOW CREATE VIEW "+qual).Scan(&name, &ddl, &cs, &coll)
		if err != nil {
			return "", fmt.Errorf("获取视图 DDL 失败: %w", err)
		}
		return ddl.String, nil
	case NodeFunction:
		// SHOW CREATE FUNCTION：Function, sql_mode, Create Function, character_set_client, collation_connection, Database Collation
		var name, mode, ddl, cs, coll, dbColl sql.NullString
		err := a.db.QueryRowContext(ctx, "SHOW CREATE FUNCTION "+qual).Scan(&name, &mode, &ddl, &cs, &coll, &dbColl)
		if err != nil {
			return "", fmt.Errorf("获取函数 DDL 失败: %w", err)
		}
		return ddl.String, nil
	case NodeProcedure:
		var name, mode, ddl, cs, coll, dbColl sql.NullString
		err := a.db.QueryRowContext(ctx, "SHOW CREATE PROCEDURE "+qual).Scan(&name, &mode, &ddl, &cs, &coll, &dbColl)
		if err != nil {
			return "", fmt.Errorf("获取存储过程 DDL 失败: %w", err)
		}
		return ddl.String, nil
	case NodeTrigger:
		// SHOW CREATE TRIGGER：Trigger, sql_mode, SQL Original Statement, character_set_client, collation_connection, Database Collation, Created
		var name, mode, ddl, cs, coll, dbColl sql.NullString
		var created sql.NullString
		err := a.db.QueryRowContext(ctx, "SHOW CREATE TRIGGER "+qual).Scan(&name, &mode, &ddl, &cs, &coll, &dbColl, &created)
		if err != nil {
			// 部分版本无 Created 列
			err = a.db.QueryRowContext(ctx, "SHOW CREATE TRIGGER "+qual).Scan(&name, &mode, &ddl, &cs, &coll, &dbColl)
			if err != nil {
				return "", fmt.Errorf("获取触发器 DDL 失败: %w", err)
			}
		}
		return ddl.String, nil
	case NodeTable, "":
		// SHOW CREATE TABLE：Table, Create Table
		var name, ddl sql.NullString
		err := a.db.QueryRowContext(ctx, "SHOW CREATE TABLE "+qual).Scan(&name, &ddl)
		if err != nil {
			return "", fmt.Errorf("获取表 DDL 失败: %w", err)
		}
		return ddl.String, nil
	default:
		return "", fmt.Errorf("不支持导出该对象类型的 DDL: %s", obj.Kind)
	}
}

func (a *mysqlAdapter) SQLTemplate(obj MetadataNode, operation string) (string, error) {
	// SQLTemplate 无 ctx：若配置了绑定库则校验；未配置时由后续执行路径约束
	if a.schema != "" && obj.Schema != "" && obj.Schema != a.schema {
		return "", fmt.Errorf("禁止访问其他数据库")
	}
	qualified := a.QuoteIdentifier(obj.Schema) + "." + a.QuoteIdentifier(obj.Name)
	switch operation {
	case "SELECT":
		// 不写 LIMIT：行数由服务端隐式分页（默认 100）
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

func (a *mysqlAdapter) PageSQL(query string, limit, offset int) (string, error) {
	// MySQL 要求派生表列名唯一；SELECT * FROM a JOIN b 两表都有 id 时
	// SELECT * FROM (...) AS _page 会 ERROR 1060 Duplicate column name。
	// 用户 SQL 无顶层 LIMIT 时直接追加，保留 JOIN 原投影。
	q := strings.TrimRight(strings.TrimSpace(query), ";")
	if sqlHasTopLevelRowLimiter(q) {
		return fmt.Sprintf("SELECT * FROM (%s) AS _page LIMIT %d OFFSET %d", q, limit, offset), nil
	}
	return fmt.Sprintf("%s LIMIT %d OFFSET %d", q, limit, offset), nil
}

func (a *mysqlAdapter) CountSQL(query string) (string, error) {
	// COUNT 包装在 JOIN 重名列时仍可能失败；调用方已有 totalStatus=unavailable 兜底。
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
	sqlState := ""
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) && myErr != nil {
		if myErr.SQLState != [5]byte{} {
			sqlState = string(myErr.SQLState[:])
		}
		if myErr.Message != "" {
			msg = myErr.Message
		}
		code = classifyMySQLErrorNumber(myErr.Number)
	} else {
		lower := strings.ToLower(msg)
		switch {
		case strings.Contains(msg, "Unknown column") || strings.Contains(lower, "doesn't exist") ||
			strings.Contains(msg, "不存在"):
			code = "OBJECT_NOT_FOUND"
		case strings.Contains(msg, "Access denied") || strings.Contains(msg, "拒绝访问") ||
			strings.Contains(msg, "权限"):
			code = "PERMISSION_DENIED"
		case strings.Contains(msg, "Duplicate entry") || strings.Contains(msg, "重复"):
			code = "DUPLICATE_KEY"
		case strings.Contains(lower, "constraint") || strings.Contains(lower, "foreign key") ||
			strings.Contains(msg, "约束"):
			code = "CONSTRAINT_VIOLATION"
		case strings.Contains(lower, "deadlock") || strings.Contains(msg, "死锁"):
			code = "DEADLOCK"
		case strings.Contains(lower, "syntax") || strings.Contains(msg, "语法"):
			code = "SYNTAX_ERROR"
		}
	}
	return DatabaseError{Code: code, Message: sanitizeErrMsg(msg), SQLState: sqlState}
}

func classifyMySQLErrorNumber(n uint16) string {
	switch n {
	case 1054, 1146, 1049: // unknown column / table / database
		return "OBJECT_NOT_FOUND"
	case 1044, 1045, 1142, 1227: // access denied / privilege
		return "PERMISSION_DENIED"
	case 1792: // Cannot execute statement in a READ ONLY transaction
		return "DATA_RESOURCE_READ_ONLY"
	case 1317, 1969: // Query execution was interrupted / statement timeout
		return "QUERY_CANCELED"
	case 1062: // duplicate entry
		return "DUPLICATE_KEY"
	case 1451, 1452, 1216, 1217, 1048: // FK / not null
		return "CONSTRAINT_VIOLATION"
	case 1213: // deadlock
		return "DEADLOCK"
	case 1064: // syntax
		return "SYNTAX_ERROR"
	case 1060: // Duplicate column name（派生表重名列等）
		return "DUPLICATE_COLUMN"
	default:
		return "DB_ERROR"
	}
}

func (a *mysqlAdapter) Capabilities() Capabilities {
	return Capabilities{
		DDLSupported:        true,
		ImportSupported:     true,
		ReadOnlyTxSupported: true,
		StoredProcSupported: true,
	}
}
