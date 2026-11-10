package conn

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/sqltext"
)

// dm 是达梦 DM8 方言。元数据走 Oracle 兼容视图（all_tab_columns 等）。
//
// 真机结论（DM8 + gitee.com/chunanyong/dm v1.8.20，CASE_SENSITIVE=Y）：
//   - 流式读取无需显式 fetch size：5000 行结果集首行 ~350µs 即返回。
//   - 扫描 Go 类型：NUMBER/DECIMAL → string（走文本→decimal 路径）；BIT → int8；
//     DATE/TIMESTAMP → time.Time；TIMESTAMP WITH TIME ZONE（元数据名
//     DATETIME WITH TIME ZONE）→ 带固定偏移的 time.Time。
//   - 单语句绑定参数上限实测 65535（IN 列表二分探测）。
//   - 未加引号标识符一律存为大写：FoldIdent 把全小写输入折叠为大写，
//     元数据查询用 UPPER()=UPPER() 匹配两种存储形态。
type dm struct{}

func init() { register(dm{}) }

func (dm) ID() string             { return "dm" }
func (dm) Label() string          { return "达梦 DM8" }
func (dm) DefaultPort() int       { return 5236 }
func (dm) DriverName() string     { return "dm" }
func (dm) HasCatalog() bool       { return false }
func (dm) Internal() bool         { return false }
// 真机实测（DM8，dmdms/chunanyong 驱动）：单语句绑定参数上限 65535。
func (dm) MaxParams() int         { return 65535 }
func (dm) VersionSQL() string     { return "SELECT banner FROM v$version WHERE rownum = 1" }
func (dm) Placeholder(int) string { return "?" }
func (dm) Quote(s string) string  { return quoteDouble(s) }
// DM8 CASE_SENSITIVE=Y（与既有业务库一致）下未加引号标识符一律存为大写。
// 全小写输入按未加引号 SQL 语义折叠为大写；含大写的输入视为用户刻意给出确切大小写
// （可能是引号创建的混合大小写对象），原样引用。
func (dm) FoldIdent(s string) string {
	if s == strings.ToLower(s) {
		return strings.ToUpper(s)
	}
	return s
}
func (dm) Lexer() sqltext.Options    { return sqltext.Options{} }

func (d dm) Qualified(t TableRef) string {
	if t.Schema == "" {
		return d.Quote(t.Name)
	}
	return d.Quote(t.Schema) + "." + d.Quote(t.Name)
}

// DSN：官方驱动不对 userinfo/query 做 URL 解码，凭据须原样拼接（见 dataresource/dsn.go 的真机结论）。
func (d dm) DSN(c Config, password string) (string, error) {
	if strings.Contains(c.Username, ":") {
		return "", fmt.Errorf("达梦用户名不能包含冒号")
	}
	addr := net.JoinHostPort(strings.Trim(c.Host, "[]"), strconv.Itoa(c.Port))
	dsn := fmt.Sprintf("dm://%s:%s@%s", c.Username, password, addr)
	schema := c.DefaultSchema
	if schema == "" {
		schema = c.Database
	}
	if schema == "" {
		schema = c.Username
	}
	if strings.ContainsAny(schema, "?&") {
		return "", fmt.Errorf("模式名不能包含 ? 或 &")
	}
	if schema != "" {
		dsn += "?schema=" + d.Quote(d.FoldIdent(schema))
	}
	return dsn, nil
}

func (dm) FieldFor(col ColumnMeta) core.Field {
	t := baseTypeName(col.DBType)
	if t == "BIT" {
		return fieldFromColumn(col, core.KindBoolean)
	}
	if t == "CLOB" || t == "TEXT" || t == "LONGVARCHAR" {
		return fieldFromColumn(col, core.KindString)
	}
	return fieldFromColumn(col, kindForType(col))
}

func (dm) Normalize(v any, f core.Field, loc *time.Location) (any, error) {
	return normalizeCommon(v, f, loc)
}

func (dm) Bind(v any, f core.Field) (any, error) { return bindCommon(v, f) }

func (dm) ListSchemas(ctx context.Context, q Querier, c Config) ([]string, error) {
	if s := firstNonEmpty(c.DefaultSchema, c.Database); s != "" {
		return []string{s}, nil
	}
	return scanStrings(ctx, q, `SELECT username FROM all_users
		WHERE username NOT IN ('SYS','SYSDBA','SYSAUDITOR','SYSSSO','CTISYS','SYSJOB')
		ORDER BY username`)
}

func (dm) ListTables(ctx context.Context, q Querier, schema string) ([]TableInfo, error) {
	rows, err := q.QueryContext(ctx, `SELECT object_name, object_type FROM all_objects
		WHERE UPPER(owner) = UPPER(?) AND object_type IN ('TABLE','VIEW') ORDER BY object_name`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TableInfo
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return nil, err
		}
		out = append(out, TableInfo{Name: name, Kind: strings.ToLower(typ)})
	}
	return out, rows.Err()
}

func (dm) Columns(ctx context.Context, q Querier, t TableRef) ([]ColumnMeta, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT c.column_name, c.data_type, c.data_length, c.data_precision, c.data_scale, c.nullable,
		       CASE WHEN EXISTS (
		         SELECT 1 FROM all_constraints k JOIN all_cons_columns kc
		           ON kc.owner = k.owner AND kc.constraint_name = k.constraint_name
		         WHERE k.owner = c.owner AND k.table_name = c.table_name
		           AND k.constraint_type = 'P' AND kc.column_name = c.column_name) THEN 1 ELSE 0 END
		FROM all_tab_columns c
		WHERE UPPER(c.owner) = UPPER(?) AND UPPER(c.table_name) = UPPER(?)
		ORDER BY c.column_id`, t.Schema, t.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ColumnMeta
	for rows.Next() {
		var c ColumnMeta
		var l, p, s sql.NullInt64
		var nullable string
		var pk int
		if err := rows.Scan(&c.Name, &c.DBType, &l, &p, &s, &nullable, &pk); err != nil {
			return nil, err
		}
		c.Length, c.Precision, c.Scale = nullInt(l), nullInt(p), max(nullInt(s), 0)
		c.Nullable, c.PrimaryKey = nullable == "Y", pk == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

func (dm) Indexes(ctx context.Context, q Querier, t TableRef) ([]IndexMeta, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT i.index_name,
		       CASE WHEN i.uniqueness = 'UNIQUE' THEN 1 ELSE 0 END,
		       CASE WHEN EXISTS (SELECT 1 FROM all_constraints k WHERE k.owner = i.table_owner
		            AND k.index_name = i.index_name AND k.constraint_type = 'P') THEN 1 ELSE 0 END,
		       ic.column_name
		FROM all_indexes i
		JOIN all_ind_columns ic ON ic.index_owner = i.owner AND ic.index_name = i.index_name
		WHERE UPPER(i.table_owner) = UPPER(?) AND UPPER(i.table_name) = UPPER(?)
		ORDER BY i.index_name, ic.column_position`, t.Schema, t.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IndexMeta
	idx := map[string]int{}
	for rows.Next() {
		var name, col string
		var unique, primary int
		if err := rows.Scan(&name, &unique, &primary, &col); err != nil {
			return nil, err
		}
		i, ok := idx[name]
		if !ok {
			i = len(out)
			idx[name] = i
			out = append(out, IndexMeta{Name: name, Unique: unique == 1, Primary: primary == 1})
		}
		out[i].Columns = append(out[i].Columns, col)
	}
	return out, rows.Err()
}

func (d dm) TruncateSQL(t TableRef) string { return "TRUNCATE TABLE " + d.Qualified(t) }

func (dm) ProbeSQL(query string) string {
	return "SELECT * FROM (" + strings.TrimRight(strings.TrimSpace(query), ";") + ") mc_probe WHERE 1=0"
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
