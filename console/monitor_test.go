package main

import (
	"encoding/json"
	"sync/atomic"
	"testing"
)

func putApp(t *testing.T, s *Store, id, status string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"id": id, "name": id, "runner": "systemd", "status": status})
	if err := s.putEntity("app", id, b); err != nil {
		t.Fatalf("putApp: %v", err)
	}
}

func appStatus(t *testing.T, s *Store, id string) string {
	t.Helper()
	raw, ok := s.getEntity("app", id)
	if !ok {
		t.Fatalf("app %s 不存在", id)
	}
	var m map[string]any
	json.Unmarshal(raw, &m)
	st, _ := m["status"].(string)
	return st
}

func auditCount(s *Store) int {
	_, total, _ := s.pageAudit(0, 1)
	return total
}

// 健康巡检写回:只做保守迁移,Agent 不可达不臆造,审计仅在迁移时记一条。
func TestApplyMonitorStateTransitions(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	a := &api{store: s}

	// running + 探到不活动 → failed,记一条「掉线」审计。
	putApp(t, s, "app1", "running")
	a.applyMonitorState(monApp{ID: "app1", Name: "应用1", Runner: "systemd"}, false, "", "", "")
	if got := appStatus(t, s, "app1"); got != "failed" {
		t.Errorf("running→不活动 应判 failed,实际 %s", got)
	}
	if auditCount(s) != 1 {
		t.Errorf("掉线应记 1 条审计,实际 %d", auditCount(s))
	}

	// failed + 探到活动 → running(恢复),再记一条审计。
	a.applyMonitorState(monApp{ID: "app1", Name: "应用1", Runner: "systemd"}, true, "123", "1.0%", "50MB")
	if got := appStatus(t, s, "app1"); got != "running" {
		t.Errorf("failed→活动 应恢复 running,实际 %s", got)
	}
	if auditCount(s) != 2 {
		t.Errorf("恢复应再记 1 条(共 2),实际 %d", auditCount(s))
	}

	// running + 活动 → 幂等保持 running,不新增审计。
	a.applyMonitorState(monApp{ID: "app1", Name: "应用1", Runner: "systemd"}, true, "123", "1.0%", "50MB")
	if auditCount(s) != 2 {
		t.Errorf("running 幂等不应记审计,实际 %d", auditCount(s))
	}

	// 手动 stopped + 探到不活动 → 保持 stopped,不判掉线、不记审计。
	putApp(t, s, "app2", "stopped")
	a.applyMonitorState(monApp{ID: "app2", Name: "应用2", Runner: "systemd"}, false, "", "", "")
	if got := appStatus(t, s, "app2"); got != "stopped" {
		t.Errorf("手动 stopped 不应被翻动,实际 %s", got)
	}
	if auditCount(s) != 2 {
		t.Errorf("stopped 不活动不应记审计,实际 %d", auditCount(s))
	}
}

func TestBusyGuard(t *testing.T) {
	a := &api{busy: map[string]int{}}
	if a.isBusy("x") {
		t.Error("初始不应 busy")
	}
	a.markBusy("x")
	a.markBusy("x") // 重入(部署+巡检并发等)
	if !a.isBusy("x") {
		t.Error("mark 后应 busy")
	}
	a.unmarkBusy("x")
	if !a.isBusy("x") {
		t.Error("引用计数 2→1 仍应 busy")
	}
	a.unmarkBusy("x")
	if a.isBusy("x") {
		t.Error("计数归零应解除 busy")
	}
}

func TestMetricsStore(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	s.insertMetric("default", 1000, 10, 20, 30)
	s.insertMetric("default", 2000, 11, 21, 31)
	s.insertMetric("other", 1500, 5, 5, 5)

	pts, err := s.listMetrics("default", 0)
	if err != nil || len(pts) != 2 {
		t.Fatalf("default 应 2 点,实际 %d (err=%v)", len(pts), err)
	}
	if pts[0].Ts != 1000 || pts[1].Ts != 2000 {
		t.Errorf("应按时间升序,实际 %d,%d", pts[0].Ts, pts[1].Ts)
	}
	// since 过滤。
	if pts2, _ := s.listMetrics("default", 1500); len(pts2) != 1 {
		t.Errorf("since=1500 应剩 1 点,实际 %d", len(pts2))
	}
	// 裁剪早于 1500 的:删 default@1000 + other@... 不,other@1500 不删;default@1000 删。
	if n := s.trimMetrics(1500); n != 1 {
		t.Errorf("trimMetrics(1500) 应删 1(default@1000),实际 %d", n)
	}
	if pts3, _ := s.listMetrics("default", 0); len(pts3) != 1 {
		t.Errorf("裁剪后 default 应剩 1,实际 %d", len(pts3))
	}
}

// TestMonitorWorkerPool 验证有界并发池:每个下标恰好执行一次,且并发度不超上限。
func TestMonitorWorkerPool(t *testing.T) {
	n := 50
	var done int64
	var inflight int64
	var maxInflight int64
	monitorWorkerPool(n, 8, func(i int) {
		cur := atomic.AddInt64(&inflight, 1)
		for {
			mi := atomic.LoadInt64(&maxInflight)
			if cur <= mi || atomic.CompareAndSwapInt64(&maxInflight, mi, cur) {
				break
			}
		}
		atomic.AddInt64(&done, 1)
		atomic.AddInt64(&inflight, -1)
	})
	if int(done) != n {
		t.Errorf("应执行 %d 次,实际 %d", n, done)
	}
	if maxInflight > 8 {
		t.Errorf("并发度应 ≤ 8,实际峰值 %d", maxInflight)
	}
}

// TestMonitorWorkerPoolEmpty 验证空任务与边界收敛(concurrency>n 时按 n 起 worker)。
func TestMonitorWorkerPoolEmpty(t *testing.T) {
	monitorWorkerPool(0, 8, func(i int) { t.Error("空任务不应执行") })
	called := 0
	monitorWorkerPool(3, 100, func(i int) { called++ })
	if called != 3 {
		t.Errorf("n=3 应执行 3 次,实际 %d", called)
	}
}
