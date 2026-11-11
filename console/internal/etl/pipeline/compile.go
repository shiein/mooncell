package pipeline

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"mooncell/console/internal/etl/core"
)

// MaxCopies 是单个步骤并行副本的上限。
const MaxCopies = 8

// copiesWriteRace 是按键读/写库的步骤（R1-B3）：多副本并发时，目标表键上
// 缺少唯一约束会产生竞态与重复行 → 编译期告警。
var copiesWriteRace = map[string]bool{
	"insert_update": true, "update": true, "delete": true,
	"sync_after_merge": true, "db_lookup": true,
}

// Plan 是编译结果：拓扑有序的节点、每条连线的 schema 与全部问题。
type Plan struct {
	Def      *Definition
	Settings Settings
	Nodes    []*PlanNode // 拓扑序
	ByID     map[string]*PlanNode
	Problems core.Problems
	VarKinds map[string]core.Kind
	// HopSchemas：连线 ID → 字段（设计器在连线与下游表单上展示）。
	HopSchemas map[string]*core.Schema
}

// PlanNode 是编译后的步骤。
type PlanNode struct {
	Node    Node
	Type    core.StepType
	Config  any
	Inputs  map[string]*core.Schema // 输入端口 → 合并后的 schema
	Outputs map[string]*core.Schema // 输出端口（含 error）→ schema
	InHops  map[string][]Hop        // 输入端口 → 连线
	OutHops map[string][]Hop        // 输出端口 → 连线
	// Blocked：上游有错误导致本节点未做推导（避免级联出一串误导性问题）。
	Blocked bool
}

// CompileOptions 控制编译。
type CompileOptions struct {
	Registry  *core.Registry[core.StepType] // 默认 core.Steps
	Env       core.Env
	ExtraVars map[string]core.Kind // 作业传入的变量（参数之外）
	// VarSamples 覆盖设计期样例值（如作业里已知的变量默认值）。
	VarSamples map[string]any
}

// sampleFor 返回某类型的设计期样例值（仅用于探测 SQL 字段，不会写入任何数据）。
func sampleFor(k core.Kind) any {
	switch k {
	case core.KindString:
		return ""
	case core.KindInteger:
		return int64(0)
	case core.KindNumber:
		return 0.0
	case core.KindDecimal:
		v, _ := core.Convert("0", core.KindDecimal, core.ConvertOptions{})
		return v
	case core.KindBoolean:
		return false
	case core.KindDate, core.KindTimestamp:
		return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return nil
}

// Compile 把定义编译为执行计划。永远返回 Plan；能否运行看 Plan.Problems.HasErrors()。
func Compile(ctx context.Context, def *Definition, opts CompileOptions) *Plan {
	reg := opts.Registry
	if reg == nil {
		reg = core.Steps
	}
	p := &Plan{Def: def, Settings: def.Settings.Normalized(), ByID: map[string]*PlanNode{}, HopSchemas: map[string]*core.Schema{}}
	errf := func(nodeID, hopID, code, format string, args ...any) {
		p.Problems = append(p.Problems, core.Problem{Severity: core.SeverityError, NodeID: nodeID, HopID: hopID, Code: code, Message: fmt.Sprintf(format, args...)})
	}

	// ---- 变量
	p.VarKinds = BuiltinVars()
	for k, v := range opts.ExtraVars {
		p.VarKinds[k] = v
	}
	for i, prm := range def.Params {
		if !core.ValidVarName(prm.Name) {
			p.Problems = append(p.Problems, core.Problem{Severity: core.SeverityError, Path: fmt.Sprintf("params[%d].name", i), Code: "param_name", Message: fmt.Sprintf("参数名 %q 不合法（仅字母、数字、下划线）", prm.Name)})
			continue
		}
		if !prm.Kind.Valid() {
			p.Problems = append(p.Problems, core.Problem{Severity: core.SeverityError, Path: fmt.Sprintf("params[%d].kind", i), Code: "param_kind", Message: fmt.Sprintf("参数 %s 的类型无效", prm.Name)})
			continue
		}
		p.VarKinds[prm.Name] = prm.Kind
	}
	samples := map[string]any{}
	for name, k := range p.VarKinds {
		samples[name] = sampleFor(k)
	}
	for _, prm := range def.Params {
		if prm.Default != nil && prm.Default.V != nil {
			samples[prm.Name] = prm.Default.V
		}
	}
	for k, v := range opts.VarSamples {
		samples[k] = v
	}

	// ---- 节点
	for _, n := range def.Nodes {
		if strings.TrimSpace(n.ID) == "" {
			errf("", "", "node_id", "存在没有 ID 的步骤")
			continue
		}
		if _, dup := p.ByID[n.ID]; dup {
			errf(n.ID, "", "node_dup", "步骤 ID 重复：%s", n.ID)
			continue
		}
		pn := &PlanNode{Node: n, Inputs: map[string]*core.Schema{}, Outputs: map[string]*core.Schema{}, InHops: map[string][]Hop{}, OutHops: map[string][]Hop{}}
		p.ByID[n.ID] = pn
		t, ok := reg.Get(n.Type)
		if !ok {
			errf(n.ID, "", "unknown_type", "未知的步骤类型 %q", n.Type)
			pn.Blocked = true
			continue
		}
		if t.Meta().Maturity == core.MaturityPlanned {
			errf(n.ID, "", "planned", "「%s」尚未实现，暂不能运行", t.Meta().Label)
			pn.Blocked = true
			continue
		}
		pn.Type = t
		cfg, err := t.Decode(n.Config, n.Version)
		if err != nil {
			errf(n.ID, "", "config", "%s", err.Error())
			pn.Blocked = true
			continue
		}
		pn.Config = cfg
		// 并行副本校验
		if n.Copies < 0 || n.Copies > MaxCopies {
			errf(n.ID, "", "copies_range", "并行副本数须在 1–%d 之间（当前 %d）", MaxCopies, n.Copies)
			pn.Blocked = true
			continue
		}
		if n.CopiesDistribution != "" && n.CopiesDistribution != core.DistCopy && n.CopiesDistribution != core.DistDistribute {
			errf(n.ID, "", "copies_dist", "copiesDistribution 只能是 copy 或 distribute（当前 %q）", n.CopiesDistribution)
			pn.Blocked = true
			continue
		}
		if n.Copies > 1 {
			if len(t.Meta().Inputs) == 0 {
				errf(n.ID, "", "copies_source", "「%s」是输入源步骤，副本会让每份都读全量数据；如需并行拆分请在源 SQL/上游用 distribute 分发", n.Name)
				pn.Blocked = true
				continue
			}
			if t.Meta().Stateful {
				p.Problems = append(p.Problems, core.Problem{Severity: core.SeverityWarning, NodeID: n.ID, Code: "copies_stateful", Message: fmt.Sprintf("「%s」依赖全量/有序输入，%d 个副本间不共享状态，结果只是各自分片的局部正确", n.Name, n.Copies)})
			}
			if copiesWriteRace[n.Type] {
				p.Problems = append(p.Problems, core.Problem{Severity: core.SeverityWarning, NodeID: n.ID, Code: "copies_write_race", Message: fmt.Sprintf("「%s」的 %d 个副本会并发按键读/写同一张表，键上缺少唯一约束时会产生竞态和重复行", n.Name, n.Copies)})
			}
		}
	}

	// ---- 连线
	seenHop := map[string]bool{}
	for _, h := range def.Hops {
		if h.Disabled {
			continue
		}
		from, to := p.ByID[h.From], p.ByID[h.To]
		if from == nil || to == nil {
			errf("", h.ID, "hop_dangling", "连线指向不存在的步骤")
			continue
		}
		if h.From == h.To {
			errf(h.From, h.ID, "hop_self", "步骤不能连向自身")
			continue
		}
		if h.FromPort == "" {
			h.FromPort = core.PortMain
		}
		if h.ToPort == "" {
			h.ToPort = core.PortMain
		}
		key := h.From + "\x00" + h.FromPort + "\x00" + h.To + "\x00" + h.ToPort
		if seenHop[key] {
			errf(h.From, h.ID, "hop_dup", "重复的连线")
			continue
		}
		seenHop[key] = true
		if from.Type != nil {
			if _, ok := from.Type.Meta().OutputPort(h.FromPort); !ok {
				errf(h.From, h.ID, "port", "「%s」没有输出端口 %q", from.Node.Name, h.FromPort)
				continue
			}
		}
		if to.Type != nil {
			if _, ok := to.Type.Meta().InputPort(h.ToPort); !ok {
				errf(h.To, h.ID, "port", "「%s」没有输入端口 %q", to.Node.Name, h.ToPort)
				continue
			}
		}
		from.OutHops[h.FromPort] = append(from.OutHops[h.FromPort], h)
		to.InHops[h.ToPort] = append(to.InHops[h.ToPort], h)
	}
	// 输入端口约束
	for _, pn := range p.ByID {
		if pn.Type == nil {
			continue
		}
		for _, port := range pn.Type.Meta().Inputs {
			hops := pn.InHops[port.Name]
			if len(hops) > 1 && !port.Multi {
				errf(pn.Node.ID, "", "port_multi", "「%s」的输入「%s」只能有一条连线", pn.Node.Name, port.Label)
			}
			if port.Required && len(hops) == 0 {
				errf(pn.Node.ID, "", "port_required", "「%s」缺少输入「%s」", pn.Node.Name, port.Label)
			}
		}
	}

	// R1-B3：多副本节点的下游若存在要求有序输入的步骤（unique_sorted、merge_rows 等），
	// 各副本分片汇流后行序被打乱 → 告警。sort_rows 重建全序，不再向下游传播。
	for _, pn := range p.ByID {
		if pn.Node.Copies <= 1 {
			continue
		}
		seen, warned := map[string]bool{}, map[string]bool{}
		var dfs func(id string)
		dfs = func(id string) {
			if seen[id] {
				return
			}
			seen[id] = true
			cur := p.ByID[id]
			if cur == nil || cur.Type == nil {
				return
			}
			if cur.Type.Meta().OrderedInput && !warned[id] {
				warned[id] = true
				p.Problems = append(p.Problems, core.Problem{Severity: core.SeverityWarning, NodeID: id, Code: "copies_order", Message: fmt.Sprintf("「%s」要求有序输入，但上游「%s」的 %d 个副本会打乱行序；如需有序请先加「排序记录」", cur.Node.Name, pn.Node.Name, pn.Node.Copies)})
			}
			if cur.Node.Type == "sort_rows" { // 重建全序
				return
			}
			for _, hops := range cur.OutHops {
				for _, h := range hops {
					dfs(h.To)
				}
			}
		}
		for _, hops := range pn.OutHops {
			for _, h := range hops {
				dfs(h.To)
			}
		}
	}

	// ---- 拓扑排序（Kahn）；环上的节点报错
	order, cyclic := topo(def.Nodes, p.ByID)
	for _, id := range cyclic {
		errf(id, "", "cycle", "存在环路：转换中的数据流不能回到上游")
	}

	// ---- 逐节点推导
	for _, id := range order {
		pn := p.ByID[id]
		p.Nodes = append(p.Nodes, pn)
		if pn.Blocked || pn.Type == nil {
			pn.Blocked = true
			continue
		}
		ok := true
		for port, hops := range pn.InHops {
			var merged *core.Schema
			for _, h := range hops {
				src := p.ByID[h.From]
				s := src.Outputs[h.FromPort]
				if src.Blocked || s == nil {
					ok = false
					break
				}
				if merged == nil {
					merged = s
				} else if err := merged.Compatible(s); err != nil {
					errf(pn.Node.ID, h.ID, "merge_schema", "汇入「%s」的多条连线字段布局不一致：%s", port, err.Error())
					ok = false
				}
			}
			if merged != nil {
				pn.Inputs[port] = merged
			}
		}
		if !ok {
			pn.Blocked = true
			continue
		}
		ic := core.NewInferContext(ctx, pn.Node.ID, opts.Env, p.VarKinds, pn.Inputs)
		ic.VarSamples = samples
		if err := pn.Type.Infer(ic, pn.Config); err != nil {
			ic.Errorf("", "infer", "%s", err.Error())
		}
		p.Problems = append(p.Problems, ic.Problems()...)
		if ic.Problems().HasErrors() {
			pn.Blocked = true
			continue
		}
		for port, s := range ic.Outputs() {
			pn.Outputs[port] = s
		}
		for _, port := range pn.Type.Meta().Outputs {
			if pn.Outputs[port.Name] == nil && len(pn.OutHops[port.Name]) > 0 {
				errf(pn.Node.ID, "", "no_output_schema", "「%s」未产出端口 %q 的字段定义（步骤实现缺陷）", pn.Node.Name, port.Name)
				pn.Blocked = true
			}
		}
		if pn.Type.Meta().ErrorPort && len(pn.OutHops[core.PortError]) > 0 {
			es, err := ic.ErrorBase().Append(core.ErrorFields()...)
			if err != nil {
				errf(pn.Node.ID, "", "error_schema", "错误输出字段与现有字段冲突：%s", err.Error())
			} else {
				pn.Outputs[core.PortError] = es
			}
		}
	}

	for _, pn := range p.Nodes {
		for port, hops := range pn.OutHops {
			for _, h := range hops {
				if s := pn.Outputs[port]; s != nil {
					p.HopSchemas[h.ID] = s
				}
			}
		}
	}
	if len(def.Nodes) == 0 {
		p.Problems = append(p.Problems, core.Problem{Severity: core.SeverityWarning, Code: "empty", Message: "转换还没有任何步骤"})
	}
	sort.SliceStable(p.Problems, func(i, j int) bool { return p.Problems[i].Severity < p.Problems[j].Severity })
	return p
}

// topo 返回拓扑序（按定义中的出现顺序稳定）与环上节点。
func topo(nodes []Node, byID map[string]*PlanNode) (order []string, cyclic []string) {
	indeg := map[string]int{}
	pos := map[string]int{}
	for i, n := range nodes {
		if _, ok := byID[n.ID]; ok {
			if _, seen := pos[n.ID]; !seen {
				pos[n.ID] = i
				indeg[n.ID] = 0
			}
		}
	}
	for id, pn := range byID {
		_ = id
		for _, hops := range pn.InHops {
			indeg[pn.Node.ID] += len(hops)
		}
	}
	var ready []string
	for id, d := range indeg {
		if d == 0 {
			ready = append(ready, id)
		}
	}
	byPos := func(s []string) { sort.Slice(s, func(i, j int) bool { return pos[s[i]] < pos[s[j]] }) }
	byPos(ready)
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		var next []string
		for _, hops := range byID[id].OutHops {
			for _, h := range hops {
				indeg[h.To]--
				if indeg[h.To] == 0 {
					next = append(next, h.To)
				}
			}
		}
		ready = append(ready, next...)
		byPos(ready)
	}
	for id, d := range indeg {
		if d > 0 {
			cyclic = append(cyclic, id)
		}
	}
	sort.Strings(cyclic)
	return order, cyclic
}
