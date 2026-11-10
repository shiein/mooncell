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

// postgres 方言；Kingbase 复用其实现，仅 ID/驱动/默认端口不同（兼容性差异在真机认证后按需覆写）。
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
	return scanStrings(ctx, q, `SELECT schema_name FROM information_schema.schemata
		WHERE schema_name NOT IN ('pg_catalog','information_schema','pg_toast')
		  AND schema_name NOT LIKE 'pg_temp_%' AND schema_name NOT LIKE 'pg_toast_temp_%'
		ORDER BY schema_name`)
}

func (d *postgres) ListTables(ctx context.Context, q Querier, schema string) ([]TableInfo, error) {
	rows, err := q.QueryContext(ctx, `SELECT table_name, table_type FROM information_schema.tables
		WHERE table_schema = $1 ORDER BY table_name`, schema)
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
		WHERE c.table_schema = $1 AND c.table_name = $2
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
	rows, err := q.QueryContext(ctx, `
		SELECT i.relname, ix.indisunique, ix.indisprimary, a.attname
		FROM pg_class tbl
		JOIN pg_namespace ns ON ns.oid = tbl.relnamespace
		JOIN pg_index ix ON ix.indrelid = tbl.oid
		JOIN pg_class i ON i.oid = ix.indexrelid
		JOIN LATERAL unnest(ix.indkey) WITH ORDINALITY AS k(attnum, ord) ON true
		JOIN pg_attribute a ON a.attrelid = tbl.oid AND a.attnum = k.attnum
		WHERE ns.nspname = $1 AND tbl.relname = $2
		ORDER BY i.relname, k.ord`, t.Schema, t.Name)
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
