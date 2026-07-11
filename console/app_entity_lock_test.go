package main

import (
	"encoding/json"
	"sync"
	"testing"
)

// TestMergePreserveRuntime:配置字段取新值,运行态字段以既有实体为准;既有实体没有的运行态字段
// 从结果删除(不让前端夹带的旧运行态残留)。
func TestMergePreserveRuntime(t *testing.T) {
	old := json.RawMessage(`{"id":"a1","name":"旧名","port":8080,"status":"running","pid":"1234","version":"v9"}`)
	// 前端提交:改了 name/port,但夹带了 hydrate 来的旧运行态(status=stopped,version=v1),还带了个 old 里没有的 cpu。
	m := map[string]any{
		"id": "a1", "name": "新名", "port": float64(9090),
		"status": "stopped", "version": "v1", "cpu": "50%",
	}
	merged, err := mergePreserveRuntime(old, m)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatal(err)
	}
	if got["name"] != "新名" || got["port"].(float64) != 9090 {
		t.Fatalf("配置字段应取新值,got name=%v port=%v", got["name"], got["port"])
	}
	if got["status"] != "running" || got["version"] != "v9" {
		t.Fatalf("运行态应以既有实体为准(running/v9),got status=%v version=%v", got["status"], got["version"])
	}
	if got["pid"] != "1234" {
		t.Fatalf("既有 pid 应保留,got %v", got["pid"])
	}
	if _, ok := got["cpu"]; ok {
		t.Fatalf("既有实体无 cpu,前端夹带的 cpu 应被删除,got %v", got["cpu"])
	}
}

// TestLockAppEntitySerializes:并发读改写同一 app 实体在锁下不丢更新——G 个 goroutine 各自
// "读—计数+1—写回",最终计数须恰为 G。配 -race 跑可捕捉未串行化的问题。
func TestLockAppEntitySerializes(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	a := &api{store: s, appMu: map[string]*sync.Mutex{}}
	if err := s.putEntity("app", "a1", json.RawMessage(`{"id":"a1","n":0}`)); err != nil {
		t.Fatal(err)
	}

	const G = 50
	var wg sync.WaitGroup
	for i := 0; i < G; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := a.lockAppEntity("a1")
			defer unlock()
			raw, _ := s.getEntity("app", "a1")
			var m map[string]any
			json.Unmarshal(raw, &m)
			m["n"] = m["n"].(float64) + 1
			b, _ := json.Marshal(m)
			s.putEntity("app", "a1", b)
		}()
	}
	wg.Wait()

	raw, _ := s.getEntity("app", "a1")
	var m map[string]any
	json.Unmarshal(raw, &m)
	if m["n"].(float64) != G {
		t.Fatalf("锁下并发读改写不应丢更新,期望 n=%d,got %v", G, m["n"])
	}
}
