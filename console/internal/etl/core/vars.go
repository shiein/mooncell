package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// Value 是类型化变量值。JSON 形态 {"kind":"timestamp","value":"2026-01-02T03:04:05Z"}，可无损往返。
type Value struct {
	Kind Kind
	V    any // Kind 的规范 Go 表示或 nil
}

// NewValue 用 Go 值构造 Value，Kind 由值推断（nil 需显式 Kind，调用方用 Value{Kind:k}）。
func NewValue(v any) Value {
	v = Normalize(v)
	return Value{Kind: KindOf(v), V: v}
}

type valueJSON struct {
	Kind  Kind `json:"kind"`
	Value any  `json:"value"`
}

func (v Value) MarshalJSON() ([]byte, error) {
	out := valueJSON{Kind: v.Kind}
	switch x := v.V.(type) {
	case nil:
	case time.Time:
		out.Value = x.Format(time.RFC3339Nano)
	case decimal.Decimal:
		out.Value = x.String()
	case []byte:
		out.Value = FormatValue(x) // base64
	default:
		out.Value = x
	}
	return json.Marshal(out)
}

func (v *Value) UnmarshalJSON(b []byte) error {
	var in struct {
		Kind  Kind            `json:"kind"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	if !in.Kind.Valid() {
		return fmt.Errorf("变量类型 %q 无效", in.Kind)
	}
	v.Kind = in.Kind
	if len(in.Value) == 0 || string(in.Value) == "null" {
		v.V = nil
		return nil
	}
	if in.Kind == KindBinary {
		return fmt.Errorf("二进制变量不支持 JSON 传入")
	}
	// UseNumber：整数/精确小数从原始数字文本转换，避免经 float64 丢精度。
	dec := json.NewDecoder(bytes.NewReader(in.Value))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	switch x := raw.(type) {
	case json.Number:
		raw = x.String()
	case string, bool:
	default:
		return fmt.Errorf("变量值必须是标量")
	}
	conv, err := Convert(raw, in.Kind, ConvertOptions{})
	if err != nil {
		return err
	}
	v.V = conv
	return nil
}

// ParamDecl 声明转换/作业的参数。
type ParamDecl struct {
	Name        string `json:"name"`
	Kind        Kind   `json:"kind"`
	Default     *Value `json:"default,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Description string `json:"description,omitempty"`
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidVarName 报告变量名是否合法（ASCII 标识符，便于在 SQL/表达式中无歧义引用）。
func ValidVarName(name string) bool { return identRe.MatchString(name) }

// Vars 是变量作用域链。子作用域可遮蔽父作用域；写入只发生在本层。并发安全。
type Vars struct {
	parent *Vars
	mu     sync.RWMutex
	m      map[string]Value
}

// NewVars 创建以 parent 为父的作用域（parent 可为 nil）。
func NewVars(parent *Vars) *Vars { return &Vars{parent: parent, m: map[string]Value{}} }

// Get 沿作用域链查找变量。
func (v *Vars) Get(name string) (Value, bool) {
	for s := v; s != nil; s = s.parent {
		s.mu.RLock()
		val, ok := s.m[name]
		s.mu.RUnlock()
		if ok {
			return val, true
		}
	}
	return Value{}, false
}

// Set 在本层写入变量。
func (v *Vars) Set(name string, val Value) {
	v.mu.Lock()
	v.m[name] = val
	v.mu.Unlock()
}

// Local 返回本层变量快照（用于持久化作业项输出变量）。
func (v *Vars) Local() map[string]Value {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make(map[string]Value, len(v.m))
	for k, val := range v.m {
		out[k] = val
	}
	return out
}

// Flatten 返回整条链合并后的快照（子层覆盖父层）。
func (v *Vars) Flatten() map[string]Value {
	var chain []*Vars
	for s := v; s != nil; s = s.parent {
		chain = append(chain, s)
	}
	out := map[string]Value{}
	for i := len(chain) - 1; i >= 0; i-- {
		for k, val := range chain[i].Local() {
			out[k] = val
		}
	}
	return out
}

// Names 返回可见变量名（排序）。
func (v *Vars) Names() []string {
	m := v.Flatten()
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
