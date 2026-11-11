package pipeline

import (
	"context"
	"encoding/json"
	"strings"

	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/expr"
	"mooncell/console/internal/etl/sqltext"
)

// PushdownNote 记录一处下推改写，供运行日志与测试断言。
type PushdownNote struct {
	Into string   // 并入的源步骤（表输入）名
	From []string // 被并入的步骤名（过滤/字段选择）
}

// Pushdown 把「表输入 → 过滤记录(若干) → 字段选择(至多一个,纯改名)」折叠为源端 SQL：
// SELECT <选择列|*> FROM (<原 SQL>) _mc WHERE <条件1 AND 条件2…>。
// 仅 settings.pushdown 显式开启时生效（默认关闭，旧定义行为不变）。
// 条件经 expr.ToSQL 白名单翻译——字面量全部生成绑定参数，按 ${} 参数之后的
// 占位符序号续接，值写入源配置的 pushArgs；任一环节不可安全下推即放弃该分支
// （返回原定义）。前置要求：plan 由 def 编译得到且无 error。
// 返回的总是新对象，def 不被修改。
func Pushdown(ctx context.Context, def *Definition, plan *Plan, env core.Env) (*Definition, []PushdownNote) {
	if !plan.Settings.Pushdown {
		return def, nil
	}
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
			MaxRows    int64  `json:"maxRows"`
		}
		if json.Unmarshal(pn.Node.Config, &cfg) != nil || strings.TrimSpace(cfg.SQL) == "" || cfg.Connection == "" {
			continue
		}
		if cfg.MaxRows > 0 {
			// 源端限行作用于「过滤前」的前 N 行；条件并入 SQL 后会变成「过滤后」的前 N 行，结果不等价。
			continue
		}
		h, err := pool.Get(ctx, cfg.Connection)
		if err != nil {
			continue // 连接不可用：保留原定义，由编译器报告真实错误
		}
		fields := map[string]core.Kind{}
		notNull := map[string]bool{} // 驱动/元数据明确声明不可空的字段；未知一律按可空
		if s := pn.Outputs[core.PortMain]; s != nil {
			for _, f := range s.Fields() {
				fields[f.Name] = f.Kind
				if !f.Nullable {
					notNull[f.Name] = true
				}
			}
		}
		// 原 SQL 的 ${} 参数已占 1..bindN 号占位符；下推字面量参数从 bindN+1 续接，
		// 值按同序收集进 pushArgs（table_input 在 ${} 参数之后绑定）。
		refs, err := sqltext.Scan(cfg.SQL, h.Dialect.Lexer())
		if err != nil {
			continue // 词法错误由编译器报告；此处不改写
		}
		bindN := 0
		for _, r := range refs {
			if !r.Raw {
				bindN++
			}
		}
		var pushArgs []any
		emit := func() string {
			bindN++
			return h.Dialect.Placeholder(bindN)
		}
		// toSQL 失败时回滚占位符序号与参数，保证 SQL 占位符与 pushArgs 一一对应。
		toSQL := func(cond string) (expr.SQLCond, error) {
			saveN, saveA := bindN, len(pushArgs)
			c, err := expr.ToSQL(cond, h.Dialect.Quote, fields, notNull, emit)
			if err != nil {
				bindN, pushArgs = saveN, pushArgs[:saveA]
				return expr.SQLCond{}, err
			}
			pushArgs = append(pushArgs, c.Args...)
			return c, nil
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
			if next.Node.Copies > 1 && next.Node.CopiesDistribution == core.DistCopy {
				// 显式 copy 的多副本：每个副本各收全量、各自输出一份，并入源后行数会变少。
				// 须先于条件翻译检查，避免登记孤儿参数。
				goto chainEnd
			}
			switch next.Node.Type {
			case "filter_rows":
				var fc struct {
					Condition string `json:"condition"`
				}
				if json.Unmarshal(next.Node.Config, &fc) != nil {
					goto chainEnd
				}
				// 只允许「是」端口单出边；否/错误端口被使用则不可下推。
				// 必须先于翻译检查：翻译会登记绑定参数，放弃时不能留下孤儿参数。
				th := next.OutHops[core.PortTrue]
				if len(th) != 1 || len(next.OutHops[core.PortFalse]) > 0 || len(next.OutHops[core.PortError]) > 0 {
					goto chainEnd
				}
				c, err := toSQL(fc.Condition)
				if err != nil {
					goto chainEnd
				}
				conds = append(conds, c.SQL)
				if c.Residual {
					// 超集条件：源端先筛，过滤步骤保留在行内复核（NULL 行照常让过滤报错）。
					// 链在此结束：上游并入的节点删去后，原本进入该过滤的连线改接源。
					names = append(names, next.Node.Name+"（保留行内复核）")
					endHops = []Hop{cur}
					goto chainEnd
				}
				chain = append(chain, next.Node.ID)
				names = append(names, next.Node.Name)
				cur = th[0]
			case "select_fields":
				cols, ok := pushableSelect(next.Node.Config, h.Dialect.Quote)
				if !ok {
					goto chainEnd
				}
				// 字段选择的出边会改接到源的 main 端口，按源的分发方式发送；
				// 多条出边且两者分发方式不同（copy/distribute）时，下游行数会变 → 不并入。
				// 错误端口被使用则不并入：并入后出边统一改接源的 main，错误分支会收到全部行。
				// 无下游（如只作预览观察点）也不并入：链尾回退为 cur 会留下指向已删节点的连线。
				if len(next.OutHops[core.PortError]) > 0 || len(next.OutHops[core.PortMain]) == 0 {
					goto chainEnd
				}
				if n := len(next.OutHops[core.PortMain]); n > 1 && distOf(next.Node, core.PortMain) != distOf(pn.Node, core.PortMain) {
					goto chainEnd
				}
				selectCols = cols
				chain = append(chain, next.Node.ID)
				names = append(names, next.Node.Name)
				endHops = append(endHops, next.OutHops[core.PortMain]...)
				goto chainEnd
			default:
				goto chainEnd
			}
		}
	chainEnd:
		if len(chain) == 0 && len(conds) == 0 {
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

		// 改写定义副本：更新源 SQL、携带下推参数、删除被并入节点、链尾出边改接源。
		// 用 map 改写以保留 connection/maxRows 等未读字段。
		for i := range out.Nodes {
			if out.Nodes[i].ID == pn.Node.ID {
				var cfgMap map[string]any
				if json.Unmarshal(pn.Node.Config, &cfgMap) != nil {
					continue
				}
				cfgMap["sql"] = sb.String()
				if len(pushArgs) > 0 {
					cfgMap["pushArgs"] = pushArgs
				}
				b, _ := json.Marshal(cfgMap)
				out.Nodes[i].Config = b
			}
		}
		notes = append(notes, PushdownNote{Into: pn.Node.Name, From: names})
		if len(chain) == 0 {
			continue // 仅首个过滤以超集下推：不删节点、连线不变
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
	}
	if len(notes) == 0 {
		return def, nil
	}
	return out, notes
}

// distOf 返回节点输出端口的有效分发方式（缺省 copy，同 core.NewOutPort）。
func distOf(n Node, port string) core.Distribution {
	if d := n.Distribution[port]; d != "" {
		return d
	}
	return core.DistCopy
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
