package conn

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/sqltext"
)

// postgres 方言；Kingbase 复用其实现，仅 ID/驱动/默认端口与少量目录查询不同。
//
// 真机结论（KingbaseES V008R003，aarch64，initdb --case-insensitive）：
//   - 系统目录为 sys_*（sys_class/sys_index/sys_namespace/sys_attribute），
//     无 pg_catalog 兼容层；Indexes() 按方言切换目录前缀，列名与 pg 相同。
//   - ON CONFLICT DO NOTHING / DO UPDATE 可用（与 PG 语义一致）。
//   - information_schema 的 schema/table 名存储为大写（PUBLIC 等），
//     元数据查询统一用 UPPER()=UPPER() 兼容两种部署形态。
//   - 无独立 catalog 层差异，ListSchemas 过滤 SYS_*/XLOG_* 等系统模式。
type postgres struct {
	id, label, driver string
	port              int
}

func init() {
	register(&postgres{id: "postgres", label: "PostgreSQL", driver: "pgx", port: 5432})
	register(&postgres{id: "kingbase", label: "KingbaseES", driver: "kingbase", port: 54321})
}

func (d *postgres) ID() string               { return d.id }
func (d *postgres) Label() string            { return d.label }
func (d *postgres) DefaultPort() int         { return d.port }
func (d *postgres) DriverName() string       { return d.driver }
func (d *postgres) HasCatalog() bool         { return true }
func (d *postgres) Internal() bool           { return false }
func (d *postgres) MaxParams() int           { return 65535 }
func (d *postgres) VersionSQL() string       { return "SELECT version()" }
func (d *postgres) Quote(s string) string    { return quoteDouble(s) }
func (d *postgres) FoldIdent(s string) string { return s } // 保留用户输入大小写
func (d *postgres) Placeholder(i int) string { return fmt.Sprintf("$%d", i) }
func (d *postgres) Lexer() sqltext.Options   { return sqltext.Options{DollarQuotes: true} }

func (d *postgres) Qualified(t TableRef) string {
	if t.Schema == "" {
		return d.Quote(t.Name)
	}
	return d.Quote(t.Schema) + "." + d.Quote(t.Name)
}

func pgQuote(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `'`, `\'`) + "'"
}

func (d *postgres) DSN(c Config, password string) (string, error) {
	ssl := c.SSLMode
	if ssl == "" {
		ssl = "disable"
	}
	if ssl != "disable" && ssl != "require" {
		return "", fmt.Errorf("不支持的 SSL 模式 %q", ssl)
	}
	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=%d",
		pgQuote(c.Host), c.Port, pgQuote(c.Username), pgQuote(password), pgQuote(c.Database), ssl,
		int(c.Options.connectTimeout()/time.Second))
	if c.DefaultSchema != "" {
		dsn += " search_path=" + pgQuote(c.DefaultSchema)
	}
	return dsn, nil
}

func (d *postgres) FieldFor(col ColumnMeta) core.Field {
	return fieldFromColumn(col, kindForType(col))
}

func (d *postgres) Normalize(v any, f core.Field, loc *time.Location) (any, error) {
	return normalizeCommon(v, f, loc)
}

func (d *postgres) Bind(v any, f core.Field) (any, error) { return bindCommon(v, f) }

func (d *postgres) ListSchemas(ctx context.Context, q Querier, _ Config) ([]string, error) {
	names, err := scanStrings(ctx, q, `SELECT schema_name FROM information_schema.schemata ORDER BY schema_name`)
	if err != nil {
		return nil, err
	}
	// Kingbase 无 pg_catalog：系统 schema 为 sys_catalog/SYS_*；pg 为 pg_catalog/pg_toast/pg_temp_*。
	var out []string
	for _, s := range names {
		u := strings.ToUpper(s)
		if u == "INFORMATION_SCHEMA" || strings.HasPrefix(u, "PG_") || strings.HasPrefix(u, "SYS_") ||
			u == "SYSLOGICAL" || u == "SYSAUDIT" || strings.HasPrefix(u, "XLOG_") {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

func (d *postgres) ListTables(ctx context.Context, q Querier, schema string) ([]TableInfo, error) {
	rows, err := q.QueryContext(ctx, `SELECT table_name, table_type FROM information_schema.tables
		WHERE UPPER(table_schema) = UPPER($1) ORDER BY table_name`, schema)
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
		kind := "table"
		if strings.Contains(typ, "VIEW") {
			kind = "view"
		}
		out = append(out, TableInfo{Name: name, Kind: kind})
	}
	return out, rows.Err()
}

func (d *postgres) Columns(ctx context.Context, q Querier, t TableRef) ([]ColumnMeta, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT c.column_name, c.data_type, c.character_maximum_length, c.numeric_precision, c.numeric_scale,
		       c.is_nullable = 'YES',
		       EXISTS (SELECT 1 FROM information_schema.table_constraints tc
		               JOIN information_schema.key_column_usage k
		                 ON k.constraint_name = tc.constraint_name AND k.table_schema = tc.table_schema
		               WHERE tc.constraint_type = 'PRIMARY KEY' AND tc.table_schema = c.table_schema
		                 AND tc.table_name = c.table_name AND k.column_name = c.column_name)
		FROM information_schema.columns c
		WHERE UPPER(c.table_schema) = UPPER($1) AND UPPER(c.table_name) = UPPER($2)
		ORDER BY c.ordinal_position`, t.Schema, t.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ColumnMeta
	for rows.Next() {
		var c ColumnMeta
		var l, p, s sql.NullInt64
		if err := rows.Scan(&c.Name, &c.DBType, &l, &p, &s, &c.Nullable, &c.PrimaryKey); err != nil {
			return nil, err
		}
		c.Length, c.Precision, c.Scale = nullInt(l), nullInt(p), max(nullInt(s), 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *postgres) Indexes(ctx context.Context, q Querier, t TableRef) ([]IndexMeta, error) {
	// KingbaseES 的系统目录为 sys_*（无 pg_catalog 兼容层）；PostgreSQL 为 pg_*。
	cat := "pg"
	if d.id == "kingbase" {
		cat = "sys"
	}
	rows, err := q.QueryContext(ctx, fmt.Sprintf(`
		SELECT i.relname, ix.indisunique, ix.indisprimary, a.attname
		FROM %[1]s_class tbl
		JOIN %[1]s_namespace ns ON ns.oid = tbl.relnamespace
		JOIN %[1]s_index ix ON ix.indrelid = tbl.oid
		JOIN %[1]s_class i ON i.oid = ix.indexrelid
		JOIN LATERAL unnest(ix.indkey) WITH ORDINALITY AS k(attnum, ord) ON true
		JOIN %[1]s_attribute a ON a.attrelid = tbl.oid AND a.attnum = k.attnum
		WHERE UPPER(ns.nspname) = UPPER($1) AND UPPER(tbl.relname) = UPPER($2)
		ORDER BY i.relname, k.ord`, cat), t.Schema, t.Name)
	if err != nil {
		return nil, err
	}
	return collectIndexes(rows)
}

func (d *postgres) TruncateSQL(t TableRef) string { return "TRUNCATE TABLE " + d.Qualified(t) }

func (d *postgres) ProbeSQL(query string) string {
	return "SELECT * FROM (" + strings.TrimRight(strings.TrimSpace(query), ";") + ") mc_probe WHERE 1=0"
}

// collectIndexes 把 (索引名, unique, primary, 列) 行聚合为 IndexMeta。
func collectIndexes(rows *sql.Rows) ([]IndexMeta, error) {
	defer rows.Close()
	var out []IndexMeta
	idx := map[string]int{}
	for rows.Next() {
		var name, col string
		var unique, primary bool
		if err := rows.Scan(&name, &unique, &primary, &col); err != nil {
			return nil, err
		}
		i, ok := idx[name]
		if !ok {
			i = len(out)
			idx[name] = i
			out = append(out, IndexMeta{Name: name, Unique: unique, Primary: primary})
		}
		out[i].Columns = append(out[i].Columns, col)
	}
	return out, rows.Err()
}
