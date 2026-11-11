package workflow

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"mooncell/console/internal/etl/core"
)

// 节点状态。
const (
	NodePending   = "pending"
	NodeRunning   = "running"
	NodeSucceeded = "succeeded"
	NodeFailed    = "failed"
	NodeSkipped   = "skipped"
	NodeCanceled  = "canceled"
	NodeReused    = "reused" // 续跑时复用上次成功结果
)

// NodeEvent 是作业项状态变化。
type NodeEvent struct {
	NodeID     string                `json:"nodeId"`
	NodeName   string                `json:"nodeName"`
	NodeType   string                `json:"nodeType"`
	Status     string                `json:"status"`
	Port       string                `json:"port,omitempty"`
	Attempt    int                   `json:"attempt"`
	Message    string                `json:"message,omitempty"`
	Error      string                `json:"error,omitempty"`
	Vars       map[string]core.Value `json:"vars,omitempty"`
	StartedAt  int64                 `json:"startedAt,omitempty"`
	FinishedAt int64                 `json:"finishedAt,omitempty"`
}

// Reused 是续跑时复用的上次结果。
type Reused struct {
	Port string
	Vars map[string]core.Value
}

// RunOptions 控制作业运行。
type RunOptions struct {
	RunID  string
	Env    core.Env
	Vars   *core.Vars // 作业作用域变量（已含参数与内置变量）
	Log    *core.Logger
	Reuse  map[string]Reused // 续跑：节点 ID → 上次成功结果
	OnNode func(NodeEvent)
}

// Result 是作业运行结果。
type Result struct {
	Status string // succeeded | failed | canceled
	Err    error
	Nodes  map[string]NodeEvent
}

// ErrAbort 由「中止」作业项返回。
var ErrAbort = errors.New("作业被「中止」作业项主动终止")

type nodeState struct {
	pn        *PlanNode
	pendingIn int
	activated bool
	resolved  bool
	ev        NodeEvent
}

type nodeDone struct {
	id  string
	ev  NodeEvent
	err error
}

// Run 执行作业。
func Run(ctx context.Context, plan *Plan, opts RunOptions) Result {
	res := Result{Nodes: map[string]NodeEvent{}}
	if plan.Problems.HasErrors() {
		res.Status, res.Err = NodeFailed, fmt.Errorf("作业存在错误，无法运行：%v", plan.Problems.First())
		return res
	}
	if opts.Vars == nil {
		opts.Vars = core.NewVars(nil)
	}
	if opts.Log == nil {
		opts.Log = core.NewLogger(nil, core.LevelError)
	}
	emit := func(ev NodeEvent) {
		res.Nodes[ev.NodeID] = ev
		if opts.OnNode != nil {
			opts.OnNode(ev)
		}
	}

	// 汇合节点只等「开始」可达的入线：不可达节点永远不会决出状态，若计入入度，
	// 汇合节点会一直挂起、最后被当作未触达跳过，作业却报成功。
	reach := plan.reachable()
	states := map[string]*nodeState{}
	for _, pn := range plan.Nodes {
		pending := 0
		for _, h := range pn.In {
			if reach[h.From] {
				pending++
			}
		}
		states[pn.Node.ID] = &nodeState{pn: pn, pendingIn: pending,
			ev: NodeEvent{NodeID: pn.Node.ID, NodeName: pn.Node.Name, NodeType: pn.Node.Type, Status: NodePending}}
	}
	if plan.Start == nil {
		res.Status = NodeSucceeded
		return res
	}

	var (
		ready     = []string{plan.Start.Node.ID}
		running   int
		done      = make(chan nodeDone)
		failed    error // 首个未处理失败
		stopping  bool
		maxPar    = plan.Settings.MaxParallel
		propagate func(id, port string, ok bool)
	)

	// resolve 节点已决：沿出线递减入度，激活匹配端口的连线；入线全决后执行或跳过。
	propagate = func(id, port string, completed bool) {
		st := states[id]
		st.resolved = true
		for p, hops := range st.pn.Out {
			for _, h := range hops {
				dst := states[h.To]
				if dst == nil {
					continue
				}
				dst.pendingIn--
				if completed && (p == port || p == PortAlways) {
					dst.activated = true
				}
				if dst.pendingIn == 0 {
					if dst.activated && !stopping {
						ready = append(ready, h.To)
					} else {
						dst.ev.Status = NodeSkipped
						emit(dst.ev)
						propagate(h.To, "", false)
					}
				}
			}
		}
	}

	launch := func(id string) {
		st := states[id]
		running++
		if r, ok := opts.Reuse[id]; ok {
			go func() {
				done <- nodeDone{id: id, ev: NodeEvent{Status: NodeReused, Port: r.Port, Vars: r.Vars, Message: "复用上次成功结果"}}
			}()
			return
		}
		st.ev.Status, st.ev.StartedAt = NodeRunning, time.Now().UnixMilli()
		emit(st.ev)
		go func() { done <- execute(ctx, st.pn, opts) }()
	}

	for len(ready) > 0 || running > 0 {
		for len(ready) > 0 && running < maxPar && !stopping && ctx.Err() == nil {
			id := ready[0]
			ready = ready[1:]
			launch(id)
		}
		if running == 0 {
			break
		}
		d := <-done
		running--
		st := states[d.id]
		ev := st.ev
		ev.Status, ev.Port, ev.Attempt, ev.Message, ev.Vars = d.ev.Status, d.ev.Port, d.ev.Attempt, d.ev.Message, d.ev.Vars
		ev.Error, ev.FinishedAt = d.ev.Error, time.Now().UnixMilli()
		if ev.StartedAt == 0 {
			ev.StartedAt = ev.FinishedAt
		}
		st.ev = ev
		emit(ev)

		switch ev.Status {
		case NodeSucceeded, NodeReused:
			for k, v := range ev.Vars {
				opts.Vars.Set(k, v)
			}
			propagate(d.id, ev.Port, true)
		case NodeFailed:
			handled := len(st.pn.Out[core.PortFailure]) > 0 || len(st.pn.Out[PortAlways]) > 0
			if errors.Is(d.err, ErrAbort) {
				handled = false
			}
			if handled {
				opts.Log.Warn("「%s」失败，已按失败分支继续：%s", st.pn.Node.Name, ev.Error)
				propagate(d.id, core.PortFailure, true)
			} else {
				if failed == nil {
					failed = fmt.Errorf("「%s」失败：%s", st.pn.Node.Name, ev.Error)
				}
				stopping = true // 快速失败：不再启动新作业项，在途的自然结束
				propagate(d.id, "", false)
			}
		default: // canceled
			propagate(d.id, "", false)
		}
	}

	// 未被触达的节点（不可达或因停止而未启动）
	for _, pn := range plan.Nodes {
		st := states[pn.Node.ID]
		if st.ev.Status == NodePending {
			st.ev.Status = NodeSkipped
			emit(st.ev)
		}
	}
	switch {
	case ctx.Err() != nil:
		res.Status, res.Err = NodeCanceled, context.Cause(ctx)
	case failed != nil:
		res.Status, res.Err = NodeFailed, failed
	default:
		res.Status = NodeSucceeded
	}
	return res
}

// execute 运行单个作业项（含超时与重试）。
func execute(ctx context.Context, pn *PlanNode, opts RunOptions) nodeDone {
	log := opts.Log.For(pn.Node.ID)
	attempts := 1 + pn.Node.Retry.Max
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			wait := time.Duration(pn.Node.Retry.BackoffSec) * time.Second
			log.Warn("第 %d 次重试（%s 后）", attempt-1, wait)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nodeDone{id: pn.Node.ID, ev: NodeEvent{Status: NodeCanceled, Attempt: attempt - 1, Error: ctx.Err().Error()}, err: ctx.Err()}
			}
		}
		actx := ctx
		var cancel context.CancelFunc = func() {}
		if pn.Node.TimeoutSec > 0 {
			actx, cancel = context.WithTimeout(ctx, time.Duration(pn.Node.TimeoutSec)*time.Second)
		}
		ec := &ExecContext{RunID: opts.RunID, NodeID: pn.Node.ID, NodeName: pn.Node.Name, Attempt: attempt, Vars: opts.Vars, Log: log, Env: opts.Env}
		out, err := safeExecute(actx, pn, ec)
		timedOut := actx.Err() == context.DeadlineExceeded && ctx.Err() == nil
		cancel()
		if err == nil {
			port := out.Port
			if port == "" {
				port = core.PortSuccess
			}
			if out.Message != "" {
				log.Info("%s", out.Message)
			}
			return nodeDone{id: pn.Node.ID, ev: NodeEvent{Status: NodeSucceeded, Port: port, Attempt: attempt, Message: out.Message, Vars: out.Vars}}
		}
		if timedOut {
			err = fmt.Errorf("超时（%ds）：%w", pn.Node.TimeoutSec, err)
		}
		if ctx.Err() != nil {
			return nodeDone{id: pn.Node.ID, ev: NodeEvent{Status: NodeCanceled, Attempt: attempt, Error: err.Error()}, err: err}
		}
		lastErr = err
		log.Error("「%s」失败（第 %d/%d 次）：%v", pn.Node.Name, attempt, attempts, err)
		if errors.Is(err, ErrAbort) {
			break
		}
	}
	return nodeDone{id: pn.Node.ID, ev: NodeEvent{Status: NodeFailed, Port: core.PortFailure, Attempt: attempts, Error: lastErr.Error()}, err: lastErr}
}

func safeExecute(ctx context.Context, pn *PlanNode, ec *ExecContext) (out Outcome, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("内部错误：%v", p)
			ec.Log.Error("作业项 panic：%v\n%s", p, debug.Stack())
		}
	}()
	return pn.Type.Execute(ctx, ec, pn.Config)
}
