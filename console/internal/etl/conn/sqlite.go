package conn

import (
	"context"
	"fmt"
	"strings"
	"time"

	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/sqltext"
)

// sqliteDialect 仅用于测试：让引擎、步骤与端到端用例在无外部数据库时也能跑真实 SQL。
// Internal()=true，不出现在连接类型目录中；Database 字段即文件路径（或 :memory: 形式的 DSN）。
type sqliteDialect struct{}

func init() { register(sqliteDialect{}) }

func (sqliteDialect) ID() string             { return "sqlite" }
func (sqliteDialect) Label() string          { return "SQLite（测试）" }
func (sqliteDialect) DefaultPort() int       { return 0 }
func (sqliteDialect) DriverName() string     { return "sqlite" }
func (sqliteDialect) HasCatalog() bool       { return false }
func (sqliteDialect) Internal() bool         { return true }
func (sqliteDialect) MaxParams() int         { return 32766 }
func (sqliteDialect) VersionSQL() string     { return "SELECT sqlite_version()" }
func (sqliteDialect) Placeholder(int) string { return "?" }
func (sqliteDialect) Quote(s string) string  { return quoteDouble(s) }
func (sqliteDialect) Lexer() sqltext.Options { return sqltext.Options{} }

func (d sqliteDialect) Qualified(t TableRef) string {
	if t.Schema == "" || t.Schema == "main" {
		return d.Quote(t.Name)
	}
	return d.Quote(t.Schema) + "." + d.Quote(t.Name)
}

func (sqliteDialect) DSN(c Config, _ string) (string, error) {
	if c.Database == "" {
		return "", fmt.Errorf("SQLite 需要文件路径")
	}
	return c.Database, nil
}

func (sqliteDialect) FieldFor(col ColumnMeta) core.Field {
	t := baseTypeName(col.DBType)
	switch {
	case strings.Contains(t, "INT"):
		return fieldFromColumn(col, core.KindInteger)
	case strings.Contains(t, "DECIMAL") || strings.Contains(t, "NUMERIC"):
		return fieldFromColumn(col, core.KindDecimal)
	}
	return fieldFromColumn(col, kindForType(col))
}

func (sqliteDialect) Normalize(v any, f core.Field, loc *time.Location) (any, error) {
	return normalizeCommon(v, f, loc)
}

func (sqliteDialect) Bind(v any, f core.Field) (any, error) {
	switch x := v.(type) {
	case time.Time: // SQLite 无原生时间类型：以文本存储，保证可排序可比较
		if f.Kind == core.KindDate {
			return x.Format("2006-01-02"), nil
		}
		return x.Format("2006-01-02 15:04:05.999999999"), nil
	}
	return bindCommon(v, f)
}

func (sqliteDialect) ListSchemas(context.Context, Querier, Config) ([]string, error) {
	return []string{"main"}, nil
}

func (sqliteDialect) ListTables(ctx context.Context, q Querier, _ string) ([]TableInfo, error) {
	rows, err := q.QueryContext(ctx, `SELECT name, type FROM sqlite_master WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TableInfo
	for rows.Next() {
		var t TableInfo
		if err := rows.Scan(&t.Name, &t.Kind); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (d sqliteDialect) Columns(ctx context.Context, q Querier, t TableRef) ([]ColumnMeta, error) {
	rows, err := q.QueryContext(ctx, "SELECT name, type, \"notnull\", pk FROM pragma_table_info(?)", t.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ColumnMeta
	for rows.Next() {
		var c ColumnMeta
		var notnull, pk int
		if err := rows.Scan(&c.Name, &c.DBType, &notnull, &pk); err != nil {
			return nil, err
		}
		c.Length, c.Precision = -1, -1
		c.Nullable, c.PrimaryKey = notnull == 0, pk > 0
		out = append(out, c)
	}
	return out, rows.Err()
}

func (sqliteDialect) Indexes(ctx context.Context, q Querier, t TableRef) ([]IndexMeta, error) {
	rows, err := q.QueryContext(ctx, `SELECT il.name, il."unique", il.origin = 'pk', ii.name
		FROM pragma_index_list(?) il JOIN pragma_index_info(il.name) ii ORDER BY il.name, ii.seqno`, t.Name)
	if err != nil {
		return nil, err
	}
	return collectIndexes(rows)
}

func (d sqliteDialect) TruncateSQL(t TableRef) string { return "DELETE FROM " + d.Qualified(t) }

func (sqliteDialect) ProbeSQL(query string) string {
	return "SELECT * FROM (" + strings.TrimRight(strings.TrimSpace(query), ";") + ") mc_probe LIMIT 0"
}
