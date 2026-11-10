package core

import (
	"sync/atomic"
	"time"
)

// Metrics 是单个步骤的运行度量，列与 Kettle「步骤度量」对齐。全部原子计数，可被快照协程并发读取。
//
//	Read     从上游连线读入的行
//	Written  写到下游连线的行
//	Input    从数据库/文件读入的行
//	Output   写入数据库/文件的行（插入）
//	Updated  更新的行
//	Rejected 进入错误输出的行
//	Errors   导致失败的错误次数
type Metrics struct {
	Read, Written, Input, Output, Updated, Rejected, Errors atomic.Int64
	startedAt, finishedAt                                   atomic.Int64 // UnixNano
}

// MetricsSnapshot 是可序列化的度量快照。
type MetricsSnapshot struct {
	Read       int64   `json:"read"`
	Written    int64   `json:"written"`
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	Updated    int64   `json:"updated"`
	Rejected   int64   `json:"rejected"`
	Errors     int64   `json:"errors"`
	StartedAt  int64   `json:"startedAt,omitempty"`  // Unix 毫秒
	FinishedAt int64   `json:"finishedAt,omitempty"` // Unix 毫秒
	DurationMs int64   `json:"durationMs"`
	RowsPerSec float64 `json:"rowsPerSec"` // 以 max(Read, Input) 计
}

// Start 记录开始时间（幂等）。
func (m *Metrics) Start() { m.startedAt.CompareAndSwap(0, time.Now().UnixNano()) }

// Finish 记录结束时间（幂等）。
func (m *Metrics) Finish() { m.finishedAt.CompareAndSwap(0, time.Now().UnixNano()) }

// Snapshot 生成一致性足够的快照（各计数独立原子读，界面展示用途无需全局一致）。
func (m *Metrics) Snapshot() MetricsSnapshot {
	s := MetricsSnapshot{
		Read: m.Read.Load(), Written: m.Written.Load(), Input: m.Input.Load(), Output: m.Output.Load(),
		Updated: m.Updated.Load(), Rejected: m.Rejected.Load(), Errors: m.Errors.Load(),
	}
	start, end := m.startedAt.Load(), m.finishedAt.Load()
	if start == 0 {
		return s
	}
	s.StartedAt = start / int64(time.Millisecond)
	if end == 0 {
		end = time.Now().UnixNano()
	} else {
		s.FinishedAt = end / int64(time.Millisecond)
	}
	dur := time.Duration(end - start)
	s.DurationMs = dur.Milliseconds()
	if dur > 0 {
		n := s.Read
		if s.Input > n {
			n = s.Input
		}
		s.RowsPerSec = float64(n) / dur.Seconds()
	}
	return s
}
