// Package pipeline 实现「转换」：数据在步骤之间以批次流动。
//
//	Definition（JSON） → Compile → Plan（类型推导 + 问题） → Run（goroutine + 有界通道）
//
// Plan 同时服务于设计器（字段提示、连线 schema）、预检与运行，三者共享同一份推导。
package pipeline

import (
	"encoding/json"
	"fmt"

	"mooncell/console/internal/etl/core"
)

// FormatVersion 是转换定义的格式版本。
const FormatVersion = 1

// Definition 是转换定义（持久化为 JSON 文档）。
type Definition struct {
	Format   int              `json:"format"`
	Kind     string           `json:"kind"` // 固定 "pipeline"
	Params   []core.ParamDecl `json:"params"`
	Nodes    []Node           `json:"nodes"`
	Hops     []Hop            `json:"hops"`
	Settings Settings         `json:"settings"`
	Notes    []Note           `json:"notes,omitempty"`
}

// Node 是一个步骤实例。
type Node struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Name    string          `json:"name"`
	Version int             `json:"version"`
	Config  json.RawMessage `json:"config"`
	// Distribution 按输出端口指定多连线分发方式；缺省 copy。
	Distribution map[string]core.Distribution `json:"distribution,omitempty"`
	UI           json.RawMessage              `json:"ui,omitempty"` // 画布坐标等，引擎不解释
}

// Hop 是连线。
type Hop struct {
	ID       string `json:"id"`
	From     string `json:"from"`
	FromPort string `json:"fromPort"`
	To       string `json:"to"`
	ToPort   string `json:"toPort"`
	Disabled bool   `json:"disabled,omitempty"`
}

// Note 是画布注释（引擎忽略）。
type Note struct {
	ID   string          `json:"id"`
	Text string          `json:"text"`
	UI   json.RawMessage `json:"ui,omitempty"`
}

// Settings 是转换级运行参数。
type Settings struct {
	BatchSize      int    `json:"batchSize"`      // 每批行数，默认 1000
	ChannelBatches int    `json:"channelBatches"` // 每条连线缓冲批数，默认 8
	MemoryBudgetMB int    `json:"memoryBudgetMB"` // 每个有状态步骤的内存预算，默认 512
	LogLevel       string `json:"logLevel"`       // error|warn|info|detail|debug，默认 info
}

// Normalized 返回填充默认值后的设置。
func (s Settings) Normalized() Settings {
	if s.BatchSize <= 0 {
		s.BatchSize = 1000
	}
	if s.ChannelBatches <= 0 {
		s.ChannelBatches = 8
	}
	if s.MemoryBudgetMB <= 0 {
		s.MemoryBudgetMB = 512
	}
	if s.LogLevel == "" {
		s.LogLevel = "info"
	}
	return s
}

// Decode 解析转换定义 JSON。
func Decode(raw []byte) (*Definition, error) {
	var d Definition
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("转换定义格式错误：%w", err)
	}
	if d.Kind != "" && d.Kind != "pipeline" {
		return nil, fmt.Errorf("不是转换定义（kind=%s）", d.Kind)
	}
	if d.Format > FormatVersion {
		return nil, fmt.Errorf("转换定义格式版本 %d 高于当前程序支持的 %d", d.Format, FormatVersion)
	}
	d.Kind = "pipeline"
	if d.Format == 0 {
		d.Format = FormatVersion
	}
	return &d, nil
}

// BuiltinVars 是每次运行都可用的内置变量。
func BuiltinVars() map[string]core.Kind {
	return map[string]core.Kind{
		"RUN_ID":    core.KindString,
		"RUN_START": core.KindTimestamp,
		"TRIGGER":   core.KindString,
	}
}
