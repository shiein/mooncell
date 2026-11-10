package steps

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"strings"

	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/expr"
)

// ---------------------------------------------------------------- 字段选择

// SelectFieldsConfig 是「字段选择」：选择、重命名、排序、改类型。
type SelectFieldsConfig struct {
	Fields []SelectField `json:"fields"`
	// KeepOthers：未列出的字段是否保留（追加在末尾）。默认 false = 只保留列出的字段。
	KeepOthers bool `json:"keepOthers"`
}

// SelectField 是一个输出字段。
type SelectField struct {
	Source string    `json:"source"`
	Name   string    `json:"name"`   // 空 = 同 Source
	Kind   core.Kind `json:"kind"`   // 空 = 不改类型
	Format string    `json:"format"` // 文本与日期互转格式（yyyy-MM-dd）
}

type selectPlan struct {
	src    []int
	conv   []*core.Kind
	opts   []core.ConvertOptions
	schema *core.Schema
}

func planSelect(in *core.Schema, c *SelectFieldsConfig) (*selectPlan, []core.Problem) {
	var probs []core.Problem
	p := &selectPlan{}
	var fields []core.Field
	used := map[int]bool{}
	for i, f := range c.Fields {
		idx := in.Index(f.Source)
		if idx < 0 {
			probs = append(probs, core.Problem{Severity: core.SeverityError, Path: fmt.Sprintf("fields[%d].source", i), Code: "field_missing", Message: fmt.Sprintf("输入中没有字段 %q", f.Source)})
			continue
		}
		used[idx] = true
		out := in.Field(idx)
		if f.Name != "" {
			out.Name = f.Name
		}
		var conv *core.Kind
		if f.Kind != "" && f.Kind != out.Kind {
			if !f.Kind.Valid() {
				probs = append(probs, core.Problem{Severity: core.SeverityError, Path: fmt.Sprintf("fields[%d].kind", i), Code: "kind", Message: "类型无效"})
				continue
			}
			k := f.Kind
			conv = &k
			out.Kind = k
			out.NativeType = ""
		}
		fields = append(fields, out)
		p.src = append(p.src, idx)
		p.conv = append(p.conv, conv)
		p.opts = append(p.opts, core.ConvertOptions{Layout: goLayout(f.Format)})
	}
	if c.KeepOthers || len(c.Fields) == 0 {
		for i, f := range in.Fields() {
			if !used[i] {
				fields = append(fields, f)
				p.src = append(p.src, i)
				p.conv = append(p.conv, nil)
				p.opts = append(p.opts, core.ConvertOptions{})
			}
		}
	}
	s, err := core.NewSchema(fields...)
	if err != nil {
		probs = append(probs, core.Problem{Severity: core.SeverityError, Path: "fields", Code: "schema", Message: err.Error()})
		return nil, probs
	}
	p.schema = s
	return p, probs
}

func init() {
	core.Steps.Register(core.Define(core.Definition[SelectFieldsConfig]{
		Meta: core.Meta{
			ID: "select_fields", Order: 20, Label: "字段选择", Category: core.CategoryTransform, Icon: "columns",
			Description: "选择、重命名、调整顺序与转换字段类型。类型转换失败的行可连到错误输出。",
			Inputs:      []core.PortDecl{{Name: core.PortMain, Label: "输入", Required: true}},
			Outputs:     []core.PortDecl{{Name: core.PortMain, Label: "输出"}},
			ErrorPort:   true,
			Keywords:    []string{"Select values", "重命名", "改类型", "元数据"},
		},
		Defaults: func() SelectFieldsConfig { return SelectFieldsConfig{} },
		Infer: func(ic *core.InferContext, c *SelectFieldsConfig) error {
			p, probs := planSelect(ic.Main(), c)
			for _, pr := range probs {
				ic.Errorf(pr.Path, pr.Code, "%s", pr.Message)
			}
			if p != nil {
				ic.SetOutput(core.PortMain, p.schema)
			}
			return nil
		},
		Open: func(rc *core.RunContext, c *SelectFieldsConfig) (core.Runner, error) {
			p, probs := planSelect(rc.In(core.PortMain).Schema, c)
			if len(probs) > 0 {
				return nil, probs[0]
			}
			return core.MapRows(rc, func(r core.Row) (core.Row, error) {
				out := make(core.Row, len(p.src))
				for i, idx := range p.src {
					v := r[idx]
					if k := p.conv[i]; k != nil {
						cv, err := core.Convert(v, *k, p.opts[i])
						if err != nil {
							return nil, core.NewRowError(p.schema.Field(i).Name, err)
						}
						v = cv
					}
					out[i] = v
				}
				return out, nil
			}), nil
		},
	}))
}

// ---------------------------------------------------------------- 表达式（计算器）

// CalculatorConfig 是「表达式」：逐个计算新字段或覆盖已有字段；后面的表达式可引用前面算出的字段。
type CalculatorConfig struct {
	Fields []CalcField `json:"fields"`
}

// CalcField 是一个计算字段。
type CalcField struct {
	Name string    `json:"name"`
	Expr string    `json:"expr"`
	Kind core.Kind `json:"kind"` // 空 = 按表达式推导；推导不出时必须指定
}

type calcPlan struct {
	progs  []*expr.Program
	slots  []int // 写入位置（覆盖时为原下标，新增时为追加下标）
	kinds  []core.Kind
	width  []int // 求值时行的宽度（追加前）
	schema *core.Schema
}

func planCalc(in *core.Schema, vars map[string]core.Kind, c *CalculatorConfig) (*calcPlan, []core.Problem) {
	var probs []core.Problem
	p := &calcPlan{}
	cur := in
	for i, f := range c.Fields {
		path := fmt.Sprintf("fields[%d]", i)
		if strings.TrimSpace(f.Name) == "" {
			probs = append(probs, core.Problem{Severity: core.SeverityError, Path: path + ".name", Code: "name", Message: "请填写字段名"})
			continue
		}
		prog, err := expr.Compile(f.Expr, expr.Env{Schema: cur, Vars: vars})
		if err != nil {
			probs = append(probs, core.Problem{Severity: core.SeverityError, Path: path + ".expr", Code: "expr", Message: fmt.Sprintf("%s：%s", f.Name, err.Error())})
			continue
		}
		kind := f.Kind
		if kind == "" {
			kind = prog.Kind
		}
		if kind == "" {
			probs = append(probs, core.Problem{Severity: core.SeverityError, Path: path + ".kind", Code: "kind_unknown", Message: fmt.Sprintf("%s：无法推导表达式结果类型，请指定类型", f.Name)})
			continue
		}
		field := core.NewField(f.Name, kind)
		var fields []core.Field
		slot := cur.Index(f.Name)
		if slot >= 0 {
			fields = cur.Fields()
			fields[slot] = field
		} else {
			slot = cur.Len()
			fields = append(cur.Fields(), field)
		}
		next, err := core.NewSchema(fields...)
		if err != nil {
			probs = append(probs, core.Problem{Severity: core.SeverityError, Path: path, Code: "schema", Message: err.Error()})
			continue
		}
		p.progs = append(p.progs, prog)
		p.slots = append(p.slots, slot)
		p.kinds = append(p.kinds, kind)
		p.width = append(p.width, cur.Len())
		cur = next
	}
	p.schema = cur
	return p, probs
}

func init() {
	core.Steps.Register(core.Define(core.Definition[CalculatorConfig]{
		Meta: core.Meta{
			ID: "calculator", Order: 10, Label: "表达式", Category: core.CategoryTransform, Icon: "function",
			Description: "用表达式计算新字段或改写已有字段，支持字符串、数值、日期、空值处理等函数。求值失败的行可连到错误输出。",
			Inputs:      []core.PortDecl{{Name: core.PortMain, Label: "输入", Required: true}},
			Outputs:     []core.PortDecl{{Name: core.PortMain, Label: "输出"}},
			ErrorPort:   true,
			Keywords:    []string{"Calculator", "Formula", "公式", "计算", "字段映射", "常量", "增加常量"},
		},
		Defaults: func() CalculatorConfig { return CalculatorConfig{} },
		Infer: func(ic *core.InferContext, c *CalculatorConfig) error {
			p, probs := planCalc(ic.Main(), ic.VarKinds, c)
			for _, pr := range probs {
				ic.Errorf(pr.Path, pr.Code, "%s", pr.Message)
			}
			ic.SetOutput(core.PortMain, p.schema)
			return nil
		},
		Open: func(rc *core.RunContext, c *CalculatorConfig) (core.Runner, error) {
			p, probs := planCalc(rc.In(core.PortMain).Schema, nil, c)
			if len(probs) > 0 {
				return nil, probs[0]
			}
			evs := make([]*expr.Evaluator, len(p.progs))
			for i, prog := range p.progs {
				evs[i] = prog.NewEvaluator(rc.Vars)
			}
			width := p.schema.Len()
			return core.MapRows(rc, func(r core.Row) (core.Row, error) {
				out := make(core.Row, len(r), width)
				copy(out, r)
				for i, ev := range evs {
					v, err := ev.Eval(out[:p.width[i]])
					if err != nil {
						return nil, core.NewRowError(p.schema.Field(p.slots[i]).Name, err)
					}
					if v != nil && core.KindOf(v) != p.kinds[i] {
						if v, err = core.Convert(v, p.kinds[i], core.ConvertOptions{}); err != nil {
							return nil, core.NewRowError(p.schema.Field(p.slots[i]).Name, err)
						}
					}
					if p.slots[i] < len(out) {
						out[p.slots[i]] = v
					} else {
						out = append(out, v)
					}
				}
				return out, nil
			}), nil
		},
	}))
}

// ---------------------------------------------------------------- 过滤记录

// FilterRowsConfig 是「过滤记录」：条件为真走 true，为假或 NULL 走 false。
type FilterRowsConfig struct {
	Condition string `json:"condition"`
}

func init() {
	core.Steps.Register(core.Define(core.Definition[FilterRowsConfig]{
		Meta: core.Meta{
			ID: "filter_rows", Order: 10, Label: "过滤记录", Category: core.CategoryFlow, Icon: "filter",
			Description: "按条件把记录分成两路：满足条件走「是」，不满足（含 NULL）走「否」。未连接的分支直接丢弃。",
			Inputs:      []core.PortDecl{{Name: core.PortMain, Label: "输入", Required: true}},
			Outputs: []core.PortDecl{
				{Name: core.PortTrue, Label: "是", Tone: "success"},
				{Name: core.PortFalse, Label: "否", Tone: "warn"},
			},
			ErrorPort: true,
			Keywords:  []string{"Filter rows", "条件", "where", "分支"},
		},
		Defaults: func() FilterRowsConfig { return FilterRowsConfig{} },
		Infer: func(ic *core.InferContext, c *FilterRowsConfig) error {
			prog, err := expr.Compile(c.Condition, expr.Env{Schema: ic.Main(), Vars: ic.VarKinds})
			if err != nil {
				ic.Errorf("condition", "expr", "%s", err.Error())
			} else if prog.Kind != "" && prog.Kind != core.KindBoolean {
				ic.Errorf("condition", "not_bool", "条件必须返回布尔值，实际为%s", prog.Kind.Label())
			}
			ic.SetOutput(core.PortTrue, ic.Main())
			ic.SetOutput(core.PortFalse, ic.Main())
			return nil
		},
		Open: func(rc *core.RunContext, c *FilterRowsConfig) (core.Runner, error) {
			prog, err := expr.Compile(c.Condition, expr.Env{Schema: rc.In(core.PortMain).Schema})
			if err != nil {
				return nil, err
			}
			ev := prog.NewEvaluator(rc.Vars)
			return core.RouteRows(rc, func(r core.Row) (core.RoutePort, error) {
				v, err := ev.Eval(r)
				if err != nil {
					return "", core.NewRowError("", err)
				}
				switch b := v.(type) {
				case bool:
					if b {
						return core.PortTrue, nil
					}
				case nil:
				default:
					return "", core.NewRowError("", fmt.Errorf("条件结果不是布尔值：%v", v))
				}
				return core.PortFalse, nil
			}), nil
		},
	}))
}

// ---------------------------------------------------------------- 唯一行（哈希）

// UniqueHashConfig 是「唯一行（哈希）」：按键去重，保留首次出现的行。无需有序输入，内存受预算约束。
type UniqueHashConfig struct {
	Keys []string `json:"keys"` // 空 = 全部字段
}

// 每个键的估算内存：16 字节摘要 + map 开销。
const uniqueEntryBytes = 64

func init() {
	core.Steps.Register(core.Define(core.Definition[UniqueHashConfig]{
		Meta: core.Meta{
			ID: "unique_hash", Order: 30, Label: "唯一行（哈希）", Category: core.CategoryTransform, Icon: "fingerprint",
			Description: "按指定字段去重并保留首次出现的行，不要求输入有序。键数量受转换内存预算限制，超大数据量请改用排序后去重。",
			Inputs:      []core.PortDecl{{Name: core.PortMain, Label: "输入", Required: true}},
			Outputs:     []core.PortDecl{{Name: core.PortMain, Label: "输出"}},
			Keywords:    []string{"Unique rows (HashSet)", "去重", "distinct"},
		},
		Defaults: func() UniqueHashConfig { return UniqueHashConfig{} },
		Infer: func(ic *core.InferContext, c *UniqueHashConfig) error {
			for i, k := range c.Keys {
				if ic.Main().Index(k) < 0 {
					ic.Errorf(fmt.Sprintf("keys[%d]", i), "field_missing", "输入中没有字段 %q", k)
				}
			}
			ic.SetOutput(core.PortMain, ic.Main())
			return nil
		},
		Open: func(rc *core.RunContext, c *UniqueHashConfig) (core.Runner, error) {
			in := rc.In(core.PortMain).Schema
			var idx []int
			for _, k := range c.Keys {
				idx = append(idx, in.Index(k))
			}
			if len(idx) == 0 {
				for i := 0; i < in.Len(); i++ {
					idx = append(idx, i)
				}
			}
			maxKeys := int64(math.MaxInt64)
			if rc.MemoryBudget > 0 {
				maxKeys = rc.MemoryBudget / uniqueEntryBytes
			}
			seen := map[[16]byte]struct{}{}
			return core.RunnerFunc(func(ctx context.Context) error {
				defer func() { seen = nil }()
				out := rc.Out(core.PortMain)
				buf := make([]byte, 0, 256)
				return rc.In(core.PortMain).Each(ctx, func(b core.Batch) error {
					rows := make([]core.Row, 0, len(b.Rows))
					for _, r := range b.Rows {
						buf = rowKey(buf[:0], r, idx)
						sum := sha256.Sum256(buf)
						var k [16]byte
						copy(k[:], sum[:16])
						if _, dup := seen[k]; dup {
							continue
						}
						if int64(len(seen)) >= maxKeys {
							return fmt.Errorf("不同键已超过内存预算（%d 个），请调大转换内存预算或改用排序后去重", maxKeys)
						}
						seen[k] = struct{}{}
						rows = append(rows, r)
					}
					return out.Send(ctx, core.Batch{Schema: out.Schema, Rows: rows})
				})
			}), nil
		},
	}))
}

// rowKey 把键字段编码为无歧义字节串：每个值 = 类型标记 + 长度 + 文本。
func rowKey(buf []byte, r core.Row, idx []int) []byte {
	var lenBuf [binary.MaxVarintLen64]byte
	for _, i := range idx {
		v := r[i]
		if v == nil {
			buf = append(buf, 0)
			continue
		}
		s := core.FormatValue(v)
		buf = append(buf, byte(len(core.KindOf(v))))
		buf = append(buf, core.KindOf(v)...)
		n := binary.PutUvarint(lenBuf[:], uint64(len(s)))
		buf = append(buf, lenBuf[:n]...)
		buf = append(buf, s...)
	}
	return buf
}

// ---------------------------------------------------------------- 空操作

type DummyConfig struct{}

func init() {
	core.Steps.Register(core.Define(core.Definition[DummyConfig]{
		Meta: core.Meta{
			ID: "dummy", Order: 90, Label: "空操作", Category: core.CategoryFlow, Icon: "circle",
			Description: "原样透传。可用于汇合多条流（字段布局须一致）或作为调试观察点。",
			Inputs:      []core.PortDecl{{Name: core.PortMain, Label: "输入", Multi: true}},
			Outputs:     []core.PortDecl{{Name: core.PortMain, Label: "输出"}},
			Keywords:    []string{"Dummy", "汇合", "union"},
		},
		Infer: func(ic *core.InferContext, _ *DummyConfig) error {
			ic.SetOutput(core.PortMain, ic.Main())
			return nil
		},
		Open: func(rc *core.RunContext, _ *DummyConfig) (core.Runner, error) {
			return core.RunnerFunc(func(ctx context.Context) error {
				out := rc.Out(core.PortMain)
				return rc.In(core.PortMain).Each(ctx, func(b core.Batch) error { return out.Send(ctx, b) })
			}), nil
		},
	}))
}
