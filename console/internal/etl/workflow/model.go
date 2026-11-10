// Package workflow 实现「作业」：作业项按结果端口分支执行的有向无环图。
//
// 相对 Kettle 的语义改进（见 docs/etl-design-v1.md §9）：
//   - 命名输出端口（success/failure/true/false + 虚拟端口 always），与转换层同构；
//   - 汇合确定：节点等全部上游已决后，至少一条入线被激活才执行，否则跳过并向下传播；
//   - 失败是否被处理：失败节点的 failure/always 端口有连线即视为已处理；
//   - 并行分支、作业项级超时与重试、从失败处续跑。
package workflow

import (
	"context"
	"encoding/json"
	"fmt"

	"mooncell/console/internal/etl/core"
)

// FormatVersion 是作业定义格式版本。
const FormatVersion = 1

// PortAlways 是每个作业项都有的虚拟输出端口：无论成功失败都激活（Kettle「无条件」连线）。
const PortAlways = "always"

// Definition 是作业定义。
type Definition struct {
	Format   int              `json:"format"`
	Kind     string           `json:"kind"` // 固定 "workflow"
	Params   []core.ParamDecl `json:"params"`
	Nodes    []Node           `json:"nodes"`
	Hops     []Hop            `json:"hops"`
	Settings Settings         `json:"settings"`
	Notes    []Note           `json:"notes,omitempty"`
}

// Node 是作业项实例。
type Node struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Name       string          `json:"name"`
	Version    int             `json:"version"`
	Config     json.RawMessage `json:"config"`
	TimeoutSec int             `json:"timeoutSec,omitempty"`
	Retry      Retry           `json:"retry,omitempty"`
	UI         json.RawMessage `json:"ui,omitempty"`
}

// Retry 是失败重试策略。
type Retry struct {
	Max        int `json:"max"`        // 额外重试次数，0 = 不重试
	BackoffSec int `json:"backoffSec"` // 每次重试前等待秒数（线性）
}

// Hop 是作业连线（作业项只有一个输入）。
type Hop struct {
	ID       string `json:"id"`
	From     string `json:"from"`
	FromPort string `json:"fromPort"` // success | failure | true | false | always
	To       string `json:"to"`
	Disabled bool   `json:"disabled,omitempty"`
}

// Note 是画布注释。
type Note struct {
	ID   string          `json:"id"`
	Text string          `json:"text"`
	UI   json.RawMessage `json:"ui,omitempty"`
}

// Settings 是作业级参数。
type Settings struct {
	MaxParallel int    `json:"maxParallel"` // 同时执行的作业项上限，默认 4
	LogLevel    string `json:"logLevel"`
}

// Normalized 填充默认值。
func (s Settings) Normalized() Settings {
	if s.MaxParallel <= 0 {
		s.MaxParallel = 4
	}
	if s.LogLevel == "" {
		s.LogLevel = "info"
	}
	return s
}

// Decode 解析作业定义 JSON。
func Decode(raw []byte) (*Definition, error) {
	var d Definition
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("作业定义格式错误：%w", err)
	}
	if d.Kind != "" && d.Kind != "workflow" {
		return nil, fmt.Errorf("不是作业定义（kind=%s）", d.Kind)
	}
	if d.Format > FormatVersion {
		return nil, fmt.Errorf("作业定义格式版本 %d 高于当前程序支持的 %d", d.Format, FormatVersion)
	}
	d.Kind = "workflow"
	if d.Format == 0 {
		d.Format = FormatVersion
	}
	return &d, nil
}

// ---------------------------------------------------------------- 作业项契约

// Outcome 是作业项执行结果。
type Outcome struct {
	Port    string                // 激活的输出端口；空 = success
	Vars    map[string]core.Value // 写入作业变量
	Message string                // 结果摘要（展示在运行详情）
}

// ValidateContext 是作业项校验上下文。
type ValidateContext struct {
	Ctx      context.Context
	NodeID   string
	Env      core.Env
	VarKinds map[string]core.Kind
	problems core.Problems
}

// Errorf 报告阻断问题。
func (vc *ValidateContext) Errorf(path, code, format string, args ...any) {
	vc.problems = append(vc.problems, core.Problem{Severity: core.SeverityError, NodeID: vc.NodeID, Path: path, Code: code, Message: fmt.Sprintf(format, args...)})
}

// Warnf 报告非阻断问题。
func (vc *ValidateContext) Warnf(path, code, format string, args ...any) {
	vc.problems = append(vc.problems, core.Problem{Severity: core.SeverityWarning, NodeID: vc.NodeID, Path: path, Code: code, Message: fmt.Sprintf(format, args...)})
}

// ExecContext 是作业项执行上下文。
type ExecContext struct {
	RunID    string
	NodeID   string
	NodeName string
	Attempt  int
	Vars     *core.Vars
	Log      *core.Logger
	Env      core.Env
}

// ActionType 是类型擦除后的作业项类型。
type ActionType interface {
	Meta() core.Meta
	DefaultConfig() json.RawMessage
	Decode(raw json.RawMessage, version int) (any, error)
	// Outputs 返回作业项写入的变量及类型（编译期即可知，供下游表达式类型检查）。
	Outputs(cfg any) map[string]core.Kind
	Validate(vc *ValidateContext, cfg any) error
	Execute(ctx context.Context, ec *ExecContext, cfg any) (Outcome, error)
}

// DefRef 是作业项引用的其他定义（如「运行转换」引用的转换）。
// 触发运行时按此把被引定义文档一并存入运行快照，保证续跑/复跑语义不被后续修改影响。
type DefRef struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // pipeline | workflow
}

// ActionDefinition 用泛型绑定配置结构。
type ActionDefinition[C any] struct {
	Meta     core.Meta
	Defaults func() C
	Migrate  func(from int, raw json.RawMessage) (json.RawMessage, error)
	Outputs  func(cfg *C) map[string]core.Kind
	// Refs 可选：声明该作业项引用的定义，供运行快照收集。
	Refs     func(cfg *C) []DefRef
	Validate func(vc *ValidateContext, cfg *C) error
	Execute  func(ctx context.Context, ec *ExecContext, cfg *C) (Outcome, error)
}

// DefineAction 生成 ActionType。
func DefineAction[C any](d ActionDefinition[C]) ActionType {
	if d.Meta.Version == 0 {
		d.Meta.Version = 1
	}
	if d.Meta.Maturity == "" {
		d.Meta.Maturity = core.MaturityStable
	}
	if d.Meta.Category == "" {
		d.Meta.Category = core.CategoryControl
	}
	return &typedAction[C]{d: d}
}

type typedAction[C any] struct{ d ActionDefinition[C] }

func (t *typedAction[C]) Meta() core.Meta { return t.d.Meta }

func (t *typedAction[C]) defaults() C {
	var c C
	if t.d.Defaults != nil {
		c = t.d.Defaults()
	}
	return c
}

func (t *typedAction[C]) DefaultConfig() json.RawMessage {
	b, _ := json.Marshal(t.defaults())
	return b
}

func (t *typedAction[C]) Decode(raw json.RawMessage, version int) (any, error) {
	return core.DecodeConfig[C](raw, version, t.d.Meta.Version, t.defaults, t.d.Migrate)
}

func (t *typedAction[C]) Outputs(cfg any) map[string]core.Kind {
	if t.d.Outputs == nil {
		return nil
	}
	return t.d.Outputs(cfg.(*C))
}

func (t *typedAction[C]) Validate(vc *ValidateContext, cfg any) error {
	if t.d.Validate == nil {
		return nil
	}
	return t.d.Validate(vc, cfg.(*C))
}

func (t *typedAction[C]) Execute(ctx context.Context, ec *ExecContext, cfg any) (Outcome, error) {
	if t.d.Execute == nil {
		return Outcome{}, fmt.Errorf("作业项 %s 尚未实现", t.d.Meta.ID)
	}
	return t.d.Execute(ctx, ec, cfg.(*C))
}

// Actions 是全局作业项注册表（actions 包 init 注册）。
var Actions = core.NewRegistry[ActionType]()

// refLister 是实现了引用声明的作业项类型。
type refLister interface{ RefList(cfg any) []DefRef }

// RefList 实现 refLister（未声明 Refs 时返回 nil）。
func (t *typedAction[C]) RefList(cfg any) []DefRef {
	if t.d.Refs == nil {
		return nil
	}
	return t.d.Refs(cfg.(*C))
}

// CollectRefs 收集作业定义中全部作业项声明的引用（去重前的原始列表）。
// 未知类型或解码失败的节点跳过——编译校验会另行报告，此处只做尽力收集。
func CollectRefs(def *Definition) []DefRef {
	var out []DefRef
	for _, n := range def.Nodes {
		at, ok := Actions.Get(n.Type)
		if !ok {
			continue
		}
		rl, ok := at.(refLister)
		if !ok {
			continue
		}
		cfg, err := at.Decode(n.Config, n.Version)
		if err != nil {
			continue
		}
		for _, r := range rl.RefList(cfg) {
			if r.ID != "" {
				out = append(out, r)
			}
		}
	}
	return out
}
