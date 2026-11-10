package core

import "fmt"

// Severity 是校验问题级别：error 阻止运行，warning 允许运行但需提示。
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Problem 是编译/校验发现的问题，界面据 NodeID/Path 定位到节点与配置项。
type Problem struct {
	Severity Severity `json:"severity"`
	NodeID   string   `json:"nodeId,omitempty"`
	HopID    string   `json:"hopId,omitempty"`
	Path     string   `json:"path,omitempty"` // 配置路径，如 "fields[2].expr"
	Code     string   `json:"code"`
	Message  string   `json:"message"`
}

func (p Problem) Error() string {
	if p.NodeID != "" {
		return fmt.Sprintf("[%s] %s", p.NodeID, p.Message)
	}
	return p.Message
}

// Problems 是问题列表。
type Problems []Problem

// HasErrors 报告是否含阻断性问题。
func (ps Problems) HasErrors() bool {
	for _, p := range ps {
		if p.Severity == SeverityError {
			return true
		}
	}
	return false
}

// First 返回第一个 error 级问题（无则 nil），用于在「必须可运行」的路径上转为 error。
func (ps Problems) First() error {
	for _, p := range ps {
		if p.Severity == SeverityError {
			return p
		}
	}
	return nil
}
