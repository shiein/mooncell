package workflow

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"mooncell/console/internal/etl/core"
)

// Plan 是作业编译结果。
type Plan struct {
	Def      *Definition
	Settings Settings
	Nodes    []*PlanNode // 拓扑序
	ByID     map[string]*PlanNode
	Start    *PlanNode
	VarKinds map[string]core.Kind
	Problems core.Problems
}

// PlanNode 是编译后的作业项。
type PlanNode struct {
	Node    Node
	Type    ActionType
	Config  any
	In      []Hop            // 入线
	Out     map[string][]Hop // 输出端口 → 出线
	Outputs map[string]core.Kind
}

// CompileOptions 控制编译。
type CompileOptions struct {
	Registry *core.Registry[ActionType]
	Env      core.Env
}

// BuiltinVars 是作业内置变量。
func BuiltinVars() map[string]core.Kind {
	return map[string]core.Kind{"RUN_ID": core.KindString, "RUN_START": core.KindTimestamp, "TRIGGER": core.KindString}
}

// Compile 编译作业定义。永远返回 Plan；能否运行看 Problems.HasErrors()。
func Compile(ctx context.Context, def *Definition, opts CompileOptions) *Plan {
	reg := opts.Registry
	if reg == nil {
		reg = Actions
	}
	p := &Plan{Def: def, Settings: def.Settings.Normalized(), ByID: map[string]*PlanNode{}, VarKinds: BuiltinVars()}
	errf := func(nodeID, hopID, code, format string, args ...any) {
		p.Problems = append(p.Problems, core.Problem{Severity: core.SeverityError, NodeID: nodeID, HopID: hopID, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	for i, prm := range def.Params {
		if !core.ValidVarName(prm.Name) || !prm.Kind.Valid() {
			p.Problems = append(p.Problems, core.Problem{Severity: core.SeverityError, Path: fmt.Sprintf("params[%d]", i), Code: "param", Message: fmt.Sprintf("参数 %q 的名称或类型无效", prm.Name)})
			continue
		}
		p.VarKinds[prm.Name] = prm.Kind
	}

	// 节点 + 配置 + 输出变量
	producers := map[string]string{} // 变量 → 首个产出它的节点
	for _, n := range def.Nodes {
		if strings.TrimSpace(n.ID) == "" {
			errf("", "", "node_id", "存在没有 ID 的作业项")
			continue
		}
		if _, dup := p.ByID[n.ID]; dup {
			errf(n.ID, "", "node_dup", "作业项 ID 重复：%s", n.ID)
			continue
		}
		pn := &PlanNode{Node: n, Out: map[string][]Hop{}}
		p.ByID[n.ID] = pn
		t, ok := reg.Get(n.Type)
		if !ok {
			errf(n.ID, "", "unknown_type", "未知的作业项类型 %q", n.Type)
			continue
		}
		if t.Meta().Maturity == core.MaturityPlanned {
			errf(n.ID, "", "planned", "「%s」尚未实现，暂不能运行", t.Meta().Label)
			continue
		}
		pn.Type = t
		cfg, err := t.Decode(n.Config, n.Version)
		if err != nil {
			errf(n.ID, "", "config", "%s", err.Error())
			continue
		}
		pn.Config = cfg
		pn.Outputs = t.Outputs(cfg)
		for name, k := range pn.Outputs {
			if !core.ValidVarName(name) {
				errf(n.ID, "", "var_name", "变量名 %q 不合法", name)
				continue
			}
			if prev, ok := p.VarKinds[name]; ok && prev != k {
				errf(n.ID, "", "var_kind", "变量 %s 已被定义为%s，这里又定义为%s", name, prev.Label(), k.Label())
				continue
			}
			p.VarKinds[name] = k
			if _, ok := producers[name]; !ok {
				producers[name] = n.ID
			}
		}
		if n.Type == "start" {
			if p.Start != nil {
				errf(n.ID, "", "start_dup", "作业只能有一个「开始」")
			} else {
				p.Start = pn
			}
		}
	}
	if p.Start == nil && len(def.Nodes) > 0 {
		errf("", "", "start_missing", "作业缺少「开始」")
	}

	// 连线
	seen := map[string]bool{}
	for _, h := range def.Hops {
		if h.Disabled {
			continue
		}
		from, to := p.ByID[h.From], p.ByID[h.To]
		if from == nil || to == nil {
			errf("", h.ID, "hop_dangling", "连线指向不存在的作业项")
			continue
		}
		if h.FromPort == "" {
			h.FromPort = core.PortSuccess
		}
		if h.From == h.To {
			errf(h.From, h.ID, "hop_self", "作业项不能连向自身")
			continue
		}
		if from.Type != nil && h.FromPort != PortAlways {
			if _, ok := from.Type.Meta().OutputPort(h.FromPort); !ok {
				errf(h.From, h.ID, "port", "「%s」没有输出端口 %q", from.Node.Name, h.FromPort)
				continue
			}
		}
		if to.Node.Type == "start" {
			errf(h.To, h.ID, "start_in", "「开始」不能有入线")
			continue
		}
		key := h.From + "\x00" + h.FromPort + "\x00" + h.To
		if seen[key] {
			errf(h.From, h.ID, "hop_dup", "重复的连线")
			continue
		}
		seen[key] = true
		from.Out[h.FromPort] = append(from.Out[h.FromPort], h)
		to.In = append(to.In, h)
	}

	// 拓扑序 + 环
	order, cyclic := topo(def.Nodes, p.ByID)
	for _, id := range cyclic {
		errf(id, "", "cycle", "作业中存在环路（需要循环时请使用重试或调度）")
	}
	for _, id := range order {
		p.Nodes = append(p.Nodes, p.ByID[id])
	}

	// 可达性
	if p.Start != nil {
		reach := p.reachable()
		for _, pn := range p.Nodes {
			if !reach[pn.Node.ID] {
				p.Problems = append(p.Problems, core.Problem{Severity: core.SeverityWarning, NodeID: pn.Node.ID, Code: "unreachable", Message: fmt.Sprintf("「%s」不会被执行：从「开始」无法到达", pn.Node.Name)})
			}
		}
	}

	// 作业项自校验
	for _, pn := range p.Nodes {
		if pn.Type == nil || pn.Config == nil {
			continue
		}
		vc := &ValidateContext{Ctx: ctx, NodeID: pn.Node.ID, Env: opts.Env, VarKinds: p.VarKinds}
		if err := pn.Type.Validate(vc, pn.Config); err != nil {
			vc.Errorf("", "validate", "%s", err.Error())
		}
		p.Problems = append(p.Problems, vc.problems...)
		if pn.Node.Retry.Max < 0 || pn.Node.Retry.BackoffSec < 0 || pn.Node.TimeoutSec < 0 {
			errf(pn.Node.ID, "", "retry", "超时与重试参数不能为负")
		}
	}
	sort.SliceStable(p.Problems, func(i, j int) bool { return p.Problems[i].Severity < p.Problems[j].Severity })
	return p
}

func topo(nodes []Node, byID map[string]*PlanNode) (order, cyclic []string) {
	indeg := map[string]int{}
	pos := map[string]int{}
	for i, n := range nodes {
		if _, ok := byID[n.ID]; ok {
			if _, seen := pos[n.ID]; !seen {
				pos[n.ID] = i
				indeg[n.ID] = len(byID[n.ID].In)
			}
		}
	}
	var ready []string
	for id, d := range indeg {
		if d == 0 {
			ready = append(ready, id)
		}
	}
	sortByPos := func() { sort.Slice(ready, func(i, j int) bool { return pos[ready[i]] < pos[ready[j]] }) }
	sortByPos()
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		for _, hops := range byID[id].Out {
			for _, h := range hops {
				indeg[h.To]--
				if indeg[h.To] == 0 {
					ready = append(ready, h.To)
				}
			}
		}
		sortByPos()
	}
	for id, d := range indeg {
		if d > 0 {
			cyclic = append(cyclic, id)
		}
	}
	sort.Strings(cyclic)
	return order, cyclic
}

// reachable 返回从「开始」沿出线可到达的节点集合（Nodes 为拓扑序，单趟即可）。
func (p *Plan) reachable() map[string]bool {
	reach := map[string]bool{}
	if p.Start == nil {
		return reach
	}
	reach[p.Start.Node.ID] = true
	for _, pn := range p.Nodes {
		if !reach[pn.Node.ID] {
			continue
		}
		for _, hops := range pn.Out {
			for _, h := range hops {
				reach[h.To] = true
			}
		}
	}
	return reach
}
