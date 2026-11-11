package pipeline

import (
	"context"
	"encoding/json"
	"strings"

	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/expr"
)

// PushdownNote 记录一处下推改写，供运行日志与测试断言。
type PushdownNote struct {
	Into string   // 并入的源步骤（表输入）名
	From []string // 被并入的步骤名（过滤/字段选择）
}

// Pushdown 把「表输入 → 过滤记录(若干) → 字段选择(至多一个,纯改名)」折叠为源端 SQL：
// SELECT <选择列|*> FROM (<原 SQL>) _mc WHERE <条件1 AND 条件2…>。
// 条件经 expr.ToSQL 白名单翻译；任一环节不可安全下推即放弃该分支（返回原定义）。
// 前置要求：plan 由 def 编译得到且无 error。返回的总是新对象，def 不被修改。
func Pushdown(ctx context.Context, def *Definition, plan *Plan, env core.Env) (*Definition, []PushdownNote) {
	pool, err := conn.PoolFrom(env)
	if err != nil || pool == nil {
		return def, nil
	}
	out := &Definition{
		Format: def.Format, Kind: def.Kind, Params: def.Params,
		Settings: def.Settings,
		Nodes:    append([]Node(nil), def.Nodes...),
		Hops:     append([]Hop(nil), def.Hops...),
		Notes:    def.Notes,
	}
	var notes []PushdownNote
	removed := map[string]bool{}

	for _, pn := range plan.Nodes {
		if pn.Node.Type != "table_input" || removed[pn.Node.ID] {
			continue
		}
		// 源的唯一出边才允许下推（否则改 SQL 会影响其他分支）
		var outHops []Hop
		for _, hs := range pn.OutHops {
			outHops = append(outHops, hs...)
		}
		if len(outHops) != 1 || outHops[0].FromPort != core.PortMain {
			continue
		}
		var cfg struct {
			Connection string `json:"connection"`
			SQL        string `json:"sql"`
		}
		if json.Unmarshal(pn.Node.Config, &cfg) != nil || strings.TrimSpace(cfg.SQL) == "" || cfg.Connection == "" {
			continue
		}
		h, err := pool.Get(ctx, cfg.Connection)
		if err != nil {
			continue // 连接不可用：保留原定义，由编译器报告真实错误
		}
		fields := map[string]bool{}
		if s := pn.Outputs[core.PortMain]; s != nil {
			for _, n := range s.Names() {
				fields[n] = true
			}
		}
		toSQL := func(cond string) (string, error) {
			return expr.ToSQL(cond, h.Dialect.Quote, fields)
		}

		// 沿唯一出边链收集可下推步骤
		var conds []string
		var selectCols []string
		var chain []string // 被并入的节点 ID
		var names []string // 展示名
		var endHops []Hop  // 链尾出边（改接到源）
		cur := outHops[0]
		for {
			next := plan.ByID[cur.To]
			if next == nil || removed[next.Node.ID] {
				break
			}
			switch next.Node.Type {
			case "filter_rows":
				var fc struct {
					Condition string `json:"condition"`
				}
				if json.Unmarshal(next.Node.Config, &fc) != nil {
					goto chainEnd
				}
				w, err := toSQL(fc.Condition)
				if err != nil {
					goto chainEnd
				}
				// 只允许「是」端口单出边；否/错误端口被使用则不可下推
				th := next.OutHops[core.PortTrue]
				if len(th) != 1 || len(next.OutHops[core.PortFalse]) > 0 || len(next.OutHops[core.PortError]) > 0 {
					goto chainEnd
				}
				conds = append(conds, w)
				chain = append(chain, next.Node.ID)
				names = append(names, next.Node.Name)
				cur = th[0]
			case "select_fields":
				cols, ok := pushableSelect(next.Node.Config, h.Dialect.Quote)
				if !ok {
					goto chainEnd
				}
				selectCols = cols
				chain = append(chain, next.Node.ID)
				names = append(names, next.Node.Name)
				for _, hs := range next.OutHops {
					endHops = append(endHops, hs...)
				}
				goto chainEnd
			default:
				goto chainEnd
			}
		}
	chainEnd:
		if len(chain) == 0 {
			continue
		}
		if len(endHops) == 0 {
			endHops = []Hop{cur} // 链未以字段选择收尾：最后一步的出边即链尾
		}

		// 组装下推 SQL
		var sb strings.Builder
		sb.WriteString("SELECT ")
		if len(selectCols) > 0 {
			sb.WriteString(strings.Join(selectCols, ", "))
		} else {
			sb.WriteString("*")
		}
		sb.WriteString("\nFROM (\n")
		sb.WriteString(strings.TrimRight(strings.TrimSpace(cfg.SQL), ";"))
		sb.WriteString("\n) _mc_push")
		if len(conds) > 0 {
			sb.WriteString("\nWHERE ")
			sb.WriteString(strings.Join(conds, "\n  AND "))
		}

		// 改写定义副本：更新源 SQL、删除被并入节点、链尾出边改接源
		for i := range out.Nodes {
			if out.Nodes[i].ID == pn.Node.ID {
				cfg.SQL = sb.String()
				b, _ := json.Marshal(cfg)
				out.Nodes[i].Config = b
			}
		}
		for _, id := range chain {
			removed[id] = true
		}
		var keepNodes []Node
		for _, n := range out.Nodes {
			if !removed[n.ID] {
				keepNodes = append(keepNodes, n)
			}
		}
		out.Nodes = keepNodes
		// 移除链内连线 + 被并入节点的其他出边（已并入语义无意义），把链尾出边改接源
		inChain := map[string]bool{}
		for _, id := range chain {
			inChain[id] = true
		}
		var hops []Hop
		for _, hp := range out.Hops {
			if inChain[hp.To] || inChain[hp.From] {
				continue
			}
			hops = append(hops, hp)
		}
		for _, eh := range endHops {
			eh.From = pn.Node.ID
			eh.FromPort = core.PortMain
			hops = append(hops, eh)
		}
		out.Hops = hops
		notes = append(notes, PushdownNote{Into: pn.Node.Name, From: names})
	}
	if len(notes) == 0 {
		return def, nil
	}
	return out, notes
}

// pushableSelect 判定字段选择是否可折叠为 SELECT 列：只保留列出的字段、无类型/格式转换。
// 返回方言引用后的列表达式（改名 → `src AS name`）。
func pushableSelect(raw json.RawMessage, quote func(string) string) ([]string, bool) {
	var c struct {
		Fields []struct {
			Source string `json:"source"`
			Name   string `json:"name"`
			Kind   string `json:"kind"`
			Format string `json:"format"`
		} `json:"fields"`
		KeepOthers bool `json:"keepOthers"`
	}
	if json.Unmarshal(raw, &c) != nil || c.KeepOthers || len(c.Fields) == 0 {
		return nil, false
	}
	var cols []string
	for _, f := range c.Fields {
		if f.Source == "" || f.Kind != "" || f.Format != "" {
			return nil, false
		}
		col := quote(f.Source)
		if f.Name != "" && f.Name != f.Source {
			col += " AS " + quote(f.Name)
		}
		cols = append(cols, col)
	}
	return cols, true
}
