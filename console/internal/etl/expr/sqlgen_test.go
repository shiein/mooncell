package expr

import (
	"errors"
	"testing"
)

func q(s string) string { return `"` + s + `"` }

func TestToSQL(t *testing.T) {
	fields := map[string]bool{"a": true, "b": true, "c": true, "weird name": true}
	ok := map[string]string{
		`a > 5`:                    `("a" > 5)`,
		`a >= 1 && b < 2`:          `(("a" >= 1) AND ("b" < 2))`,
		`a == "x'"`:                `("a" = 'x''')`,
		`a == nil`:                 `("a" IS NULL)`,
		`a != nil`:                 `("a" IS NOT NULL)`,
		`nil == a`:                 `("a" IS NULL)`,
		`not (a == 1 or b == 2)`:   `(NOT (("a" = 1) OR ("b" = 2)))`,
		`a in [1, 2, 3]`:           `("a" IN (1, 2, 3))`,
		`a not in ["x"]`:           `(NOT ("a" IN ('x')))`,
		`a in []`:                  `(1 = 0)`,
		`a + b * 2`:                `("a" + ("b" * 2))`,
		`-a < 0`:                   `((-"a") < 0)`,
		`lower(a) == "x"`:          `(LOWER("a") = 'x')`,
		`substr(a, 1, 2) != "xy"`:  `(SUBSTR("a", 1, 2) <> 'xy')`,
		`isNull(a) || isNull(b)`:   `(("a" IS NULL) OR ("b" IS NULL))`,
		`(a ?? 0) > 1`:             `(COALESCE("a", 0) > 1)`,
		`a ? b : c`:                `(CASE WHEN "a" THEN "b" ELSE "c" END)`,
		`field("weird name") == 3`: `("weird name" = 3)`,
		`ifNull(a, "x") == "x"`:    `(COALESCE("a", 'x') = 'x')`,
		`coalesce(a, b) != nil`:    `(COALESCE("a", "b") IS NOT NULL)`,
		`a == true`:                `("a" = TRUE)`,
		`year(a) > 2020 && year(a) < 2030 || a == 1`: `x`, // 占位：year 不可下推，应走失败用例
	}
	delete(ok, `year(a) > 2020 && year(a) < 2030 || a == 1`)
	for src, want := range ok {
		got, err := ToSQL(src, q, fields)
		if err != nil {
			t.Errorf("%s → 意外失败: %v", src, err)
			continue
		}
		if got != want {
			t.Errorf("%s →\n got: %s\nwant: %s", src, got, want)
		}
	}

	bad := []string{
		`var("v") > 1`,        // 运行时变量
		`a / 2`,               // 整除语义漂移
		`a % 2`,               // 取模函数名漂移
		`a contains "x"`,      // LIKE 大小写规则漂移
		`a startsWith "x"`,    // 同上
		`a matches "^x"`,      // 正则引擎差异
		`a in b`,              // 非字面量列表
		`a > nil`,             // nil 仅 =/!=
		`unknown > 1`,         // 未声明字段
		`concat(a, b) == "x"`, // sqlite<3.44 无 CONCAT
		`md5(a) == "x"`,       // 哈希函数方言差异
		`toString(a) == "x"`,  // 转换函数方言差异
		`a.b > 1`,             // 成员访问
		`len([1]) > 0`,        // 非白名单（len 不是 length）
	}
	for _, src := range bad {
		if _, err := ToSQL(src, q, fields); err == nil {
			t.Errorf("%s → 应拒绝下推", src)
		} else if !errors.Is(err, ErrNotPushable) && !errors.As(err, new(*CompileError)) {
			t.Errorf("%s → 错误类型应为 ErrNotPushable/CompileError，实际 %v", src, err)
		}
	}
}
