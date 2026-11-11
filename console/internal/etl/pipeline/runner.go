package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"mooncell/console/internal/etl/core"
)

// RunOptions 控制一次运行。
type RunOptions struct {
	Env      core.Env
	Vars     *core.Vars
	Log      *core.Logger
	TempRoot string // 有状态步骤溢写目录的根；空 = os.TempDir()
	Preview  bool   // 输出步骤不写库

	// OnMetrics 周期（约 1s）与结束时回调全部步骤度量快照；可为 nil。
	OnMetrics func(map[string]core.MetricsSnapshot)
	// OnNodeState 在步骤开始/结束时回调；可为 nil。
	OnNodeState func(nodeID, state string, err error)
	// OnReject 在某步骤把错误行写入 error 端口时回调（含追加的错误字段）；可为 nil。
	OnReject func(nodeID string, row core.Row)

	// capture 由 Preview() 设置：在目标节点的输出上挂接收集器。
	capture *capture
}

// Result 是运行结果。
type Result struct {
	Metrics    map[string]core.MetricsSnapshot
	FailedNode string
	Err        error
}

// ErrNotRunnable 表示计划存在阻断性问题。
var ErrNotRunnable = errors.New("转换存在错误，无法运行")

// errPreviewDone 是预览收集够行数后的主动取消原因（不算失败）。
var errPreviewDone = errors.New("preview done")

// node 是运行期的步骤实例。
type node struct {
	pn      *PlanNode
	rc      *core.RunContext
	runner  core.Runner
	metrics *core.Metrics
	// done 在步骤结束时对每条出边调用一次，最后一个生产者关闭目标通道。
	outEdges []*inbox
	inboxes  []*inbox
}

// inbox 是某个（节点, 输入端口）的通道与生产者计数。
type inbox struct {
	ch        chan core.Batch
	producers atomic.Int32
}

func (b *inbox) producerDone() {
	if b.producers.Add(-1) == 0 {
		close(b.ch)
	}
}

// Run 执行计划。ctx 取消即中止（Result.Err = ctx 错误）。
func Run(ctx context.Context, plan *Plan, opts RunOptions) Result {
	if plan.Problems.HasErrors() {
		return Result{Err: fmt.Errorf("%w：%v", ErrNotRunnable, plan.Problems.First())}
	}
	settings := plan.Settings
	if opts.Vars == nil {
		opts.Vars = core.NewVars(nil)
	}
	if opts.Log == nil {
		opts.Log = core.NewLogger(nil, core.LevelError)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	// ---- 通道
	inboxes := map[string]map[string]*inbox{} // node → port → inbox
	for _, pn := range plan.Nodes {
		for port, hops := range pn.InHops {
			if len(hops) == 0 {
				continue
			}
			b := &inbox{ch: make(chan core.Batch, settings.ChannelBatches)}
			b.producers.Store(int32(len(hops)))
			if inboxes[pn.Node.ID] == nil {
				inboxes[pn.Node.ID] = map[string]*inbox{}
			}
			inboxes[pn.Node.ID][port] = b
		}
	}

	// ---- 临时目录（惰性创建，结束清理）
	var tempMu sync.Mutex
	var tempDirs []string
	tempRoot := opts.TempRoot
	if tempRoot == "" {
		tempRoot = os.TempDir()
	}
	defer func() {
		for _, d := range tempDirs {
			os.RemoveAll(d)
		}
	}()

	// ---- 节点
	nodes := make([]*node, 0, len(plan.Nodes))
	for _, pn := range plan.Nodes {
		m := &core.Metrics{}
		n := &node{pn: pn, metrics: m}
		rc := &core.RunContext{
			NodeID: pn.Node.ID, NodeName: pn.Node.Name, Vars: opts.Vars, Log: opts.Log.For(pn.Node.ID),
			Metrics: m, Env: opts.Env, BatchSize: settings.BatchSize,
			MemoryBudget: int64(settings.MemoryBudgetMB) << 20, Preview: opts.Preview,
			Inputs: map[string]*core.InPort{}, Outputs: map[string]*core.OutPort{},
		}
		nodeID := pn.Node.ID
		if opts.OnReject != nil {
			rc.OnReject = func(row core.Row) { opts.OnReject(nodeID, row) }
		}
		rc.TempDir = func() (string, error) {
			tempMu.Lock()
			defer tempMu.Unlock()
			d, err := os.MkdirTemp(tempRoot, "mc-etl-"+safeName(nodeID)+"-")
			if err == nil {
				tempDirs = append(tempDirs, d)
			}
			return d, err
		}
		for port, b := range inboxes[pn.Node.ID] {
			rc.Inputs[port] = core.NewInPort(pn.Inputs[port], b.ch, m)
			n.inboxes = append(n.inboxes, b)
		}
		for port, hops := range pn.OutHops {
			var targets []core.Target
			for _, h := range hops {
				b := inboxes[h.To][h.ToPort]
				targets = append(targets, core.Target{Ch: b.ch})
				n.outEdges = append(n.outEdges, b)
			}
			rc.Outputs[port] = core.NewOutPort(pn.Outputs[port], pn.Node.Distribution[port], targets, m)
		}
		if opts.capture != nil && opts.capture.nodeID == pn.Node.ID {
			opts.capture.attach(rc, pn)
		}
		n.rc = rc
		nodes = append(nodes, n)
	}

	// ---- 打开（任何一个失败都不启动）
	for _, n := range nodes {
		r, err := safeOpen(n)
		if err != nil {
			return Result{FailedNode: n.pn.Node.ID, Err: fmt.Errorf("「%s」初始化失败：%w", n.pn.Node.Name, err), Metrics: snapshot(nodes)}
		}
		n.runner = r
	}

	// ---- 度量快照
	stopTick := make(chan struct{})
	tickDone := make(chan struct{})
	go func() {
		defer close(tickDone)
		if opts.OnMetrics == nil {
			return
		}
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				opts.OnMetrics(snapshot(nodes))
			case <-stopTick:
				return
			}
		}
	}()

	// ---- 运行
	var (
		wg       sync.WaitGroup
		failOnce sync.Once
		failErr  error
		failNode string
	)
	start := time.Now()
	opts.Log.Info("转换开始，共 %d 个步骤", len(nodes))
	for _, n := range nodes {
		wg.Add(1)
		go func(n *node) {
			defer wg.Done()
			name := n.pn.Node.Name
			log := n.rc.Log
			n.metrics.Start()
			if opts.OnNodeState != nil {
				opts.OnNodeState(n.pn.Node.ID, "running", nil)
			}
			err := safeRun(ctx, n)
			if err == nil {
				if e := flushErrorPort(ctx, n); e != nil {
					err = e
				}
			}
			n.metrics.Finish()
			// 关闭下游：每条出边对应一个生产者
			for _, b := range n.outEdges {
				b.producerDone()
			}
			if opts.capture != nil && opts.capture.nodeID == n.pn.Node.ID {
				opts.capture.close()
			}
			// 排空输入：步骤提前正常结束时，上游不能因此阻塞
			for _, b := range n.inboxes {
				go func(ch <-chan core.Batch) {
					for range ch {
					}
				}(b.ch)
			}
			state := "succeeded"
			if err != nil && !isCancel(ctx, err) {
				state = "failed"
				n.metrics.Errors.Add(1)
				log.Error("「%s」失败：%v", name, err)
				failOnce.Do(func() {
					failErr, failNode = fmt.Errorf("「%s」失败：%w", name, err), n.pn.Node.ID
					cancel(failErr)
				})
			} else if err != nil {
				state = "canceled"
			} else {
				s := n.metrics.Snapshot()
				log.Detail("「%s」完成：读 %d / 写 %d / 输入 %d / 输出 %d / 更新 %d / 拒绝 %d", name, s.Read, s.Written, s.Input, s.Output, s.Updated, s.Rejected)
			}
			if opts.OnNodeState != nil {
				opts.OnNodeState(n.pn.Node.ID, state, err)
			}
		}(n)
	}
	wg.Wait()
	close(stopTick)
	<-tickDone

	res := Result{Metrics: snapshot(nodes)}
	if opts.OnMetrics != nil {
		opts.OnMetrics(res.Metrics)
	}
	switch {
	case failErr != nil:
		res.Err, res.FailedNode = failErr, failNode
	case errors.Is(context.Cause(ctx), errPreviewDone):
		// 预览收集完成，属于正常结束
	case ctx.Err() != nil:
		res.Err = context.Cause(ctx)
	}
	if res.Err == nil {
		opts.Log.Info("转换完成，耗时 %s", time.Since(start).Round(time.Millisecond))
	}
	return res
}

func isCancel(ctx context.Context, err error) bool {
	return ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Cause(ctx)))
}

func safeOpen(n *node) (r core.Runner, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("内部错误：%v", p)
			n.rc.Log.Error("初始化 panic：%v\n%s", p, debug.Stack())
		}
	}()
	return n.pn.Type.Open(n.rc, n.pn.Config)
}

func safeRun(ctx context.Context, n *node) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("内部错误：%v", p)
			n.rc.Log.Error("运行 panic：%v\n%s", p, debug.Stack())
		}
	}()
	return n.runner.Run(ctx)
}

func flushErrorPort(ctx context.Context, n *node) error {
	if p := n.rc.Outputs[core.PortError]; p != nil {
		return p.FlushBuffered(ctx)
	}
	return nil
}

func snapshot(nodes []*node) map[string]core.MetricsSnapshot {
	out := make(map[string]core.MetricsSnapshot, len(nodes))
	for _, n := range nodes {
		out[n.pn.Node.ID] = n.metrics.Snapshot()
	}
	return out
}

func safeName(s string) string {
	b := []byte(filepath.Base(s))
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			b[i] = '_'
		}
	}
	return string(b)
}
