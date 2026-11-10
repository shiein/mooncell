// Package actions 是内置作业项：开始、运行转换、执行 SQL、SQL 取值、条件、设置变量、等待、成功、中止。
package actions

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/expr"
	"mooncell/console/internal/etl/sqltext"
	"mooncell/console/internal/etl/workflow"
)

// ---------------------------------------------------------------- 宿主接口

// PipelineRunner 由 service 实现：在作业中运行一个转换（步骤度量与日志归入同一运行记录）。
type PipelineRunner interface {
	PipelineParams(ctx context.Context, pipelineID string) (name string, params []core.ParamDecl, err error)
	RunPipeline(ctx context.Context, ec *workflow.ExecContext, pipelineID string, params map[string]core.Value) (PipelineSummary, error)
}

// PipelineSummary 是转换运行摘要。
type PipelineSummary struct {
	Read, Written, Rejected int64
}

type runnerKey struct{}

// RunnerEntry 返回注入 Env 的键值。
func RunnerEntry(r PipelineRunner) (any, any) { return runnerKey{}, r }

func runnerFrom(env core.Env) (PipelineRunner, error) {
	if env != nil {
		if r, ok := env.Lookup(runnerKey{}).(PipelineRunner); ok && r != nil {
			return r, nil
		}
	}
	return nil, errors.New("运行环境未注入转换执行器")
}

// ---------------------------------------------------------------- 变量表达式

// varProgram 编译以变量为「字段」的表达式：作业里可直接写 cnt > 1000，而不必 var("cnt")。
type varProgram struct {
	prog  *expr.Program
	names []string
}

func varSchema(kinds map[string]core.Kind) (*core.Schema, []string) {
	names := make([]string, 0, len(kinds))
	for n := range kinds {
		names = append(names, n)
	}
	sort.Strings(names)
	fields := make([]core.Field, len(names))
	for i, n := range names {
		fields[i] = core.NewField(n, kinds[n])
	}
	return core.MustSchema(fields...), names
}

func compileVarExpr(src string, kinds map[string]core.Kind) (*varProgram, error) {
	s, names := varSchema(kinds)
	p, err := expr.Compile(src, expr.Env{Schema: s, Vars: kinds})
	if err != nil {
		return nil, err
	}
	return &varProgram{prog: p, names: names}, nil
}

func (vp *varProgram) eval(vars *core.Vars) (any, error) {
	row := make(core.Row, len(vp.names))
	for i, n := range vp.names {
		v, _ := vars.Get(n)
		row[i] = v.V
	}
	return vp.prog.NewEvaluator(vars).Eval(row)
}

func kindsFromVars(vars *core.Vars) map[string]core.Kind {
	m := map[string]core.Kind{}
	for k, v := range vars.Flatten() {
		m[k] = v.Kind
	}
	return m
}

// ---------------------------------------------------------------- SQL 辅助

func handle(ctx context.Context, env core.Env, id string) (*conn.Handle, error) {
	pool, err := conn.PoolFrom(env)
	if err != nil {
		return nil, err
	}
	return pool.Get(ctx, id)
}

func renderVars(h *conn.Handle, sql string, vars *core.Vars) (string, []any, error) {
	text, args, err := sqltext.Render(sql, h.Dialect.Lexer(), h.Dialect.Placeholder, func(n string) (any, bool) {
		v, ok := vars.Get(n)
		return v.V, ok
	})
	if err != nil {
		return "", nil, err
	}
	for i, a := range args {
		if args[i], err = h.Dialect.Bind(a, core.Field{Kind: core.KindOf(a)}); err != nil {
			return "", nil, err
		}
	}
	return text, args, nil
}

func validateSQL(vc *workflow.ValidateContext, connID, sql string) {
	if strings.TrimSpace(connID) == "" {
		vc.Errorf("connection", "connection_required", "请选择数据库连接")
	}
	if strings.TrimSpace(sql) == "" {
		vc.Errorf("sql", "sql_required", "请填写 SQL")
		return
	}
	refs, err := sqltext.Scan(sql, sqltext.Options{})
	if err != nil {
		vc.Errorf("sql", "sql_vars", "%s", err.Error())
		return
	}
	for _, r := range refs {
		if _, ok := vc.VarKinds[r.Name]; !ok {
			vc.Errorf("sql", "var_undeclared", "SQL 引用了未定义的变量 %q", r.Name)
		}
	}
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}

var (
	successPort = []core.PortDecl{{Name: core.PortSuccess, Label: "成功", Tone: "success"}, {Name: core.PortFailure, Label: "失败", Tone: "error"}}
)

// ---------------------------------------------------------------- 注册

func init() {
	// 开始
	workflow.Actions.Register(workflow.DefineAction(workflow.ActionDefinition[struct{}]{
		Meta: core.Meta{ID: "start", Label: "开始", Icon: "play", Description: "作业的入口。每个作业有且仅有一个；定时调度从这里触发。",
			Outputs: []core.PortDecl{{Name: core.PortSuccess, Label: "开始", Tone: "success"}}, Keywords: []string{"Start"}},
		Execute: func(ctx context.Context, ec *workflow.ExecContext, _ *struct{}) (workflow.Outcome, error) {
			return workflow.Outcome{}, nil
		},
	}))

	// 运行转换
	workflow.Actions.Register(workflow.DefineAction(workflow.ActionDefinition[PipelineConfig]{
		Meta: core.Meta{ID: "pipeline", Order: 30, Label: "运行转换", Icon: "pipeline", Description: "运行一个转换，可把作业变量映射为转换参数（表达式）。转换的步骤度量与日志汇入本次作业运行。",
			Inputs: []core.PortDecl{{Name: core.PortMain, Label: "输入"}}, Outputs: successPort, Keywords: []string{"Transformation", "转换"}},
		Defaults: func() PipelineConfig { return PipelineConfig{Params: map[string]string{}} },
		Refs:     func(c *PipelineConfig) []workflow.DefRef { return []workflow.DefRef{{ID: c.Pipeline, Kind: "pipeline"}} },
		Validate: func(vc *workflow.ValidateContext, c *PipelineConfig) error {
			if c.Pipeline == "" {
				vc.Errorf("pipeline", "pipeline_required", "请选择要运行的转换")
				return nil
			}
			r, err := runnerFrom(vc.Env)
			if err != nil {
				return nil // 无宿主（单元测试）时跳过跨定义校验
			}
			name, params, err := r.PipelineParams(vc.Ctx, c.Pipeline)
			if err != nil {
				vc.Errorf("pipeline", "pipeline_missing", "转换不存在或无法读取：%s", err.Error())
				return nil
			}
			declared := map[string]core.Kind{}
			for _, p := range params {
				declared[p.Name] = p.Kind
				if _, ok := c.Params[p.Name]; !ok && p.Required && p.Default == nil {
					vc.Errorf("params."+p.Name, "param_required", "转换「%s」的必填参数 %s 未赋值", name, p.Name)
				}
			}
			for k, src := range c.Params {
				want, ok := declared[k]
				if !ok {
					vc.Errorf("params."+k, "param_unknown", "转换「%s」没有参数 %s", name, k)
					continue
				}
				vp, err := compileVarExpr(src, vc.VarKinds)
				if err != nil {
					vc.Errorf("params."+k, "expr", "参数 %s：%s", k, err.Error())
					continue
				}
				if got := vp.prog.Kind; got != "" && got != want && !(got.IsNumeric() && want.IsNumeric()) && !(got.IsTemporal() && want.IsTemporal()) {
					vc.Warnf("params."+k, "param_kind", "参数 %s 需要%s，表达式结果为%s，运行时将尝试转换", k, want.Label(), got.Label())
				}
			}
			return nil
		},
		Execute: func(ctx context.Context, ec *workflow.ExecContext, c *PipelineConfig) (workflow.Outcome, error) {
			r, err := runnerFrom(ec.Env)
			if err != nil {
				return workflow.Outcome{}, err
			}
			kinds := kindsFromVars(ec.Vars)
			params := map[string]core.Value{}
			for k, src := range c.Params {
				vp, err := compileVarExpr(src, kinds)
				if err != nil {
					return workflow.Outcome{}, fmt.Errorf("参数 %s：%w", k, err)
				}
				v, err := vp.eval(ec.Vars)
				if err != nil {
					return workflow.Outcome{}, fmt.Errorf("参数 %s：%w", k, err)
				}
				params[k] = core.NewValue(v)
			}
			sum, err := r.RunPipeline(ctx, ec, c.Pipeline, params)
			if err != nil {
				return workflow.Outcome{}, err
			}
			return workflow.Outcome{Message: fmt.Sprintf("转换完成：读 %d 行，写 %d 行，拒绝 %d 行", sum.Read, sum.Written, sum.Rejected)}, nil
		},
	}))

	// 执行 SQL
	workflow.Actions.Register(workflow.DefineAction(workflow.ActionDefinition[SQLConfig]{
		Meta: core.Meta{ID: "sql", Order: 40, Label: "执行 SQL", Icon: "terminal", Description: "在指定连接上执行一条 SQL（DDL / DML / 存储过程）。可把影响行数写入变量。",
			Inputs: []core.PortDecl{{Name: core.PortMain, Label: "输入"}}, Outputs: successPort, Keywords: []string{"SQL", "Execute SQL script"}},
		Outputs: func(c *SQLConfig) map[string]core.Kind {
			if c.AffectedVar == "" {
				return nil
			}
			return map[string]core.Kind{c.AffectedVar: core.KindInteger}
		},
		Validate: func(vc *workflow.ValidateContext, c *SQLConfig) error {
			validateSQL(vc, c.Connection, c.SQL)
			return nil
		},
		Execute: func(ctx context.Context, ec *workflow.ExecContext, c *SQLConfig) (workflow.Outcome, error) {
			h, err := handle(ctx, ec.Env, c.Connection)
			if err != nil {
				return workflow.Outcome{}, err
			}
			text, args, err := renderVars(h, c.SQL, ec.Vars)
			if err != nil {
				return workflow.Outcome{}, err
			}
			ec.Log.Detail("执行：%s", oneLine(text))
			res, err := h.DB.ExecContext(ctx, text, args...)
			if err != nil {
				return workflow.Outcome{}, err
			}
			n, _ := res.RowsAffected()
			out := workflow.Outcome{Message: fmt.Sprintf("执行完成，影响 %d 行", n)}
			if c.AffectedVar != "" {
				out.Vars = map[string]core.Value{c.AffectedVar: core.NewValue(n)}
			}
			return out, nil
		},
	}))

	// SQL 取值
	workflow.Actions.Register(workflow.DefineAction(workflow.ActionDefinition[SQLValueConfig]{
		Meta: core.Meta{ID: "sql_value", Order: 10, Label: "SQL 取值", Icon: "variable", Description: "执行查询，把第一行的列写入作业变量（如 select max(updated_at) as maxTs）。后续作业项可直接引用。",
			Inputs: []core.PortDecl{{Name: core.PortMain, Label: "输入"}}, Outputs: successPort, Keywords: []string{"变量", "取值", "max", "count"}},
		Defaults: func() SQLValueConfig { return SQLValueConfig{OnEmpty: "null"} },
		Outputs: func(c *SQLValueConfig) map[string]core.Kind {
			m := map[string]core.Kind{}
			for _, a := range c.Assign {
				if a.Var != "" && a.Kind.Valid() {
					m[a.Var] = a.Kind
				}
			}
			return m
		},
		Validate: func(vc *workflow.ValidateContext, c *SQLValueConfig) error {
			validateSQL(vc, c.Connection, c.SQL)
			if len(c.Assign) == 0 {
				vc.Errorf("assign", "assign_required", "至少需要一个「列 → 变量」赋值")
			}
			for i, a := range c.Assign {
				if !core.ValidVarName(a.Var) {
					vc.Errorf(fmt.Sprintf("assign[%d].var", i), "var_name", "变量名 %q 不合法", a.Var)
				}
				if !a.Kind.Valid() {
					vc.Errorf(fmt.Sprintf("assign[%d].kind", i), "kind", "请为变量 %s 选择类型", a.Var)
				}
			}
			if c.OnEmpty != "null" && c.OnEmpty != "fail" {
				vc.Errorf("onEmpty", "on_empty", "无结果时的处理方式无效")
			}
			return nil
		},
		Execute: func(ctx context.Context, ec *workflow.ExecContext, c *SQLValueConfig) (workflow.Outcome, error) {
			h, err := handle(ctx, ec.Env, c.Connection)
			if err != nil {
				return workflow.Outcome{}, err
			}
			text, args, err := renderVars(h, c.SQL, ec.Vars)
			if err != nil {
				return workflow.Outcome{}, err
			}
			ec.Log.Detail("查询：%s", oneLine(text))
			rows, err := h.DB.QueryContext(ctx, text, args...)
			if err != nil {
				return workflow.Outcome{}, err
			}
			defer rows.Close()
			schema, err := conn.SchemaFromRows(h.Dialect, rows)
			if err != nil {
				return workflow.Outcome{}, err
			}
			reader := conn.NewRowReader(h, rows, schema)
			row, ok, err := reader.Next()
			if err != nil {
				return workflow.Outcome{}, err
			}
			if !ok && c.OnEmpty == "fail" {
				return workflow.Outcome{}, errors.New("查询没有返回任何行")
			}
			vars := map[string]core.Value{}
			var parts []string
			for _, a := range c.Assign {
				var v any
				if ok {
					idx := schema.Index(a.Column)
					if idx < 0 { // 列名大小写不敏感兜底（达梦/Oracle 风格返回大写列名）
						for i, n := range schema.Names() {
							if strings.EqualFold(n, a.Column) {
								idx = i
								break
							}
						}
					}
					if idx < 0 {
						return workflow.Outcome{}, fmt.Errorf("查询结果中没有列 %q（实际列：%s）", a.Column, strings.Join(schema.Names(), ", "))
					}
					if v, err = core.Convert(row[idx], a.Kind, core.ConvertOptions{Location: h.Location()}); err != nil {
						return workflow.Outcome{}, fmt.Errorf("列 %s → 变量 %s：%w", a.Column, a.Var, err)
					}
				}
				vars[a.Var] = core.Value{Kind: a.Kind, V: v}
				parts = append(parts, fmt.Sprintf("%s = %s", a.Var, core.FormatValue(v)))
			}
			return workflow.Outcome{Vars: vars, Message: strings.Join(parts, "，")}, nil
		},
	}))

	// 条件
	workflow.Actions.Register(workflow.DefineAction(workflow.ActionDefinition[ConditionConfig]{
		Meta: core.Meta{ID: "condition", Order: 20, Label: "条件", Icon: "branch", Description: "对作业变量求布尔表达式（如 cnt > 1000），按结果走「是 / 否」分支。",
			Inputs: []core.PortDecl{{Name: core.PortMain, Label: "输入"}},
			Outputs: []core.PortDecl{{Name: core.PortTrue, Label: "是", Tone: "success"}, {Name: core.PortFalse, Label: "否", Tone: "warn"},
				{Name: core.PortFailure, Label: "出错", Tone: "error"}}, Keywords: []string{"Simple evaluation", "判断", "if"}},
		Validate: func(vc *workflow.ValidateContext, c *ConditionConfig) error {
			vp, err := compileVarExpr(c.Expr, vc.VarKinds)
			if err != nil {
				vc.Errorf("expr", "expr", "%s", err.Error())
			} else if vp.prog.Kind != "" && vp.prog.Kind != core.KindBoolean {
				vc.Errorf("expr", "not_bool", "条件必须返回布尔值，实际为%s", vp.prog.Kind.Label())
			}
			return nil
		},
		Execute: func(ctx context.Context, ec *workflow.ExecContext, c *ConditionConfig) (workflow.Outcome, error) {
			vp, err := compileVarExpr(c.Expr, kindsFromVars(ec.Vars))
			if err != nil {
				return workflow.Outcome{}, err
			}
			v, err := vp.eval(ec.Vars)
			if err != nil {
				return workflow.Outcome{}, err
			}
			b, ok := v.(bool)
			if !ok && v != nil {
				return workflow.Outcome{}, fmt.Errorf("条件结果不是布尔值：%v", v)
			}
			if b {
				return workflow.Outcome{Port: core.PortTrue, Message: c.Expr + " → 是"}, nil
			}
			return workflow.Outcome{Port: core.PortFalse, Message: c.Expr + " → 否"}, nil
		},
	}))

	// 设置变量
	workflow.Actions.Register(workflow.DefineAction(workflow.ActionDefinition[SetVarsConfig]{
		Meta: core.Meta{ID: "set_vars", Order: 50, Label: "设置变量", Icon: "variable", Description: "用表达式计算并设置作业变量，例如 addDays(today(), -1)。",
			Inputs: []core.PortDecl{{Name: core.PortMain, Label: "输入"}}, Outputs: successPort, Keywords: []string{"Set variables"}},
		Outputs: func(c *SetVarsConfig) map[string]core.Kind {
			m := map[string]core.Kind{}
			for _, a := range c.Assign {
				if a.Var != "" && a.Kind.Valid() {
					m[a.Var] = a.Kind
				}
			}
			return m
		},
		Validate: func(vc *workflow.ValidateContext, c *SetVarsConfig) error {
			for i, a := range c.Assign {
				if !core.ValidVarName(a.Var) || !a.Kind.Valid() {
					vc.Errorf(fmt.Sprintf("assign[%d]", i), "assign", "变量 %q 的名称或类型无效", a.Var)
					continue
				}
				if _, err := compileVarExpr(a.Expr, vc.VarKinds); err != nil {
					vc.Errorf(fmt.Sprintf("assign[%d].expr", i), "expr", "%s：%s", a.Var, err.Error())
				}
			}
			return nil
		},
		Execute: func(ctx context.Context, ec *workflow.ExecContext, c *SetVarsConfig) (workflow.Outcome, error) {
			vars := map[string]core.Value{}
			var parts []string
			for _, a := range c.Assign {
				vp, err := compileVarExpr(a.Expr, kindsFromVars(ec.Vars))
				if err != nil {
					return workflow.Outcome{}, err
				}
				v, err := vp.eval(ec.Vars)
				if err != nil {
					return workflow.Outcome{}, fmt.Errorf("%s：%w", a.Var, err)
				}
				if v, err = core.Convert(v, a.Kind, core.ConvertOptions{}); err != nil {
					return workflow.Outcome{}, fmt.Errorf("%s：%w", a.Var, err)
				}
				vars[a.Var] = core.Value{Kind: a.Kind, V: v}
				ec.Vars.Set(a.Var, vars[a.Var]) // 同一作业项内后面的赋值可引用前面的
				parts = append(parts, fmt.Sprintf("%s = %s", a.Var, core.FormatValue(v)))
			}
			return workflow.Outcome{Vars: vars, Message: strings.Join(parts, "，")}, nil
		},
	}))

	// 等待
	workflow.Actions.Register(workflow.DefineAction(workflow.ActionDefinition[WaitConfig]{
		Meta: core.Meta{ID: "wait", Order: 60, Label: "等待", Icon: "clock", Description: "等待指定秒数后继续。",
			Inputs: []core.PortDecl{{Name: core.PortMain, Label: "输入"}}, Outputs: []core.PortDecl{{Name: core.PortSuccess, Label: "继续", Tone: "success"}}, Keywords: []string{"Wait", "sleep"}},
		Defaults: func() WaitConfig { return WaitConfig{Seconds: 10} },
		Validate: func(vc *workflow.ValidateContext, c *WaitConfig) error {
			if c.Seconds < 0 || c.Seconds > 86400 {
				vc.Errorf("seconds", "seconds", "等待时间需在 0–86400 秒之间")
			}
			return nil
		},
		Execute: func(ctx context.Context, ec *workflow.ExecContext, c *WaitConfig) (workflow.Outcome, error) {
			select {
			case <-time.After(time.Duration(c.Seconds) * time.Second):
				return workflow.Outcome{}, nil
			case <-ctx.Done():
				return workflow.Outcome{}, ctx.Err()
			}
		},
	}))

	// 成功
	workflow.Actions.Register(workflow.DefineAction(workflow.ActionDefinition[struct{}]{
		Meta: core.Meta{ID: "success", Order: 80, Label: "成功", Icon: "check", Description: "标记分支正常结束。",
			Inputs: []core.PortDecl{{Name: core.PortMain, Label: "输入"}}, Keywords: []string{"Success"}},
		Execute: func(ctx context.Context, ec *workflow.ExecContext, _ *struct{}) (workflow.Outcome, error) {
			return workflow.Outcome{}, nil
		},
	}))

	// 中止
	workflow.Actions.Register(workflow.DefineAction(workflow.ActionDefinition[AbortConfig]{
		Meta: core.Meta{ID: "abort", Order: 90, Label: "中止", Icon: "stop", Description: "以失败状态终止整个作业，并记录原因（可引用变量：${cnt}）。",
			Inputs: []core.PortDecl{{Name: core.PortMain, Label: "输入"}}, Keywords: []string{"Abort", "失败"}},
		Execute: func(ctx context.Context, ec *workflow.ExecContext, c *AbortConfig) (workflow.Outcome, error) {
			msg := c.Message
			for _, n := range ec.Vars.Names() {
				v, _ := ec.Vars.Get(n)
				msg = strings.ReplaceAll(msg, "${"+n+"}", core.FormatValue(v.V))
			}
			if msg == "" {
				return workflow.Outcome{}, workflow.ErrAbort
			}
			return workflow.Outcome{}, fmt.Errorf("%w：%s", workflow.ErrAbort, msg)
		},
	}))

	// 规划中（面板可见、置灰）
	for _, m := range []core.Meta{
		{ID: "http", Label: "HTTP 请求", Icon: "globe", Description: "调用 HTTP 接口，把响应中的值写入变量。"},
		{ID: "workflow", Label: "子作业", Icon: "workflow", Description: "运行另一个作业。"},
		{ID: "notify", Label: "通知", Icon: "bell", Description: "发送运行结果通知（邮件 / Webhook）。"},
	} {
		m.Maturity = core.MaturityPlanned
		m.Order = 100
		m.Inputs = []core.PortDecl{{Name: core.PortMain, Label: "输入"}}
		m.Outputs = successPort
		workflow.Actions.Register(workflow.DefineAction(workflow.ActionDefinition[struct{}]{Meta: m}))
	}
}

// ---------------------------------------------------------------- 配置结构

// PipelineConfig：运行转换。Params 的值是对作业变量求值的表达式。
type PipelineConfig struct {
	Pipeline string            `json:"pipeline"`
	Params   map[string]string `json:"params"`
}

// SQLConfig：执行 SQL。
type SQLConfig struct {
	Connection  string `json:"connection"`
	SQL         string `json:"sql"`
	AffectedVar string `json:"affectedVar"`
}

// SQLValueConfig：SQL 取值。
type SQLValueConfig struct {
	Connection string      `json:"connection"`
	SQL        string      `json:"sql"`
	Assign     []ColAssign `json:"assign"`
	OnEmpty    string      `json:"onEmpty"` // null | fail
}

// ColAssign：列 → 变量。
type ColAssign struct {
	Column string    `json:"column"`
	Var    string    `json:"var"`
	Kind   core.Kind `json:"kind"`
}

// ConditionConfig：条件。
type ConditionConfig struct {
	Expr string `json:"expr"`
}

// SetVarsConfig：设置变量。
type SetVarsConfig struct {
	Assign []ExprAssign `json:"assign"`
}

// ExprAssign：表达式 → 变量。
type ExprAssign struct {
	Var  string    `json:"var"`
	Kind core.Kind `json:"kind"`
	Expr string    `json:"expr"`
}

// WaitConfig：等待。
type WaitConfig struct {
	Seconds int `json:"seconds"`
}

// AbortConfig：中止。
type AbortConfig struct {
	Message string `json:"message"`
}
