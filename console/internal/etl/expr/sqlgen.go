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

// SQLCond 是 ToSQL 的翻译结果。
type SQLCond struct {
	SQL  string
	Args []any
	// Residual 为 true 表示 SQL 是行内条件的「超集」：NULL 敏感叶引用了可空字段，
	// 已改写为 (叶 OR 字段 IS NULL …)。调用方必须保留原过滤步骤在行内复核——
	// 否则 NULL 行在引擎里会让过滤报错（运行失败），而源端 SQL 会把它静默滤掉。
	Residual bool
}

// ToSQL 把过滤条件翻译为可证等价（或带行内复核的超集）的 SQL 片段。
//
// 所有值字面量一律生成绑定参数：emit 按序返回下一个占位符（nil → "?"），
// 对应值记入 Args——SQL 文本中不允许出现任何值字面量。
// fields 是编译期字段表（字段名 → Kind）；notNull 列出已知不可空的字段
// （缺省或未列出一律按可空处理）；quote 为方言标识符引用。
//
// 可下推子集（NULL 语义已按引擎折算）：
//
//   - 比较两侧为白名单 kind 字段（整数/精确小数/浮点/日期/时间戳）或数值字面量：
//     a == b → (a = b OR (a IS NULL AND b IS NULL)) —— 字段-字面量简化为 (a = ?)
//     a != b → (a <> b OR (a IS NULL AND b IS NOT NULL) OR (a IS NOT NULL AND b IS NULL))
//     字段-字面量 a != v → ((a <> ?) OR (a IS NULL))（引擎里 nil != 1 为真）
//   - 次序比较 a < <= > >= b（含数值 +/- 操作数）与裸布尔字段是「NULL 敏感叶」：
//     NULL 时引擎求值报错（无错误端口即运行失败），SQL 却只是不投递。
//     叶内字段全部不可空 → 原样 (a op b)；
//     否则 → ((a op b) OR (a IS NULL) …) 并置 Residual，由行内过滤复核（NULL 行照常报错）。
//     这类叶只允许出现在顶层 / && 内（超集在合取下保持超集），不允许进 || / not 复合。
//   - f in [数值字面量…]；f not in […] → ((f NOT IN (…)) OR (f IS NULL))
//   - f == nil / f != nil / isNull(f)（任意已声明字段）→ IS NULL / IS NOT NULL
//   - && / ||：|| 的操作数必须全部是 NULL 安全结构（上述等值/in/nil 及其组合、not 改写、
//     布尔字面量）；&& 不做此限制
//   - not X：X 是 NULL 安全结构时按德摩根律递归改写（not(a==v) → (a<>? OR a IS NULL)）；
//     X 是单个 NULL 敏感叶时生成 (NOT 叶)（可空则同样补 IS NULL 超集），位置限制同上
//   - true / false 常量 → (TRUE) / (FALSE)（关键字，不是值字面量）
//
// 其余一律 ErrNotPushable：字符串比较与 in、一切函数调用（field() 引用除外）、
// var()、now()/today()、三元、??、/ % * ** ..、matches/contains/startsWith/endsWith、
// 成员访问、非字面量 in 列表。
func ToSQL(src string, quote func(string) string, fields map[string]core.Kind, notNull map[string]bool, emit func() string) (SQLCond, error) {
	if quote == nil {
		quote = func(s string) string { return s }
	}
	if emit == nil {
		emit = func() string { return "?" }
	}
	tr, err := parser.Parse(src)
	if err != nil {
		return SQLCond{}, toCompileError(err)
	}
	w := &sqlWriter{quote: quote, fields: fields, notNull: notNull, emit: emit}
	sql, err := w.cond(tr.Node, false)
	if err != nil {
		return SQLCond{}, err
	}
	return SQLCond{SQL: sql, Args: w.args, Residual: w.residual}, nil
}

type sqlWriter struct {
	quote    func(string) string
	fields   map[string]core.Kind
	notNull  map[string]bool
	emit     func() string
	args     []any
	residual bool
	collect  *[]string // 非 nil 时 ref() 记录引用到的字段名（NULL 敏感叶守卫用）
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
	if w.collect != nil {
		*w.collect = append(*w.collect, name)
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
				return w.fail("次序比较不能在 || / not 内下推（NULL 时引擎报错而 SQL 会并入另一侧结果）")
			}
			raw, names, err := w.leaf(t)
			if err != nil {
				return "", err
			}
			return w.guard(raw, names), nil
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
	if safeOnly {
		if _, err := w.boolFieldRaw(n); err != nil {
			return "", err
		}
		return w.fail("裸布尔字段不能在 || / not 内下推（NULL 时引擎报错而 SQL 并入另一侧）")
	}
	raw, names, err := w.leaf(n)
	if err != nil {
		return "", err
	}
	return w.guard(raw, names), nil
}

// boolFieldRaw 翻译裸布尔字段（不加 NULL 守卫），并把字段名记入 collect。
func (w *sqlWriter) boolFieldRaw(n ast.Node) (string, error) {
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
	if w.collect != nil {
		*w.collect = append(*w.collect, name)
	}
	return "(" + w.quote(name) + ")", nil
}

// leaf 翻译单个 NULL 敏感叶（次序比较或裸布尔字段），不加守卫；返回叶内引用的字段名。
func (w *sqlWriter) leaf(n ast.Node) (string, []string, error) {
	var names []string
	prev := w.collect
	w.collect = &names
	defer func() { w.collect = prev }()
	var s string
	var err error
	if b, ok := n.(*ast.BinaryNode); ok && orderingOp[b.Operator] {
		s, err = w.ordering(b)
	} else {
		s, err = w.boolFieldRaw(n)
	}
	return s, names, err
}

// guard 给 NULL 敏感叶补可空字段的 IS NULL 分支，使 SQL 成为行内条件的超集：
// 引擎对这些行求值会报错（运行失败），源端不能把它们静默滤掉，须留给行内过滤复核。
// 叶内字段全部声明不可空时原样返回（与行内求值严格等价，无需复核）。
func (w *sqlWriter) guard(raw string, names []string) string {
	var g []string
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] || w.notNull[n] {
			continue
		}
		seen[n] = true
		g = append(g, "("+w.quote(n)+" IS NULL)")
	}
	if len(g) == 0 {
		return raw
	}
	w.residual = true
	return "(" + raw + " OR " + strings.Join(g, " OR ") + ")"
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
	// 单个 NULL 敏感叶：NOT 作用在未守卫的叶上，再整体补可空字段的 IS NULL 超集分支
	// （先守卫再取反会变成子集）。在 || / not 内则会把 NULL 并入另一侧 → 只允许非 NULL 敏感位置。
	if w.unsafeLeaf(inner) {
		if safeOnly {
			return w.fail("not 不能包裹次序比较或裸布尔字段（NULL 时引擎报错而 SQL 并入另一侧）")
		}
		raw, names, err := w.leaf(inner)
		if err != nil {
			return "", err
		}
		return w.guard("(NOT "+raw+")", names), nil
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
