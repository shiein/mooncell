package expr

import (
	"errors"
	"fmt"
	"strings"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/parser"

	"mooncell/console/internal/etl/core"
)

// ErrNotPushable 表示表达式含有无法证明与行内求值等价的结构。
// 调用方应放弃下推，退回行内求值（不是错误，只是不能优化）。
var ErrNotPushable = errors.New("表达式含不可下推的结构")

// ToSQL 把过滤条件翻译为可证等价的 SQL 片段，返回 (sql, args)。
//
// 所有值字面量一律生成绑定参数：emit 按序返回下一个占位符（nil → "?"），
// 对应值记入返回的 args——SQL 文本中不允许出现任何值字面量。
// fields 是编译期字段表（字段名 → Kind）；quote 为方言标识符引用。
//
// 可下推子集（NULL 语义已按引擎折算）：
//
//   - 比较两侧为白名单 kind 字段（整数/精确小数/浮点/日期/时间戳）或数值字面量：
//     a == b → (a = b OR (a IS NULL AND b IS NULL)) —— 字段-字面量简化为 (a = ?)
//     a != b → (a <> b OR (a IS NULL AND b IS NOT NULL) OR (a IS NOT NULL AND b IS NULL))
//     字段-字面量 a != v → ((a <> ?) OR (a IS NULL))（引擎里 nil != 1 为真）
//     a < <= > >= b → (a op b)：NULL 时引擎求值报错拒行、SQL 的 NULL 比较不投递，
//     投递结果一致——但正因如此次序比较不允许出现在 || / not 内
//     （NULL OR true → true、NOT(NULL AND false) → true 会把拒行的行改判通过）
//   - f in [数值字面量…]；f not in […] → ((f NOT IN (…)) OR (f IS NULL))
//   - f == nil / f != nil / isNull(f)（任意已声明字段）→ IS NULL / IS NOT NULL
//   - && / ||：|| 的操作数必须全部是 NULL 安全结构（上三条及其组合、not 改写、
//     布尔字面量）；&& 不做此限制
//   - not X：X 是 NULL 安全结构时按德摩根律递归改写（not(a==v) → (a<>? OR a IS NULL)）；
//     X 是单个次序比较或裸布尔字段时生成 (NOT …)，同样只允许在非 NULL 敏感位置
//   - 数值 +/-（两侧均须数值）：只允许作为次序比较的操作数
//   - 布尔 kind 字段单独作条件 → (f)，视为 NULL 敏感叶（位置限制同次序比较）
//   - true / false 常量 → (TRUE) / (FALSE)（关键字，不是值字面量）
//
// 其余一律 ErrNotPushable：字符串比较与 in、一切函数调用（field() 引用除外）、
// var()、now()/today()、三元、??、/ % * ** ..、matches/contains/startsWith/endsWith、
// 成员访问、非字面量 in 列表。
func ToSQL(src string, quote func(string) string, fields map[string]core.Kind, emit func() string) (string, []any, error) {
	if quote == nil {
		quote = func(s string) string { return s }
	}
	if emit == nil {
		emit = func() string { return "?" }
	}
	tr, err := parser.Parse(src)
	if err != nil {
		return "", nil, toCompileError(err)
	}
	w := &sqlWriter{quote: quote, fields: fields, emit: emit}
	sql, err := w.cond(tr.Node, false)
	if err != nil {
		return "", nil, err
	}
	return sql, w.args, nil
}

type sqlWriter struct {
	quote  func(string) string
	fields map[string]core.Kind
	emit   func() string
	args   []any
}

// ph 记录一个绑定参数并返回其占位符文本。
func (w *sqlWriter) ph(v any) string {
	w.args = append(w.args, v)
	return w.emit()
}

func (w *sqlWriter) fail(format string, args ...any) (string, error) {
	return "", fmt.Errorf("%w：%s", ErrNotPushable, fmt.Sprintf(format, args...))
}

// refName 提取字段名：裸标识符或 field("x")。
func (w *sqlWriter) refName(n ast.Node) (string, error) {
	switch t := n.(type) {
	case *ast.IdentifierNode:
		return t.Value, nil
	case *ast.CallNode:
		if calleeName(t.Callee) == "field" && len(t.Arguments) == 1 {
			if s, ok := t.Arguments[0].(*ast.StringNode); ok {
				return s.Value, nil
			}
		}
		return "", fmt.Errorf("%w：field() 需要一个字符串字面量参数", ErrNotPushable)
	default:
		return "", fmt.Errorf("%w：%T 不是字段引用", ErrNotPushable, n)
	}
}

// ref 解析已声明的字段引用，返回引用文本与编译期 kind。
func (w *sqlWriter) ref(n ast.Node) (string, core.Kind, error) {
	name, err := w.refName(n)
	if err != nil {
		return "", "", err
	}
	k, ok := w.fields[name]
	if !ok {
		return "", "", fmt.Errorf("%w：字段 %q 不在上游字段中", ErrNotPushable, name)
	}
	return w.quote(name), k, nil
}

// atom 是比较操作数：白名单 kind 字段或数值字面量。
// 返回（SQL 片段, kind, 是否字段）。
func (w *sqlWriter) atom(n ast.Node) (string, core.Kind, bool, error) {
	switch t := n.(type) {
	case *ast.IntegerNode:
		return w.ph(int64(t.Value)), core.KindInteger, false, nil
	case *ast.FloatNode:
		return w.ph(t.Value), core.KindNumber, false, nil
	}
	x, k, err := w.ref(n)
	if err != nil {
		return "", "", false, err
	}
	if !k.IsNumeric() && !k.IsTemporal() {
		return "", "", false, fmt.Errorf("%w：字段类型 %s 的比较不可下推", ErrNotPushable, k)
	}
	return x, k, true, nil
}

// value 是次序比较操作数：atom 或两侧均为数值的 +/-（含一元 +/-）。
// 返回（SQL 片段, 结果 kind）。
func (w *sqlWriter) value(n ast.Node) (string, core.Kind, error) {
	switch t := n.(type) {
	case *ast.BinaryNode:
		if t.Operator != "+" && t.Operator != "-" {
			return "", "", fmt.Errorf("%w：运算符 %q 不可下推", ErrNotPushable, t.Operator)
		}
		l, lk, err := w.value(t.Left)
		if err != nil {
			return "", "", err
		}
		r, rk, err := w.value(t.Right)
		if err != nil {
			return "", "", err
		}
		if !lk.IsNumeric() || !rk.IsNumeric() {
			return "", "", fmt.Errorf("%w：%s 两侧必须都是数值", ErrNotPushable, t.Operator)
		}
		return "(" + l + " " + t.Operator + " " + r + ")", core.KindNumber, nil
	case *ast.UnaryNode:
		if t.Operator != "-" && t.Operator != "+" {
			return "", "", fmt.Errorf("%w：一元运算符 %q 不可下推", ErrNotPushable, t.Operator)
		}
		x, k, err := w.value(t.Node)
		if err != nil {
			return "", "", err
		}
		if !k.IsNumeric() {
			return "", "", fmt.Errorf("%w：%s 的操作数必须是数值", ErrNotPushable, t.Operator)
		}
		return "(" + t.Operator + x + ")", k, nil
	default:
		sql, k, _, err := w.atom(n)
		return sql, k, err
	}
}

var orderingOp = map[string]bool{"<": true, "<=": true, ">": true, ">=": true}

// cond 翻译布尔条件。safeOnly=true 时只允许 NULL 安全结构
// （|| 任一臂与 not 的复合操作数内，次序比较/裸布尔字段在 NULL 时
// 引擎报错拒行而 SQL 三值会把 NULL 并入另一侧结果，投递结果不再一致）。
func (w *sqlWriter) cond(n ast.Node, safeOnly bool) (string, error) {
	switch t := n.(type) {
	case *ast.BinaryNode:
		switch t.Operator {
		case "&&", "and":
			l, err := w.cond(t.Left, safeOnly)
			if err != nil {
				return "", err
			}
			r, err := w.cond(t.Right, safeOnly)
			if err != nil {
				return "", err
			}
			return "(" + l + " AND " + r + ")", nil
		case "||", "or":
			// || 的 NULL 敏感：任一侧次序比较为 NULL 时引擎报错而 SQL 可能得 true
			l, err := w.cond(t.Left, true)
			if err != nil {
				return "", err
			}
			r, err := w.cond(t.Right, true)
			if err != nil {
				return "", err
			}
			return "(" + l + " OR " + r + ")", nil
		case "in", "not in":
			return w.inList(t, false)
		case "==", "!=":
			return w.equality(t, false)
		default:
			if !orderingOp[t.Operator] {
				return w.fail("运算符 %q 不可下推", t.Operator)
			}
			if safeOnly {
				return w.fail("次序比较不能在 || / not 内下推（NULL 时引擎拒行而 SQL 会并入另一侧结果）")
			}
			return w.ordering(t)
		}
	case *ast.UnaryNode:
		if t.Operator != "not" && t.Operator != "!" {
			return w.fail("一元运算符 %q 不可下推", t.Operator)
		}
		return w.not(t, safeOnly)
	case *ast.CallNode:
		switch calleeName(t.Callee) {
		case "isNull":
			if len(t.Arguments) != 1 {
				return w.fail("isNull() 需要 1 个参数")
			}
			x, _, err := w.ref(t.Arguments[0])
			if err != nil {
				return "", err
			}
			return "(" + x + " IS NULL)", nil
		case "field":
			return w.boolField(t, safeOnly)
		case "var":
			return w.fail("var() 是运行时变量，不能下推")
		default:
			return w.fail("函数不可下推")
		}
	case *ast.IdentifierNode:
		return w.boolField(t, safeOnly)
	case *ast.BoolNode:
		if t.Value {
			return "(TRUE)", nil
		}
		return "(FALSE)", nil
	default:
		return w.fail("结构 %T 不可下推", n)
	}
}

// boolField 是裸字段作条件：须布尔 kind，且属 NULL 敏感叶。
func (w *sqlWriter) boolField(n ast.Node, safeOnly bool) (string, error) {
	name, err := w.refName(n)
	if err != nil {
		return "", err
	}
	k, ok := w.fields[name]
	if !ok {
		return w.fail("字段 %q 不在上游字段中", name)
	}
	if k != core.KindBoolean {
		return w.fail("字段 %q 不是布尔类型，不能单独作条件", name)
	}
	if safeOnly {
		return w.fail("裸布尔字段不能在 || / not 内下推（NULL 时引擎报错而 SQL 并入另一侧）")
	}
	return "(" + w.quote(name) + ")", nil
}

// unsafeLeaf 报告节点是否为 NULL 敏感叶：次序比较或裸布尔字段。
// 这类叶子在 NULL 时引擎报错拒行、SQL 得 NULL，只有「NOT NULL → NULL 被排除」
// 保持一致的顶层/&& 位置才能出现。
func (w *sqlWriter) unsafeLeaf(n ast.Node) bool {
	switch t := n.(type) {
	case *ast.BinaryNode:
		return orderingOp[t.Operator]
	case *ast.IdentifierNode:
		k, ok := w.fields[t.Value]
		return ok && k == core.KindBoolean
	case *ast.CallNode:
		if name, err := w.refName(t); err == nil {
			k, ok := w.fields[name]
			return ok && k == core.KindBoolean
		}
	}
	return false
}

// not 翻译 not/!。
func (w *sqlWriter) not(t *ast.UnaryNode, safeOnly bool) (string, error) {
	inner := t.Node
	// 单个 NULL 敏感叶：NOT NULL → NULL 被排除，与引擎报错拒行的投递结果一致；
	// 在 || / not 内则会把 NULL 并入另一侧 → 只允许非 NULL 敏感位置。
	if w.unsafeLeaf(inner) {
		if safeOnly {
			return w.fail("not 不能包裹次序比较或裸布尔字段（NULL 时引擎报错而 SQL 并入另一侧）")
		}
		s, err := w.cond(inner, false)
		if err != nil {
			return "", err
		}
		return "(NOT " + s + ")", nil
	}
	return w.neg(inner)
}

// neg 对 NULL 安全子树按德摩根律取反；遇到 NULL 敏感结构即拒绝。
func (w *sqlWriter) neg(n ast.Node) (string, error) {
	switch t := n.(type) {
	case *ast.BinaryNode:
		switch t.Operator {
		case "&&", "and":
			l, err := w.neg(t.Left)
			if err != nil {
				return "", err
			}
			r, err := w.neg(t.Right)
			if err != nil {
				return "", err
			}
			return "(" + l + " OR " + r + ")", nil
		case "||", "or":
			l, err := w.neg(t.Left)
			if err != nil {
				return "", err
			}
			r, err := w.neg(t.Right)
			if err != nil {
				return "", err
			}
			return "(" + l + " AND " + r + ")", nil
		case "in", "not in":
			return w.inList(t, true)
		case "==", "!=":
			return w.equality(t, true)
		default:
			return w.fail("not 内只允许 NULL 安全结构，不支持 %q", t.Operator)
		}
	case *ast.UnaryNode:
		if t.Operator != "not" && t.Operator != "!" {
			return w.fail("not 内不支持一元运算符 %q", t.Operator)
		}
		return w.cond(t.Node, true) // not not X ≡ X，X 须 NULL 安全
	case *ast.CallNode:
		if calleeName(t.Callee) == "isNull" && len(t.Arguments) == 1 {
			x, _, err := w.ref(t.Arguments[0])
			if err != nil {
				return "", err
			}
			return "(" + x + " IS NOT NULL)", nil
		}
		return w.fail("not 不能包裹函数")
	case *ast.BoolNode:
		if t.Value {
			return "(FALSE)", nil
		}
		return "(TRUE)", nil
	default:
		return w.fail("not 不能包裹结构 %T", n)
	}
}

// ordering 翻译次序比较：两侧为白名单 atom 或数值 +/-。
// NULL 时引擎报错拒行、SQL 的 NULL 比较不投递——投递结果一致。
func (w *sqlWriter) ordering(t *ast.BinaryNode) (string, error) {
	l, _, err := w.value(t.Left)
	if err != nil {
		return "", err
	}
	r, _, err := w.value(t.Right)
	if err != nil {
		return "", err
	}
	return "(" + l + " " + t.Operator + " " + r + ")", nil
}

// equality 翻译 ==/!=（negated 时取反）。按引擎 NULL 语义补 IS NULL 守卫。
func (w *sqlWriter) equality(t *ast.BinaryNode, negated bool) (string, error) {
	op := t.Operator
	if negated {
		if op == "==" {
			op = "!="
		} else {
			op = "=="
		}
	}
	// nil 一侧：x == nil → IS NULL；x != nil → IS NOT NULL（任意已声明字段）
	_, lNil := t.Left.(*ast.NilNode)
	_, rNil := t.Right.(*ast.NilNode)
	if lNil || rNil {
		if lNil && rNil {
			if op == "==" {
				return "(TRUE)", nil
			}
			return "(FALSE)", nil
		}
		other := t.Left
		if lNil {
			other = t.Right
		}
		x, _, err := w.ref(other)
		if err != nil {
			return "", err
		}
		if op == "==" {
			return "(" + x + " IS NULL)", nil
		}
		return "(" + x + " IS NOT NULL)", nil
	}
	l, _, lf, err := w.atom(t.Left)
	if err != nil {
		return "", err
	}
	r, _, rf, err := w.atom(t.Right)
	if err != nil {
		return "", err
	}
	if op == "==" {
		switch {
		case lf && rf:
			return "((" + l + " = " + r + ") OR ((" + l + " IS NULL) AND (" + r + " IS NULL)))", nil
		default:
			// 单字段+字面量：字段为 NULL 时 (f = ?) → NULL 被排除 ≈ 引擎 false
			return "(" + l + " = " + r + ")", nil
		}
	}
	switch {
	case lf && rf:
		return "((" + l + " <> " + r + ") OR ((" + l + " IS NULL) AND (" + r + " IS NOT NULL)) OR ((" + l + " IS NOT NULL) AND (" + r + " IS NULL)))", nil
	case lf != rf:
		f := l
		if !lf {
			f = r
		}
		// 引擎里 nil != v 为真：须补 OR f IS NULL
		return "((" + l + " <> " + r + ") OR (" + f + " IS NULL))", nil
	default:
		return "(" + l + " <> " + r + ")", nil
	}
}

// inList 翻译 in / not in：左为白名单字段，右为数值字面量列表。
// negated 取反（not(x in …) ≡ x not in …）。
func (w *sqlWriter) inList(t *ast.BinaryNode, negated bool) (string, error) {
	l, _, lf, err := w.atom(t.Left)
	if err != nil {
		return "", err
	}
	if !lf {
		return w.fail("in 左边必须是字段")
	}
	arr, ok := t.Right.(*ast.ArrayNode)
	if !ok {
		return w.fail("in 只支持字面量列表")
	}
	in := (t.Operator == "in") != negated
	if len(arr.Nodes) == 0 {
		if in {
			return "(FALSE)", nil
		}
		return "(TRUE)", nil
	}
	parts := make([]string, 0, len(arr.Nodes))
	for _, e := range arr.Nodes {
		s, _, isF, err := w.atom(e)
		if err != nil {
			return "", err
		}
		if isF {
			return w.fail("in 列表只接受数值字面量")
		}
		parts = append(parts, s)
	}
	if in {
		return "(" + l + " IN (" + strings.Join(parts, ", ") + "))", nil
	}
	// 引擎里 nil not in […] 为真：须补 OR f IS NULL
	return "((" + l + " NOT IN (" + strings.Join(parts, ", ") + ")) OR (" + l + " IS NULL))", nil
}

// calleeName 提取函数名（field/var 等自定义函数走 CallNode）。
func calleeName(n ast.Node) string {
	if id, ok := n.(*ast.IdentifierNode); ok {
		return id.Value
	}
	return ""
}
