package expr

import (
	"errors"
	"reflect"
	"regexp"
	"strconv"
	"testing"

	"mooncell/console/internal/etl/core"
)

func q(s string) string { return `"` + s + `"` }

// valueLiteralRe 匹配剥离引号标识符后仍残留的「值字面量」（字符串引号或数字）。
// 下推 SQL 只允许占位符 + 标识符 + 关键字，值必须全部走绑定参数。
var valueLiteralRe = regexp.MustCompile(`'|\d`)

// stripIdents 移除双引号标识符（字段名里可能含数字/特殊字符，不应误判为字面量）。
var identRe2 = regexp.MustCompile(`"[^"]*"`)

func TestToSQL(t *testing.T) {
	fields := map[string]core.Kind{
		"a": core.KindInteger, "b": core.KindInteger, "c": core.KindInteger,
		"amt": core.KindDecimal, "f": core.KindNumber,
		"d": core.KindDate, "ts": core.KindTimestamp,
		"name": core.KindString, "flag": core.KindBoolean,
		"weird name": core.KindInteger,
	}
	type okCase struct {
		src  string
		sql  string
		args []any
	}
	oks := []okCase{
		// ---- 次序比较：NULL 时引擎报错拒行 ≈ SQL NULL 被排除（允许顶层/&&）----
		{`a > 5`, `("a" > ?)`, []any{int64(5)}},
		{`a >= 1 && b < 2`, `(("a" >= ?) AND ("b" < ?))`, []any{int64(1), int64(2)}},
		{`amt >= 1.5`, `("amt" >= ?)`, []any{1.5}},
		{`d >= ts`, `("d" >= "ts")`, nil},
		{`a + 1 > 5`, `(("a" + ?) > ?)`, []any{int64(1), int64(5)}},
		{`a - b <= 10`, `(("a" - "b") <= ?)`, []any{int64(10)}},
		{`-a < 0`, `((-"a") < ?)`, []any{int64(0)}},
		// ---- 等值/不等：按引擎 NULL 语义补 IS NULL ----
		{`a == 5`, `("a" = ?)`, []any{int64(5)}},
		{`5 == a`, `(? = "a")`, []any{int64(5)}},
		{`a != 5`, `(("a" <> ?) OR ("a" IS NULL))`, []any{int64(5)}},
		{`5 != a`, `((? <> "a") OR ("a" IS NULL))`, []any{int64(5)}},
		{`a == b`, `(("a" = "b") OR (("a" IS NULL) AND ("b" IS NULL)))`, nil},
		{`a != b`, `(("a" <> "b") OR (("a" IS NULL) AND ("b" IS NOT NULL)) OR (("a" IS NOT NULL) AND ("b" IS NULL)))`, nil},
		{`a == b && a > 0`, `((("a" = "b") OR (("a" IS NULL) AND ("b" IS NULL))) AND ("a" > ?))`, []any{int64(0)}},
		// ---- nil 判定（任意已声明字段）----
		{`a == nil`, `("a" IS NULL)`, nil},
		{`a != nil`, `("a" IS NOT NULL)`, nil},
		{`nil == a`, `("a" IS NULL)`, nil},
		{`name == nil`, `("name" IS NULL)`, nil},
		{`name != nil`, `("name" IS NOT NULL)`, nil},
		{`isNull(a)`, `("a" IS NULL)`, nil},
		// ---- in / not in：数值字面量列表，not in 补 OR IS NULL ----
		{`a in [1, 2, 3]`, `("a" IN (?, ?, ?))`, []any{int64(1), int64(2), int64(3)}},
		{`a not in [1]`, `(("a" NOT IN (?)) OR ("a" IS NULL))`, []any{int64(1)}},
		{`a in []`, `(FALSE)`, nil},
		{`a not in []`, `(TRUE)`, nil},
		// ---- || 两侧都是 NULL 安全结构时可下推 ----
		{`a == 1 || b == 2`, `(("a" = ?) OR ("b" = ?))`, []any{int64(1), int64(2)}},
		{`a != 1 || b in [2]`, `((("a" <> ?) OR ("a" IS NULL)) OR ("b" IN (?)))`, []any{int64(1), int64(2)}},
		{`a == nil || b != nil`, `(("a" IS NULL) OR ("b" IS NOT NULL))`, nil},
		// ---- not：NULL 安全结构按德摩根律改写；单个次序比较可 NOT 包裹 ----
		{`not (a == 1)`, `(("a" <> ?) OR ("a" IS NULL))`, []any{int64(1)}},
		{`not (a != 1)`, `("a" = ?)`, []any{int64(1)}},
		{`not (a in [1, 2])`, `(("a" NOT IN (?, ?)) OR ("a" IS NULL))`, []any{int64(1), int64(2)}},
		{`not (a not in [1])`, `("a" IN (?))`, []any{int64(1)}},
		{`not (a == nil)`, `("a" IS NOT NULL)`, nil},
		{`not (a != nil)`, `("a" IS NULL)`, nil},
		{`not isNull(a)`, `("a" IS NOT NULL)`, nil},
		{`not (a == b)`, `(("a" <> "b") OR (("a" IS NULL) AND ("b" IS NOT NULL)) OR (("a" IS NOT NULL) AND ("b" IS NULL)))`, nil},
		{`not (a != b)`, `(("a" = "b") OR (("a" IS NULL) AND ("b" IS NULL)))`, nil},
		{`not (a == 1 && b == 2)`, `((("a" <> ?) OR ("a" IS NULL)) OR (("b" <> ?) OR ("b" IS NULL)))`, []any{int64(1), int64(2)}},
		{`not (a == 1 or b == 2)`, `((("a" <> ?) OR ("a" IS NULL)) AND (("b" <> ?) OR ("b" IS NULL)))`, []any{int64(1), int64(2)}},
		{`not (not (a == 1))`, `("a" = ?)`, []any{int64(1)}},
		{`not (a < 1)`, `(NOT ("a" < ?))`, []any{int64(1)}},
		{`a < 1 && not (a > 5)`, `(("a" < ?) AND (NOT ("a" > ?)))`, []any{int64(1), int64(5)}},
		// ---- 裸布尔字段：NULL 敏感叶，允许顶层/&& ----
		{`flag`, `("flag")`, nil},
		{`flag && a == 1`, `(("flag") AND ("a" = ?))`, []any{int64(1)}},
		{`not flag`, `(NOT ("flag"))`, nil},
		// ---- 常量布尔与 field("…") ----
		{`true`, `(TRUE)`, nil},
		{`field("weird name") == 3`, `("weird name" = ?)`, []any{int64(3)}},
		{`field("weird name") > 1 && a == nil`, `(("weird name" > ?) AND ("a" IS NULL))`, []any{int64(1)}},
	}
	for _, c := range oks {
		got, args, err := ToSQL(c.src, q, fields, nil)
		if err != nil {
			t.Errorf("%s → 意外失败: %v", c.src, err)
			continue
		}
		if got != c.sql {
			t.Errorf("%s →\n got: %s\nwant: %s", c.src, got, c.sql)
		}
		if !reflect.DeepEqual(args, c.args) {
			t.Errorf("%s → args got %#v want %#v", c.src, args, c.args)
		}
		// SQL 里不允许出现值字面量
		if valueLiteralRe.MatchString(identRe2.ReplaceAllString(got, "")) {
			t.Errorf("%s → SQL 含值字面量: %s", c.src, got)
		}
	}

	bad := []string{
		// 验收六个反例中属字符串语义的五条：必须拒推
		`name == "abc"`,        // 字符串比较（MySQL 排序规则下语义漂移）
		`length(name) == 4`,    // 字符串函数（LENGTH 按字节/字符漂移）
		`name == "a\\b"`,       // 字符串字面量（MySQL 反斜杠转义漂移）
		`trim(name) == "x"`,    // 字符串函数（TRIM 空白集漂移）
		`name + "x" == "abcx"`, // 字符串 +（MySQL 数值化强转）
		// 变量 / 运行期值
		`var("v") > 1`,
		// 字符串/日期/哈希等函数全部拒推
		`upper(name) == "X"`, `lower(name) == "x"`, `ltrim(name) == "x"`,
		`rtrim(name) == "x"`, `substr(a, 1, 2) > 0`, `replace(name, "a", "b") == "x"`,
		`concat(a, b) == 1`, `md5(name) == "x"`, `toString(a) == "x"`,
		`year(d) > 2020`, `month(d) == 1`, `len(name) > 0`,
		`coalesce(a, b) > 1`, `ifNull(a, "x") == "x"`, `nullIf(a, 1) > 0`,
		// 时钟函数：库时钟/时区与引擎不一致
		`now() > ts`, `d < today()`, `a < 1 || now() > ts`,
		// NULL 语义陷阱：次序比较/裸布尔字段不能进 || 或 not 的复合操作数
		`a < 1 || b == 1`, `a < 1 || a < 2`, `a > 0 || b in [1]`,
		`not (a < 1 && b == 1)`, `not (a < 1 || b == 1)`, `not (a > 0 and a < 9)`,
		`flag || a == 1`, `not (flag && a == 1)`, `a == 1 || flag`,
		// 运算符/结构不在白名单
		`a / 2`, `a % 2`, `a * 2 > 4`, `a ** 2 > 4`, `a .. b`,
		`a ?? 1 > 0`, `a ? b : c`, `flag ? a : b`,
		`a contains "x"`, `a startsWith "x"`, `a endsWith "x"`, `a matches "^x"`,
		// 字符串比较 / 字符串 in / 布尔比较
		`name != "a"`, `name < "b"`, `name in ["a", "b"]`,
		`flag == true`, `flag != a`, `a == "1"`, `a == "1" || b == 1`,
		// 其它拒绝面
		`a > nil`, `a < nil`, `a + nil > 1`,
		`unknown > 1`, `a.b > 1`, `a in b`, `a in [1, "x"]`, `a in [b]`, `1 in [1, 2]`,
		`isNull(a + b)`, `-name < 0`, `a - d > 0`, `not a`, `a`,
		`not not (a < 1)`, `a == nil || a < 1`,
		`a ? b : c == 1`,
	}
	for _, src := range bad {
		if sql, args, err := ToSQL(src, q, fields, nil); err == nil {
			t.Errorf("%s → 应拒绝下推，得到 %s args=%v", src, sql, args)
		} else if !errors.Is(err, ErrNotPushable) && !errors.As(err, new(*CompileError)) {
			t.Errorf("%s → 错误类型应为 ErrNotPushable/CompileError，实际 %v", src, err)
		}
	}
}

// TestToSQLPlaceholders 校验 emit 生成的占位符按出现顺序进入 SQL 且与 args 一一对应。
func TestToSQLPlaceholders(t *testing.T) {
	fields := map[string]core.Kind{"a": core.KindInteger, "b": core.KindInteger}
	n := 2 // 模拟源 SQL 已占用 $1、$2
	emit := func() string {
		n++
		return "$" + strconv.Itoa(n)
	}
	got, args, err := ToSQL(`a == 5 && b in [7, 9]`, q, fields, emit)
	if err != nil {
		t.Fatal(err)
	}
	want := `(("a" = $3) AND ("b" IN ($4, $5)))`
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if !reflect.DeepEqual(args, []any{int64(5), int64(7), int64(9)}) {
		t.Fatalf("args %#v", args)
	}
}
