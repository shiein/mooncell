package actions_test

import (
	"context"
	"testing"

	"mooncell/console/internal/etl/workflow"
)

// 汇合节点的一条入线来自「开始」不可达的孤立节点：只等可达入线，
// 不能因孤立入线永不决出而被跳过、作业却报成功。
func TestRegressJoinIgnoresUnreachableInEdge(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stopType   string
		stopCfg    any
		wantStatus string
	}{
		{"abort", "abort", map[string]any{"message": "reachable"}, workflow.NodeFailed},
		{"success", "success", nil, workflow.NodeSucceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := &workflow.Definition{
				Nodes: []workflow.Node{
					wnode("start", "start", nil),
					wnode("detached", "wait", map[string]any{"seconds": 0}),
					wnode("stop", tc.stopType, tc.stopCfg),
				},
				Hops: []workflow.Hop{whop("start", "success", "stop"), whop("detached", "success", "stop")},
			}
			p := workflow.Compile(context.Background(), def, workflow.CompileOptions{})
			if p.Problems.HasErrors() {
				t.Fatal(p.Problems)
			}
			r := workflow.Run(context.Background(), p, workflow.RunOptions{})
			if r.Status != tc.wantStatus {
				t.Fatalf("作业状态 %s，期望 %s（err=%v）", r.Status, tc.wantStatus, r.Err)
			}
			if r.Nodes["stop"].Status == workflow.NodeSkipped {
				t.Fatal("可达的汇合节点被跳过")
			}
			if got := r.Nodes["detached"].Status; got != workflow.NodeSkipped {
				t.Fatalf("孤立节点应为 skipped，实际 %s", got)
			}
		})
	}
}
