// Package expr 是数据集成的表达式语言：基于 expr-lang/expr（纯 Go、沙箱、编译期类型检查），
// 叠加函数目录（释义/示例）、精确小数运算符重载与字段/变量环境。
//
// 用法：
//
//	p, err := expr.Compile(`round(price * qty, 2)`, expr.Env{Schema: in, Vars: kinds})
//	ev := p.NewEvaluator(vars)   // 每个 goroutine 一个
//	v, err := ev.Eval(row)
package expr

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/vm"
	"github.com/shopspring/decimal"

	"mooncell/console/internal/etl/core"
)

// Env 描述编译环境：当前行字段与可见变量。
type Env struct {
	Schema *core.Schema
	Vars   map[string]core.Kind
}

// Program 是编译后的表达式。并发安全（求值请用 Evaluator）。
type Program struct {
	Source string
	Kind   core.Kind // 推导出的结果类型；"" = 无法静态推导（如 coalesce / field()）
	prog   *vm.Program
	// 直接暴露为标识符的字段：env key → 行下标
	direct []directField
	schema *core.Schema
}

type directField struct {
	key string
	idx int
}

// identRe 是 expr 合法标识符（含中文字母）。
var identRe = regexp.MustCompile(`^[\p{L}_][\p{L}\p{N}_]*$`)

// CompileError 携带位置，前端据此标红。
type CompileError struct {
	Message string `json:"message"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
}

func (e *CompileError) Error() string { return e.Message }

// Compile 编译并类型检查表达式。
func Compile(src string, env Env) (*Program, error) {
	if strings.TrimSpace(src) == "" {
		return nil, &CompileError{Message: "表达式为空"}
	}
	schema := env.Schema
	if schema == nil {
		schema = core.EmptySchema
	}
	reserved := reservedNames()
	typed := map[string]any{}
	p := &Program{Source: src, schema: schema}
	for i, f := range schema.Fields() {
		if !identRe.MatchString(f.Name) || reserved[f.Name] {
			continue // 只能经 field("…") 访问
		}
		typed[f.Name] = zeroOf(f.Kind)
		p.direct = append(p.direct, directField{key: f.Name, idx: i})
	}
	// field()/var() 是运行期绑定到行/变量的闭包；编译期只需签名。
	typed["field"] = func(string) any { return nil }
	typed["var"] = func(string) any { return nil }

	opts := []expr.Option{expr.Env(typed), expr.DisableAllBuiltins()}
	for _, d := range functions() {
		if d.impl == nil {
			continue
		}
		opts = append(opts, expr.Function(d.doc.Name, d.impl, d.types...))
	}
	opts = append(opts, decimalOptions()...)

	prog, err := expr.Compile(src, opts...)
	if err != nil {
		return nil, toCompileError(err)
	}
	p.prog = prog
	p.Kind = kindOfType(prog.Node().Type())
	if p.Kind == core.KindTimestamp {
		p.Kind = dateResult(prog.Node(), p.Kind)
	}
	if err := checkNames(src, schema, env.Vars); err != nil {
		return nil, err
	}
	return p, nil
}

// dateFns 是语义上返回日期（丢弃时分秒）的函数。
var dateFns = map[string]bool{"today": true, "toDate": true, "truncDay": true}

// dateResult 整个表达式就是 date 函数调用时返回 KindDate（库只认 time.Time，推导为 timestamp）。
func dateResult(n ast.Node, kind core.Kind) core.Kind {
	call, ok := n.(*ast.CallNode)
	if !ok {
		return kind
	}
	if id, ok := call.Callee.(*ast.IdentifierNode); ok && dateFns[id.Value] {
		return core.KindDate
	}
	return kind
}

// checkNames 静态检查 field("x") / var("x") 的字面量参数是否存在，尽早暴露拼写错误。
var refCallRe = regexp.MustCompile(`\b(field|var)\(\s*"([^"]*)"\s*\)`)

func checkNames(src string, schema *core.Schema, vars map[string]core.Kind) error {
	for _, m := range refCallRe.FindAllStringSubmatch(src, -1) {
		switch m[1] {
		case "field":
			if schema.Index(m[2]) < 0 {
				return &CompileError{Message: fmt.Sprintf("字段 %q 不存在", m[2])}
			}
		case "var":
			if vars != nil {
				if _, ok := vars[m[2]]; !ok {
					return &CompileError{Message: fmt.Sprintf("变量 %q 未声明", m[2])}
				}
			}
		}
	}
	return nil
}

func toCompileError(err error) error {
	msg := err.Error()
	ce := &CompileError{Message: translate(msg)}
	// expr 的错误文本形如 "unknown name foo (1:5)\n | foo + 1\n | ....^"
	if i := strings.Index(msg, " ("); i > 0 {
		var line, col int
		if _, scanErr := fmt.Sscanf(msg[i:], " (%d:%d)", &line, &col); scanErr == nil {
			ce.Line, ce.Column = line, col
			ce.Message = translate(msg[:i])
		}
	}
	return ce
}

// translate 把 expr 常见英文错误转成中文提示；无法识别的保持原文。
func translate(msg string) string {
	first := strings.SplitN(msg, "\n", 2)[0]
	switch {
	case strings.HasPrefix(first, "unknown name "):
		return "未知的字段或函数：" + strings.TrimPrefix(first, "unknown name ") + "（字段名含特殊字符或与函数同名时请用 field(\"名\")）"
	case strings.HasPrefix(first, "invalid operation"):
		return "类型不匹配：" + first
	case strings.Contains(first, "not enough arguments") || strings.Contains(first, "too many arguments"):
		return "参数个数不对：" + first
	case strings.HasPrefix(first, "unexpected token"):
		return "语法错误：" + first
	}
	return first
}

func zeroOf(k core.Kind) any {
	switch k {
	case core.KindString:
		return ""
	case core.KindInteger:
		return int64(0)
	case core.KindNumber:
		return 0.0
	case core.KindDecimal:
		return decimal.Decimal{}
	case core.KindBoolean:
		return false
	case core.KindDate, core.KindTimestamp:
		return time.Time{}
	case core.KindBinary:
		return []byte(nil)
	}
	return nil
}

var (
	tTime    = reflect.TypeOf(time.Time{})
	tDecimal = reflect.TypeOf(decimal.Decimal{})
	tBytes   = reflect.TypeOf([]byte(nil))
)

func kindOfType(t reflect.Type) core.Kind {
	if t == nil {
		return ""
	}
	switch t {
	case tTime:
		return core.KindTimestamp
	case tDecimal:
		return core.KindDecimal
	case tBytes:
		return core.KindBinary
	}
	switch t.Kind() {
	case reflect.String:
		return core.KindString
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint8, reflect.Uint16, reflect.Uint32:
		return core.KindInteger
	case reflect.Float32, reflect.Float64:
		return core.KindNumber
	case reflect.Bool:
		return core.KindBoolean
	}
	return ""
}

func funcSig(a, b, ret any) any {
	ft := reflect.FuncOf([]reflect.Type{reflect.TypeOf(a), reflect.TypeOf(b)}, []reflect.Type{reflect.TypeOf(ret)}, false)
	return reflect.New(ft).Interface()
}

// Evaluator 对行求值，复用环境 map 以避免逐行分配。非并发安全：每个 goroutine 一个。
type Evaluator struct {
	p    *Program
	env  map[string]any
	row  core.Row
	vars *core.Vars
}

// NewEvaluator 创建求值器；vars 可为 nil。
func (p *Program) NewEvaluator(vars *core.Vars) *Evaluator {
	e := &Evaluator{p: p, env: make(map[string]any, len(p.direct)+2), vars: vars}
	e.env["field"] = func(name string) any {
		if i := p.schema.Index(name); i >= 0 && i < len(e.row) {
			return e.row[i]
		}
		return nil
	}
	e.env["var"] = func(name string) any {
		if e.vars == nil {
			return nil
		}
		v, _ := e.vars.Get(name)
		return v.V
	}
	return e
}

// ErrEval 包装运行期求值错误（调用方通常转为 core.RowError）。
var ErrEval = errors.New("表达式求值失败")

// Eval 对一行求值，结果已规整为逻辑类型的规范表示。
func (e *Evaluator) Eval(row core.Row) (any, error) {
	e.row = row
	for _, d := range e.p.direct {
		if d.idx < len(row) {
			e.env[d.key] = row[d.idx]
		} else {
			e.env[d.key] = nil
		}
	}
	out, err := vm.Run(e.p.prog, e.env)
	if err != nil {
		return nil, fmt.Errorf("%w：%s", ErrEval, translate(err.Error()))
	}
	return core.Normalize(out), nil
}
