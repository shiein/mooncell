package expr

import (
	"errors"
	"fmt"

	"github.com/expr-lang/expr"
	"github.com/shopspring/decimal"

	"mooncell/console/internal/etl/core"
)

// 精确小数的运算符重载：任一侧为 decimal.Decimal 时，+ - * / 与比较走十进制运算，避免浮点误差。
// 数值另一侧可为 int / int64 / float64（float 先按最短十进制表示转换）。

var decType = decimal.Decimal{}

func toDecimal(v any) (decimal.Decimal, bool) {
	switch x := core.Normalize(v).(type) {
	case decimal.Decimal:
		return x, true
	case int64:
		return decimal.NewFromInt(x), true
	case float64:
		return decimal.NewFromFloat(x), true
	}
	return decimal.Decimal{}, false
}

func decArgs(a []any) (decimal.Decimal, decimal.Decimal, error) {
	if a[0] == nil || a[1] == nil {
		return decimal.Decimal{}, decimal.Decimal{}, errNullArg
	}
	x, ok1 := toDecimal(a[0])
	y, ok2 := toDecimal(a[1])
	if !ok1 || !ok2 {
		return x, y, fmt.Errorf("无法对 %T 与 %T 做精确小数运算", a[0], a[1])
	}
	return x, y, nil
}

// decimalOptions 返回运算符重载所需的 expr 选项。
func decimalOptions() []expr.Option {
	pairs := [][2]any{
		{decType, decType}, {decType, 0}, {0, decType}, {decType, int64(0)}, {int64(0), decType},
		{decType, 0.0}, {0.0, decType},
	}
	arith := map[string]func(x, y decimal.Decimal) (any, error){
		"decAdd": func(x, y decimal.Decimal) (any, error) { return x.Add(y), nil },
		"decSub": func(x, y decimal.Decimal) (any, error) { return x.Sub(y), nil },
		"decMul": func(x, y decimal.Decimal) (any, error) { return x.Mul(y), nil },
		"decDiv": func(x, y decimal.Decimal) (any, error) {
			if y.IsZero() {
				return nil, errors.New("除数为 0")
			}
			return x.DivRound(y, 16), nil
		},
	}
	cmp := map[string]func(c int) bool{
		"decLt": func(c int) bool { return c < 0 }, "decLe": func(c int) bool { return c <= 0 },
		"decGt": func(c int) bool { return c > 0 }, "decGe": func(c int) bool { return c >= 0 },
		"decEq": func(c int) bool { return c == 0 }, "decNe": func(c int) bool { return c != 0 },
	}
	var opts []expr.Option
	for name, f := range arith {
		f := f
		opts = append(opts, expr.Function(name, func(a ...any) (any, error) {
			x, y, err := decArgs(a)
			if err != nil {
				return nil, err
			}
			return f(x, y)
		}, signatures(pairs, decType)...))
	}
	for name, f := range cmp {
		f := f
		opts = append(opts, expr.Function(name, func(a ...any) (any, error) {
			x, y, err := decArgs(a)
			if err != nil {
				return nil, err
			}
			return f(x.Cmp(y)), nil
		}, signatures(pairs, true)...))
	}
	ops := map[string]string{"+": "decAdd", "-": "decSub", "*": "decMul", "/": "decDiv",
		"<": "decLt", "<=": "decLe", ">": "decGt", ">=": "decGe", "==": "decEq", "!=": "decNe"}
	for op, fn := range ops {
		opts = append(opts, expr.Operator(op, fn))
	}
	return opts
}

// signatures 为每对操作数类型生成 func(A, B) R 的类型签名（expr 用 new(func…) 指针表达签名）。
func signatures(pairs [][2]any, ret any) []any {
	out := make([]any, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, funcSig(p[0], p[1], ret))
	}
	return out
}
