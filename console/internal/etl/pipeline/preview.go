package pipeline

import (
	"context"
	"sync"

	"mooncell/console/internal/etl/core"
)

// PreviewResult 是某个步骤输出的预览。
type PreviewResult struct {
	NodeID   string                          `json:"nodeId"`
	Ports    map[string]PreviewPort          `json:"ports"` // main / true / false / error …
	Metrics  map[string]core.MetricsSnapshot `json:"metrics"`
	Logs     []core.LogEntry                 `json:"logs"`
	Error    string                          `json:"error,omitempty"`
	Problems core.Problems                   `json:"problems,omitempty"`
}

// PreviewPort 是一个输出端口的预览数据。
type PreviewPort struct {
	Fields    []core.Field `json:"fields"`
	Rows      [][]string   `json:"rows"` // 已格式化为展示文本（NULL → nil）
	Nulls     [][]bool     `json:"nulls"`
	Truncated bool         `json:"truncated"`
}

// Preview 只运行目标节点及其上游，收集目标节点各输出端口的前 limit 行；输出步骤处于预览模式不写库。
func Preview(ctx context.Context, def *Definition, nodeID string, limit int, opts CompileOptions, run RunOptions) PreviewResult {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	sub := upstream(def, nodeID)
	plan := Compile(ctx, sub, opts)
	res := PreviewResult{NodeID: nodeID, Ports: map[string]PreviewPort{}, Problems: plan.Problems}
	if plan.Problems.HasErrors() {
		res.Error = plan.Problems.First().Error()
		return res
	}
	pn := plan.ByID[nodeID]
	if pn == nil {
		res.Error = "步骤不存在"
		return res
	}
	sink := &core.MemorySink{}
	run.Log = core.NewLogger(sink, core.LevelDetail)
	run.Preview = true
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	c := &capture{nodeID: nodeID, limit: limit, done: func() { cancel(errPreviewDone) }, ports: map[string]*capPort{}}
	run.capture = c
	r := Run(ctx, plan, run)
	res.Metrics = r.Metrics
	res.Logs = sink.Snapshot()
	if r.Err != nil {
		res.Error = r.Err.Error()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for port, cp := range c.ports {
		pp := PreviewPort{Fields: cp.schema.Fields(), Truncated: cp.truncated}
		for _, row := range cp.rows {
			vals := make([]string, len(row))
			nulls := make([]bool, len(row))
			for i, v := range row {
				if v == nil {
					nulls[i] = true
					continue
				}
				vals[i] = core.FormatFieldValue(v, pp.Fields[i])
			}
			pp.Rows = append(pp.Rows, vals)
			pp.Nulls = append(pp.Nulls, nulls)
		}
		res.Ports[port] = pp
	}
	return res
}

// upstream 返回只含目标节点及其全部上游的子定义。
func upstream(def *Definition, target string) *Definition {
	keep := map[string]bool{target: true}
	for changed := true; changed; {
		changed = false
		for _, h := range def.Hops {
			if !h.Disabled && keep[h.To] && !keep[h.From] {
				keep[h.From] = true
				changed = true
			}
		}
	}
	sub := *def
	sub.Nodes = nil
	sub.Hops = nil
	for _, n := range def.Nodes {
		if keep[n.ID] {
			sub.Nodes = append(sub.Nodes, n)
		}
	}
	for _, h := range def.Hops {
		if keep[h.From] && keep[h.To] {
			sub.Hops = append(sub.Hops, h)
		}
	}
	return &sub
}

// capture 在目标节点的每个输出端口上挂一个收集通道。
type capture struct {
	nodeID string
	limit  int
	done   func()

	mu    sync.Mutex
	ports map[string]*capPort
	chans []chan core.Batch
	wg    sync.WaitGroup
}

// close 在目标节点结束后由运行器调用：关闭收集通道并等待收集协程排空。
func (c *capture) close() {
	for _, ch := range c.chans {
		close(ch)
	}
	c.wg.Wait()
}

type capPort struct {
	schema    *core.Schema
	rows      []core.Row
	truncated bool
}

// attach 在每个副本的输出端口挂收集通道；copies>1 时各副本汇入同一 capPort（行序不保证）。
func (c *capture) attach(rc *core.RunContext, pn *PlanNode) {
	for port, schema := range pn.Outputs {
		cp := c.ports[port]
		if cp == nil {
			cp = &capPort{schema: schema}
			c.ports[port] = cp
		}
		ch := make(chan core.Batch, 4)
		// 子定义中目标节点没有下游：输出只接收集通道
		targets := []core.Target{{Chans: []chan<- core.Batch{ch}}}
		rc.Outputs[port] = core.NewOutPort(schema, core.DistCopy, targets, rc.Metrics)
		c.chans = append(c.chans, ch)
		c.wg.Add(1)
		go c.collect(ch, cp)
	}
}

func (c *capture) collect(ch <-chan core.Batch, cp *capPort) {
	defer c.wg.Done()
	for b := range ch {
		c.mu.Lock()
		for _, r := range b.Rows {
			if len(cp.rows) >= c.limit {
				cp.truncated = true
				break
			}
			cp.rows = append(cp.rows, r)
		}
		full := len(cp.rows) >= c.limit
		c.mu.Unlock()
		if full {
			c.done()
		}
	}
}
