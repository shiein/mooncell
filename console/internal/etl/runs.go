package etl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"mooncell/console/internal/etl/actions"
	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/pipeline"
	"mooncell/console/internal/etl/store"
	"mooncell/console/internal/etl/workflow"
)

// TriggerOptions 描述一次运行请求。
type TriggerOptions struct {
	Trigger    string // manual | schedule | resume
	ScheduleID string
	Params     map[string]core.Value
	User       string
	ResumeOf   string
	// Serial：同一定义的运行串行执行（调度重叠策略 queue）。
	Serial bool
}

// runManager 负责运行的排队、并发上限、取消、事件广播与日志落库。
type runManager struct {
	s       *Service
	sem     chan struct{}
	mu      sync.Mutex
	active  map[string]*activeRun
	defSems map[string]chan struct{} // 定义级串行令牌（Serial 运行使用）
}

func newRunManager(s *Service) *runManager {
	return &runManager{s: s, sem: make(chan struct{}, s.cfg.MaxConcurrentRuns), active: map[string]*activeRun{}, defSems: map[string]chan struct{}{}}
}

func (m *runManager) defSem(defID string) chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch, ok := m.defSems[defID]
	if !ok {
		ch = make(chan struct{}, 1)
		m.defSems[defID] = ch
	}
	return ch
}

// activeRun 是在途运行的内存状态。
type activeRun struct {
	run    store.Run
	cancel context.CancelCauseFunc
	hub    *hub
	logs   *runLogs

	mu      sync.Mutex
	nodes   map[string]store.RunNode // key = parent + "/" + nodeID
	metrics map[string]map[string]core.MetricsSnapshot
	samples map[string]*store.ErrorSample // key 同 nodes：error 端口拒绝行采样
}

func nodeKey(parent, id string) string { return parent + "/" + id }

func (m *runManager) get(id string) *activeRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active[id]
}

func (m *runManager) activeNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var names []string
	for _, a := range m.active {
		names = append(names, a.run.DefinitionName)
	}
	sort.Strings(names)
	return names
}

func (m *runManager) cancelAll(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.active {
		a.cancel(errors.New(reason))
	}
}

// Trigger 创建运行并异步执行。
func (s *Service) Trigger(defID string, opts TriggerOptions) (store.Run, error) {
	if s.isDraining() {
		return store.Run{}, errors.New("Console 正在升级，暂不接受新的运行")
	}
	var (
		def      store.Definition
		snapshot json.RawMessage
		refs     json.RawMessage
		revision int
		err      error
	)
	if opts.ResumeOf != "" {
		prev, err := s.st.GetRun(opts.ResumeOf)
		if err != nil {
			return store.Run{}, err
		}
		if prev.Kind != "workflow" {
			return store.Run{}, errors.New("只有作业支持从失败处继续；转换请重新运行")
		}
		if prev.Status != store.RunFailed && prev.Status != store.RunInterrupted && prev.Status != store.RunCanceled {
			return store.Run{}, errors.New("只有失败、中断或取消的运行可以继续")
		}
		def = store.Definition{ID: prev.DefinitionID, Kind: prev.Kind, Name: prev.DefinitionName}
		snapshot, revision, refs = prev.Snapshot, prev.Revision, prev.Refs
		if opts.Params == nil {
			_ = json.Unmarshal(prev.Params, &opts.Params)
		}
	} else {
		if def, err = s.st.GetDefinition(defID); err != nil {
			return store.Run{}, err
		}
		snapshot, revision = def.Doc, def.Revision
		// 作业引用的转换定义随快照固定：执行与续跑都不再读后续修改。
		if def.Kind == "workflow" {
			collected, err := s.collectRefs(def.Doc)
			if err != nil {
				return store.Run{}, err
			}
			refs, _ = json.Marshal(collected)
		}
	}
	params, _ := json.Marshal(orEmpty(opts.Params))
	run, err := s.st.CreateRun(store.Run{
		Kind: def.Kind, DefinitionID: def.ID, DefinitionName: def.Name, Revision: revision, Trigger: opts.Trigger,
		ScheduleID: opts.ScheduleID, ResumeOf: opts.ResumeOf, Params: params, Snapshot: snapshot, Refs: refs, TriggeredBy: opts.User,
	})
	if err != nil {
		return run, err
	}
	ctx, cancel := context.WithCancelCause(s.ctx)
	a := &activeRun{run: run, cancel: cancel, hub: newHub(), nodes: map[string]store.RunNode{}, metrics: map[string]map[string]core.MetricsSnapshot{}}
	a.logs = newRunLogs(s.st, run.ID, s.cfg.RunLogLimit, a.hub)
	s.runs.mu.Lock()
	s.runs.active[run.ID] = a
	s.runs.mu.Unlock()
	go s.execute(ctx, a, opts)
	return run, nil
}

func orEmpty(m map[string]core.Value) map[string]core.Value {
	if m == nil {
		return map[string]core.Value{}
	}
	return m
}

// execute 是一次运行的完整生命周期：排队 → 运行 → 落库终态 → 广播结束。
func (s *Service) execute(ctx context.Context, a *activeRun, opts TriggerOptions) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("[etl] 运行 %s panic：%v", a.run.ID, p)
			s.finish(a, store.RunFailed, fmt.Errorf("内部错误：%v", p))
		}
	}()
	// 排队：先拿定义级串行令牌（如需要），再拿全局并发令牌
	if opts.Serial {
		ds := s.runs.defSem(a.run.DefinitionID)
		select {
		case ds <- struct{}{}:
			defer func() { <-ds }()
		case <-ctx.Done():
			s.finish(a, store.RunCanceled, context.Cause(ctx))
			return
		}
	}
	select {
	case s.runs.sem <- struct{}{}:
	case <-ctx.Done():
		s.finish(a, store.RunCanceled, context.Cause(ctx))
		return
	}
	defer func() { <-s.runs.sem }()

	if err := s.st.StartRun(a.run.ID); err != nil {
		s.finish(a, store.RunFailed, err)
		return
	}
	a.run.Status, a.run.StartedAt = store.RunRunning, time.Now().UnixMilli()
	a.hub.publish(event{Type: "status", Data: a.run})

	logger := core.NewLogger(a.logs, levelOf(a.run))
	vars := core.NewVars(nil)
	vars.Set("RUN_ID", core.NewValue(a.run.ID))
	vars.Set("RUN_START", core.NewValue(time.Now()))
	vars.Set("TRIGGER", core.NewValue(a.run.Trigger))

	var err error
	switch a.run.Kind {
	case "pipeline":
		err = s.executePipeline(ctx, a, vars, opts.Params, logger)
	case "workflow":
		err = s.executeWorkflow(ctx, a, vars, opts, logger)
	default:
		err = fmt.Errorf("未知的定义类型 %q", a.run.Kind)
	}
	status := store.RunSucceeded
	switch {
	case err == nil:
	case ctx.Err() != nil:
		status, err = store.RunCanceled, context.Cause(ctx)
	default:
		status = store.RunFailed
	}
	s.finish(a, status, err)
}

func levelOf(r store.Run) core.Level {
	var head struct {
		Settings struct {
			LogLevel string `json:"logLevel"`
		} `json:"settings"`
	}
	_ = json.Unmarshal(r.Snapshot, &head)
	return core.ParseLevel(head.Settings.LogLevel)
}

// applyParams 按声明把默认值与覆盖值写入变量；类型不符时转换，失败报错。
func applyParams(vars *core.Vars, decls []core.ParamDecl, overrides map[string]core.Value) error {
	for _, d := range decls {
		v, ok := overrides[d.Name]
		if !ok && d.Default != nil {
			v, ok = *d.Default, true
		}
		if !ok {
			if d.Required {
				return fmt.Errorf("缺少必填参数 %s", d.Name)
			}
			vars.Set(d.Name, core.Value{Kind: d.Kind})
			continue
		}
		if v.V != nil && v.Kind != d.Kind {
			cv, err := core.Convert(v.V, d.Kind, core.ConvertOptions{})
			if err != nil {
				return fmt.Errorf("参数 %s：%w", d.Name, err)
			}
			v = core.Value{Kind: d.Kind, V: cv}
		}
		v.Kind = d.Kind
		vars.Set(d.Name, v)
	}
	return nil
}

func (s *Service) env(withRunner actions.PipelineRunner) core.Env {
	pk, pv := s.pool.EnvEntry()
	e := core.MapEnv{pk: pv}
	sk, sv := actions.SecretEntry(s.box)
	e[sk] = sv
	if withRunner != nil {
		rk, rv := actions.RunnerEntry(withRunner)
		e[rk] = rv
	}
	return e
}

func (s *Service) executePipeline(ctx context.Context, a *activeRun, vars *core.Vars, params map[string]core.Value, logger *core.Logger) error {
	def, err := pipeline.Decode(a.run.Snapshot)
	if err != nil {
		return err
	}
	if err := applyParams(vars, def.Params, params); err != nil {
		return err
	}
	_, err = s.runPipeline(ctx, a, "", def, vars, logger)
	return err
}

// maxErrorSamples 是单节点 error 端口拒绝行的采样上限（避免运行记录膨胀）。
const maxErrorSamples = 20

// maxSampleValueLen 限制采样中单值长度（大字段截断，防止样本行过大）。
const maxSampleValueLen = 512

// runPipeline 编译并运行转换，步骤状态以 parent 归属写入运行节点。
func (s *Service) runPipeline(ctx context.Context, a *activeRun, parent string, def *pipeline.Definition, vars *core.Vars, logger *core.Logger) (pipeline.Result, error) {
	e := s.env(nil)
	plan := pipeline.Compile(ctx, def, pipeline.CompileOptions{Env: e, ExtraVars: kindsOf(vars)})
	if plan.Problems.HasErrors() {
		for _, p := range plan.Problems {
			if p.Severity == core.SeverityError {
				logger.For(p.NodeID).Error("%s", p.Message)
			}
		}
		return pipeline.Result{}, fmt.Errorf("转换编译失败：%v", plan.Problems.First())
	}
	// 下推优化：「表输入 → 过滤 → 字段选择」折叠为源端 SQL。改写后重编译失败则回退原计划（仅少优化，不改变语义）。
	if pd, notes := pipeline.Pushdown(ctx, def, plan, e); len(notes) > 0 {
		if p2 := pipeline.Compile(ctx, pd, pipeline.CompileOptions{Env: e, ExtraVars: kindsOf(vars)}); !p2.Problems.HasErrors() {
			for _, n := range notes {
				logger.Detail("下推优化：%s 并入「%s」的源端 SQL", strings.Join(n.From, "、"), n.Into)
			}
			plan = p2
		}
	}
	// error 端口连线的步骤：采样其拒绝行供运行详情查看。
	errFields := map[string][]string{}
	for _, pn := range plan.Nodes {
		a.putNode(s, store.RunNode{RunID: a.run.ID, NodeID: pn.Node.ID, NodeName: pn.Node.Name, NodeType: pn.Node.Type, ParentNode: parent, Status: "pending"}, false)
		if es := pn.Outputs[core.PortError]; es != nil {
			errFields[pn.Node.ID] = es.Names()
		}
	}
	plog := logger
	if parent != "" {
		plog = logger.WithPrefix(parent + "/")
	}
	res := pipeline.Run(ctx, plan, pipeline.RunOptions{
		Env: e, Vars: vars, Log: plog, TempRoot: s.cfg.TempDir,
		OnMetrics: func(m map[string]core.MetricsSnapshot) { a.setMetrics(parent, m) },
		OnReject: func(id string, row core.Row) {
			a.addSample(parent, id, errFields[id], row)
		},
		OnNodeState: func(id, state string, err error) {
			a.updateNode(s, parent, id, func(n *store.RunNode) {
				n.Status = state
				now := time.Now().UnixMilli()
				if state == "running" {
					n.StartedAt = now
				} else {
					n.FinishedAt = now
				}
				if err != nil && state == "failed" {
					n.Error = err.Error()
				}
			})
		},
	})
	a.setMetrics(parent, res.Metrics)
	a.flushNodes(s, parent)
	return res, res.Err
}

func kindsOf(vars *core.Vars) map[string]core.Kind {
	m := map[string]core.Kind{}
	for k, v := range vars.Flatten() {
		m[k] = v.Kind
	}
	return m
}

// runRef 是随运行快照保存的被引定义。
type runRef struct {
	Kind     string          `json:"kind"`
	Name     string          `json:"name"`
	Revision int             `json:"revision"`
	Doc      json.RawMessage `json:"doc"`
}

// collectRefs 收集作业定义引用的全部定义文档（含被引作业的传递引用）。
func (s *Service) collectRefs(doc json.RawMessage) (map[string]runRef, error) {
	out := map[string]runRef{}
	wf, err := workflow.Decode(doc)
	if err != nil {
		return nil, err // 与执行时同样的解码失败，尽早暴露
	}
	queue := workflow.CollectRefs(wf)
	for len(queue) > 0 {
		r := queue[0]
		queue = queue[1:]
		if _, ok := out[r.ID]; ok {
			continue
		}
		d, err := s.st.GetDefinition(r.ID)
		if err != nil {
			return nil, fmt.Errorf("读取作业引用的定义失败（%s）：%w", r.ID, err)
		}
		out[r.ID] = runRef{Kind: d.Kind, Name: d.Name, Revision: d.Revision, Doc: d.Doc}
		if d.Kind == "workflow" { // 传递引用（嵌套作业项声明 Refs 时生效）
			sub, err := workflow.Decode(d.Doc)
			if err != nil {
				return nil, fmt.Errorf("解析作业引用的定义失败（%s）：%w", d.Name, err)
			}
			queue = append(queue, workflow.CollectRefs(sub)...)
		}
	}
	return out, nil
}

// executeWorkflow 运行作业；续跑时复用上次成功节点。
func (s *Service) executeWorkflow(ctx context.Context, a *activeRun, vars *core.Vars, opts TriggerOptions, logger *core.Logger) error {
	def, err := workflow.Decode(a.run.Snapshot)
	if err != nil {
		return err
	}
	if err := applyParams(vars, def.Params, opts.Params); err != nil {
		return err
	}
	host := &pipelineHost{s: s, a: a, defs: map[string]runRef{}}
	_ = json.Unmarshal(a.run.Refs, &host.defs)
	e := s.env(host)
	plan := workflow.Compile(ctx, def, workflow.CompileOptions{Env: e})
	if plan.Problems.HasErrors() {
		return fmt.Errorf("作业编译失败：%v", plan.Problems.First())
	}
	reuse := map[string]workflow.Reused{}
	if opts.ResumeOf != "" {
		prevNodes, err := s.st.ListRunNodes(opts.ResumeOf)
		if err != nil {
			return err
		}
		for _, n := range prevNodes {
			if n.ParentNode == "" && (n.Status == workflow.NodeSucceeded || n.Status == workflow.NodeReused) {
				reuse[n.NodeID] = workflow.Reused{Port: n.Port, Vars: n.OutputVars}
			}
		}
		logger.Info("从运行 %s 继续：复用 %d 个已成功的作业项", opts.ResumeOf, len(reuse))
	}
	for _, pn := range plan.Nodes {
		a.putNode(s, store.RunNode{RunID: a.run.ID, NodeID: pn.Node.ID, NodeName: pn.Node.Name, NodeType: pn.Node.Type, Status: workflow.NodePending}, false)
	}
	res := workflow.Run(ctx, plan, workflow.RunOptions{
		RunID: a.run.ID, Env: e, Vars: vars, Log: logger, Reuse: reuse,
		OnNode: func(ev workflow.NodeEvent) {
			a.updateNode(s, "", ev.NodeID, func(n *store.RunNode) {
				n.Status, n.Port, n.Attempt, n.Error = ev.Status, ev.Port, ev.Attempt, ev.Error
				n.StartedAt, n.FinishedAt = ev.StartedAt, ev.FinishedAt
				if ev.Vars != nil {
					n.OutputVars = ev.Vars
				}
			})
		},
	})
	a.flushNodes(s, "")
	return res.Err
}

// pipelineHost 实现 actions.PipelineRunner：作业中运行转换，结果归入同一运行。
// 被引定义优先取运行快照（defs）；快照没有（旧运行）时回落当前定义。
type pipelineHost struct {
	s     *Service
	a     *activeRun
	defs  map[string]runRef
	mu    sync.Mutex
	depth int // 子作业嵌套深度（并发分支各自计数取最大值）
}

// refDoc 取引用定义：快照优先，回落当前库（旧运行没有 refs）。
func (h *pipelineHost) refDoc(id string) (runRef, error) {
	if r, ok := h.defs[id]; ok {
		return r, nil
	}
	d, err := h.s.st.GetDefinition(id)
	if err != nil {
		return runRef{}, err
	}
	return runRef{Kind: d.Kind, Name: d.Name, Revision: d.Revision, Doc: d.Doc}, nil
}

func (h *pipelineHost) PipelineParams(ctx context.Context, id string) (string, []core.ParamDecl, error) {
	d, err := h.refDoc(id)
	if err != nil {
		return "", nil, err
	}
	if d.Kind != "pipeline" {
		return "", nil, errors.New("目标不是转换")
	}
	def, err := pipeline.Decode(d.Doc)
	if err != nil {
		return "", nil, err
	}
	return d.Name, def.Params, nil
}

// maxSubWorkflowDepth 子作业嵌套上限（防循环引用）。
const maxSubWorkflowDepth = 8

func (h *pipelineHost) WorkflowParams(ctx context.Context, id string) (string, []core.ParamDecl, error) {
	d, err := h.refDoc(id)
	if err != nil {
		return "", nil, err
	}
	if d.Kind != "workflow" {
		return "", nil, errors.New("目标不是作业")
	}
	def, err := workflow.Decode(d.Doc)
	if err != nil {
		return "", nil, err
	}
	return d.Name, def.Params, nil
}

func (h *pipelineHost) RunWorkflow(ctx context.Context, ec *workflow.ExecContext, id string, params map[string]core.Value) (actions.WorkflowSummary, error) {
	h.mu.Lock()
	h.depth++
	depth := h.depth
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.depth--
		h.mu.Unlock()
	}()
	if depth > maxSubWorkflowDepth {
		return actions.WorkflowSummary{}, fmt.Errorf("子作业嵌套超过 %d 层（可能存在循环引用）", maxSubWorkflowDepth)
	}
	d, err := h.refDoc(id)
	if err != nil {
		return actions.WorkflowSummary{}, fmt.Errorf("读取作业失败：%w", err)
	}
	if d.Kind != "workflow" {
		return actions.WorkflowSummary{}, errors.New("目标不是作业")
	}
	def, err := workflow.Decode(d.Doc)
	if err != nil {
		return actions.WorkflowSummary{}, err
	}
	ec.Log.Info("运行子作业「%s」（版本 %d）", d.Name, d.Revision)
	e := h.s.env(h)
	plan := workflow.Compile(ctx, def, workflow.CompileOptions{Env: e})
	if plan.Problems.HasErrors() {
		return actions.WorkflowSummary{}, fmt.Errorf("子作业编译失败：%v", plan.Problems.First())
	}
	vars := core.NewVars(ec.Vars)
	if err := applyParams(vars, def.Params, params); err != nil {
		return actions.WorkflowSummary{}, err
	}
	parent := ec.NodeID
	for _, pn := range plan.Nodes {
		h.a.putNode(h.s, store.RunNode{RunID: h.a.run.ID, NodeID: pn.Node.ID, NodeName: pn.Node.Name, NodeType: pn.Node.Type, ParentNode: parent, Status: workflow.NodePending}, false)
	}
	res := workflow.Run(ctx, plan, workflow.RunOptions{
		RunID: h.a.run.ID, Env: e, Vars: vars, Log: ec.Log.WithPrefix(parent + "/"),
		OnNode: func(ev workflow.NodeEvent) {
			h.a.updateNode(h.s, parent, ev.NodeID, func(n *store.RunNode) {
				n.Status, n.Port, n.Attempt, n.Error = ev.Status, ev.Port, ev.Attempt, ev.Error
				n.StartedAt, n.FinishedAt = ev.StartedAt, ev.FinishedAt
				if ev.Vars != nil {
					n.OutputVars = ev.Vars
				}
			})
		},
	})
	h.a.flushNodes(h.s, parent)
	var sum actions.WorkflowSummary
	for _, ev := range res.Nodes {
		switch ev.Status {
		case workflow.NodeSucceeded, workflow.NodeReused:
			sum.Succeeded++
		case workflow.NodeFailed:
			sum.Failed++
		case workflow.NodeSkipped, workflow.NodeCanceled, workflow.NodePending:
			sum.Skipped++
		}
	}
	return sum, res.Err
}

func (h *pipelineHost) RunPipeline(ctx context.Context, ec *workflow.ExecContext, id string, params map[string]core.Value) (actions.PipelineSummary, error) {
	d, err := h.refDoc(id)
	if err != nil {
		return actions.PipelineSummary{}, fmt.Errorf("读取转换失败：%w", err)
	}
	def, err := pipeline.Decode(d.Doc)
	if err != nil {
		return actions.PipelineSummary{}, err
	}
	ec.Log.Info("运行转换「%s」（版本 %d）", d.Name, d.Revision)
	vars := core.NewVars(ec.Vars)
	if err := applyParams(vars, def.Params, params); err != nil {
		return actions.PipelineSummary{}, err
	}
	res, err := h.s.runPipeline(ctx, h.a, ec.NodeID, def, vars, ec.Log)
	var sum actions.PipelineSummary
	for _, m := range res.Metrics {
		sum.Read += m.Input
		sum.Written += m.Output + m.Updated
		sum.Rejected += m.Rejected
	}
	return sum, err
}

// ---------------------------------------------------------------- 节点状态与度量

// putNode 写入节点初始状态。
func (a *activeRun) putNode(s *Service, n store.RunNode, persist bool) {
	a.mu.Lock()
	a.nodes[nodeKey(n.ParentNode, n.NodeID)] = n
	a.mu.Unlock()
	if persist {
		_ = s.st.UpsertRunNode(n)
	}
	a.hub.publish(event{Type: "node", Data: n})
}

// addSample 采样一条 error 端口拒绝行（上限 maxErrorSamples，Total 持续计数）。
func (a *activeRun) addSample(parent, id string, fields []string, row core.Row) {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := nodeKey(parent, id)
	if a.samples == nil {
		a.samples = map[string]*store.ErrorSample{}
	}
	es := a.samples[k]
	if es == nil {
		es = &store.ErrorSample{Fields: fields}
		a.samples[k] = es
	}
	es.Total++
	if len(es.Rows) >= maxErrorSamples {
		return
	}
	out := make([]any, len(row))
	for i, v := range row {
		out[i] = sampleValue(v)
	}
	if _, err := json.Marshal(out); err != nil {
		return // 不可序列化的行不采样（不应发生：行值均为受支持类型）
	}
	es.Rows = append(es.Rows, out)
}

// sampleValue 截断过大的单值，保证运行记录体积受控。
func sampleValue(v any) any {
	switch t := v.(type) {
	case string:
		if len(t) > maxSampleValueLen {
			return t[:maxSampleValueLen] + "…"
		}
		return t
	case []byte:
		if len(t) > maxSampleValueLen {
			return fmt.Sprintf("%x…(%d 字节)", t[:16], len(t))
		}
		return fmt.Sprintf("%x", t)
	default:
		return v
	}
}

// attachSamples 把已采集的拒绝行样本挂到节点上（调用方须持 a.mu）。
func (a *activeRun) attachSamples(k string, n *store.RunNode) {
	if es := a.samples[k]; es != nil {
		n.Samples = es
	}
}

// updateNode 修改并持久化节点状态（状态变化频率低，直接落库）。
func (a *activeRun) updateNode(s *Service, parent, id string, fn func(*store.RunNode)) {
	a.mu.Lock()
	k := nodeKey(parent, id)
	n := a.nodes[k]
	n.RunID, n.NodeID, n.ParentNode = a.run.ID, id, parent
	fn(&n)
	if m, ok := a.metrics[parent][id]; ok {
		n.Metrics = m
	}
	a.attachSamples(k, &n)
	a.nodes[k] = n
	a.mu.Unlock()
	if err := s.st.UpsertRunNode(n); err != nil {
		log.Printf("[etl] 保存节点状态失败：%v", err)
	}
	a.hub.publish(event{Type: "node", Data: n})
}

func (a *activeRun) setMetrics(parent string, m map[string]core.MetricsSnapshot) {
	if m == nil {
		return
	}
	a.mu.Lock()
	a.metrics[parent] = m
	for id, snap := range m {
		k := nodeKey(parent, id)
		if n, ok := a.nodes[k]; ok {
			n.Metrics = snap
			a.nodes[k] = n
		}
	}
	a.mu.Unlock()
	a.hub.publish(event{Type: "metrics", Data: map[string]any{"parent": parent, "metrics": m}})
}

// flushNodes 把某层节点（含最终度量与错误行样本）落库。
func (a *activeRun) flushNodes(s *Service, parent string) {
	a.mu.Lock()
	var list []store.RunNode
	for k, n := range a.nodes {
		if n.ParentNode == parent {
			a.attachSamples(k, &n)
			list = append(list, n)
		}
	}
	a.mu.Unlock()
	for _, n := range list {
		if err := s.st.UpsertRunNode(n); err != nil {
			log.Printf("[etl] 保存节点失败：%v", err)
		}
	}
}

// finish 写入终态、汇总行数、广播结束并移出活动表。
func (s *Service) finish(a *activeRun, status string, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
		a.logs.Write(core.LogEntry{TS: time.Now().UnixMilli(), Level: "error", Message: "运行结束：" + msg})
	} else {
		a.logs.Write(core.LogEntry{TS: time.Now().UnixMilli(), Level: "info", Message: "运行成功结束"})
	}
	var read, written int64
	a.mu.Lock()
	for _, layer := range a.metrics {
		for _, m := range layer {
			read += m.Input
			written += m.Output + m.Updated
		}
	}
	a.mu.Unlock()
	count, dropped := a.logs.close()
	if e := s.st.FinishRun(a.run.ID, status, msg, read, written, count, dropped); e != nil {
		log.Printf("[etl] 保存运行终态失败：%v", e)
	}
	a.run.Status, a.run.Error, a.run.RowsRead, a.run.RowsWritten = status, msg, read, written
	a.run.FinishedAt = time.Now().UnixMilli()
	a.hub.publish(event{Type: "status", Data: a.run})
	a.hub.close()
	s.runs.mu.Lock()
	delete(s.runs.active, a.run.ID)
	s.runs.mu.Unlock()
	a.cancel(nil)
	result := "成功"
	if status != store.RunSucceeded {
		result = "失败：" + status
	}
	s.audit(a.run.TriggeredBy, "运行"+kindLabel(a.run.Kind), "数据集成·"+a.run.DefinitionName, result)
}

func kindLabel(k string) string {
	if k == "workflow" {
		return "作业"
	}
	return "转换"
}

// Cancel 取消在途运行。
func (s *Service) Cancel(runID, user string) error {
	a := s.runs.get(runID)
	if a == nil {
		return errors.New("运行不在进行中")
	}
	a.logs.Write(core.LogEntry{TS: time.Now().UnixMilli(), Level: "warn", Message: "由 " + user + " 取消"})
	a.cancel(fmt.Errorf("由 %s 取消", user))
	return nil
}

// ---------------------------------------------------------------- 日志：内存缓冲 + 限量批量落库 + 广播

type runLogs struct {
	st    *store.Store
	runID string
	limit int64
	hub   *hub

	mu      sync.Mutex
	seq     int64
	stored  int64
	dropped int64
	buf     []core.LogEntry
	closed  bool
	stop    chan struct{}
	done    chan struct{}
}

func newRunLogs(st *store.Store, runID string, limit int, h *hub) *runLogs {
	l := &runLogs{st: st, runID: runID, limit: int64(limit), hub: h, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(l.done)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				l.flush()
			case <-l.stop:
				l.flush()
				return
			}
		}
	}()
	return l
}

// Write 实现 core.LogSink。超过上限的日志只计数（error 级始终保留）。
func (l *runLogs) Write(e core.LogEntry) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.seq++
	e.Seq = l.seq
	keep := l.stored < l.limit || e.Level == "error"
	if keep {
		l.stored++
		l.buf = append(l.buf, e)
	} else {
		l.dropped++
	}
	l.mu.Unlock()
	if keep {
		l.hub.publish(event{Type: "log", Data: e})
	}
}

func (l *runLogs) flush() {
	l.mu.Lock()
	buf := l.buf
	l.buf = nil
	l.mu.Unlock()
	if err := l.st.AppendLogs(l.runID, buf); err != nil {
		log.Printf("[etl] 写入运行日志失败：%v", err)
	}
}

func (l *runLogs) close() (count, dropped int64) {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.stop)
	}
	l.mu.Unlock()
	<-l.done
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stored, l.dropped
}

// ---------------------------------------------------------------- 事件广播（SSE 订阅）

type event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

type hub struct {
	mu     sync.Mutex
	subs   map[chan event]struct{}
	closed bool
}

func newHub() *hub { return &hub{subs: map[chan event]struct{}{}} }

func (h *hub) subscribe() (chan event, func()) {
	ch := make(chan event, 256)
	h.mu.Lock()
	if h.closed {
		close(ch)
		h.mu.Unlock()
		return ch, func() {}
	}
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// publish 非阻塞投递：慢订阅者丢事件（客户端可用 REST 补齐），绝不拖慢运行。
func (h *hub) publish(e event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for ch := range h.subs {
		close(ch)
		delete(h.subs, ch)
	}
}

// ---------------------------------------------------------------- 调度触发

// fireSchedule 按重叠策略创建运行。
func (s *Service) fireSchedule(ctx context.Context, sc store.Schedule, firedAt time.Time) error {
	if s.isDraining() {
		return errors.New("Console 正在升级，本次触发跳过")
	}
	active, err := s.st.ActiveRunCount(sc.TargetID)
	if err != nil {
		return err
	}
	switch sc.Overlap {
	case "parallel":
	case "queue":
		if active > 1 { // 至多 1 个运行中 + 1 个排队
			log.Printf("[etl] 调度「%s」：已有运行与排队，本次跳过", sc.Name)
			return nil
		}
	default: // skip
		if active > 0 {
			log.Printf("[etl] 调度「%s」：上一次运行尚未结束，本次跳过", sc.Name)
			return nil
		}
	}
	_, err = s.Trigger(sc.TargetID, TriggerOptions{Trigger: "schedule", ScheduleID: sc.ID, Params: sc.Params,
		User: "调度:" + sc.Name, Serial: sc.Overlap == "queue"})
	return err
}
