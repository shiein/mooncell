package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Category 是组件分类，决定面板分组与节点配色。
type Category string

const (
	CategoryInput     Category = "input"
	CategoryTransform Category = "transform"
	CategoryFlow      Category = "flow"
	CategoryLookup    Category = "lookup"
	CategoryJoin      Category = "join"
	CategoryOutput    Category = "output"
	CategoryControl   Category = "control" // 作业项
	CategoryUtility   Category = "utility"
)

// Maturity 标记组件成熟度：planned 只出现在面板（置灰），不可编译。
type Maturity string

const (
	MaturityStable  Maturity = "stable"
	MaturityPreview Maturity = "preview"
	MaturityPlanned Maturity = "planned"
)

// 标准端口名。
const (
	PortMain    = "main"
	PortError   = "error"
	PortTrue    = "true"
	PortFalse   = "false"
	PortSuccess = "success"
	PortFailure = "failure"
)

// PortDecl 声明一个命名端口。
type PortDecl struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	// Multi：输入端口是否允许多条连线汇入（汇流要求 schema 兼容）；输出端口恒允许多条连线。
	Multi bool `json:"multi,omitempty"`
	// Info：信息端口（如流查询的 lookup 源），整流读完后主流才开始。
	Info     bool `json:"info,omitempty"`
	Required bool `json:"required,omitempty"`
	// Tone：界面语义色（success/error/warn/info），空 = 默认。
	Tone string `json:"tone,omitempty"`
}

// Meta 是组件的静态描述，前端面板与属性检查器据此渲染。
type Meta struct {
	ID          string     `json:"id"`
	Label       string     `json:"label"`
	Category    Category   `json:"category"`
	Icon        string     `json:"icon"`
	Description string     `json:"description"`
	Inputs      []PortDecl `json:"inputs"`
	Outputs     []PortDecl `json:"outputs"`
	ErrorPort   bool       `json:"errorPort,omitempty"` // 支持把出错行转到 error 端口
	Version     int        `json:"version"`             // 当前配置结构版本
	Maturity    Maturity   `json:"maturity"`
	Keywords    []string   `json:"keywords,omitempty"` // 面板搜索同义词（如 Kettle 原名）
	Order       int        `json:"order"`              // 分类内的展示顺序（小在前），相同则按 ID
}

// InputPort 返回指定名称的输入端口声明。
func (m Meta) InputPort(name string) (PortDecl, bool) {
	for _, p := range m.Inputs {
		if p.Name == name {
			return p, true
		}
	}
	return PortDecl{}, false
}

// OutputPort 返回指定名称的输出端口声明（含隐式 error 端口）。
func (m Meta) OutputPort(name string) (PortDecl, bool) {
	for _, p := range m.Outputs {
		if p.Name == name {
			return p, true
		}
	}
	if m.ErrorPort && name == PortError {
		return PortDecl{Name: PortError, Label: "错误", Tone: "error"}, true
	}
	return PortDecl{}, false
}

// Env 是宿主注入的服务定位器（连接池、状态存储等），沿用 context.Value 的类型化键模式：
// 由提供方包导出 FromEnv 帮助函数（如 conn.PoolFrom(env)），core 不感知具体服务类型。
type Env interface {
	Lookup(key any) any
}

// MapEnv 是简单的 Env 实现。
type MapEnv map[any]any

func (m MapEnv) Lookup(key any) any { return m[key] }

// ---------------------------------------------------------------- 设计期

// InferContext 是 Infer 的上下文：读输入 schema、声明输出 schema、报告问题。
type InferContext struct {
	Ctx      context.Context
	NodeID   string
	Env      Env
	VarKinds map[string]Kind // 可见变量及其类型（转换参数 + 内置变量）
	// VarSamples 是设计期变量样例值（参数默认值或按类型的样例），用于探测 SQL 字段；可为 nil。
	VarSamples map[string]any

	inputs    map[string]*Schema
	outputs   map[string]*Schema
	errorBase *Schema
	problems  Problems
}

// NewInferContext 由编译器构造。
func NewInferContext(ctx context.Context, nodeID string, env Env, varKinds map[string]Kind, inputs map[string]*Schema) *InferContext {
	if inputs == nil {
		inputs = map[string]*Schema{}
	}
	return &InferContext{Ctx: ctx, NodeID: nodeID, Env: env, VarKinds: varKinds, inputs: inputs, outputs: map[string]*Schema{}}
}

// Sample 返回设计期变量样例值。
func (ic *InferContext) Sample(name string) (any, bool) {
	v, ok := ic.VarSamples[name]
	return v, ok
}

// Input 返回端口的输入 schema；端口未连接返回 nil。
func (ic *InferContext) Input(port string) *Schema { return ic.inputs[port] }

// Main 返回主输入 schema（未连接时返回 EmptySchema，便于步骤统一处理）。
func (ic *InferContext) Main() *Schema {
	if s := ic.inputs[PortMain]; s != nil {
		return s
	}
	return EmptySchema
}

// HasInput 报告端口是否已连接。
func (ic *InferContext) HasInput(port string) bool { return ic.inputs[port] != nil }

// SetOutput 声明输出端口的 schema。
func (ic *InferContext) SetOutput(port string, s *Schema) { ic.outputs[port] = s }

// SetErrorBase 声明错误行的基础布局（默认：主输入；无输入的源步骤默认主输出）。
func (ic *InferContext) SetErrorBase(s *Schema) { ic.errorBase = s }

// Outputs 返回已声明的输出（编译器用）。
func (ic *InferContext) Outputs() map[string]*Schema { return ic.outputs }

// ErrorBase 返回错误行基础布局（编译器用）。
func (ic *InferContext) ErrorBase() *Schema {
	if ic.errorBase != nil {
		return ic.errorBase
	}
	if s := ic.inputs[PortMain]; s != nil {
		return s
	}
	if s := ic.outputs[PortMain]; s != nil {
		return s
	}
	return EmptySchema
}

// Errorf 报告阻断性问题。
func (ic *InferContext) Errorf(path, code, format string, args ...any) {
	ic.problems = append(ic.problems, Problem{Severity: SeverityError, NodeID: ic.NodeID, Path: path, Code: code, Message: fmt.Sprintf(format, args...)})
}

// Warnf 报告非阻断问题。
func (ic *InferContext) Warnf(path, code, format string, args ...any) {
	ic.problems = append(ic.problems, Problem{Severity: SeverityWarning, NodeID: ic.NodeID, Path: path, Code: code, Message: fmt.Sprintf(format, args...)})
}

// Problems 返回已报告问题。
func (ic *InferContext) Problems() Problems { return ic.problems }

// ErrorFields 是错误行追加的四个字段。
func ErrorFields() []Field {
	return []Field{
		NewField("_error_code", KindString),
		NewField("_error_message", KindString),
		NewField("_error_field", KindString),
		NewField("_error_step", KindString),
	}
}

// ---------------------------------------------------------------- 运行期

// Runner 是步骤的运行实例。Run 返回即视为该步骤结束，引擎随后关闭其全部输出。
type Runner interface {
	Run(ctx context.Context) error
}

// RunnerFunc 适配函数为 Runner。
type RunnerFunc func(ctx context.Context) error

func (f RunnerFunc) Run(ctx context.Context) error { return f(ctx) }

// RunContext 是步骤运行期上下文，由 pipeline 运行器构造。
type RunContext struct {
	NodeID       string
	NodeName     string
	Vars         *Vars
	Log          *Logger
	Metrics      *Metrics
	Env          Env
	BatchSize    int
	MemoryBudget int64 // 字节；有状态步骤超出须溢写或失败
	Preview      bool  // 预览模式：输出步骤不写库，只把将写入的行转发到 main 输出
	TempDir      func() (string, error)

	Inputs  map[string]*InPort
	Outputs map[string]*OutPort

	// OnReject 在行被拒绝（写入 error 端口）时回调采样；可为 nil。
	OnReject func(row Row)
}

// In 返回输入端口；未连接返回一个立即耗尽的端口，步骤无需判空。
func (rc *RunContext) In(port string) *InPort {
	if p := rc.Inputs[port]; p != nil {
		return p
	}
	return closedInPort(rc.Metrics)
}

// Out 返回输出端口；未连接返回丢弃端口。
func (rc *RunContext) Out(port string) *OutPort {
	if p := rc.Outputs[port]; p != nil {
		return p
	}
	return &OutPort{metrics: rc.Metrics}
}

// HasOutput 报告输出端口是否有下游。
func (rc *RunContext) HasOutput(port string) bool {
	p := rc.Outputs[port]
	return p != nil && len(p.targets) > 0
}

// Reject 把出错行送往 error 端口；未连接 error 端口时返回 cause（使步骤失败）。
// row 必须是主输入布局（或 SetErrorBase 声明的布局）的行。
func (rc *RunContext) Reject(ctx context.Context, row Row, cause error, field string) error {
	if !rc.HasOutput(PortError) {
		rc.Metrics.Errors.Add(1)
		return cause
	}
	code := "row_error"
	var ce *ConvertError
	if errors.As(cause, &ce) {
		code = "convert_error"
	}
	er := make(Row, 0, len(row)+4)
	er = append(er, row...)
	er = append(er, code, cause.Error(), nullIfEmpty(field), rc.NodeID)
	rc.Metrics.Rejected.Add(1)
	if rc.OnReject != nil {
		rc.OnReject(er)
	}
	return rc.Outputs[PortError].buffer(ctx, er, rc.BatchSize)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Emitter 把逐行产出攒成批次发送。用完必须 Flush。
type Emitter struct {
	out  *OutPort
	size int
	buf  []Row
}

// Emitter 返回端口的行攒批器。
func (rc *RunContext) Emitter(port string) *Emitter {
	size := rc.BatchSize
	if size <= 0 {
		size = 1000
	}
	return &Emitter{out: rc.Out(port), size: size}
}

// Emit 追加一行，满批即发送。
func (e *Emitter) Emit(ctx context.Context, r Row) error {
	e.buf = append(e.buf, r)
	if len(e.buf) >= e.size {
		return e.Flush(ctx)
	}
	return nil
}

// Flush 发送剩余行。
func (e *Emitter) Flush(ctx context.Context) error {
	if len(e.buf) == 0 {
		return nil
	}
	b := e.buf
	e.buf = make([]Row, 0, e.size)
	return e.out.Send(ctx, Batch{Schema: e.out.Schema, Rows: b})
}

// InPort 是输入端口：可能有多条上游连线汇入同一通道。
type InPort struct {
	Schema  *Schema
	ch      <-chan Batch
	metrics *Metrics
}

// NewInPort 由运行器构造。
func NewInPort(s *Schema, ch <-chan Batch, m *Metrics) *InPort {
	return &InPort{Schema: s, ch: ch, metrics: m}
}

func closedInPort(m *Metrics) *InPort {
	ch := make(chan Batch)
	close(ch)
	return &InPort{Schema: EmptySchema, ch: ch, metrics: m}
}

// Next 读下一批；上游全部结束返回 ok=false。ctx 取消返回 ctx.Err()。
func (p *InPort) Next(ctx context.Context) (Batch, bool, error) {
	select {
	case b, ok := <-p.ch:
		if !ok {
			return Batch{}, false, nil
		}
		if p.metrics != nil {
			p.metrics.Read.Add(int64(len(b.Rows)))
		}
		return b, true, nil
	case <-ctx.Done():
		return Batch{}, false, ctx.Err()
	}
}

// Each 逐批消费直到上游结束。
func (p *InPort) Each(ctx context.Context, fn func(Batch) error) error {
	for {
		b, ok, err := p.Next(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if err := fn(b); err != nil {
			return err
		}
	}
}

// Distribution 是一个输出端口有多条连线时的分发方式。
type Distribution string

const (
	DistCopy       Distribution = "copy"       // 每条下游收到全量（零拷贝，依赖不可变批次规则）
	DistDistribute Distribution = "distribute" // 轮询分发
)

// Target 是输出端口的一条下游连线，由运行器构造。
type Target struct {
	Ch chan<- Batch
}

// OutPort 是输出端口。
type OutPort struct {
	Schema  *Schema
	mode    Distribution
	targets []Target
	metrics *Metrics
	next    int

	mu  sync.Mutex // 仅保护 buf（Reject 可能被并发调用的步骤使用）
	buf []Row
}

// NewOutPort 由运行器构造。
func NewOutPort(s *Schema, mode Distribution, targets []Target, m *Metrics) *OutPort {
	if mode == "" {
		mode = DistCopy
	}
	return &OutPort{Schema: s, mode: mode, targets: targets, metrics: m}
}

// Send 发送批次；无下游时丢弃。空批次不发送。
func (p *OutPort) Send(ctx context.Context, b Batch) error {
	if len(b.Rows) == 0 || len(p.targets) == 0 {
		return nil
	}
	if b.Schema == nil {
		b.Schema = p.Schema
	}
	switch p.mode {
	case DistDistribute:
		t := p.targets[p.next%len(p.targets)]
		p.next++
		if err := sendOne(ctx, t.Ch, b); err != nil {
			return err
		}
	default:
		for _, t := range p.targets {
			if err := sendOne(ctx, t.Ch, b); err != nil {
				return err
			}
		}
	}
	if p.metrics != nil {
		p.metrics.Written.Add(int64(len(b.Rows)))
	}
	return nil
}

func sendOne(ctx context.Context, ch chan<- Batch, b Batch) error {
	select {
	case ch <- b:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *OutPort) buffer(ctx context.Context, r Row, size int) error {
	if size <= 0 {
		size = 1000
	}
	p.mu.Lock()
	p.buf = append(p.buf, r)
	var flush []Row
	if len(p.buf) >= size {
		flush, p.buf = p.buf, nil
	}
	p.mu.Unlock()
	if flush != nil {
		return p.Send(ctx, Batch{Schema: p.Schema, Rows: flush})
	}
	return nil
}

// FlushBuffered 发送 Reject 攒下的错误行，由运行器在步骤结束时调用。
func (p *OutPort) FlushBuffered(ctx context.Context) error {
	p.mu.Lock()
	flush := p.buf
	p.buf = nil
	p.mu.Unlock()
	return p.Send(ctx, Batch{Schema: p.Schema, Rows: flush})
}

// ---------------------------------------------------------------- 注册

// StepType 是类型擦除后的步骤类型，由 Define 从 Definition[C] 生成。
type StepType interface {
	Meta() Meta
	DefaultConfig() json.RawMessage
	// Decode 把（可能是旧版本的）原始配置迁移并解码为 *C。
	Decode(raw json.RawMessage, version int) (any, error)
	Infer(ic *InferContext, cfg any) error
	Open(rc *RunContext, cfg any) (Runner, error)
}

// Definition 用泛型把配置结构 C 绑定到步骤实现。
type Definition[C any] struct {
	Meta     Meta
	Defaults func() C
	// Migrate 把 from 版本的配置升级到 Meta.Version；可为 nil（仅一个版本时）。
	Migrate func(from int, raw json.RawMessage) (json.RawMessage, error)
	Infer   func(ic *InferContext, cfg *C) error
	Open    func(rc *RunContext, cfg *C) (Runner, error)
}

// Define 生成 StepType。
func Define[C any](d Definition[C]) StepType {
	if d.Meta.Version == 0 {
		d.Meta.Version = 1
	}
	if d.Meta.Maturity == "" {
		d.Meta.Maturity = MaturityStable
	}
	return &typedStep[C]{d: d}
}

type typedStep[C any] struct{ d Definition[C] }

func (t *typedStep[C]) Meta() Meta { return t.d.Meta }

func (t *typedStep[C]) defaults() C {
	var c C
	if t.d.Defaults != nil {
		c = t.d.Defaults()
	}
	return c
}

func (t *typedStep[C]) DefaultConfig() json.RawMessage {
	b, _ := json.Marshal(t.defaults())
	return b
}

func (t *typedStep[C]) Decode(raw json.RawMessage, version int) (any, error) {
	return DecodeConfig[C](raw, version, t.d.Meta.Version, t.defaults, t.d.Migrate)
}

func (t *typedStep[C]) Infer(ic *InferContext, cfg any) error {
	if t.d.Infer == nil {
		return nil
	}
	return t.d.Infer(ic, cfg.(*C))
}

func (t *typedStep[C]) Open(rc *RunContext, cfg any) (Runner, error) {
	if t.d.Open == nil {
		return nil, fmt.Errorf("步骤 %s 尚未实现运行逻辑", t.d.Meta.ID)
	}
	return t.d.Open(rc, cfg.(*C))
}

// DecodeConfig 是步骤与作业项共用的配置解码：版本迁移 → 以默认值为底严格解码（拒绝未知字段）。
func DecodeConfig[C any](raw json.RawMessage, version, current int, defaults func() C, migrate func(int, json.RawMessage) (json.RawMessage, error)) (*C, error) {
	if version == 0 {
		version = current
	}
	if version > current {
		return nil, fmt.Errorf("配置版本 %d 高于当前程序支持的 %d，请升级 Console", version, current)
	}
	if version < current {
		if migrate == nil {
			return nil, fmt.Errorf("缺少从版本 %d 到 %d 的配置迁移", version, current)
		}
		var err error
		if raw, err = migrate(version, raw); err != nil {
			return nil, fmt.Errorf("配置迁移失败：%w", err)
		}
	}
	c := defaults()
	if len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("配置格式错误：%w", err)
		}
	}
	return &c, nil
}

// Registry 是组件注册表（步骤与作业项各一个实例）。
type Registry[T interface{ Meta() Meta }] struct {
	mu    sync.RWMutex
	items map[string]T
}

// NewRegistry 创建注册表。
func NewRegistry[T interface{ Meta() Meta }]() *Registry[T] {
	return &Registry[T]{items: map[string]T{}}
}

// Register 注册组件；ID 重复 panic（属于编程错误，应在 init 阶段暴露）。
func (r *Registry[T]) Register(t T) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := t.Meta().ID
	if id == "" {
		panic("etl: 组件 ID 为空")
	}
	if _, dup := r.items[id]; dup {
		panic("etl: 组件 ID 重复：" + id)
	}
	r.items[id] = t
}

// Get 按 ID 取组件。
func (r *Registry[T]) Get(id string) (T, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.items[id]
	return t, ok
}

// Metas 返回全部组件元信息（按分类、名称排序）。
func (r *Registry[T]) Metas() []Meta {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Meta, 0, len(r.items))
	for _, t := range r.items {
		out = append(out, t.Meta())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return categoryOrder(out[i].Category) < categoryOrder(out[j].Category)
		}
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func categoryOrder(c Category) int {
	switch c {
	case CategoryInput:
		return 0
	case CategoryTransform:
		return 1
	case CategoryFlow:
		return 2
	case CategoryLookup:
		return 3
	case CategoryJoin:
		return 4
	case CategoryOutput:
		return 5
	case CategoryControl:
		return 6
	}
	return 9
}

// Steps 是全局步骤注册表；内置步骤在 steps 包 init 中注册。
var Steps = NewRegistry[StepType]()
