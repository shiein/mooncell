package steps

import (
	"context"
	"fmt"

	"mooncell/console/internal/etl/core"
)

// UniqueSortedConfig 是「去除重复记录」：输入须按键有序（通常前置 sort_rows），
// 流式保留每个相邻键组的首行。键为空 = 整行比较。
type UniqueSortedConfig struct {
	Keys []string `json:"keys"`
}

func uniqueSortedKeys(in *core.Schema, keys []string) ([]int, error) {
	idx := make([]int, 0, len(keys))
	if len(keys) == 0 {
		for i := range in.Fields() {
			idx = append(idx, i)
		}
		return idx, nil
	}
	for _, k := range keys {
		i := in.Index(k)
		if i < 0 {
			return nil, fmt.Errorf("去重字段 %q 不在输入中", k)
		}
		idx = append(idx, i)
	}
	return idx, nil
}

func init() {
	core.Steps.Register(core.Define(core.Definition[UniqueSortedConfig]{
		Meta: core.Meta{
			ID: "unique_sorted", Order: 35, Label: "去除重复记录", Category: core.CategoryTransform, Icon: "unique",
			Stateful:     true,
			OrderedInput: true,
			Description:  "保留每组相邻重复键的首行（输入须已按去重字段排序，一般前置「排序记录」）。流式单遍处理，不占内存。",
			Inputs:       []core.PortDecl{{Name: core.PortMain, Label: "输入", Required: true}},
			Outputs:      []core.PortDecl{{Name: core.PortMain, Label: "输出"}},
			Keywords:     []string{"Unique rows", "去除重复", "distinct", "去重", "sorted"},
		},
		Defaults: func() UniqueSortedConfig { return UniqueSortedConfig{} },
		Infer: func(ic *core.InferContext, c *UniqueSortedConfig) error {
			ic.SetOutput(core.PortMain, ic.Main())
			if _, err := uniqueSortedKeys(ic.Main(), c.Keys); err != nil {
				ic.Errorf("keys", "field_missing", "%s", err.Error())
			}
			return nil
		},
		Open: func(rc *core.RunContext, c *UniqueSortedConfig) (core.Runner, error) {
			keys, err := uniqueSortedKeys(rc.In(core.PortMain).Schema, c.Keys)
			if err != nil {
				return nil, err
			}
			return core.RunnerFunc(func(ctx context.Context) error {
				out := rc.Out(core.PortMain)
				var prev core.Row
				return rc.In(core.PortMain).Each(ctx, func(b core.Batch) error {
					rows := make([]core.Row, 0, len(b.Rows))
					for _, r := range b.Rows {
						if prev == nil || !sameKeys(r, prev, keys) {
							rows = append(rows, r)
						}
						prev = r
					}
					return out.Send(ctx, core.Batch{Schema: out.Schema, Rows: rows})
				})
			}), nil
		},
	}))
}

func sameKeys(a, b core.Row, keys []int) bool {
	for _, k := range keys {
		if compareValues(a[k], b[k]) != 0 {
			return false
		}
	}
	return true
}
