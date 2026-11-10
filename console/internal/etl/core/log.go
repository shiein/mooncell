package core

import (
	"fmt"
	"sync"
	"time"
)

// Level 是日志级别，从严到宽。运行配置的级别以下的日志不产生。
type Level int8

const (
	LevelError  Level = 1
	LevelWarn   Level = 2
	LevelInfo   Level = 3
	LevelDetail Level = 4
	LevelDebug  Level = 5
)

func (l Level) String() string {
	switch l {
	case LevelError:
		return "error"
	case LevelWarn:
		return "warn"
	case LevelInfo:
		return "info"
	case LevelDetail:
		return "detail"
	case LevelDebug:
		return "debug"
	}
	return "info"
}

// ParseLevel 解析级别文本，未知返回 LevelInfo。
func ParseLevel(s string) Level {
	switch s {
	case "error":
		return LevelError
	case "warn":
		return LevelWarn
	case "detail":
		return LevelDetail
	case "debug":
		return LevelDebug
	}
	return LevelInfo
}

// LogEntry 是一条结构化日志。
type LogEntry struct {
	Seq     int64  `json:"seq"`
	TS      int64  `json:"ts"` // Unix 毫秒
	Level   string `json:"level"`
	NodeID  string `json:"nodeId,omitempty"`
	Message string `json:"message"`
}

// LogSink 接收日志（运行管理器实现：内存环 + 限量落库 + SSE 推送）。
type LogSink interface {
	Write(e LogEntry)
}

// Logger 是带节点上下文的日志器。并发安全。
type Logger struct {
	sink   LogSink
	nodeID string
	prefix string // 嵌套运行（作业中的转换）时为「作业项 ID/」，使步骤日志可区分归属
	level  Level
}

// NewLogger 构造日志器；sink 为 nil 时丢弃。
func NewLogger(sink LogSink, level Level) *Logger { return &Logger{sink: sink, level: level} }

// For 返回绑定节点 ID 的子日志器。
func (l *Logger) For(nodeID string) *Logger {
	if l == nil {
		return nil
	}
	return &Logger{sink: l.sink, nodeID: l.prefix + nodeID, prefix: l.prefix, level: l.level}
}

// WithPrefix 返回为后续 For 的节点 ID 加前缀的日志器（自身节点 ID 不变）。
func (l *Logger) WithPrefix(prefix string) *Logger {
	if l == nil {
		return nil
	}
	return &Logger{sink: l.sink, nodeID: l.nodeID, prefix: prefix, level: l.level}
}

// Enabled 报告某级别是否会输出（避免构造昂贵消息）。
func (l *Logger) Enabled(lv Level) bool { return l != nil && l.sink != nil && lv <= l.level }

func (l *Logger) log(lv Level, format string, args ...any) {
	if !l.Enabled(lv) {
		return
	}
	l.sink.Write(LogEntry{TS: time.Now().UnixMilli(), Level: lv.String(), NodeID: l.nodeID, Message: fmt.Sprintf(format, args...)})
}

func (l *Logger) Error(format string, args ...any)  { l.log(LevelError, format, args...) }
func (l *Logger) Warn(format string, args ...any)   { l.log(LevelWarn, format, args...) }
func (l *Logger) Info(format string, args ...any)   { l.log(LevelInfo, format, args...) }
func (l *Logger) Detail(format string, args ...any) { l.log(LevelDetail, format, args...) }
func (l *Logger) Debug(format string, args ...any)  { l.log(LevelDebug, format, args...) }

// MemorySink 是测试与预览用的内存日志汇。
type MemorySink struct {
	mu      sync.Mutex
	seq     int64
	Entries []LogEntry
}

func (m *MemorySink) Write(e LogEntry) {
	m.mu.Lock()
	m.seq++
	e.Seq = m.seq
	m.Entries = append(m.Entries, e)
	m.mu.Unlock()
}

// Snapshot 返回日志副本。
func (m *MemorySink) Snapshot() []LogEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]LogEntry(nil), m.Entries...)
}
