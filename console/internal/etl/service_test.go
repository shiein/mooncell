package etl

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/store"
)

type harness struct {
	t   *testing.T
	s   *Service
	srv *httptest.Server
	db  *sql.DB
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "console.db")
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // 与 Console 一致：单连接，暴露任何「rows 未关闭又发语句」的自锁
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, dbPath, Config{})
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	mux := http.NewServeMux()
	s.RegisterRoutes(mux, func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { next(w, WithUser(r, "admin")) }
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close(); s.Close(); db.Close() })
	return &harness{t: t, s: s, srv: srv, db: db}
}

func (h *harness) call(method, path string, body any, out any) int {
	h.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func TestServiceLifecycle(t *testing.T) {
	h := newHarness(t)

	// 连接：sqlite 是测试方言，API 拒绝；直接经 store 建（密码仍加密）
	var bad map[string]any
	if code := h.call("POST", "/api/etl/connections", map[string]any{"name": "x", "dbType": "sqlite", "host": "h", "port": 1, "username": "u"}, &bad); code != 400 {
		t.Fatalf("测试方言不应可经 API 创建：%d %v", code, bad)
	}
	target := filepath.Join(t.TempDir(), "target.db")
	ct, _ := h.s.box.Seal("secret-pw", "conn_test")
	c, err := h.s.st.CreateConnectionWithID(store.Connection{Config: conn.Config{ID: "conn_test", Name: "目标库", DBType: "sqlite", Database: target}}, &ct, "admin")
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	h.db.QueryRow(`SELECT secret_cipher FROM etl_connections WHERE id=?`, c.ID).Scan(&raw)
	if strings.Contains(raw, "secret-pw") || !strings.HasPrefix(raw, "v1:") {
		t.Fatalf("密码未加密落库：%q", raw)
	}
	var listed map[string][]map[string]any
	h.call("GET", "/api/etl/connections", nil, &listed)
	if b, _ := json.Marshal(listed); strings.Contains(string(b), "v1:") || strings.Contains(string(b), "secret") {
		t.Fatalf("连接列表泄露密文：%s", b)
	}
	hd, err := h.s.pool.Get(t.Context(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hd.DB.Exec(`CREATE TABLE out (n INTEGER, label TEXT)`); err != nil {
		t.Fatal(err)
	}

	// 定义
	doc := map[string]any{
		"kind": "pipeline", "params": []any{},
		"nodes": []map[string]any{
			{"id": "gen", "type": "generate_rows", "name": "生成", "config": map[string]any{"limit": 2500, "sequenceField": "n"}},
			{"id": "calc", "type": "calculator", "name": "标签", "config": map[string]any{"fields": []map[string]any{{"name": "label", "expr": `"#" + toString(n)`}}}},
			{"id": "out", "type": "table_output", "name": "写入", "config": map[string]any{"connection": c.ID, "table": "out", "commitSize": 1000}},
		},
		"hops": []map[string]any{
			{"id": "h1", "from": "gen", "fromPort": "main", "to": "calc", "toPort": "main"},
			{"id": "h2", "from": "calc", "fromPort": "main", "to": "out", "toPort": "main"},
		},
	}
	var def store.Definition
	if code := h.call("POST", "/api/etl/definitions", map[string]any{"kind": "pipeline", "name": "生成到目标", "doc": doc}, &def); code != 200 || def.ID == "" {
		t.Fatalf("创建失败：%d", code)
	}

	var v struct {
		Problems []map[string]any             `json:"problems"`
		Hops     map[string][]conn.ColumnMeta `json:"hops"`
	}
	h.call("POST", "/api/etl/definitions/validate", map[string]any{"kind": "pipeline", "doc": doc}, &v)
	if len(v.Problems) != 0 || len(v.Hops["h2"]) != 2 {
		t.Fatalf("校验结果异常：%+v", v)
	}

	var pv map[string]any
	h.call("POST", "/api/etl/pipelines/preview", map[string]any{"doc": doc, "nodeId": "calc", "limit": 5}, &pv)
	if rows := pv["ports"].(map[string]any)["main"].(map[string]any)["rows"].([]any); len(rows) != 5 {
		t.Fatalf("预览行数错误：%v", pv)
	}

	// 乐观锁
	var conflict map[string]any
	if code := h.call("PUT", "/api/etl/definitions/"+def.ID, map[string]any{"doc": doc, "revision": def.Revision + 5}, &conflict); code != 409 {
		t.Fatalf("过期 revision 应 409：%d", code)
	}

	// 运行
	var run store.Run
	h.call("POST", "/api/etl/definitions/"+def.ID+"/run", map[string]any{}, &run)
	deadline := time.Now().Add(15 * time.Second)
	var detail struct {
		Run   store.Run       `json:"run"`
		Nodes []store.RunNode `json:"nodes"`
	}
	for time.Now().Before(deadline) {
		h.call("GET", "/api/etl/runs/"+run.ID, nil, &detail)
		if detail.Run.Status != store.RunQueued && detail.Run.Status != store.RunRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if detail.Run.Status != store.RunSucceeded {
		t.Fatalf("运行应成功：%+v", detail.Run)
	}
	if detail.Run.RowsWritten != 2500 || len(detail.Nodes) != 3 {
		t.Fatalf("运行汇总错误：written=%d nodes=%d", detail.Run.RowsWritten, len(detail.Nodes))
	}
	for _, n := range detail.Nodes {
		if n.Status != "succeeded" || n.Metrics.Read == 0 && n.NodeID != "gen" {
			t.Fatalf("节点状态/度量异常：%+v", n)
		}
	}
	var n int
	hd.DB.QueryRow(`SELECT count(*) FROM out`).Scan(&n)
	if n != 2500 {
		t.Fatalf("目标表应有 2500 行，实际 %d", n)
	}
	var logs struct {
		Logs []map[string]any `json:"logs"`
	}
	h.call("GET", "/api/etl/runs/"+run.ID+"/logs", nil, &logs)
	if len(logs.Logs) == 0 {
		t.Fatal("应有运行日志")
	}

	// 调度预览与保存
	var sp map[string]any
	h.call("GET", "/api/etl/schedules/preview?cron=0+2+*+*+*&tz=Asia%2FShanghai", nil, &sp)
	if sp["valid"] != true || sp["description"] != "每天 02:00" || len(sp["next"].([]any)) != 5 {
		t.Fatalf("调度预览异常：%v", sp)
	}
	var sched store.Schedule
	if code := h.call("POST", "/api/etl/schedules", map[string]any{"targetId": def.ID, "name": "夜间", "cron": "0 2 * * *", "enabled": true}, &sched); code != 200 || sched.NextFireAt == 0 {
		t.Fatalf("保存调度失败：%d %+v", code, sched)
	}
	var badSched map[string]any
	if code := h.call("POST", "/api/etl/schedules", map[string]any{"targetId": def.ID, "cron": "99 * * * *", "enabled": true}, &badSched); code != 400 {
		t.Fatalf("非法 cron 应 400：%d", code)
	}

	// 概览
	var ov map[string]any
	h.call("GET", "/api/etl/overview", nil, &ov)
	if ov["pipelines"].(float64) != 1 || ov["schedulesEnabled"].(float64) != 1 {
		t.Fatalf("概览异常：%v", ov)
	}
}

// drain：有在途运行时拒绝；结束后允许；draining 期间拒绝新运行。
func TestDrainGate(t *testing.T) {
	h := newHarness(t)
	doc, _ := json.Marshal(map[string]any{"kind": "pipeline", "nodes": []map[string]any{
		{"id": "g", "type": "generate_rows", "name": "g", "config": map[string]any{"limit": 200_000_000, "sequenceField": "n"}},
	}})
	def, err := h.s.st.CreateDefinition(store.Definition{Kind: "pipeline", Name: "长任务", Doc: doc}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	run, err := h.s.Trigger(def.ID, TriggerOptions{Trigger: "manual", User: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := h.s.BeginDrain(); !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "长任务") {
		t.Fatalf("有在途运行时应拒绝 drain：%v", err)
	}
	if err := h.s.Cancel(run.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for h.s.runs.get(run.ID) != nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	r, _ := h.s.st.GetRun(run.ID)
	if r.Status != store.RunCanceled {
		t.Fatalf("取消后状态应为 canceled：%s", r.Status)
	}
	if err := h.s.BeginDrain(); err != nil {
		t.Fatalf("空闲时应允许 drain：%v", err)
	}
	if _, err := h.s.Trigger(def.ID, TriggerOptions{Trigger: "manual"}); err == nil {
		t.Fatal("draining 期间应拒绝新运行")
	}
	h.s.EndDrain()
}

// 作业引用的转换定义随运行快照固定：触发后修改转换不影响本次执行与续跑。
func TestRunSnapshotRefs(t *testing.T) {
	h := newHarness(t)
	target := filepath.Join(t.TempDir(), "target.db")
	ct, _ := h.s.box.Seal("pw", "conn_t")
	c, err := h.s.st.CreateConnectionWithID(store.Connection{Config: conn.Config{ID: "conn_t", Name: "目标", DBType: "sqlite", Database: target}}, &ct, "admin")
	if err != nil {
		t.Fatal(err)
	}
	hd, err := h.s.pool.Get(t.Context(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hd.DB.Exec(`CREATE TABLE out (n INTEGER)`); err != nil {
		t.Fatal(err)
	}

	pipeDoc := func(limit int, connID string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{
			"kind": "pipeline", "params": []any{},
			"nodes": []map[string]any{
				{"id": "gen", "type": "generate_rows", "name": "生成", "config": map[string]any{"limit": limit, "sequenceField": "n"}},
				{"id": "out", "type": "table_output", "name": "写入", "config": map[string]any{"connection": connID, "table": "out"}},
			},
			"hops": []map[string]any{{"id": "h", "from": "gen", "fromPort": "main", "to": "out", "toPort": "main"}},
		})
		return b
	}
	wfDoc := func(pid string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{
			"kind": "workflow", "params": []any{},
			"nodes": []map[string]any{
				{"id": "start", "type": "start", "name": "开始", "config": map[string]any{}},
				{"id": "t", "type": "pipeline", "name": "转换", "config": map[string]any{"pipeline": pid, "params": map[string]any{}}},
			},
			"hops": []map[string]any{{"id": "h", "from": "start", "fromPort": "success", "to": "t", "toPort": "main"}},
		})
		return b
	}
	waitDone := func(runID string) store.Run {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			r, _ := h.s.st.GetRun(runID)
			if r.Status != store.RunQueued && r.Status != store.RunRunning {
				return r
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatal("运行未在时限内结束")
		return store.Run{}
	}

	// ① 触发即固定引用：运行后修改转换，refs 仍是触发时版本
	p, err := h.s.st.CreateDefinition(store.Definition{Kind: "pipeline", Name: "写目标", Doc: pipeDoc(3, c.ID)}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	w, err := h.s.st.CreateDefinition(store.Definition{Kind: "workflow", Name: "作业", Doc: wfDoc(p.ID)}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	run, err := h.s.Trigger(w.ID, TriggerOptions{Trigger: "manual", User: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	r1 := waitDone(run.ID)
	if r1.Status != store.RunSucceeded {
		t.Fatalf("首次运行应成功：%s %s", r1.Status, r1.Error)
	}
	// 修改转换（limit 3→9），快照仍应是 v1
	if _, err := h.s.st.UpdateDefinition(store.Definition{ID: p.ID, Kind: "pipeline", Name: "写目标", Doc: pipeDoc(9, c.ID)}, p.Revision, "admin"); err != nil {
		t.Fatal(err)
	}
	got, _ := h.s.st.GetRun(run.ID)
	var refs map[string]runRef
	if err := json.Unmarshal(got.Refs, &refs); err != nil || len(refs) != 1 {
		t.Fatalf("refs 应含 1 个被引定义：%s %v", got.Refs, err)
	}
	rr := refs[p.ID]
	if rr.Name != "写目标" || rr.Revision != 1 || rr.Kind != "pipeline" {
		t.Fatalf("refs 内容异常：%+v", rr)
	}
	var snapDoc struct {
		Nodes []struct {
			Config struct {
				Limit int `json:"limit"`
			} `json:"config"`
		} `json:"nodes"`
	}
	_ = json.Unmarshal(rr.Doc, &snapDoc)
	if snapDoc.Nodes[0].Config.Limit != 3 {
		t.Fatalf("快照应固定 limit=3，实际 %d", snapDoc.Nodes[0].Config.Limit)
	}
	var n int
	hd.DB.QueryRow(`SELECT count(*) FROM out`).Scan(&n)
	if n != 3 {
		t.Fatalf("目标表应 3 行：%d", n)
	}

	// ② 续跑固定快照：转换在快照中是坏版本（连接不存在），续跑前修好定义也不应生效
	p2, err := h.s.st.CreateDefinition(store.Definition{Kind: "pipeline", Name: "坏转换", Doc: pipeDoc(1, "missing_conn")}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	w2, err := h.s.st.CreateDefinition(store.Definition{Kind: "workflow", Name: "坏作业", Doc: wfDoc(p2.ID)}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	run2, err := h.s.Trigger(w2.ID, TriggerOptions{Trigger: "manual", User: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	r2 := waitDone(run2.ID)
	if r2.Status != store.RunFailed {
		t.Fatalf("连接缺失的运行应失败：%s", r2.Status)
	}
	// 修好转换：换成真连接
	if _, err := h.s.st.UpdateDefinition(store.Definition{ID: p2.ID, Kind: "pipeline", Name: "坏转换", Doc: pipeDoc(1, c.ID)}, p2.Revision, "admin"); err != nil {
		t.Fatal(err)
	}
	// 续跑必须用快照里的坏版本，仍应失败（修复前行为：读当前定义 → 会成功）
	res2, err := h.s.Trigger("", TriggerOptions{Trigger: "resume", ResumeOf: run2.ID, User: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	rr2 := waitDone(res2.ID)
	if rr2.Status != store.RunFailed {
		t.Fatalf("续跑应沿用快照中的坏版本而失败，实际 %s", rr2.Status)
	}

	// ③ 兼容：旧运行无 refs（模拟 refs_json='{}'），续跑回落当前定义 → 修好的转换应成功
	if _, err := h.db.Exec(`UPDATE etl_runs SET refs_json='{}' WHERE id=?`, run2.ID); err != nil {
		t.Fatal(err)
	}
	res3, err := h.s.Trigger("", TriggerOptions{Trigger: "resume", ResumeOf: run2.ID, User: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	rr3 := waitDone(res3.ID)
	if rr3.Status != store.RunSucceeded {
		t.Fatalf("旧运行（无 refs）续跑应回落当前定义并成功：%s %s", rr3.Status, rr3.Error)
	}

	// ④ 引用缺失在触发时尽早报错
	wBad, err := h.s.st.CreateDefinition(store.Definition{Kind: "workflow", Name: "空引用", Doc: wfDoc("no_such_def")}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Trigger(wBad.ID, TriggerOptions{Trigger: "manual"}); err == nil || !strings.Contains(err.Error(), "引用的定义") {
		t.Fatalf("引用缺失应在触发时报错：%v", err)
	}
}
