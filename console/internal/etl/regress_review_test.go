package etl

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"mooncell/console/internal/etl/store"
)

func waitRun(t *testing.T, s *Service, id string, ok func(store.Run) bool) store.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r, err := s.st.GetRun(id)
		if err != nil {
			t.Fatal(err)
		}
		if ok(r) {
			return r
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("运行 %s 未在期限内达到预期状态", id)
	return store.Run{}
}

func finished(r store.Run) bool {
	return r.Status == store.RunSucceeded || r.Status == store.RunFailed || r.Status == store.RunCanceled
}

// 调度重叠策略 queue：须等同定义的手动运行结束才开始；parallel 照常重叠；排队中可取消。
func TestRegressQueueWaitsForInflightRuns(t *testing.T) {
	h := newHarness(t)
	s := h.s
	doc := json.RawMessage(`{"kind":"workflow","nodes":[{"id":"s","type":"start"},{"id":"wait","type":"wait","config":{"seconds":1}}],"hops":[{"from":"s","to":"wait","fromPort":"success"}]}`)
	def, err := s.st.CreateDefinition(store.Definition{Kind: "workflow", Name: "queue", Doc: doc}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	running := func(r store.Run) bool { return r.Status == store.RunRunning }

	// ① 手动运行中触发 queue 调度 → 不重叠
	manual, err := s.Trigger(def.ID, TriggerOptions{Trigger: "manual", User: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, s, manual.ID, running)
	if err := s.fireSchedule(context.Background(), store.Schedule{ID: "sc", TargetID: def.ID, Overlap: "queue", Name: "q"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	var queued store.Run
	runs, _ := s.st.ListRuns(store.RunFilter{DefinitionID: def.ID})
	for _, r := range runs {
		if r.ID != manual.ID {
			queued = r
		}
	}
	if queued.ID == "" {
		t.Fatal("queue 调度应创建一次排队运行")
	}
	a := waitRun(t, s, manual.ID, finished)
	b := waitRun(t, s, queued.ID, finished)
	if a.Status != store.RunSucceeded || b.Status != store.RunSucceeded {
		t.Fatalf("两次运行都应成功：%s / %s", a.Status, b.Status)
	}
	if b.StartedAt < a.FinishedAt {
		t.Fatalf("queue 运行在手动运行结束前就开始了：manual 结束 %d，queue 开始 %d", a.FinishedAt, b.StartedAt)
	}

	// ② 排队等待中可取消，取消后不阻塞后续
	manual, _ = s.Trigger(def.ID, TriggerOptions{Trigger: "manual", User: "admin"})
	waitRun(t, s, manual.ID, running)
	q, err := s.Trigger(def.ID, TriggerOptions{Trigger: "schedule", User: "调度:q", Serial: true})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if r, _ := s.st.GetRun(q.ID); r.Status == store.RunRunning {
		t.Fatal("queue 运行不应在手动运行期间开始")
	}
	if err := s.Cancel(q.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if r := waitRun(t, s, q.ID, finished); r.Status != store.RunCanceled {
		t.Fatalf("排队中取消应为 canceled：%s", r.Status)
	}
	waitRun(t, s, manual.ID, finished)

	// ③ parallel：与手动运行重叠（不受影响）
	manual, _ = s.Trigger(def.ID, TriggerOptions{Trigger: "manual", User: "admin"})
	waitRun(t, s, manual.ID, running)
	p, _ := s.Trigger(def.ID, TriggerOptions{Trigger: "schedule", User: "调度:p"})
	pr := waitRun(t, s, p.ID, running)
	mr := waitRun(t, s, manual.ID, finished)
	if pr.StartedAt >= mr.FinishedAt {
		t.Fatalf("parallel 运行应与手动运行重叠：manual 结束 %d，parallel 开始 %d", mr.FinishedAt, pr.StartedAt)
	}
	waitRun(t, s, p.ID, finished)
}

// 恢复历史版本与普通保存同一授权口径：旧版本引用已撤权定义 → 403。
func TestRegressRestoreVersionChecksRefs(t *testing.T) {
	h := newHarness(t)
	plDoc, _ := json.Marshal(map[string]any{
		"kind": "pipeline", "params": []any{},
		"nodes": []map[string]any{{"id": "g", "type": "generate_rows", "name": "g", "config": map[string]any{"limit": 1}}},
	})
	pA, _ := h.s.st.CreateDefinition(store.Definition{Kind: "pipeline", Name: "甲", Doc: plDoc}, "admin")
	pB, _ := h.s.st.CreateDefinition(store.Definition{Kind: "pipeline", Name: "乙", Doc: plDoc}, "admin")
	wfDoc := func(ref string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{
			"kind": "workflow", "params": []any{},
			"nodes": []map[string]any{
				{"id": "start", "type": "start", "name": "开始", "config": map[string]any{}},
				{"id": "t", "type": "pipeline", "name": "转换", "config": map[string]any{"pipeline": ref}},
			},
			"hops": []map[string]any{{"id": "h", "from": "start", "fromPort": "success", "to": "t", "toPort": "main"}},
		})
		return b
	}
	// r1 引用乙，r2 改为引用甲（均由 admin 保存）
	wf, err := h.s.st.CreateDefinition(store.Definition{Kind: "workflow", Name: "作业", Doc: wfDoc(pB.ID)}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	oldRev := wf.Revision
	if wf, err = h.s.st.UpdateDefinition(store.Definition{ID: wf.ID, Kind: "workflow", Name: wf.Name, Doc: wfDoc(pA.ID)}, wf.Revision, "admin"); err != nil {
		t.Fatal(err)
	}
	h.user, h.role = "u1", "user"
	if err := store.SetUserGrants(h.db, "u1", []store.DefinitionGrant{
		{DefinitionID: wf.ID, AccessMode: store.ModeEdit}, {DefinitionID: pA.ID, AccessMode: store.ModeUse},
	}, "admin"); err != nil {
		t.Fatal(err)
	}
	path := "/api/etl/definitions/" + wf.ID + "/versions/" + strconv.Itoa(oldRev) + "/restore"
	if code := h.call("POST", path, nil, nil); code != 403 {
		t.Fatalf("恢复引用未授权定义的版本应 403：%d", code)
	}
	if cur, _ := h.s.st.GetDefinition(wf.ID); cur.Revision != wf.Revision {
		t.Fatalf("被拒后不应产生新版本：%d → %d", wf.Revision, cur.Revision)
	}
	// 补授权后可恢复；管理员不受限
	if err := store.SetUserGrants(h.db, "u1", []store.DefinitionGrant{
		{DefinitionID: wf.ID, AccessMode: store.ModeEdit}, {DefinitionID: pA.ID, AccessMode: store.ModeUse}, {DefinitionID: pB.ID, AccessMode: store.ModeUse},
	}, "admin"); err != nil {
		t.Fatal(err)
	}
	if code := h.call("POST", path, nil, nil); code != 200 {
		t.Fatalf("补 use 授权后恢复应 200：%d", code)
	}
}
