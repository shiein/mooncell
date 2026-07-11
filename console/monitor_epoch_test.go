package main

import (
	"sync"
	"testing"
)

// TestApplyMonitorStateEpochGuard:巡检回写带派发时捕获的代际。若回写前有操作(markBusy 推进代际),
// 该陈旧结果必须被丢弃,不能把刚落库的 stopped 又改回 running。
func TestApplyMonitorStateEpochGuard(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	a := &api{store: s, busy: map[string]int{}, appMu: map[string]*sync.Mutex{}, appEpoch: map[string]uint64{}}

	// 用户已把应用停到 stopped(权威落库)。
	putApp(t, s, "app1", "stopped")

	// 巡检在代际 e0=0 时派发,观测到 active=true(陈旧:此刻起了个停止操作)。
	e0 := a.appEpochOf("app1") // 0
	a.markBusy("app1")         // 停止操作开始:代际推进到 1
	defer a.unmarkBusy("app1")

	// 较晚返回的陈旧巡检结果(active=true)带 e0,应被代际校验丢弃,不翻动 stopped。
	a.applyMonitorState(monApp{ID: "app1", Name: "应用1", Runner: "systemd"}, true, "123", "1%", "10MB", e0)
	if got := appStatus(t, s, "app1"); got != "stopped" {
		t.Fatalf("陈旧巡检回写应被代际校验丢弃,status 应仍为 stopped,实际 %s", got)
	}
}

// TestApplyMonitorStateEpochMatchApplies:代际一致(期间无操作)时,巡检回写正常生效。
func TestApplyMonitorStateEpochMatchApplies(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	a := &api{store: s, busy: map[string]int{}, appMu: map[string]*sync.Mutex{}, appEpoch: map[string]uint64{}}

	putApp(t, s, "app1", "running")
	e0 := a.appEpochOf("app1")
	// 无操作介入,代际不变:探到不活动应正常判 failed。
	a.applyMonitorState(monApp{ID: "app1", Name: "应用1", Runner: "systemd"}, false, "", "", "", e0)
	if got := appStatus(t, s, "app1"); got != "failed" {
		t.Fatalf("代际一致时巡检应正常生效,running→不活动 应判 failed,实际 %s", got)
	}
}
