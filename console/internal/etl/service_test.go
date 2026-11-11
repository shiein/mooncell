package etl

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/store"
)

type harness struct {
	t    *testing.T
	s    *Service
	srv  *httptest.Server
	db   *sql.DB
	dir  string // 数据目录（db 与 file_root 所在）
	user string
	role string
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
	h := &harness{t: t, s: s, db: db, dir: dir, user: "admin", role: "admin"}
	s.RegisterRoutes(mux, func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { next(w, WithUser(r, h.user, h.role)) }
	})
	srv := httptest.NewServer(mux)
	h.srv = srv
	t.Cleanup(func() { srv.Close(); s.Close(); db.Close() })
	return h
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

// 拒绝行随运行记录采样：error 端口连接的步骤，其前 N 条错误行可经运行详情查看。
func TestErrorRowSamples(t *testing.T) {
	h := newHarness(t)
	target := filepath.Join(t.TempDir(), "target.db")
	ct, _ := h.s.box.Seal("pw", "conn_s")
	c, err := h.s.st.CreateConnectionWithID(store.Connection{Config: conn.Config{ID: "conn_s", Name: "目标", DBType: "sqlite", Database: target}}, &ct, "admin")
	if err != nil {
		t.Fatal(err)
	}
	hd, err := h.s.pool.Get(t.Context(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{
		`CREATE TABLE src (id INTEGER PRIMARY KEY, qty INTEGER)`,
		`CREATE TABLE dst (id INTEGER PRIMARY KEY, qty INTEGER)`,
		`CREATE TABLE bad (id INTEGER, qty INTEGER, _error_code TEXT, _error_message TEXT, _error_field TEXT, _error_step TEXT)`,
	} {
		if _, err := hd.DB.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 9; i++ {
		if _, err := hd.DB.Exec(`INSERT INTO src VALUES (?, ?)`, i, i*10); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := hd.DB.Exec(`INSERT INTO dst VALUES (5, 555)`); err != nil { // id=5 冲突 → 退化逐行后拒绝
		t.Fatal(err)
	}
	doc, _ := json.Marshal(map[string]any{
		"kind": "pipeline", "params": []any{},
		"nodes": []map[string]any{
			{"id": "in", "type": "table_input", "name": "源", "config": map[string]any{"connection": c.ID, "sql": "select id, qty from src order by id"}},
			{"id": "out", "type": "table_output", "name": "写入", "config": map[string]any{"connection": c.ID, "table": "dst", "commitSize": 20}},
			{"id": "err", "type": "table_output", "name": "错误表", "config": map[string]any{"connection": c.ID, "table": "bad", "commitSize": 100}},
		},
		"hops": []map[string]any{
			{"id": "h1", "from": "in", "fromPort": "main", "to": "out", "toPort": "main"},
			{"id": "h2", "from": "out", "fromPort": "error", "to": "err", "toPort": "main"},
		},
	})
	def, err := h.s.st.CreateDefinition(store.Definition{Kind: "pipeline", Name: "冲突采样", Doc: doc}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	run, err := h.s.Trigger(def.ID, TriggerOptions{Trigger: "manual", User: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	var r store.Run
	for time.Now().Before(deadline) {
		r, _ = h.s.st.GetRun(run.ID)
		if r.Status != store.RunQueued && r.Status != store.RunRunning {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if r.Status != store.RunSucceeded {
		t.Fatalf("带错误端口的冲突行应拒绝而非失败：%s %s", r.Status, r.Error)
	}
	nodes, err := h.s.st.ListRunNodes(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out *store.RunNode
	for i := range nodes {
		if nodes[i].NodeID == "out" {
			out = &nodes[i]
		}
	}
	if out == nil || out.Samples == nil {
		t.Fatalf("out 节点应有拒绝行采样：%+v", out)
	}
	es := out.Samples
	if es.Total != 1 || len(es.Rows) != 1 {
		t.Fatalf("采样应为 Total=1/Rows=1：%+v", es)
	}
	if len(es.Fields) != 6 || es.Fields[2] != "_error_code" {
		t.Fatalf("采样字段应为基础列+4 错误字段：%v", es.Fields)
	}
	row := es.Rows[0]
	if row[0] != float64(5) && row[0] != int64(5) {
		t.Fatalf("采样行首列应为冲突的 id=5：%v", row)
	}
	if row[3] == nil || !strings.Contains(fmt.Sprint(row[3]), "UNIQUE") {
		t.Fatalf("采样行 _error_message 应含约束冲突信息：%v", row)
	}
}

// 采样上限与值截断：Total 持续计数，行数封顶 maxErrorSamples，超长字符串/字节按界截断。
func TestErrorSampleBounds(t *testing.T) {
	a := &activeRun{}
	fields := []string{"id", "msg", "_error_code"}
	for i := 0; i < maxErrorSamples+5; i++ {
		a.addSample("", "n", fields, core.Row{i, "x", "e"})
	}
	es := a.samples["/n"]
	if es.Total != maxErrorSamples+5 || len(es.Rows) != maxErrorSamples {
		t.Fatalf("上限应为 %d 行/Total=%d：%+v", maxErrorSamples, maxErrorSamples+5, es)
	}
	long := strings.Repeat("长", maxSampleValueLen) // 每字符 3 字节，总长远超上限
	a2 := &activeRun{}
	a2.addSample("", "n", fields, core.Row{1, long, []byte{0xde, 0xad, 0xbe, 0xef}})
	got := a2.samples["/n"].Rows[0]
	s, ok := got[1].(string)
	if !ok || len(s) > maxSampleValueLen+8 || !strings.HasSuffix(s, "…") {
		t.Fatalf("超长字符串应截断：%q", got[1])
	}
	if got[2] != "deadbeef" {
		t.Fatalf("字节值应转 hex：%v", got[2])
	}
	// 大字节串：hex 前缀 + 字节数说明
	big := make([]byte, maxSampleValueLen+10)
	a2.addSample("", "n", fields, core.Row{1, "m", big})
	got = a2.samples["/n"].Rows[1]
	if b, ok := got[2].(string); !ok || !strings.Contains(b, "字节") || len(b) > 80 {
		t.Fatalf("大字节串应有界：%v", got[2])
	}
	if sampleValue(nil) != nil || sampleValue(int64(7)) != int64(7) {
		t.Fatal("常规值应原样保留")
	}
}

// 导出/导入：包格式校验、引用按名映射、缺失项由调用方映射或保留。
func TestExportImport(t *testing.T) {
	h := newHarness(t)
	target := filepath.Join(t.TempDir(), "t.db")
	ct, _ := h.s.box.Seal("pw", "conn_e")
	c, err := h.s.st.CreateConnectionWithID(store.Connection{Config: conn.Config{ID: "conn_e", Name: "源库", DBType: "sqlite", Database: target}}, &ct, "admin")
	if err != nil {
		t.Fatal(err)
	}
	ct2, _ := h.s.box.Seal("pw2", "conn_e2")
	c2, err := h.s.st.CreateConnectionWithID(store.Connection{Config: conn.Config{ID: "conn_e2", Name: "源库二号", DBType: "sqlite", Database: target}}, &ct2, "admin")
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := json.Marshal(map[string]any{
		"kind": "pipeline", "params": []any{},
		"nodes": []map[string]any{
			{"id": "in", "type": "table_input", "name": "源", "config": map[string]any{"connection": c.ID, "sql": "select 1"}},
			{"id": "d", "type": "dummy", "name": "空", "config": map[string]any{}},
		},
		"hops": []map[string]any{{"id": "h", "from": "in", "fromPort": "main", "to": "d", "toPort": "main"}},
	})
	def, err := h.s.st.CreateDefinition(store.Definition{Kind: "pipeline", Name: "导出源", Folder: "组A", Doc: doc}, "admin")
	if err != nil {
		t.Fatal(err)
	}

	// ① 导出：包标识/版本/引用齐全
	var ex map[string]any
	if code := h.call("GET", "/api/etl/definitions/"+def.ID+"/export", nil, &ex); code != 200 {
		t.Fatalf("导出应 200：%d %v", code, ex)
	}
	if ex["format"] != "mooncell-etl-export" || ex["version"] != float64(1) {
		t.Fatalf("包标识/版本异常：%v", ex)
	}
	refs := ex["references"].(map[string]any)
	conns := refs["connections"].([]any)
	if len(conns) != 1 || conns[0].(map[string]any)["name"] != "源库" {
		t.Fatalf("导出引用应含连接名：%v", refs)
	}

	// ② 预览：同名冲突 → finalName 加后缀；连接按名匹配到 conn_e
	var pv map[string]any
	if code := h.call("POST", "/api/etl/definitions/import/preview", map[string]any{"document": ex}, &pv); code != 200 {
		t.Fatalf("预览应 200：%d %v", code, pv)
	}
	if pv["finalName"] != "导出源 (导入)" {
		t.Fatalf("冲突名应加后缀：%v", pv["finalName"])
	}
	item := pv["connections"].([]any)[0].(map[string]any)
	if item["matched"] == nil || item["matched"].(map[string]any)["id"] != c.ID {
		t.Fatalf("应按名匹配到原连接：%v", item)
	}

	// ③ 导入：显式映射到「源库二号」→ 文档里 connection 应换成 c2.ID
	var imp store.Definition
	if code := h.call("POST", "/api/etl/definitions/import", map[string]any{
		"document":      ex,
		"connectionMap": map[string]string{c.ID: c2.ID},
	}, &imp); code != 200 {
		t.Fatalf("导入应 200：%d %v", code, imp)
	}
	if imp.Name != "导出源 (导入)" || imp.ID == def.ID {
		t.Fatalf("导入名/ID 异常：%+v", imp)
	}
	var got struct {
		Nodes []struct {
			Config map[string]string `json:"config"`
		} `json:"nodes"`
	}
	_ = json.Unmarshal(imp.Doc, &got)
	if got.Nodes[0].Config["connection"] != c2.ID {
		t.Fatalf("连接未按映射替换：%v", got.Nodes[0].Config)
	}

	// ④ 跨库场景：连接 ID 悬空但名称存在 → 未显式映射时自动按名匹配
	var exd exportDoc
	raw, _ := json.Marshal(ex)
	_ = json.Unmarshal(raw, &exd)
	var ddoc map[string]any
	_ = json.Unmarshal(exd.Definition.Doc, &ddoc)
	ddoc["nodes"].([]any)[0].(map[string]any)["config"].(map[string]any)["connection"] = "conn_gone"
	exd.Definition.Doc, _ = json.Marshal(ddoc)
	exd.References.Connections[0].ID = "conn_gone"
	var imp2 store.Definition
	if code := h.call("POST", "/api/etl/definitions/import", map[string]any{"document": exd}, &imp2); code != 200 {
		t.Fatalf("悬空引用导入应 200：%d %v", code, imp2)
	}
	_ = json.Unmarshal(imp2.Doc, &got)
	if got.Nodes[0].Config["connection"] != c.ID {
		t.Fatalf("应自动按名匹配到「源库」：%v", got.Nodes[0].Config)
	}

	// ⑤ 格式/版本拒绝
	var bad map[string]any
	if code := h.call("POST", "/api/etl/definitions/import", map[string]any{
		"document": map[string]any{"format": "other", "version": 1},
	}, &bad); code != 400 {
		t.Fatalf("非导出格式应 400：%d %v", code, bad)
	}
	ex["version"] = 99
	if code := h.call("POST", "/api/etl/definitions/import", map[string]any{"document": ex}, &bad); code != 400 {
		t.Fatalf("未知版本应 400：%d %v", code, bad)
	}
}

// 作业引用定义的导出/导入：references.definitions 按名匹配或映射。
func TestExportImportDefRefs(t *testing.T) {
	h := newHarness(t)
	sub, _ := json.Marshal(map[string]any{
		"kind": "pipeline", "params": []any{},
		"nodes": []map[string]any{{"id": "g", "type": "generate_rows", "name": "g", "config": map[string]any{"limit": 1}}},
	})
	p, err := h.s.st.CreateDefinition(store.Definition{Kind: "pipeline", Name: "子转换", Doc: sub}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	wfdoc, _ := json.Marshal(map[string]any{
		"kind": "workflow", "params": []any{},
		"nodes": []map[string]any{
			{"id": "start", "type": "start", "name": "开始", "config": map[string]any{}},
			{"id": "t", "type": "pipeline", "name": "转换", "config": map[string]any{"pipeline": p.ID}},
			{"id": "n", "type": "notify", "name": "通知", "config": map[string]any{"channel": "webhook", "url": "https://x", "secretCipher": "v1:xyz", "aadSuffix": p.ID + "/n"}},
		},
		"hops": []map[string]any{{"id": "h", "from": "start", "fromPort": "success", "to": "t", "toPort": "main"}},
	})
	wf, err := h.s.st.CreateDefinition(store.Definition{Kind: "workflow", Name: "父作业", Doc: wfdoc}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	var ex map[string]any
	if code := h.call("GET", "/api/etl/definitions/"+wf.ID+"/export", nil, &ex); code != 200 {
		t.Fatalf("导出应 200：%d %v", code, ex)
	}
	// notify 的密文不应出现在导出包里（AAD 绑旧 defID，不可解）
	if strings.Contains(fmt.Sprint(ex), "v1:xyz") || strings.Contains(fmt.Sprint(ex), "aadSuffix") {
		t.Fatal("通知凭据密文不应随导出包迁移")
	}
	// 子转换引用应出现在 references.definitions
	dr := ex["references"].(map[string]any)["definitions"].([]any)
	if len(dr) != 1 || dr[0].(map[string]any)["name"] != "子转换" {
		t.Fatalf("引用定义缺失：%v", dr)
	}
	// 导入：自动匹配同名子转换
	var imp store.Definition
	if code := h.call("POST", "/api/etl/definitions/import", map[string]any{"document": ex}, &imp); code != 200 {
		t.Fatalf("导入应 200：%d %v", code, imp)
	}
	var got struct {
		Nodes []struct {
			Config map[string]string `json:"config"`
		} `json:"nodes"`
	}
	_ = json.Unmarshal(imp.Doc, &got)
	if got.Nodes[1].Config["pipeline"] != p.ID {
		t.Fatalf("未映射时定义引用应保留原值（同实例名匹配）：%v", got.Nodes[1].Config)
	}
}

// P3.16 定义级授权：use=查看+运行；edit=含写；无授权=不可见。admin 隐式全量。
func TestDefinitionGrants(t *testing.T) {
	h := newHarness(t)
	docRaw, _ := json.Marshal(map[string]any{
		"kind": "pipeline", "params": []any{},
		"nodes": []map[string]any{
			{"id": "g", "type": "generate_rows", "name": "生成", "config": map[string]any{"limit": 2}},
			{"id": "d", "type": "dummy", "name": "空", "config": map[string]any{}},
		},
		"hops": []map[string]any{{"id": "h", "from": "g", "fromPort": "main", "to": "d", "toPort": "main"}},
	})
	doc := json.RawMessage(docRaw)
	d1, err := h.s.st.CreateDefinition(store.Definition{Kind: "pipeline", Name: "授权目标", Doc: doc}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	d2, err := h.s.st.CreateDefinition(store.Definition{Kind: "pipeline", Name: "未授权", Doc: doc}, "admin")
	if err != nil {
		t.Fatal(err)
	}

	// ---- 普通用户无任何授权 ----
	h.user, h.role = "u1", "user"
	var defs struct {
		Definitions []store.DefinitionSummary `json:"definitions"`
	}
	if code := h.call("GET", "/api/etl/definitions", nil, &defs); code != 200 || len(defs.Definitions) != 0 {
		t.Fatalf("无授权应得空列表：%d %v", code, defs.Definitions)
	}
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/etl/definitions/" + d1.ID, nil},
		{"POST", "/api/etl/definitions/" + d1.ID + "/run", map[string]any{}},
		{"GET", "/api/etl/definitions/" + d1.ID + "/export", nil},
		{"POST", "/api/etl/definitions", map[string]any{"kind": "pipeline", "name": "x"}},
		{"POST", "/api/etl/definitions/import/preview", map[string]any{"document": map[string]any{}}},
		{"POST", "/api/etl/connections", map[string]any{"name": "c", "dbType": "postgres", "host": "h", "username": "u"}},
		{"POST", "/api/etl/pipelines/preview", map[string]any{"doc": doc, "nodeId": "d", "limit": 2}},
	} {
		if code := h.call(tc.method, tc.path, tc.body, nil); code != 403 {
			t.Fatalf("%s %s 无授权应 403，实得 %d", tc.method, tc.path, code)
		}
	}
	// 连接只读元数据（设计器需要）与普通读路径对普通用户放开
	var conns map[string]any
	if code := h.call("GET", "/api/etl/connections", nil, &conns); code != 200 {
		t.Fatalf("连接列表应 200：%d", code)
	}
	if code := h.call("GET", "/api/etl/catalog", nil, nil); code != 200 {
		t.Fatalf("目录应 200：%d", code)
	}

	// ---- use 授权：可见、可运行、不可写 ----
	if err := store.SetUserGrants(h.db, "u1", []store.DefinitionGrant{{DefinitionID: d1.ID, AccessMode: store.ModeUse}}, "admin"); err != nil {
		t.Fatal(err)
	}
	if code := h.call("GET", "/api/etl/definitions", nil, &defs); code != 200 || len(defs.Definitions) != 1 {
		t.Fatalf("use 授权应见 1 项：%d %v", code, defs.Definitions)
	}
	if defs.Definitions[0].Access != store.ModeUse {
		t.Fatalf("access 应为 use：%q", defs.Definitions[0].Access)
	}
	var got store.Definition
	if code := h.call("GET", "/api/etl/definitions/"+d1.ID, nil, &got); code != 200 || got.Access != store.ModeUse {
		t.Fatalf("详情应 200 且 access=use：%d %q", code, got.Access)
	}
	if code := h.call("GET", "/api/etl/definitions/"+d2.ID, nil, nil); code != 403 {
		t.Fatalf("未授权定义详情应 403：%d", code)
	}
	var run map[string]any
	if code := h.call("POST", "/api/etl/definitions/"+d1.ID+"/run", map[string]any{}, &run); code != 200 {
		t.Fatalf("use 授权应可运行：%d %v", code, run)
	}
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{"PUT", "/api/etl/definitions/" + d1.ID, map[string]any{"revision": got.Revision}},
		{"DELETE", "/api/etl/definitions/" + d1.ID, nil},
		{"POST", "/api/etl/definitions/" + d1.ID + "/duplicate", nil},
		{"POST", "/api/etl/schedules", map[string]any{"targetId": d1.ID, "cron": "0 2 * * *"}},
		{"POST", "/api/etl/pipelines/preview", map[string]any{"doc": doc, "nodeId": "d", "limit": 2}},
	} {
		if code := h.call(tc.method, tc.path, tc.body, nil); code != 403 {
			t.Fatalf("use 授权下 %s %s 应 403，实得 %d", tc.method, tc.path, code)
		}
	}

	// ---- 升级 edit：可写；复制出的副本自动归属 ----
	if err := store.SetUserGrants(h.db, "u1", []store.DefinitionGrant{{DefinitionID: d1.ID, AccessMode: store.ModeEdit}}, "admin"); err != nil {
		t.Fatal(err)
	}
	if code := h.call("PUT", "/api/etl/definitions/"+d1.ID, map[string]any{"revision": got.Revision}, &got); code != 200 {
		t.Fatalf("edit 授权应可保存：%d", code)
	}
	var cp store.Definition
	if code := h.call("POST", "/api/etl/definitions/"+d1.ID+"/duplicate", nil, &cp); code != 200 || cp.ID == "" {
		t.Fatalf("edit 授权应可复制：%d", code)
	}
	if mode, _ := h.s.st.AccessMode("u1", cp.ID); mode != store.ModeEdit {
		t.Fatalf("副本应自动归属 edit：%q", mode)
	}
	var pv map[string]any
	if code := h.call("POST", "/api/etl/pipelines/preview", map[string]any{"doc": doc, "nodeId": "d", "limit": 2}, &pv); code != 200 {
		t.Fatalf("edit 授权应可预览：%d %v", code, pv)
	}
	var sc map[string]any
	if code := h.call("POST", "/api/etl/schedules", map[string]any{"targetId": d1.ID, "cron": "0 2 * * *"}, &sc); code != 200 {
		t.Fatalf("edit 授权应可建调度：%d %v", code, sc)
	}

	// ---- store 层校验与级联清理 ----
	if err := store.SetUserGrants(h.db, "u2", []store.DefinitionGrant{{DefinitionID: d1.ID, AccessMode: "root"}}, "a"); err == nil {
		t.Fatal("非法 access_mode 应被拒绝")
	}
	if err := store.SetUserGrants(h.db, "u2", []store.DefinitionGrant{{DefinitionID: "ghost", AccessMode: store.ModeUse}}, "a"); err == nil {
		t.Fatal("不存在的定义应被拒绝")
	}
	if err := h.s.st.Grant("u2", d2.ID, store.ModeUse, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := h.s.st.DeleteDefinition(d2.ID); err != nil {
		t.Fatal(err)
	}
	if mode, _ := h.s.st.AccessMode("u2", d2.ID); mode != "" {
		t.Fatalf("删除定义后授权应清理：%q", mode)
	}
	// admin 不受影响（h.user/role 仍 u1，但直接调 AccessMode 无 admin 概念——回到 admin 视角验证列表全量）
	h.user, h.role = "admin", "admin"
	if code := h.call("GET", "/api/etl/definitions", nil, &defs); code != 200 || len(defs.Definitions) < 2 {
		t.Fatalf("admin 应见全部定义：%d %v", code, defs.Definitions)
	}
}

// R1-B1：file_root 边界——非管理员经 preview / run 路径都拿不到根外内容、写不出根外文件。
// file_root 默认 <dbdir>/etl-files；攻击目标文件放在 <dbdir>（根外一层）。
func TestCSVFileRootService(t *testing.T) {
	h := newHarness(t)

	// 根外哨兵文件（与 etl-files 同级，即 <dbdir>/secret.csv）+ 根内合法文件
	os.WriteFile(filepath.Join(h.dir, "secret.csv"), []byte("v\nTOP-SECRET\n"), 0o600)
	root := filepath.Join(h.dir, "etl-files")
	os.MkdirAll(root, 0o750)
	os.WriteFile(filepath.Join(root, "in.csv"), []byte("v\n1\n"), 0o644)

	csvDoc := func(path string) map[string]any {
		return map[string]any{
			"kind": "pipeline", "params": []any{},
			"nodes": []map[string]any{
				{"id": "csv", "type": "csv_input", "name": "读", "config": map[string]any{"path": path, "hasHeader": true,
					"fields": []map[string]any{{"name": "v", "kind": "string"}}}},
				{"id": "d", "type": "dummy", "name": "空", "config": map[string]any{}},
			},
			"hops": []map[string]any{{"id": "h", "from": "csv", "fromPort": "main", "to": "d", "toPort": "main"}},
		}
	}

	// 非管理员 u1：授予任一 edit 授权（preview 的准入门槛）
	docRaw, _ := json.Marshal(csvDoc("in.csv"))
	d1, err := h.s.st.CreateDefinition(store.Definition{Kind: "pipeline", Name: "跳板", Doc: docRaw}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetUserGrants(h.db, "u1", []store.DefinitionGrant{{DefinitionID: d1.ID, AccessMode: store.ModeEdit}}, "admin"); err != nil {
		t.Fatal(err)
	}
	h.user, h.role = "u1", "user"

	// ① 预览：绝对路径 / .. 越界都应在编译期报错，响应不得含根外内容
	absSecret := filepath.Join(h.dir, "secret.csv")
	for _, p := range []string{absSecret, "../secret.csv", "../../etc/hosts"} {
		var res map[string]any
		if code := h.call("POST", "/api/etl/pipelines/preview",
			map[string]any{"doc": csvDoc(p), "nodeId": "csv", "limit": 5}, &res); code != 200 {
			t.Fatalf("预览应 200：%d %v", code, res)
		}
		raw, _ := json.Marshal(res)
		if strings.Contains(string(raw), "TOP-SECRET") {
			t.Fatalf("预览泄露根外文件内容：path=%q", p)
		}
		probs, _ := res["problems"].([]any)
		hasErr := false
		for _, pr := range probs {
			if m, ok := pr.(map[string]any); ok && m["severity"] == "error" {
				hasErr = true
			}
		}
		if !hasErr {
			t.Fatalf("越界路径 %q 预览应报编译错误：%v", p, res)
		}
	}
	// 合法根内路径：预览应能读出内容且无编译错误
	var okRes map[string]any
	if code := h.call("POST", "/api/etl/pipelines/preview",
		map[string]any{"doc": csvDoc("in.csv"), "nodeId": "csv", "limit": 5}, &okRes); code != 200 {
		t.Fatalf("合法预览应 200：%d %v", code, okRes)
	}
	if probs, _ := okRes["problems"].([]any); len(probs) != 0 {
		t.Fatalf("合法路径预览不应报错：%v", okRes)
	}
	rows, _ := okRes["ports"].(map[string]any)["main"].(map[string]any)["rows"].([]any)
	if len(rows) != 1 || fmt.Sprint(rows[0]) != "[1]" && fmt.Sprint(rows[0]) != "map[v:1]" {
		t.Fatalf("合法路径预览应读出根内文件内容：%v", okRes)
	}

	// ② 运行：csv_output 写 ../evil.csv → 运行失败且根外不落盘
	outDoc, _ := json.Marshal(map[string]any{
		"kind": "pipeline", "params": []any{},
		"nodes": []map[string]any{
			{"id": "g", "type": "generate_rows", "name": "生成", "config": map[string]any{"limit": 3}},
			{"id": "o", "type": "csv_output", "name": "写", "config": map[string]any{"path": "../evil.csv"}},
		},
		"hops": []map[string]any{{"id": "h", "from": "g", "fromPort": "main", "to": "o", "toPort": "main"}},
	})
	evil, err := h.s.st.CreateDefinition(store.Definition{Kind: "pipeline", Name: "越界写", Doc: outDoc}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.st.Grant("u1", evil.ID, store.ModeUse, "admin"); err != nil {
		t.Fatal(err)
	}
	var run store.Run
	if code := h.call("POST", "/api/etl/definitions/"+evil.ID+"/run", map[string]any{}, &run); code != 200 {
		t.Fatalf("use 授权应可触发运行：%d", code)
	}
	deadline := time.Now().Add(15 * time.Second)
	var r store.Run
	for time.Now().Before(deadline) {
		r, _ = h.s.st.GetRun(run.ID)
		if r.Status != store.RunQueued && r.Status != store.RunRunning {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if r.Status != store.RunFailed {
		t.Fatalf("越界写路径的运行应失败：%s %s", r.Status, r.Error)
	}
	if _, err := os.Lstat(filepath.Join(h.dir, "evil.csv")); !os.IsNotExist(err) {
		t.Fatal("不应在 file_root 之外写出文件")
	}
}
