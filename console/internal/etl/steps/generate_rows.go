package steps

import (
	"context"
	"fmt"

	"mooncell/console/internal/etl/core"
)

// GenerateRowsConfig 是「生成记录」配置：固定字段值重复 Limit 行，可选自增序号字段。
type GenerateRowsConfig struct {
	Limit         int64           `json:"limit"`
	SequenceField string          `json:"sequenceField"` // 非空则追加 1..Limit 的整数字段
	Fields        []ConstantField `json:"fields"`
}

// ConstantField 是常量字段：Value 为文本，按 Kind 转换（Format 用于日期文本）。
type ConstantField struct {
	Name   string    `json:"name"`
	Kind   core.Kind `json:"kind"`
	Value  *string   `json:"value"` // nil = NULL
	Format string    `json:"format"`
}

func init() {
	core.Steps.Register(core.Define(core.Definition[GenerateRowsConfig]{
		Meta: core.Meta{
			ID: "generate_rows", Label: "生成记录", Category: core.CategoryInput, Icon: "sparkles",
			Description: "生成指定行数的常量记录，可带自增序号。常用于测试、补数或驱动后续步骤。",
			Outputs:     []core.PortDecl{{Name: core.PortMain, Label: "输出"}},
			Keywords:    []string{"Generate rows", "常量", "测试数据"},
		},
		Defaults: func() GenerateRowsConfig { return GenerateRowsConfig{Limit: 10} },
		Infer: func(ic *core.InferContext, c *GenerateRowsConfig) error {
			if c.Limit < 0 {
				ic.Errorf("limit", "limit", "行数不能为负")
			}
			s, _, err := constantSchema(c)
			if err != nil {
				ic.Errorf("fields", "fields", "%s", err.Error())
				return nil
			}
			ic.SetOutput(core.PortMain, s)
			return nil
		},
		Open: func(rc *core.RunContext, c *GenerateRowsConfig) (core.Runner, error) {
			_, values, err := constantSchema(c)
			if err != nil {
				return nil, err
			}
			return core.RunnerFunc(func(ctx context.Context) error {
				em := rc.Emitter(core.PortMain)
				for i := int64(1); i <= c.Limit; i++ {
					row := make(core.Row, len(values), len(values)+1)
					copy(row, values)
					if c.SequenceField != "" {
						row = append(row, i)
					}
					rc.Metrics.Input.Add(1)
					if err := em.Emit(ctx, row); err != nil {
						return err
					}
				}
				return em.Flush(ctx)
			}), nil
		},
	}))
}

func constantSchema(c *GenerateRowsConfig) (*core.Schema, []any, error) {
	fields := make([]core.Field, 0, len(c.Fields)+1)
	values := make([]any, 0, len(c.Fields))
	for _, f := range c.Fields {
		if !f.Kind.Valid() {
			return nil, nil, fmt.Errorf("字段 %q 的类型无效", f.Name)
		}
		fields = append(fields, core.NewField(f.Name, f.Kind))
		v, err := constantValue(f)
		if err != nil {
			return nil, nil, err
		}
		values = append(values, v)
	}
	if c.SequenceField != "" {
		fields = append(fields, core.NewField(c.SequenceField, core.KindInteger))
	}
	s, err := core.NewSchema(fields...)
	return s, values, err
}

func constantValue(f ConstantField) (any, error) {
	if f.Value == nil {
		return nil, nil
	}
	v, err := core.Convert(*f.Value, f.Kind, core.ConvertOptions{Layout: goLayout(f.Format)})
	if err != nil {
		return nil, fmt.Errorf("字段 %q 的值无效：%w", f.Name, err)
	}
	return v, nil
}
