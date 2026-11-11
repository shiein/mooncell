package steps

import (
	"mooncell/console/internal/etl/core"
)

// ValueMapperConfig 是「值映射」：按映射表替换字段值，输出为字符串
//（Kettle 语义：按值的文本形式比较，结果字段一律为 string 类型）。
type ValueMapperConfig struct {
	Field    string     `json:"field"`    // 取值字段
	Target   string     `json:"target"`   // 写入字段名；空 = 覆盖原字段
	Default  string     `json:"default"`  // 未匹配时的值；空 = 保留原值（转字符串）
	Mappings []MapEntry `json:"mappings"` // source → target 文本
}

// MapEntry 是一对映射。
type MapEntry struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

type valueMapperPlan struct {
	src     int
	srcName string
	dst     int // 下标 == len(in.Fields()) 表示追加新字段
	def     string
	hasDef  bool
	mapping map[string]string
	schema  *core.Schema
}

func planValueMapper(in *core.Schema, nodeID string, c *ValueMapperConfig) (*valueMapperPlan, []core.Problem) {
	var probs []core.Problem
	idx := in.Index(c.Field)
	if idx < 0 {
		probs = append(probs, core.Problem{Severity: core.SeverityError, Path: "field", Code: "field_missing", Message: "输入中没有字段 " + c.Field})
		return nil, probs
	}
	p := &valueMapperPlan{src: idx, srcName: c.Field, def: c.Default, hasDef: c.Default != "", mapping: map[string]string{}}
	for _, m := range c.Mappings {
		p.mapping[m.Source] = m.Target
	}
	fields := in.Fields()
	out := fields[idx]
	out.Kind = core.KindString
	out.NativeType = ""
	if c.Target != "" && c.Target != c.Field {
		if in.Index(c.Target) >= 0 {
			probs = append(probs, core.Problem{Severity: core.SeverityError, Path: "target", Code: "field_dup", Message: "目标字段与已有字段同名：" + c.Target})
			return nil, probs
		}
		out.Name = c.Target
		out.Origin = nodeID
		p.dst = len(fields)
		fields = append(fields, out)
	} else {
		p.dst = idx
		fields[idx] = out
	}
	s, err := core.NewSchema(fields...)
	if err != nil {
		probs = append(probs, core.Problem{Severity: core.SeverityError, Path: "target", Code: "schema", Message: err.Error()})
		return nil, probs
	}
	p.schema = s
	return p, probs
}

func init() {
	core.Steps.Register(core.Define(core.Definition[ValueMapperConfig]{
		Meta: core.Meta{
			ID: "value_mapper", Order: 40, Label: "值映射", Category: core.CategoryTransform, Icon: "map",
			Description: "按映射表把字段值替换为目标值（按文本比较）。未匹配时可给默认值或保留原值；结果字段为字符串类型。",
			Inputs:      []core.PortDecl{{Name: core.PortMain, Label: "输入", Required: true}},
			Outputs:     []core.PortDecl{{Name: core.PortMain, Label: "输出"}},
			ErrorPort:   true,
			Keywords:    []string{"Value Mapper", "值映射", "字典", "mapping"},
		},
		Defaults: func() ValueMapperConfig { return ValueMapperConfig{} },
		Infer: func(ic *core.InferContext, c *ValueMapperConfig) error {
			p, probs := planValueMapper(ic.Main(), ic.NodeID, c)
			for _, pr := range probs {
				ic.Errorf(pr.Path, pr.Code, "%s", pr.Message)
			}
			if p != nil {
				ic.SetOutput(core.PortMain, p.schema)
			}
			return nil
		},
		Open: func(rc *core.RunContext, c *ValueMapperConfig) (core.Runner, error) {
			p, probs := planValueMapper(rc.In(core.PortMain).Schema, rc.NodeID, c)
			if len(probs) > 0 {
				return nil, probs[0]
			}
			return core.MapRows(rc, func(r core.Row) (core.Row, error) {
				var v any
				raw := r[p.src]
				key := ""
				if raw != nil {
					cv, err := core.Convert(raw, core.KindString, core.ConvertOptions{})
					if err != nil {
						return nil, core.NewRowError(p.srcName, err)
					}
					key = cv.(string)
				}
				if m, ok := p.mapping[key]; ok {
					v = m
				} else if p.hasDef {
					v = p.def
				} else if raw != nil {
					v = key // 未匹配且无默认：保留原值的文本形式（NULL 保持 NULL）
				}
				if p.dst < len(r) {
					return r.With(p.dst, v), nil
				}
				return append(r.Clone(), v), nil
			}), nil
		},
	}))
}
