package main

import (
	"testing"
	"time"
)

// TestAgentDrainGate:Agent 自更新 draining 门禁——draining 时拒绝新操作,等在飞清零后放行,
// endDrain 恢复。防"部署进行中 self-exec 替换整个 Agent"。
func TestAgentDrainGate(t *testing.T) {
	a := &agent{}

	if !a.beginOp() {
		t.Fatal("非 draining 应允许操作")
	}

	if !a.beginDrain() {
		t.Fatal("首次 beginDrain 应成功")
	}
	if a.beginDrain() {
		t.Fatal("重复 beginDrain 应失败")
	}

	// draining 期间拒绝新操作。
	if a.beginOp() {
		t.Fatal("draining 时应拒绝新操作")
	}

	// 仍有在飞操作:短超时应判未 drained。
	if a.waitDrained(100 * time.Millisecond) {
		t.Fatal("仍有在飞操作,不应判 drained")
	}

	a.endOp()
	if !a.waitDrained(time.Second) {
		t.Fatal("在飞清零后应 drained")
	}

	a.endDrain()
	if !a.beginOp() {
		t.Fatal("endDrain 后应恢复接受操作")
	}
	a.endOp()
}

// TestAgentDrainBlocksLateOp:draining 置位后并发的迟到操作必须被拒,不挤进即将重启的窗口。
func TestAgentDrainBlocksLateOp(t *testing.T) {
	a := &agent{}
	a.beginOp() // 一个在飞
	if !a.beginDrain() {
		t.Fatal("beginDrain 应成功")
	}
	defer a.endDrain()

	done := make(chan bool, 1)
	go func() { done <- a.beginOp() }()
	if <-done {
		t.Fatal("draining 期间迟到的新操作必须被拒绝")
	}
	a.endOp()
}
