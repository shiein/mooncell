package main

import (
	"testing"
	"time"
)

// TestDrainGate:自更新 draining 门禁——draining 时拒绝新操作,等在飞清零后放行,endDrain 恢复。
func TestDrainGate(t *testing.T) {
	a := &api{busy: map[string]int{}, appEpoch: map[string]uint64{}}

	// 非 draining:正常开始一个操作(app1 在飞)。
	if !a.tryBeginOp("app1") {
		t.Fatal("非 draining 应允许操作")
	}

	// 进入 draining;重复进入失败。
	if !a.beginDrain() {
		t.Fatal("首次 beginDrain 应成功")
	}
	if a.beginDrain() {
		t.Fatal("重复 beginDrain 应失败(已在 draining)")
	}

	// draining 期间拒绝新操作。
	if a.tryBeginOp("app2") {
		t.Fatal("draining 时应拒绝新操作")
	}

	// app1 仍在飞:短超时内 waitDrained 应失败。
	if a.waitDrained(100 * time.Millisecond) {
		t.Fatal("仍有在飞操作,不应判 drained")
	}

	// app1 结束:应能 drained。
	a.unmarkBusy("app1")
	if !a.waitDrained(time.Second) {
		t.Fatal("在飞清零后应 drained")
	}

	// 退出 draining 后恢复接受操作。
	a.endDrain()
	if !a.tryBeginOp("app3") {
		t.Fatal("endDrain 后应恢复接受操作")
	}
	a.unmarkBusy("app3")
}

// TestDrainBlocksNewOpsDuringWait:模拟"检查通过后新操作又进来"的竞态——draining 置位后,
// 即便还没 drained,新 tryBeginOp 也一律被拒,不会挤进即将重启的窗口。
func TestDrainBlocksNewOpsDuringWait(t *testing.T) {
	a := &api{busy: map[string]int{}, appEpoch: map[string]uint64{}}
	a.tryBeginOp("inflight") // 一个在飞操作
	if !a.beginDrain() {
		t.Fatal("beginDrain 应成功")
	}
	defer a.endDrain()

	done := make(chan bool, 1)
	go func() { done <- a.tryBeginOp("late") }() // 并发的"迟到"新操作
	if <-done {
		t.Fatal("draining 期间迟到的新操作必须被拒绝")
	}
	a.unmarkBusy("inflight")
}
