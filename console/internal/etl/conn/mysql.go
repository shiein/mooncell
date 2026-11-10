package conn

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/sqltext"
)

type mysqlDialect struct{}

func init() { register(mysqlDialect{}) }

func (mysqlDialect) ID() string             { return "mysql" }
func (mysqlDialect) Label() string          { return "MySQL" }
func (mysqlDialect) DefaultPort() int       { return 3306 }
func (mysqlDialect) DriverName() string     { return "mysql" }
func (mysqlDialect) HasCatalog() bool       { return true }
func (mysqlDialect) Internal() bool         { return false }
func (mysqlDialect) MaxParams() int         { return 65535 }
func (mysqlDialect) VersionSQL() string     { return "SELECT VERSION()" }
func (mysqlDialect) Placeholder(int) string { return "?" }
func (mysqlDialect) Quote(s string) string  { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
func (mysqlDialect) Lexer() sqltext.Options {
	return sqltext.Options{BackslashEscapes: true, HashComments: true}
}

func (d mysqlDialect) Qualified(t TableRef) string {
	if t.Schema == "" {
		return d.Quote(t.Name)
	}
	return d.Quote(t.Schema) + "." + d.Quote(t.Name)
}

func (mysqlDialect) DSN(c Config, password string) (string, error) {
	cfg := mysql.NewConfig()
	cfg.User = c.Username
	cfg.Passwd = password
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(strings.Trim(c.Host, "[]"), strconv.Itoa(c.Port))
	cfg.DBName = c.Database
	cfg.ParseTime = true
	cfg.Loc = c.Options.Location()
	cfg.Timeout = c.Options.connectTimeout()
	cfg.InterpolateParams = false // 走服务端预编译绑定
	switch c.SSLMode {
	case "", "disable":
		cfg.TLSConfig = "false"
	case "require":
		cfg.TLSConfig = "true"
	default:
		return "", fmt.Errorf("不支持的 SSL 模式 %q", c.SSLMode)
	}
	return cfg.FormatDSN(), nil
}

func (mysqlDialect) FieldFor(col ColumnMeta) core.Field {
	t := baseTypeName(col.DBType)
	// TINYINT(1) 是 MySQL 的事实布尔类型；BIT(1) 同理。
	if (t == "TINYINT" && col.Precision == 3 && strings.Contains(strings.ToLower(col.DBType), "tinyint(1)")) ||
		(t == "BIT" && col.Precision <= 1) {
		return fieldFromColumn(col, core.KindBoolean)
	}
	if t == "YEAR" {
		return fieldFromColumn(col, core.KindInteger)
	}
	return fieldFromColumn(col, kindForType(col))
}

func (mysqlDialect) Normalize(v any, f core.Field, loc *time.Location) (any, error) {
	return normalizeCommon(v, f, loc)
}

func (mysqlDialect) Bind(v any, f core.Field) (any, error) { return bindCommon(v, f) }

func (mysqlDialect) ListSchemas(ctx context.Context, q Querier, _ Config) ([]string, error) {
	return scanStrings(ctx, q, `SELECT schema_name FROM information_schema.schemata
		WHERE schema_name NOT IN ('mysql','information_schema','performance_schema','sys')
		ORDER BY schema_name`)
}

func (mysqlDialect) ListTables(ctx context.Context, q Querier, schema string) ([]TableInfo, error) {
	rows, err := q.QueryContext(ctx, `SELECT table_name, table_type FROM information_schema.tables
		WHERE table_schema = ? ORDER BY table_name`, schema)
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

func (mysqlDialect) Columns(ctx context.Context, q Querier, t TableRef) ([]ColumnMeta, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT column_name, column_type, character_maximum_length, numeric_precision, numeric_scale,
		       is_nullable = 'YES', column_key = 'PRI'
		FROM information_schema.columns
		WHERE table_schema = ? AND table_name = ?
		ORDER BY ordinal_position`, t.Schema, t.Name)
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

func (mysqlDialect) Indexes(ctx context.Context, q Querier, t TableRef) ([]IndexMeta, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT index_name, non_unique = 0, index_name = 'PRIMARY', column_name
		FROM information_schema.statistics
		WHERE table_schema = ? AND table_name = ?
		ORDER BY index_name, seq_in_index`, t.Schema, t.Name)
	if err != nil {
		return nil, err
	}
	return collectIndexes(rows)
}

func (d mysqlDialect) TruncateSQL(t TableRef) string { return "TRUNCATE TABLE " + d.Qualified(t) }

func (mysqlDialect) ProbeSQL(query string) string {
	return "SELECT * FROM (" + strings.TrimRight(strings.TrimSpace(query), ";") + ") mc_probe WHERE 1=0"
}
