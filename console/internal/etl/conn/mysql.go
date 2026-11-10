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

// mysql 方言（go-sql-driver/mysql）。
//
// 真机结论（MySQL 8.4）：
//   - BIT(n) 扫描为 []byte（大端位串）；BIT(1) 映射布尔，BIT(n>1) 映射整数。
//   - TINYINT(1) 经 information_schema 识别为布尔；SELECT 探测路径驱动不暴露
//     声明宽度（DatabaseTypeName=TINYINT、DecimalSize 恒 (0,0)），与 tinyint(4) 不可区分，读为整数。
//   - UNSIGNED 整数：information_schema 类型名带 unsigned；探测路径为前缀形式
//     UNSIGNED BIGINT。BIGINT UNSIGNED（精度 20，超 int64）映射为精确小数。
//   - DATETIME/TIMESTAMP：驱动按 DSN 的 Loc（=连接时区）做双向换算，
//     与「无时区时间按连接时区解释」一致，往返墙上时钟不变。
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
func (mysqlDialect) Quote(s string) string     { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
func (mysqlDialect) FoldIdent(s string) string { return s }
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
	lower := strings.ToLower(col.DBType)
	// TINYINT(1) 是 MySQL 的事实布尔类型；BIT(1) 同理。
	// 经 information_schema 的列带声明宽度（tinyint(1)）；SELECT 探测路径的
	// DatabaseTypeName 不含宽度，Precision 恒为类型最大宽度（tinyint=3），无法区分。
	if (t == "TINYINT" && col.Precision == 3 && strings.Contains(lower, "tinyint(1)")) ||
		(t == "BIT" && col.Precision <= 1) {
		return fieldFromColumn(col, core.KindBoolean)
	}
	if t == "BIT" {
		return fieldFromColumn(col, core.KindInteger) // BIT(n>1) 是整数位串
	}
	if t == "YEAR" {
		return fieldFromColumn(col, core.KindInteger)
	}
	// unsigned 范围超 int64 的整数映射为精确小数（BIGINT UNSIGNED 最大值 2^64-1）。
	// information_schema 路径类型带 unsigned；SELECT 探测路径为前缀形式 UNSIGNED BIGINT。
	if strings.Contains(lower, "unsigned") && (t == "BIGINT" || col.Precision > 18) {
		return fieldFromColumn(col, core.KindDecimal)
	}
	return fieldFromColumn(col, kindForType(col))
}

func (mysqlDialect) Normalize(v any, f core.Field, loc *time.Location) (any, error) {
	// BIT(n) 扫描为 []byte（大端位串）：布尔取「非零」，整数按大端累计。
	if b, ok := v.([]byte); ok && strings.HasPrefix(strings.ToUpper(f.NativeType), "BIT") {
		if f.Kind == core.KindBoolean {
			for _, c := range b {
				if c != 0 {
					return true, nil
				}
			}
			return false, nil
		}
		var n uint64
		for _, c := range b {
			n = n<<8 | uint64(c)
		}
		v = n
	}
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

// UpsertSQL 生成 ON DUPLICATE KEY UPDATE upsert。
// 用 VALUES(col) 引用待插值（8.0.20+ 标记 deprecated 但仍是全版本通用写法，
// 含 5.7 与 MariaDB；别名形式 AS new 仅 ≥8.0.19 且 MariaDB 不支持）。
// doUpdate=false 时以 key=key 的空更新换取「存在即跳过」，避免 INSERT IGNORE 吞掉其他约束错误。
func (d mysqlDialect) UpsertSQL(t TableRef, cols, keys, set []string, nrows int, doUpdate bool) (string, bool) {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(d.Qualified(t))
	b.WriteString(" (")
	b.WriteString(quoteAll(d, cols))
	b.WriteString(") VALUES ")
	for r := 0; r < nrows; r++ {
		if r > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		b.WriteString(placeholders(d, 1, len(cols))) // MySQL 占位符都是 ?
		b.WriteByte(')')
	}
	b.WriteString(" ON DUPLICATE KEY UPDATE ")
	if !doUpdate || len(set) == 0 {
		b.WriteString(d.Quote(keys[0]))
		b.WriteString(" = ")
		b.WriteString(d.Quote(keys[0]))
		return b.String(), true
	}
	for i, c := range set {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(d.Quote(c))
		b.WriteString(" = VALUES(")
		b.WriteString(d.Quote(c))
		b.WriteString(")")
	}
	return b.String(), true
}

// CaseCast：go-sql-driver 按 Go 类型声明参数，MySQL 服务端按列类型推断，无需 CAST。
func (mysqlDialect) CaseCast(core.Field) string { return "" }
