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
		Problems []map[string]any         `json:"problems"`
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
