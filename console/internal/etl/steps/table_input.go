package steps

import (
	"context"
	"fmt"
	"strings"

	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/sqltext"
)

// TableInputConfig 是「表输入」配置。SQL 中 ${var} 为绑定参数，#{var} 为受控标识符替换。
type TableInputConfig struct {
	Connection string `json:"connection"`
	SQL        string `json:"sql"`
	MaxRows    int64  `json:"maxRows"` // 0 = 不限
	// PushArgs 是源端下推（settings.pushdown）写入的追加绑定参数：
	// 占位符编号续接 ${} 参数之后，故在 render 之后追加。人工勿填。
	PushArgs []any `json:"pushArgs,omitempty"`
}

func init() {
	core.Steps.Register(core.Define(core.Definition[TableInputConfig]{
		Meta: core.Meta{
			ID: "table_input", Order: 10, Label: "表输入", Category: core.CategoryInput, Icon: "database",
			Description: "执行 SQL 查询并流式读出结果。用 ${变量} 绑定参数（安全），#{变量} 仅可替换表名/模式名。",
			Outputs:     []core.PortDecl{{Name: core.PortMain, Label: "输出"}},
			Keywords:    []string{"Table input", "SQL", "查询", "select"},
		},
		Defaults: func() TableInputConfig { return TableInputConfig{SQL: "SELECT *\nFROM "} },
		Infer: func(ic *core.InferContext, c *TableInputConfig) error {
			if strings.TrimSpace(c.SQL) == "" {
				ic.Errorf("sql", "sql_required", "请填写 SQL")
				return nil
			}
			pool, h := handleFor(ic, c.Connection, "connection")
			if h == nil {
				return nil
			}
			refs, err := sqltext.Scan(c.SQL, h.Dialect.Lexer())
			if err != nil {
				ic.Errorf("sql", "sql_vars", "%s", err.Error())
				return nil
			}
			for _, r := range refs {
				if _, ok := ic.VarKinds[r.Name]; !ok {
					ic.Errorf("sql", "var_undeclared", "SQL 引用了未声明的变量 %q，请在转换参数中声明", r.Name)
				}
			}
			if ic.Problems().HasErrors() {
				return nil
			}
			text, args, err := render(h.Dialect, c.SQL, samplesResolver(ic))
			if err != nil {
				ic.Errorf("sql", "sql_render", "%s", err.Error())
				return nil
			}
			if args, err = bindExtra(h.Dialect, args, c.PushArgs); err != nil {
				ic.Errorf("sql", "sql_render", "%s", err.Error())
				return nil
			}
			s, err := pool.Probe(ic.Ctx, c.Connection, text, args)
			if err != nil {
				ic.Errorf("sql", "sql_probe", "SQL 无法执行：%s", err.Error())
				return nil
			}
			ic.SetOutput(core.PortMain, withOrigin(s, ic.NodeID))
			return nil
		},
		Open: func(rc *core.RunContext, c *TableInputConfig) (core.Runner, error) {
			return core.RunnerFunc(func(ctx context.Context) error {
				h, err := runHandle(ctx, rc, c.Connection)
				if err != nil {
					return err
				}
				text, args, err := render(h.Dialect, c.SQL, varsResolver(rc.Vars))
				if err != nil {
					return err
				}
				if args, err = bindExtra(h.Dialect, args, c.PushArgs); err != nil {
					return err
				}
				rc.Log.Detail("执行查询：%s", oneLine(text))
				rows, err := h.DB.QueryContext(ctx, text, args...)
				if err != nil {
					return fmt.Errorf("查询失败：%w", err)
				}
				defer rows.Close()
				out := rc.Out(core.PortMain)
				runtime, err := conn.SchemaFromRows(h.Dialect, rows)
				if err != nil {
					return err
				}
				// 设计期与运行期的字段布局必须一致，否则下游按下标取值会错位。
				if out.Schema != nil {
					if err := out.Schema.Compatible(runtime); err != nil {
						return fmt.Errorf("查询结果与设计时不一致（表结构可能已变更，请重新校验）：%w", err)
					}
				}
				reader := conn.NewRowReader(h, rows, runtime)
				em := rc.Emitter(core.PortMain)
				var n int64
				for {
					row, ok, err := reader.Next()
					if err != nil {
						return err
					}
					if !ok {
						break
					}
					rc.Metrics.Input.Add(1)
					if err := em.Emit(ctx, row); err != nil {
						return err
					}
					n++
					if c.MaxRows > 0 && n >= c.MaxRows {
						break
					}
				}
				return em.Flush(ctx)
			}), nil
		},
	}))
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 300 {
		return string(r[:300]) + "…"
	}
	return s
}
