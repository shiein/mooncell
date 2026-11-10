// Package conn 是数据集成的连接器层：方言（标识符、占位符、类型映射、元数据）、连接池与批量写 SQL 构造。
//
// 本包与 dataresource 模块刻意独立（连接配置、凭据、连接池都不共享），DSN 规则沿用其已真机验证的经验。
package conn

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/sqltext"
)

// Config 是连接配置（不含密码）。
type Config struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	DBType        string  `json:"dbType"`
	Host          string  `json:"host"`
	Port          int     `json:"port"`
	Database      string  `json:"database"`
	DefaultSchema string  `json:"defaultSchema"`
	Username      string  `json:"username"`
	SSLMode       string  `json:"sslMode"` // disable | require
	Options       Options `json:"options"`
}

// Options 是连接级可调参数。
type Options struct {
	ConnectTimeoutSec int    `json:"connectTimeoutSec"` // 默认 10
	MaxOpenConns      int    `json:"maxOpenConns"`      // 默认 8
	TimeZone          string `json:"timeZone"`          // 解释无时区时间，默认 Local
}

func (o Options) connectTimeout() time.Duration {
	if o.ConnectTimeoutSec <= 0 {
		return 10 * time.Second
	}
	return time.Duration(o.ConnectTimeoutSec) * time.Second
}

// Location 返回连接时区。
func (o Options) Location() *time.Location {
	if o.TimeZone == "" {
		return time.Local
	}
	if l, err := time.LoadLocation(o.TimeZone); err == nil {
		return l
	}
	return time.Local
}

// TableRef 是 schema 限定的表名。
type TableRef struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
}

func (t TableRef) String() string {
	if t.Schema == "" {
		return t.Name
	}
	return t.Schema + "." + t.Name
}

// TableInfo 是表列表项。
type TableInfo struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // table | view
}

// ColumnMeta 是数据库列元数据。
type ColumnMeta struct {
	Name       string `json:"name"`
	DBType     string `json:"dbType"`
	Length     int    `json:"length"`    // -1 未知
	Precision  int    `json:"precision"` // -1 未知
	Scale      int    `json:"scale"`
	Nullable   bool   `json:"nullable"`
	PrimaryKey bool   `json:"primaryKey"`
}

// IndexMeta 是索引元数据（插入/更新预检用）。
type IndexMeta struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique"`
	Primary bool     `json:"primary"`
}

// Querier 由 *sql.DB / *sql.Tx / *sql.Conn 实现。
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Dialect 描述一种数据库。实现必须无状态、并发安全。
type Dialect interface {
	ID() string
	Label() string
	DefaultPort() int
	DriverName() string
	HasCatalog() bool // false：达梦（实例 → 模式 → 对象，两层）
	Internal() bool   // true：仅测试用，不出现在连接类型目录

	DSN(c Config, password string) (string, error)
	Lexer() sqltext.Options
	Quote(ident string) string
	Qualified(t TableRef) string
	Placeholder(i int) string // 1-based
	MaxParams() int
	VersionSQL() string

	FieldFor(col ColumnMeta) core.Field
	Normalize(v any, f core.Field, loc *time.Location) (any, error) // 驱动扫描值 → 逻辑值
	Bind(v any, f core.Field) (any, error)                          // 逻辑值 → 驱动参数

	ListSchemas(ctx context.Context, q Querier, c Config) ([]string, error)
	ListTables(ctx context.Context, q Querier, schema string) ([]TableInfo, error)
	Columns(ctx context.Context, q Querier, t TableRef) ([]ColumnMeta, error)
	Indexes(ctx context.Context, q Querier, t TableRef) ([]IndexMeta, error)
	TruncateSQL(t TableRef) string
	// ProbeSQL 把任意查询包装为「只取列元数据、不返回行」的 SQL（设计期推导表输入字段）。
	ProbeSQL(query string) string
}

// DialectMeta 是下发前端的方言目录项。
type DialectMeta struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	DefaultPort int    `json:"defaultPort"`
	HasCatalog  bool   `json:"hasCatalog"`
}

var dialects = map[string]Dialect{}

func register(d Dialect) { dialects[d.ID()] = d }

// Get 按 ID 取方言。
func Get(id string) (Dialect, bool) {
	d, ok := dialects[id]
	return d, ok
}

// Catalog 返回对外可选的方言（按名称排序，排除 Internal）。
func Catalog() []DialectMeta {
	var out []DialectMeta
	for _, d := range dialects {
		if d.Internal() {
			continue
		}
		out = append(out, DialectMeta{ID: d.ID(), Label: d.Label(), DefaultPort: d.DefaultPort(), HasCatalog: d.HasCatalog()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// ---------------------------------------------------------------- 共享实现

// quoteDouble 是 ANSI 双引号标识符。
func quoteDouble(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// baseTypeName 规整类型名：大写、去括号参数与 UNSIGNED 等修饰。
func baseTypeName(t string) string {
	t = strings.ToUpper(strings.TrimSpace(t))
	if i := strings.IndexByte(t, '('); i >= 0 {
		t = strings.TrimSpace(t[:i]) + strings.TrimSpace(afterParen(t[i:]))
	}
	t = strings.TrimSuffix(t, " UNSIGNED")
	t = strings.TrimSuffix(t, " ZEROFILL")
	return strings.TrimSpace(t)
}

func afterParen(s string) string {
	if j := strings.IndexByte(s, ')'); j >= 0 {
		return " " + s[j+1:]
	}
	return ""
}

// kindForType 是跨库通用的类型名 → 逻辑类型映射；方言可在此之前处理特例。
func kindForType(col ColumnMeta) core.Kind {
	t := baseTypeName(col.DBType)
	switch {
	case t == "":
		return core.KindString
	case strings.Contains(t, "TIMESTAMP") || strings.HasPrefix(t, "DATETIME") || t == "SMALLDATETIME":
		return core.KindTimestamp
	case t == "DATE":
		return core.KindDate
	case t == "BOOL" || t == "BOOLEAN":
		return core.KindBoolean
	}
	switch t {
	case "INT", "INTEGER", "INT2", "INT4", "INT8", "SMALLINT", "TINYINT", "MEDIUMINT", "BIGINT",
		"SERIAL", "BIGSERIAL", "SMALLSERIAL", "BYTE", "PLS_INTEGER":
		return core.KindInteger
	case "FLOAT", "FLOAT4", "FLOAT8", "DOUBLE", "DOUBLE PRECISION", "REAL", "BINARY_FLOAT", "BINARY_DOUBLE":
		return core.KindNumber
	case "DECIMAL", "NUMERIC", "NUMBER", "DEC", "MONEY":
		// 无小数位且位数可装入 int64 → 整数（达梦/Oracle 风格的 NUMBER(10) 主键很常见）
		if col.Scale == 0 && col.Precision > 0 && col.Precision <= 18 {
			return core.KindInteger
		}
		return core.KindDecimal
	case "BYTEA", "BLOB", "TINYBLOB", "MEDIUMBLOB", "LONGBLOB", "BINARY", "VARBINARY", "IMAGE", "RAW",
		"LONG RAW", "LONGVARBINARY", "BFILE":
		return core.KindBinary
	}
	return core.KindString
}

func fieldFromColumn(col ColumnMeta, kind core.Kind) core.Field {
	f := core.NewField(col.Name, kind)
	f.Length, f.Precision, f.Scale = col.Length, col.Precision, col.Scale
	f.Nullable = col.Nullable
	f.NativeType = col.DBType
	return f
}

// normalizeCommon 把驱动扫描出的值规整为字段逻辑类型。
func normalizeCommon(v any, f core.Field, loc *time.Location) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch x := v.(type) {
	case []byte:
		if f.Kind == core.KindBinary {
			return x, nil // database/sql 扫描到 *any 时已复制
		}
		v = string(x)
	case uint64:
		if x > math.MaxInt64 {
			v = decimal.NewFromBigInt(new(big.Int).SetUint64(x), 0)
		} else {
			v = int64(x)
		}
	case time.Time:
		if f.Kind == core.KindString {
			return x.In(loc).Format("2006-01-02 15:04:05"), nil
		}
	case driver.Valuer: // 部分厂商驱动（如达梦）返回自定义类型：取其驱动值再规整
		dv, err := x.Value()
		if err != nil {
			return nil, err
		}
		return normalizeCommon(dv, f, loc)
	case fmt.Stringer:
		if !isPlain(v) {
			v = x.String()
		}
	}
	return core.Convert(v, f.Kind, core.ConvertOptions{Location: loc})
}

func isPlain(v any) bool {
	switch v.(type) {
	case string, int64, float64, bool, time.Time, decimal.Decimal:
		return true
	}
	return false
}

// bindCommon 把逻辑值转为驱动参数：decimal 走文本（各驱动均接受，避免 float 失真）。
func bindCommon(v any, f core.Field) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case decimal.Decimal:
		return x.String(), nil
	}
	return v, nil
}

// scanStrings 执行查询并把第一列读为字符串列表。
func scanStrings(ctx context.Context, q Querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func nullInt(v sql.NullInt64) int {
	if !v.Valid {
		return -1
	}
	return int(v.Int64)
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

// EffectiveSchema 返回表所在 schema：显式值 > 连接默认 schema > 方言惯例。
func EffectiveSchema(c Config, schema string) string {
	if s := strings.TrimSpace(schema); s != "" {
		return s
	}
	if s := strings.TrimSpace(c.DefaultSchema); s != "" {
		return s
	}
	switch c.DBType {
	case "postgres", "kingbase":
		return "public"
	case "mysql":
		return c.Database
	case "dm":
		if c.Database != "" {
			return c.Database
		}
		return strings.ToUpper(c.Username)
	case "sqlite":
		return "main"
	}
	return ""
}
