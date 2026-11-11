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

// node 是运行期的步骤组：copies=1 时只有一个副本。
type node struct {
	pn      *PlanNode
	metrics *core.Metrics
	reps    []*replica

	started   sync.Once    // 首个副本开始 → 状态 running、metrics.Start
	remaining atomic.Int32 // 未结束副本数，归零时发终态
	errOnce   sync.Once
	firstErr  error
}

// replica 是步骤的一个并行实例（自己的 RunContext / 端口 / 临时目录）。
type replica struct {
	idx    int
	rc     *core.RunContext
	runner core.Runner
	// outEdges 在副本结束时逐条 producerDone，最后一个生产者关闭目标通道。
	outEdges []*inbox
	inboxes  []*inbox
}

// inbox 是某个（节点, 副本, 输入端口）的通道与生产者计数。
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
	// inboxes[node][copy][port]：每个副本有独立输入通道；生产者数 = Σ 上游连线 × 上游副本数。
	inboxes := map[string][]map[string]*inbox{}
	copiesOf := func(pn *PlanNode) int {
		if pn.Node.Copies > 1 {
			return pn.Node.Copies
		}
		return 1
	}
	for _, pn := range plan.Nodes {
		nc := copiesOf(pn)
		rows := make([]map[string]*inbox, nc)
		for ci := 0; ci < nc; ci++ {
			rows[ci] = map[string]*inbox{}
		}
		inboxes[pn.Node.ID] = rows
	}
	for _, pn := range plan.Nodes {
		for port, hops := range pn.InHops {
			for _, h := range hops {
				srcCopies := int32(copiesOf(plan.ByID[h.From]))
				for ci := 0; ci < len(inboxes[pn.Node.ID]); ci++ {
					b := inboxes[pn.Node.ID][ci][port]
					if b == nil {
						b = &inbox{ch: make(chan core.Batch, settings.ChannelBatches)}
						inboxes[pn.Node.ID][ci][port] = b
					}
					b.producers.Add(srcCopies)
				}
			}
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

	// ---- 节点（copies>1 展开为多个副本，共享节点度量与节点级状态）
	nodes := make([]*node, 0, len(plan.Nodes))
	for _, pn := range plan.Nodes {
		m := &core.Metrics{}
		n := &node{pn: pn, metrics: m}
		nodeID := pn.Node.ID
		nc := copiesOf(pn)
		n.remaining.Store(int32(nc))
		for ci := 0; ci < nc; ci++ {
			rep := &replica{idx: ci}
			rc := &core.RunContext{
				NodeID: nodeID, NodeName: pn.Node.Name, Vars: opts.Vars, Log: opts.Log.For(nodeID),
				Metrics: m, Env: opts.Env, BatchSize: settings.BatchSize,
				MemoryBudget: int64(settings.MemoryBudgetMB) << 20, Preview: opts.Preview,
				Inputs: map[string]*core.InPort{}, Outputs: map[string]*core.OutPort{},
				Copies: nc, CopyIndex: ci,
			}
			if opts.OnReject != nil {
				rc.OnReject = func(row core.Row) { opts.OnReject(nodeID, row) }
			}
			tmpSuffix := "-" + safeName(nodeID) + "-"
			if nc > 1 {
				tmpSuffix = "-" + safeName(nodeID) + fmt.Sprintf("-%d-", ci)
			}
			rc.TempDir = func() (string, error) {
				tempMu.Lock()
				defer tempMu.Unlock()
				d, err := os.MkdirTemp(tempRoot, "mc-etl"+tmpSuffix)
				if err == nil {
					tempDirs = append(tempDirs, d)
				}
				return d, err
			}
			for port, b := range inboxes[nodeID][ci] {
				rc.Inputs[port] = core.NewInPort(pn.Inputs[port], b.ch, m)
				rep.inboxes = append(rep.inboxes, b)
			}
			for port, hops := range pn.OutHops {
				var targets []core.Target
				for _, h := range hops {
					// 一条连线一个 Target：其 Chans 覆盖目标节点全部副本；
					// 副本间分发方式取目标节点的 copiesDistribution（缺省轮询）。
					tg := core.Target{Mode: plan.ByID[h.To].Node.CopiesDistribution}
					for ti := range inboxes[h.To] {
						b := inboxes[h.To][ti][h.ToPort]
						if b == nil {
							continue
						}
						tg.Chans = append(tg.Chans, b.ch)
						rep.outEdges = append(rep.outEdges, b)
					}
					targets = append(targets, tg)
				}
				rc.Outputs[port] = core.NewOutPort(pn.Outputs[port], pn.Node.Distribution[port], targets, m)
			}
			if opts.capture != nil && opts.capture.nodeID == nodeID {
				opts.capture.attach(rc, pn)
			}
			rep.rc = rc
			n.reps = append(n.reps, rep)
		}
		nodes = append(nodes, n)
	}

	// ---- 打开（任何一个副本失败都不启动）
	for _, n := range nodes {
		for _, rep := range n.reps {
			r, err := safeOpen(n, rep)
			if err != nil {
				return Result{FailedNode: n.pn.Node.ID, Err: fmt.Errorf("「%s」初始化失败：%w", repName(n, rep), err), Metrics: snapshot(nodes)}
			}
			rep.runner = r
		}
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
	total := 0
	for _, n := range nodes {
		total += len(n.reps)
	}
	opts.Log.Info("转换开始，共 %d 个步骤（%d 个副本）", len(nodes), total)
	for _, n := range nodes {
		for _, rep := range n.reps {
			wg.Add(1)
			go func(n *node, rep *replica) {
				defer wg.Done()
				name := repName(n, rep)
				log := rep.rc.Log
				n.started.Do(func() {
					n.metrics.Start()
					if opts.OnNodeState != nil {
						opts.OnNodeState(n.pn.Node.ID, "running", nil)
					}
				})
				err := safeRun(ctx, n, rep)
				if err == nil {
					if e := flushErrorPort(ctx, rep); e != nil {
						err = e
					}
				}
				// 关闭下游：每条出边对应本副本一个生产者
				for _, b := range rep.outEdges {
					b.producerDone()
				}
				// 排空输入：副本提前正常结束时，上游不能因此阻塞
				for _, b := range rep.inboxes {
					go func(ch <-chan core.Batch) {
						for range ch {
						}
					}(b.ch)
				}
				if err != nil && !isCancel(ctx, err) {
					n.metrics.Errors.Add(1)
					log.Error("「%s」失败：%v", name, err)
					n.errOnce.Do(func() { n.firstErr = err })
					failOnce.Do(func() {
						failErr, failNode = fmt.Errorf("「%s」失败：%w", name, err), n.pn.Node.ID
						cancel(failErr)
					})
				}
				if n.remaining.Add(-1) == 0 {
					n.finish(opts, ctx)
				}
			}(n, rep)
		}
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

// repName 是副本的展示名：多副本加「#序号」后缀（1 起）。
func repName(n *node, rep *replica) string {
	if len(n.reps) > 1 {
		return fmt.Sprintf("%s#%d", n.pn.Node.Name, rep.idx+1)
	}
	return n.pn.Node.Name
}

// finish 在最后结束的副本里聚合节点终态并收尾：任一副本失败 → failed；
// 否则按运行 ctx 是否被取消区分 canceled / succeeded。
func (n *node) finish(opts RunOptions, ctx context.Context) {
	n.metrics.Finish()
	if opts.capture != nil && opts.capture.nodeID == n.pn.Node.ID {
		opts.capture.close()
	}
	var state string
	var err error
	if n.firstErr != nil {
		state, err = "failed", n.firstErr
	} else if ctx.Err() != nil {
		state = "canceled"
	} else {
		state = "succeeded"
		s := n.metrics.Snapshot()
		n.reps[0].rc.Log.Detail("「%s」完成：读 %d / 写 %d / 输入 %d / 输出 %d / 更新 %d / 拒绝 %d", n.pn.Node.Name, s.Read, s.Written, s.Input, s.Output, s.Updated, s.Rejected)
	}
	if opts.OnNodeState != nil {
		opts.OnNodeState(n.pn.Node.ID, state, err)
	}
}

func safeOpen(n *node, rep *replica) (r core.Runner, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("内部错误：%v", p)
			rep.rc.Log.Error("初始化 panic：%v\n%s", p, debug.Stack())
		}
	}()
	return n.pn.Type.Open(rep.rc, n.pn.Config)
}

func safeRun(ctx context.Context, n *node, rep *replica) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("内部错误：%v", p)
			rep.rc.Log.Error("运行 panic：%v\n%s", p, debug.Stack())
		}
	}()
	return rep.runner.Run(ctx)
}

func flushErrorPort(ctx context.Context, rep *replica) error {
	if p := rep.rc.Outputs[core.PortError]; p != nil {
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
