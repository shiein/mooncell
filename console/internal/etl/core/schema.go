package core

import (
	"fmt"
	"strings"
)

// Field 描述一个字段。设计期由 Infer 产生，运行期随批次流动（只读）。
type Field struct {
	Name       string `json:"name"`
	Kind       Kind   `json:"kind"`
	Length     int    `json:"length"`    // -1 = 未知
	Precision  int    `json:"precision"` // decimal 总位数；-1 = 未知
	Scale      int    `json:"scale"`
	Nullable   bool   `json:"nullable"`
	Origin     string `json:"origin,omitempty"`     // 产生该字段的步骤 ID
	NativeType string `json:"nativeType,omitempty"` // 源库原生类型提示，仅展示
}

// NewField 构造长度/精度未知的可空字段。
func NewField(name string, kind Kind) Field {
	return Field{Name: name, Kind: kind, Length: -1, Precision: -1, Nullable: true}
}

// Schema 是有序、不可变的字段集合。零值不可用，用 NewSchema 构造。
type Schema struct {
	fields []Field
	index  map[string]int // 精确名 → 下标
}

// NewSchema 构造 Schema；字段名必须非空且（大小写敏感）唯一。
func NewSchema(fields ...Field) (*Schema, error) {
	s := &Schema{fields: append([]Field(nil), fields...), index: make(map[string]int, len(fields))}
	for i, f := range s.fields {
		if strings.TrimSpace(f.Name) == "" {
			return nil, fmt.Errorf("第 %d 个字段名为空", i+1)
		}
		if !f.Kind.Valid() {
			return nil, fmt.Errorf("字段 %q 的类型 %q 无效", f.Name, f.Kind)
		}
		if _, dup := s.index[f.Name]; dup {
			return nil, fmt.Errorf("字段名重复：%s", f.Name)
		}
		s.index[f.Name] = i
	}
	return s, nil
}

// MustSchema 用于测试与静态定义。
func MustSchema(fields ...Field) *Schema {
	s, err := NewSchema(fields...)
	if err != nil {
		panic(err)
	}
	return s
}

// EmptySchema 是没有字段的 Schema（例如纯控制步骤的输出）。
var EmptySchema = MustSchema()

func (s *Schema) Len() int { return len(s.fields) }

// Field 返回第 i 个字段（副本）。
func (s *Schema) Field(i int) Field { return s.fields[i] }

// Fields 返回字段副本切片，调用方修改不影响 Schema。
func (s *Schema) Fields() []Field { return append([]Field(nil), s.fields...) }

// Index 返回字段下标；不存在返回 -1。
func (s *Schema) Index(name string) int {
	if i, ok := s.index[name]; ok {
		return i
	}
	return -1
}

// Lookup 按名查字段。
func (s *Schema) Lookup(name string) (Field, bool) {
	i := s.Index(name)
	if i < 0 {
		return Field{}, false
	}
	return s.fields[i], true
}

// Names 返回字段名列表。
func (s *Schema) Names() []string {
	out := make([]string, len(s.fields))
	for i, f := range s.fields {
		out[i] = f.Name
	}
	return out
}

// Append 返回追加字段后的新 Schema。
func (s *Schema) Append(fields ...Field) (*Schema, error) {
	return NewSchema(append(s.Fields(), fields...)...)
}

// Compatible 报告两个 Schema 能否汇入同一输入端口：字段数、顺序、名称、类型一致。
// 与 Kettle 一致：汇流要求行布局相同，否则下游按下标取值会错位。
func (s *Schema) Compatible(o *Schema) error {
	if s.Len() != o.Len() {
		return fmt.Errorf("字段数不同（%d vs %d）", s.Len(), o.Len())
	}
	for i := range s.fields {
		a, b := s.fields[i], o.fields[i]
		if a.Name != b.Name || a.Kind != b.Kind {
			return fmt.Errorf("第 %d 个字段不一致：%s(%s) vs %s(%s)", i+1, a.Name, a.Kind, b.Name, b.Kind)
		}
	}
	return nil
}

// Row 是一行值，与 Schema 按下标对应。值必须是 Kind 的规范 Go 表示或 nil。
//
// 不可变规则：Row 一旦放进已发送的 Batch，任何人都不得修改它。需要改值时用 Clone/With 产生新行。
type Row []any

// Clone 返回浅拷贝（值本身均为不可变或按约定只读）。
func (r Row) Clone() Row { return append(Row(nil), r...) }

// With 返回把下标 i 设为 v 的新行。
func (r Row) With(i int, v any) Row {
	n := r.Clone()
	n[i] = v
	return n
}

// Batch 是步骤间传递的单位。Schema 在一条连线上恒定，随批次携带用于运行期断言与调试。
type Batch struct {
	Schema *Schema
	Rows   []Row
}

// Len 返回行数。
func (b Batch) Len() int { return len(b.Rows) }
