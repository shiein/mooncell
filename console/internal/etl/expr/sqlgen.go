package expr

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/parser"
)

// ErrNotPushable 表示表达式含有无法安全下推为 SQL 的结构
// （变量、正则、成员访问、语义随方言漂移的运算符/函数等）。调用方应退回行内求值。
var ErrNotPushable = errors.New("表达式含不可下推的结构")

// sqlFuncs 是可直接映射为同名 SQL 函数的白名单。
// 仅收录在 PostgreSQL/MySQL/SQLite/达梦/金仓上同名同义的函数。
var sqlFuncs = map[string]string{
	"upper": "UPPER", "lower": "LOWER", "trim": "TRIM", "ltrim": "LTRIM", "rtrim": "RTRIM",
	"length": "LENGTH", "substr": "SUBSTR", "replace": "REPLACE",
	"abs": "ABS", "round": "ROUND", "floor": "FLOOR", "ceil": "CEIL",
	"coalesce": "COALESCE", "ifNull": "COALESCE", "nullIf": "NULLIF",
}

// sqlCmp 比较运算符映射（"=="→"=" 等）。
var sqlCmp = map[string]string{"==": "=", "!=": "<>", "<": "<", "<=": "<=", ">": ">", ">=": ">="}

// ToSQL 把表达式翻译为 SQL 片段。quote 为方言标识符引用；fields 是可用字段名集合
// （裸标识符与 field("x") 必须在其中）。所有二元结果全括号化，不依赖方言优先级。
// 不可下推返回 ErrNotPushable；语法本身错误返回解析错误。
func ToSQL(src string, quote func(string) string, fields map[string]bool) (string, error) {
	if quote == nil {
		quote = func(s string) string { return s }
	}
	tr, err := parser.Parse(src)
	if err != nil {
		return "", toCompileError(err)
	}
	w := &sqlWriter{quote: quote, fields: fields}
	return w.expr(tr.Node)
}

type sqlWriter struct {
	quote  func(string) string
	fields map[string]bool
}

func (w *sqlWriter) fail(format string, args ...any) (string, error) {
	return "", fmt.Errorf("%w：%s", ErrNotPushable, fmt.Sprintf(format, args...))
}

func (w *sqlWriter) ident(name string) (string, error) {
	if w.fields != nil && !w.fields[name] {
		return w.fail("字段 %q 不在上游字段中", name)
	}
	return w.quote(name), nil
}

func (w *sqlWriter) expr(n ast.Node) (string, error) {
	switch t := n.(type) {
	case *ast.IntegerNode:
		return strconv.FormatInt(int64(t.Value), 10), nil
	case *ast.FloatNode:
		return strconv.FormatFloat(t.Value, 'g', -1, 64), nil
	case *ast.StringNode:
		return "'" + strings.ReplaceAll(t.Value, "'", "''") + "'", nil
	case *ast.BoolNode:
		if t.Value {
			return "TRUE", nil
		}
		return "FALSE", nil
	case *ast.NilNode:
		return w.fail("nil 仅可用于 =/!= 比较")
	case *ast.IdentifierNode:
		return w.ident(t.Value)
	case *ast.UnaryNode:
		return w.unary(t)
	case *ast.BinaryNode:
		return w.binary(t)
	case *ast.ConditionalNode:
		c, err := w.expr(t.Cond)
		if err != nil {
			return "", err
		}
		a, err := w.expr(t.Exp1)
		if err != nil {
			return "", err
		}
		b, err := w.expr(t.Exp2)
		if err != nil {
			return "", err
		}
		return "(CASE WHEN " + c + " THEN " + a + " ELSE " + b + " END)", nil
	case *ast.CallNode:
		return w.call(t)
	case *ast.BuiltinNode:
		return w.builtin(t)
	default:
		return w.fail("不支持的结构 %T", n)
	}
}

func (w *sqlWriter) unary(t *ast.UnaryNode) (string, error) {
	x, err := w.expr(t.Node)
	if err != nil {
		return "", err
	}
	switch t.Operator {
	case "-":
		return "(-" + x + ")", nil
	case "+":
		return "(+" + x + ")", nil
	case "!", "not":
		return "(NOT " + x + ")", nil
	}
	return w.fail("不支持的一元运算符 %q", t.Operator)
}

func (w *sqlWriter) binary(t *ast.BinaryNode) (string, error) {
	// NULL 三值语义：x == nil → IS NULL；x != nil → IS NOT NULL。
	_, lNil := t.Left.(*ast.NilNode)
	_, rNil := t.Right.(*ast.NilNode)
	if lNil || rNil {
		if t.Operator != "==" && t.Operator != "!=" {
			return w.fail("nil 仅可用于 =/!= 比较")
		}
		var other ast.Node = t.Left
		if lNil {
			other = t.Right
		}
		x, err := w.expr(other)
		if err != nil {
			return "", err
		}
		if t.Operator == "==" {
			return "(" + x + " IS NULL)", nil
		}
		return "(" + x + " IS NOT NULL)", nil
	}

	l, err := w.expr(t.Left)
	if err != nil {
		return "", err
	}

	switch t.Operator {
	case "&&", "and":
		r, err := w.expr(t.Right)
		if err != nil {
			return "", err
		}
		return "(" + l + " AND " + r + ")", nil
	case "||", "or":
		r, err := w.expr(t.Right)
		if err != nil {
			return "", err
		}
		return "(" + l + " OR " + r + ")", nil
	case "??":
		r, err := w.expr(t.Right)
		if err != nil {
			return "", err
		}
		return "COALESCE(" + l + ", " + r + ")", nil
	case "in", "not in":
		return w.in(t, l)
	}

	if op, ok := sqlCmp[t.Operator]; ok {
		r, err := w.expr(t.Right)
		if err != nil {
			return "", err
		}
		return "(" + l + " " + op + " " + r + ")", nil
	}
	switch t.Operator {
	case "+", "-", "*":
		r, err := w.expr(t.Right)
		if err != nil {
			return "", err
		}
		return "(" + l + " " + t.Operator + " " + r + ")", nil
	}
	// 拒绝：/ % ** .. matches contains startsWith endsWith 及未列出的运算符——
	// 整数除法语义、取模函数名、LIKE 大小写规则、正则引擎均随方言漂移。
	return w.fail("不支持的运算符 %q", t.Operator)
}

func (w *sqlWriter) in(t *ast.BinaryNode, l string) (string, error) {
	arr, ok := t.Right.(*ast.ArrayNode)
	if !ok {
		return w.fail("in 仅支持字面量列表")
	}
	if len(arr.Nodes) == 0 {
		if t.Operator == "in" {
			return "(1 = 0)", nil // 空列表恒假
		}
		return "(1 = 1)", nil // not in 空列表恒真
	}
	var parts []string
	for _, e := range arr.Nodes {
		s, err := w.expr(e)
		if err != nil {
			return "", err
		}
		parts = append(parts, s)
	}
	kw := " IN ("
	if t.Operator == "not in" {
		kw = " NOT IN ("
	}
	return "(" + l + kw + strings.Join(parts, ", ") + "))", nil
}

// calleeName 提取函数名（field/var 等自定义函数走 CallNode）。
func calleeName(n ast.Node) string {
	if id, ok := n.(*ast.IdentifierNode); ok {
		return id.Value
	}
	return ""
}

func (w *sqlWriter) call(t *ast.CallNode) (string, error) {
	name := calleeName(t.Callee)
	switch name {
	case "field":
		if len(t.Arguments) != 1 {
			return w.fail("field() 需要 1 个参数")
		}
		s, ok := t.Arguments[0].(*ast.StringNode)
		if !ok {
			return w.fail("field() 参数必须是字符串字面量")
		}
		return w.ident(s.Value)
	case "var":
		return w.fail("var() 是运行时变量，不能下推")
	}
	return w.funcCall(name, t.Arguments)
}

func (w *sqlWriter) builtin(t *ast.BuiltinNode) (string, error) {
	return w.funcCall(t.Name, t.Arguments)
}

func (w *sqlWriter) funcCall(name string, args []ast.Node) (string, error) {
	switch name {
	case "isNull":
		if len(args) != 1 {
			return w.fail("isNull() 需要 1 个参数")
		}
		x, err := w.expr(args[0])
		if err != nil {
			return "", err
		}
		return "(" + x + " IS NULL)", nil
	case "now":
		if len(args) != 0 {
			return w.fail("now() 不接受参数")
		}
		return "CURRENT_TIMESTAMP", nil
	case "today":
		if len(args) != 0 {
			return w.fail("today() 不接受参数")
		}
		return "CURRENT_DATE", nil
	}
	sql, ok := sqlFuncs[name]
	if !ok {
		return w.fail("函数 %q 不可下推", name)
	}
	var parts []string
	for _, a := range args {
		s, err := w.expr(a)
		if err != nil {
			return "", err
		}
		parts = append(parts, s)
	}
	return sql + "(" + strings.Join(parts, ", ") + ")", nil
}
