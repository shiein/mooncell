package expr

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/shopspring/decimal"

	"mooncell/console/internal/etl/core"
)

// Example 是函数示例：表达式与结果文本。
type Example struct {
	Expr   string `json:"expr"`
	Result string `json:"result"`
}

// FuncDoc 是下发给前端的函数目录条目。
type FuncDoc struct {
	Name        string    `json:"name"`
	Category    string    `json:"category"`
	Signature   string    `json:"signature"`
	Returns     string    `json:"returns"`
	Description string    `json:"description"`
	Examples    []Example `json:"examples"`
	Insert      string    `json:"insert"`             // 点选插入的片段，光标放在 | 处
	Operator    bool      `json:"operator,omitempty"` // 运算符/语法，而非函数
}

type fnDef struct {
	doc   FuncDoc
	impl  func(args ...any) (any, error)
	types []any // expr 类型签名（可选，用于编译期类型检查与结果类型推导）
}

// 分类（前端按此顺序展示）。
const (
	CatString    = "字符串"
	CatNumber    = "数值"
	CatTime      = "日期时间"
	CatNull      = "条件与空值"
	CatConvert   = "类型转换"
	CatHash      = "摘要"
	CatRef       = "字段与变量"
	CatOperators = "运算符"
)

// Categories 返回分类顺序。
func Categories() []string {
	return []string{CatRef, CatString, CatNumber, CatTime, CatNull, CatConvert, CatHash, CatOperators}
}

// Catalog 返回函数目录（含运算符说明），供 GET /api/etl/catalog/functions。
func Catalog() []FuncDoc {
	defs := functions()
	out := make([]FuncDoc, 0, len(defs)+len(operatorDocs))
	for _, d := range defs {
		out = append(out, d.doc)
	}
	out = append(out, operatorDocs...)
	return out
}

var (
	fnOnce sync.Once
	fnList []fnDef
)

func functions() []fnDef {
	fnOnce.Do(func() { fnList = buildFunctions() })
	return fnList
}

// 保留名：这些名字不能作为裸字段名引用（须用 field("名")）。
func reservedNames() map[string]bool {
	m := map[string]bool{"field": true, "var": true, "nil": true, "true": true, "false": true,
		"in": true, "not": true, "and": true, "or": true, "matches": true, "contains": true,
		"startsWith": true, "endsWith": true, "let": true, "if": true, "else": true}
	for _, d := range functions() {
		m[d.doc.Name] = true
	}
	return m
}

func doc(name, cat, sig, ret, desc, insert string, ex ...Example) FuncDoc {
	return FuncDoc{Name: name, Category: cat, Signature: sig, Returns: ret, Description: desc, Examples: ex, Insert: insert}
}

func ex(e, r string) Example { return Example{Expr: e, Result: r} }

var errNullArg = errors.New("参数为 NULL")

func buildFunctions() []fnDef {
	str := func(v any) (string, bool) {
		if v == nil {
			return "", false
		}
		if s, ok := v.(string); ok {
			return s, true
		}
		return core.FormatValue(v), true
	}
	intArg := func(v any, name string) (int, error) {
		switch x := core.Normalize(v).(type) {
		case int64:
			return int(x), nil
		case float64:
			if x == math.Trunc(x) {
				return int(x), nil
			}
		case decimal.Decimal:
			if x.Equal(x.Truncate(0)) {
				return int(x.IntPart()), nil
			}
		}
		return 0, fmt.Errorf("%s 必须是整数", name)
	}
	timeArg := func(v any) (time.Time, bool, error) {
		if v == nil {
			return time.Time{}, false, nil
		}
		t, ok := v.(time.Time)
		if !ok {
			return time.Time{}, false, fmt.Errorf("需要日期/时间参数，实际为 %T", v)
		}
		return t, true, nil
	}
	strFn := func(f func(string) string) func(args ...any) (any, error) {
		return func(args ...any) (any, error) {
			s, ok := str(args[0])
			if !ok {
				return nil, nil
			}
			return f(s), nil
		}
	}

	var regexCache sync.Map
	compileRe := func(p string) (*regexp.Regexp, error) {
		if r, ok := regexCache.Load(p); ok {
			return r.(*regexp.Regexp), nil
		}
		r, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("正则表达式无效：%w", err)
		}
		regexCache.Store(p, r)
		return r, nil
	}

	addDur := func(unit time.Duration, months, years int) func(args ...any) (any, error) {
		return func(args ...any) (any, error) {
			t, ok, err := timeArg(args[0])
			if err != nil || !ok {
				return nil, err
			}
			n, err := intArg(args[1], "数量")
			if err != nil {
				return nil, err
			}
			switch {
			case years > 0:
				return t.AddDate(n, 0, 0), nil
			case months > 0:
				return t.AddDate(0, n, 0), nil
			case unit == 24*time.Hour:
				return t.AddDate(0, 0, n), nil
			}
			return t.Add(time.Duration(n) * unit), nil
		}
	}
	part := func(f func(time.Time) int) func(args ...any) (any, error) {
		return func(args ...any) (any, error) {
			t, ok, err := timeArg(args[0])
			if err != nil || !ok {
				return nil, err
			}
			return int64(f(t)), nil
		}
	}
	conv := func(k core.Kind) func(args ...any) (any, error) {
		return func(args ...any) (any, error) {
			opts := core.ConvertOptions{}
			if len(args) > 1 {
				if l, ok := args[1].(string); ok {
					opts.Layout = GoLayout(l)
				}
			}
			return core.Convert(args[0], k, opts)
		}
	}

	S, I, F, B, T, A := "string", "integer", "number", "boolean", "timestamp", "any"

	return []fnDef{
		// ---------------- 字段与变量
		{doc: doc("field", CatRef, `field(name)`, A, "按名称读取当前行字段，适用于含空格、中文标点或与函数同名的字段。", `field("|")`,
			ex(`field("订单 金额")`, "128.50"), ex(`field("year")`, "2026"))},
		{doc: doc("var", CatRef, `var(name)`, A, "读取作业/转换变量（如作业中 SQL 取值写入的 maxTs）。", `var("|")`,
			ex(`var("maxTs")`, "2026-11-01 00:00:00"))},

		// ---------------- 字符串
		{doc: doc("upper", CatString, "upper(s)", S, "转为大写。NULL 返回 NULL。", "upper(|)", ex(`upper("abc")`, `"ABC"`)),
			impl: strFn(strings.ToUpper), types: []any{new(func(string) string)}},
		{doc: doc("lower", CatString, "lower(s)", S, "转为小写。", "lower(|)", ex(`lower("ABC")`, `"abc"`)),
			impl: strFn(strings.ToLower), types: []any{new(func(string) string)}},
		{doc: doc("trim", CatString, "trim(s)", S, "去除首尾空白。", "trim(|)", ex(`trim("  a b ")`, `"a b"`)),
			impl: strFn(strings.TrimSpace), types: []any{new(func(string) string)}},
		{doc: doc("ltrim", CatString, "ltrim(s)", S, "去除开头空白。", "ltrim(|)", ex(`ltrim("  a ")`, `"a "`)),
			impl: strFn(func(s string) string { return strings.TrimLeft(s, " \t\r\n") }), types: []any{new(func(string) string)}},
		{doc: doc("rtrim", CatString, "rtrim(s)", S, "去除结尾空白。", "rtrim(|)", ex(`rtrim(" a  ")`, `" a"`)),
			impl: strFn(func(s string) string { return strings.TrimRight(s, " \t\r\n") }), types: []any{new(func(string) string)}},
		{doc: doc("length", CatString, "length(s)", I, "字符数（按字符而非字节，中文算 1）。", "length(|)", ex(`length("数据集成")`, "4")),
			impl: func(a ...any) (any, error) {
				s, ok := str(a[0])
				if !ok {
					return nil, nil
				}
				return int64(utf8.RuneCountInString(s)), nil
			}, types: []any{new(func(string) int64)}},
		{doc: doc("substr", CatString, "substr(s, start, [len])", S, "截取子串。start 从 1 开始（与 SQL 一致）；省略 len 取到末尾。", "substr(|, 1, 4)",
			ex(`substr("20261110", 1, 4)`, `"2026"`), ex(`substr("abcdef", 3)`, `"cdef"`)),
			impl: func(a ...any) (any, error) {
				s, ok := str(a[0])
				if !ok {
					return nil, nil
				}
				rs := []rune(s)
				start, err := intArg(a[1], "start")
				if err != nil {
					return nil, err
				}
				if start < 1 {
					start = 1
				}
				if start > len(rs) {
					return "", nil
				}
				end := len(rs)
				if len(a) > 2 {
					n, err := intArg(a[2], "len")
					if err != nil {
						return nil, err
					}
					if n < 0 {
						n = 0
					}
					end = min(len(rs), start-1+n)
				}
				return string(rs[start-1 : end]), nil
			}, types: []any{new(func(any, any) string), new(func(any, any, any) string)}},
		{doc: doc("replace", CatString, "replace(s, old, new)", S, "替换全部出现的子串。", `replace(|, "-", "")`, ex(`replace("2026-11-10", "-", "")`, `"20261110"`)),
			impl: func(a ...any) (any, error) {
				s, ok := str(a[0])
				if !ok {
					return nil, nil
				}
				o, _ := str(a[1])
				n, _ := str(a[2])
				return strings.ReplaceAll(s, o, n), nil
			}, types: []any{new(func(string, string, string) string)}},
		{doc: doc("regexReplace", CatString, "regexReplace(s, pattern, repl)", S, "正则替换（RE2 语法），repl 中可用 $1 引用分组。", `regexReplace(|, "\\s+", " ")`,
			ex(`regexReplace("a   b", "\\s+", " ")`, `"a b"`)),
			impl: func(a ...any) (any, error) {
				s, ok := str(a[0])
				if !ok {
					return nil, nil
				}
				p, _ := str(a[1])
				r, err := compileRe(p)
				if err != nil {
					return nil, err
				}
				rep, _ := str(a[2])
				return r.ReplaceAllString(s, rep), nil
			}, types: []any{new(func(string, string, string) string)}},
		{doc: doc("concat", CatString, "concat(a, b, ...)", S, "拼接任意个值；NULL 视为空串（与 + 不同，+ 遇 NULL 报错）。", "concat(|, )",
			ex(`concat(province, "-", city)`, `"浙江-杭州"`)),
			impl: func(a ...any) (any, error) {
				var b strings.Builder
				for _, v := range a {
					if s, ok := str(v); ok {
						b.WriteString(s)
					}
				}
				return b.String(), nil
			}, types: []any{new(func(...any) string)}},
		{doc: doc("lpad", CatString, "lpad(s, n, pad)", S, "左侧填充到 n 个字符；超长不截断。", `lpad(|, 6, "0")`, ex(`lpad("42", 6, "0")`, `"000042"`)),
			impl: padFn(true, str, intArg), types: []any{new(func(any, any, any) string)}},
		{doc: doc("rpad", CatString, "rpad(s, n, pad)", S, "右侧填充到 n 个字符。", `rpad(|, 6, " ")`, ex(`rpad("ab", 4, "*")`, `"ab**"`)),
			impl: padFn(false, str, intArg), types: []any{new(func(any, any, any) string)}},
		{doc: doc("splitPart", CatString, "splitPart(s, sep, n)", S, "按分隔符切分后取第 n 段（从 1 开始），不存在返回 NULL。", `splitPart(|, ",", 1)`,
			ex(`splitPart("a,b,c", ",", 2)`, `"b"`)),
			impl: func(a ...any) (any, error) {
				s, ok := str(a[0])
				if !ok {
					return nil, nil
				}
				sep, _ := str(a[1])
				n, err := intArg(a[2], "n")
				if err != nil {
					return nil, err
				}
				parts := strings.Split(s, sep)
				if n < 1 || n > len(parts) {
					return nil, nil
				}
				return parts[n-1], nil
			}, types: []any{new(func(any, any, any) string)}},

		// ---------------- 数值
		{doc: doc("round", CatNumber, "round(x, [n])", F, "四舍五入到 n 位小数（默认 0）。精确小数按十进制舍入，不产生浮点误差。", "round(|, 2)",
			ex(`round(3.14159, 2)`, "3.14"), ex(`round(amount, 0)`, "129")),
			impl: func(a ...any) (any, error) {
				n := 0
				if len(a) > 1 {
					var err error
					if n, err = intArg(a[1], "n"); err != nil {
						return nil, err
					}
				}
				switch x := core.Normalize(a[0]).(type) {
				case nil:
					return nil, nil
				case decimal.Decimal:
					return x.Round(int32(n)), nil
				case int64:
					return x, nil
				case float64:
					p := math.Pow10(n)
					return math.Round(x*p) / p, nil
				}
				return nil, fmt.Errorf("round 需要数值参数")
			}},
		{doc: doc("floor", CatNumber, "floor(x)", F, "向下取整。", "floor(|)", ex(`floor(3.7)`, "3")), impl: numFn(math.Floor, func(d decimal.Decimal) decimal.Decimal { return d.Floor() })},
		{doc: doc("ceil", CatNumber, "ceil(x)", F, "向上取整。", "ceil(|)", ex(`ceil(3.2)`, "4")), impl: numFn(math.Ceil, func(d decimal.Decimal) decimal.Decimal { return d.Ceil() })},
		{doc: doc("abs", CatNumber, "abs(x)", F, "绝对值。", "abs(|)", ex(`abs(-5)`, "5")),
			impl: func(a ...any) (any, error) {
				switch x := core.Normalize(a[0]).(type) {
				case nil:
					return nil, nil
				case int64:
					if x < 0 {
						return -x, nil
					}
					return x, nil
				case float64:
					return math.Abs(x), nil
				case decimal.Decimal:
					return x.Abs(), nil
				}
				return nil, fmt.Errorf("abs 需要数值参数")
			}},
		{doc: doc("greatest", CatNumber, "greatest(a, b, ...)", A, "返回最大值，忽略 NULL；全为 NULL 返回 NULL。可比较数值、文本、时间。", "greatest(|, )", ex(`greatest(3, 9, 4)`, "9")),
			impl: extremum(1)},
		{doc: doc("least", CatNumber, "least(a, b, ...)", A, "返回最小值，忽略 NULL。", "least(|, )", ex(`least(3, 9, 4)`, "3")),
			impl: extremum(-1)},

		// ---------------- 日期时间
		{doc: doc("now", CatTime, "now()", T, "当前时间（每行求值一次）。", "now()", ex(`now()`, "2026-11-10 09:30:00")),
			impl: func(a ...any) (any, error) { return time.Now(), nil }, types: []any{new(func() time.Time)}},
		{doc: doc("today", CatTime, "today()", "date", "今天的日期（00:00:00）。", "today()", ex(`today()`, "2026-11-10")),
			impl: func(a ...any) (any, error) {
				y, m, d := time.Now().Date()
				return time.Date(y, m, d, 0, 0, 0, 0, time.UTC), nil
			}, types: []any{new(func() time.Time)}},
		{doc: doc("parseDate", CatTime, "parseDate(s, [format])", T, "按格式解析时间文本，格式沿用 yyyy-MM-dd HH:mm:ss；省略时自动识别常见格式。", `parseDate(|, "yyyyMMdd")`,
			ex(`parseDate("20261110", "yyyyMMdd")`, "2026-11-10"), ex(`parseDate("2026/11/10 08:00:00")`, "2026-11-10 08:00:00")),
			impl: conv(core.KindTimestamp), types: []any{new(func(any) time.Time), new(func(any, any) time.Time)}},
		{doc: doc("formatDate", CatTime, "formatDate(t, format)", S, "按格式输出时间文本。", `formatDate(|, "yyyy-MM-dd")`,
			ex(`formatDate(created_at, "yyyy-MM-dd HH:mm")`, `"2026-11-10 09:30"`)),
			impl: func(a ...any) (any, error) {
				t, ok, err := timeArg(a[0])
				if err != nil || !ok {
					return nil, err
				}
				l, _ := str(a[1])
				return t.Format(GoLayout(l)), nil
			}, types: []any{new(func(any, any) string)}},
		{doc: doc("addSeconds", CatTime, "addSeconds(t, n)", T, "加 n 秒（n 可为负）。", "addSeconds(|, 30)", ex(`addSeconds(ts, -30)`, "…")), impl: addDur(time.Second, 0, 0), types: []any{new(func(any, any) time.Time)}},
		{doc: doc("addMinutes", CatTime, "addMinutes(t, n)", T, "加 n 分钟。", "addMinutes(|, 5)", ex(`addMinutes(ts, 5)`, "…")), impl: addDur(time.Minute, 0, 0), types: []any{new(func(any, any) time.Time)}},
		{doc: doc("addHours", CatTime, "addHours(t, n)", T, "加 n 小时。", "addHours(|, 8)", ex(`addHours(ts, 8)`, "UTC→北京时间")), impl: addDur(time.Hour, 0, 0), types: []any{new(func(any, any) time.Time)}},
		{doc: doc("addDays", CatTime, "addDays(t, n)", T, "加 n 天（按日历日）。", "addDays(|, -1)", ex(`addDays(today(), -1)`, "昨天")), impl: addDur(24*time.Hour, 0, 0), types: []any{new(func(any, any) time.Time)}},
		{doc: doc("addMonths", CatTime, "addMonths(t, n)", T, "加 n 个月。", "addMonths(|, 1)", ex(`addMonths(today(), -1)`, "上月同日")), impl: addDur(0, 1, 0), types: []any{new(func(any, any) time.Time)}},
		{doc: doc("diffSeconds", CatTime, "diffSeconds(a, b)", I, "a − b 的秒数。", "diffSeconds(|, )", ex(`diffSeconds(end_at, start_at)`, "3600")),
			impl: func(a ...any) (any, error) {
				x, ok1, err := timeArg(a[0])
				if err != nil {
					return nil, err
				}
				y, ok2, err := timeArg(a[1])
				if err != nil || !ok1 || !ok2 {
					return nil, err
				}
				return int64(x.Sub(y) / time.Second), nil
			}, types: []any{new(func(any, any) int64)}},
		{doc: doc("diffDays", CatTime, "diffDays(a, b)", I, "a − b 的日历天数（忽略时分秒）。", "diffDays(|, )", ex(`diffDays(today(), birthday)`, "12")),
			impl: func(a ...any) (any, error) {
				x, ok1, err := timeArg(a[0])
				if err != nil {
					return nil, err
				}
				y, ok2, err := timeArg(a[1])
				if err != nil || !ok1 || !ok2 {
					return nil, err
				}
				dx := time.Date(x.Year(), x.Month(), x.Day(), 0, 0, 0, 0, time.UTC)
				dy := time.Date(y.Year(), y.Month(), y.Day(), 0, 0, 0, 0, time.UTC)
				return int64(dx.Sub(dy).Hours() / 24), nil
			}, types: []any{new(func(any, any) int64)}},
		{doc: doc("truncDay", CatTime, "truncDay(t)", "date", "截断到当天 00:00:00。", "truncDay(|)", ex(`truncDay(created_at)`, "2026-11-10")),
			impl: conv(core.KindDate), types: []any{new(func(any) time.Time)}},
		{doc: doc("year", CatTime, "year(t)", I, "年份。", "year(|)", ex(`year(created_at)`, "2026")), impl: part(func(t time.Time) int { return t.Year() }), types: []any{new(func(any) int64)}},
		{doc: doc("month", CatTime, "month(t)", I, "月份 1–12。", "month(|)", ex(`month(created_at)`, "11")), impl: part(func(t time.Time) int { return int(t.Month()) }), types: []any{new(func(any) int64)}},
		{doc: doc("day", CatTime, "day(t)", I, "日 1–31。", "day(|)", ex(`day(created_at)`, "10")), impl: part(func(t time.Time) int { return t.Day() }), types: []any{new(func(any) int64)}},
		{doc: doc("hour", CatTime, "hour(t)", I, "小时 0–23。", "hour(|)", ex(`hour(created_at)`, "9")), impl: part(func(t time.Time) int { return t.Hour() }), types: []any{new(func(any) int64)}},
		{doc: doc("weekday", CatTime, "weekday(t)", I, "星期几：1=周一 … 7=周日。", "weekday(|)", ex(`weekday(today())`, "2")),
			impl: part(func(t time.Time) int {
				if w := int(t.Weekday()); w != 0 {
					return w
				}
				return 7
			}), types: []any{new(func(any) int64)}},
		{doc: doc("unixMillis", CatTime, "unixMillis(t)", I, "Unix 毫秒时间戳。", "unixMillis(|)", ex(`unixMillis(ts)`, "1762738200000")),
			impl: func(a ...any) (any, error) {
				t, ok, err := timeArg(a[0])
				if err != nil || !ok {
					return nil, err
				}
				return t.UnixMilli(), nil
			}, types: []any{new(func(any) int64)}},
		{doc: doc("fromUnixMillis", CatTime, "fromUnixMillis(n)", T, "Unix 毫秒时间戳转时间。", "fromUnixMillis(|)", ex(`fromUnixMillis(1762738200000)`, "2025-11-10 09:30:00")),
			impl: func(a ...any) (any, error) {
				if a[0] == nil {
					return nil, nil
				}
				n, err := intArg(a[0], "n")
				if err != nil {
					return nil, err
				}
				return time.UnixMilli(int64(n)), nil
			}, types: []any{new(func(any) time.Time)}},

		// ---------------- 条件与空值
		{doc: doc("coalesce", CatNull, "coalesce(a, b, ...)", A, "返回第一个非 NULL 的参数。", "coalesce(|, )", ex(`coalesce(nickname, name, "匿名")`, `"张三"`)),
			impl: func(a ...any) (any, error) {
				for _, v := range a {
					if v != nil {
						return v, nil
					}
				}
				return nil, nil
			}},
		{doc: doc("ifNull", CatNull, "ifNull(x, fallback)", A, "x 为 NULL 时返回 fallback。等价于 x ?? fallback。", "ifNull(|, 0)", ex(`ifNull(discount, 0)`, "0")),
			impl: func(a ...any) (any, error) {
				if a[0] == nil {
					return a[1], nil
				}
				return a[0], nil
			}},
		{doc: doc("nullIf", CatNull, "nullIf(a, b)", A, "a 等于 b 时返回 NULL，否则返回 a。常用于把空串当 NULL：nullIf(trim(s), \"\")。", `nullIf(|, "")`, ex(`nullIf(trim(code), "")`, "NULL")),
			impl: func(a ...any) (any, error) {
				if a[0] != nil && a[1] != nil && core.FormatValue(a[0]) == core.FormatValue(a[1]) {
					return nil, nil
				}
				return a[0], nil
			}},
		{doc: doc("isNull", CatNull, "isNull(x)", B, "是否为 NULL。", "isNull(|)", ex(`isNull(deleted_at)`, "true")),
			impl: func(a ...any) (any, error) { return a[0] == nil, nil }, types: []any{new(func(any) bool)}},
		{doc: doc("isBlank", CatNull, "isBlank(x)", B, "NULL 或去空白后为空串。", "isBlank(|)", ex(`isBlank("  ")`, "true")),
			impl: func(a ...any) (any, error) {
				s, ok := str(a[0])
				return !ok || strings.TrimSpace(s) == "", nil
			}, types: []any{new(func(any) bool)}},
		{doc: doc("decode", CatNull, "decode(x, k1, v1, k2, v2, ..., [default])", A, "按值映射：x 等于 k1 返回 v1……都不匹配返回 default（省略则 NULL）。", `decode(|, "1", "启用", "0", "停用", "未知")`,
			ex(`decode(status, 1, "启用", 0, "停用", "未知")`, `"启用"`)),
			impl: func(a ...any) (any, error) {
				if len(a) < 3 {
					return nil, errors.New("decode 至少需要 3 个参数")
				}
				key := a[0]
				rest := a[1:]
				for i := 0; i+1 < len(rest); i += 2 {
					if key != nil && rest[i] != nil && core.FormatValue(key) == core.FormatValue(rest[i]) {
						return rest[i+1], nil
					}
				}
				if len(rest)%2 == 1 {
					return rest[len(rest)-1], nil
				}
				return nil, nil
			}},

		// ---------------- 类型转换
		{doc: doc("toString", CatConvert, "toString(x, [format])", S, "转字符串；时间可指定格式。", "toString(|)", ex(`toString(42)`, `"42"`), ex(`toString(ts, "yyyyMMdd")`, `"20261110"`)), impl: conv(core.KindString), types: []any{new(func(any) string), new(func(any, any) string)}},
		{doc: doc("toInt", CatConvert, "toInt(x)", I, "转整数；含小数或不可解析时报错（该行进错误输出）。", "toInt(|)", ex(`toInt("042")`, "42")), impl: conv(core.KindInteger), types: []any{new(func(any) int64)}},
		{doc: doc("toNumber", CatConvert, "toNumber(x)", F, "转浮点数。", "toNumber(|)", ex(`toNumber("3.5")`, "3.5")), impl: conv(core.KindNumber), types: []any{new(func(any) float64)}},
		{doc: doc("toDecimal", CatConvert, "toDecimal(x)", "decimal", "转精确小数（金额请用它）。", "toDecimal(|)", ex(`toDecimal("19.90")`, "19.90")), impl: conv(core.KindDecimal), types: []any{new(func(any) decimal.Decimal)}},
		{doc: doc("toBool", CatConvert, "toBool(x)", B, "转布尔：true/false、1/0、yes/no、是/否。", "toBool(|)", ex(`toBool("是")`, "true")), impl: conv(core.KindBoolean), types: []any{new(func(any) bool)}},
		{doc: doc("toDate", CatConvert, "toDate(x, [format])", "date", "转日期（丢弃时分秒）。", "toDate(|)", ex(`toDate("2026-11-10 08:00:00")`, "2026-11-10")), impl: conv(core.KindDate), types: []any{new(func(any) time.Time), new(func(any, any) time.Time)}},
		{doc: doc("toTimestamp", CatConvert, "toTimestamp(x, [format])", T, "转日期时间。", "toTimestamp(|)", ex(`toTimestamp("2026-11-10 08:00:00")`, "2026-11-10 08:00:00")), impl: conv(core.KindTimestamp), types: []any{new(func(any) time.Time), new(func(any, any) time.Time)}},

		// ---------------- 摘要
		{doc: doc("md5", CatHash, "md5(s)", S, "MD5 十六进制摘要（用于变更检测，不用于安全场景）。", "md5(|)", ex(`md5("abc")`, `"900150983cd24fb0d6963f7d28e17f72"`)),
			impl: strFn(func(s string) string { h := md5.Sum([]byte(s)); return hex.EncodeToString(h[:]) }), types: []any{new(func(string) string)}},
		{doc: doc("sha256", CatHash, "sha256(s)", S, "SHA-256 十六进制摘要。", "sha256(|)", ex(`sha256(concat(a, "|", b))`, `"…"`)),
			impl: strFn(func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }), types: []any{new(func(string) string)}},
	}
}

func padFn(left bool, str func(any) (string, bool), intArg func(any, string) (int, error)) func(args ...any) (any, error) {
	return func(a ...any) (any, error) {
		s, ok := str(a[0])
		if !ok {
			return nil, nil
		}
		n, err := intArg(a[1], "n")
		if err != nil {
			return nil, err
		}
		pad, _ := str(a[2])
		if pad == "" {
			pad = " "
		}
		cur := utf8.RuneCountInString(s)
		if cur >= n {
			return s, nil
		}
		fill := []rune(strings.Repeat(pad, n))[:n-cur]
		if left {
			return string(fill) + s, nil
		}
		return s + string(fill), nil
	}
}

func numFn(f func(float64) float64, d func(decimal.Decimal) decimal.Decimal) func(args ...any) (any, error) {
	return func(a ...any) (any, error) {
		switch x := core.Normalize(a[0]).(type) {
		case nil:
			return nil, nil
		case int64:
			return x, nil
		case float64:
			return f(x), nil
		case decimal.Decimal:
			return d(x), nil
		}
		return nil, fmt.Errorf("需要数值参数")
	}
}

func extremum(sign int) func(args ...any) (any, error) {
	return func(a ...any) (any, error) {
		var best any
		for _, v := range a {
			if v == nil {
				continue
			}
			if best == nil {
				best = v
				continue
			}
			c, err := compareValues(v, best)
			if err != nil {
				return nil, err
			}
			if c*sign > 0 {
				best = v
			}
		}
		return best, nil
	}
}

// compareValues 比较两个非 NULL 的逻辑值（数值跨类型可比）。
func compareValues(a, b any) (int, error) {
	a, b = core.Normalize(a), core.Normalize(b)
	if da, ok := toDecimal(a); ok {
		if db, ok := toDecimal(b); ok {
			return da.Cmp(db), nil
		}
	}
	switch x := a.(type) {
	case string:
		if y, ok := b.(string); ok {
			return strings.Compare(x, y), nil
		}
	case time.Time:
		if y, ok := b.(time.Time); ok {
			return x.Compare(y), nil
		}
	}
	return 0, fmt.Errorf("无法比较 %T 与 %T", a, b)
}

var operatorDocs = []FuncDoc{
	{Name: "+ - * / %", Category: CatOperators, Signature: "a + b", Returns: "number", Operator: true, Insert: " + ",
		Description: "算术运算。精确小数参与时结果保持精确；任一侧为 NULL 时报错，请先用 ?? 或 ifNull 兜底。字符串可用 + 拼接。",
		Examples:    []Example{{Expr: "price * qty", Result: "39.80"}, {Expr: `first + " " + last`, Result: `"Ada Lovelace"`}}},
	{Name: "== != < <= > >=", Category: CatOperators, Signature: "a > b", Returns: "boolean", Operator: true, Insert: " > ",
		Description: "比较运算，可比较数值、文本、时间。", Examples: []Example{{Expr: "amount >= 1000", Result: "true"}}},
	{Name: "and or not", Category: CatOperators, Signature: "a and b", Returns: "boolean", Operator: true, Insert: " and ",
		Description: "逻辑运算，也可写作 && || !。短路求值。", Examples: []Example{{Expr: `status == 1 and not isNull(paid_at)`, Result: "true"}}},
	{Name: "?:", Category: CatOperators, Signature: "cond ? a : b", Returns: "any", Operator: true, Insert: " ? | : ",
		Description: "条件表达式：cond 为真取 a，否则取 b。", Examples: []Example{{Expr: `amount > 1000 ? "大额" : "普通"`, Result: `"大额"`}}},
	{Name: "??", Category: CatOperators, Signature: "x ?? fallback", Returns: "any", Operator: true, Insert: " ?? ",
		Description: "NULL 兜底：x 为 NULL 时取 fallback。", Examples: []Example{{Expr: "discount ?? 0", Result: "0"}}},
	{Name: "in", Category: CatOperators, Signature: "x in [a, b]", Returns: "boolean", Operator: true, Insert: " in [|]",
		Description: "是否属于列表。", Examples: []Example{{Expr: `region in ["华东", "华南"]`, Result: "true"}}},
	{Name: "contains startsWith endsWith matches", Category: CatOperators, Signature: `s contains "x"`, Returns: "boolean", Operator: true, Insert: " contains ",
		Description: "文本包含 / 前缀 / 后缀 / 正则匹配（RE2）。", Examples: []Example{{Expr: `email endsWith "@corp.cn"`, Result: "true"}, {Expr: `phone matches "^1[3-9]\\d{9}$"`, Result: "true"}}},
}
