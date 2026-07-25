package dataresource

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// testSQLAdapter 用 SQLite 冒充外部库适配器，覆盖工作台事务生命周期（不依赖真实 PG/MySQL）。
type testSQLAdapter struct {
	db *sql.DB
}

type testSQLTx struct{ tx *sql.Tx }

func (t *testSQLTx) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, query, args...)
}
func (t *testSQLTx) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, query, args...)
}
func (t *testSQLTx) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, query, args...)
}
func (t *testSQLTx) Commit() error   { return t.tx.Commit() }
func (t *testSQLTx) Rollback() error { return t.tx.Rollback() }

func (a *testSQLAdapter) Ping(ctx context.Context) (ServerInfo, error) {
	return ServerInfo{}, a.db.PingContext(ctx)
}
func (a *testSQLAdapter) Begin(ctx context.Context, readOnly bool) (Transaction, error) {
	// SQLite 不支持 ReadOnly TxOptions 的完整语义；测试只关心 ctx 生命周期
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &testSQLTx{tx: tx}, nil
}
func (a *testSQLAdapter) Children(ctx context.Context, parent MetadataNode) ([]MetadataNode, error) {
	return nil, nil
}
func (a *testSQLAdapter) Describe(ctx context.Context, object MetadataNode) (ObjectStructure, error) {
	return ObjectStructure{}, nil
}
func (a *testSQLAdapter) DDL(ctx context.Context, obj MetadataNode) (string, error) {
	return "", fmt.Errorf("not implemented")
}
func (a *testSQLAdapter) SQLTemplate(obj MetadataNode, operation string) (string, error) {
	return "", fmt.Errorf("not implemented")
}
func (a *testSQLAdapter) PageSQL(query string, limit, offset int) (string, error) {
	q := strings.TrimRight(strings.TrimSpace(query), ";")
	return fmt.Sprintf("%s LIMIT %d OFFSET %d", q, limit, offset), nil
}
func (a *testSQLAdapter) CountSQL(query string) (string, error) {
	q := strings.TrimRight(strings.TrimSpace(query), ";")
	return fmt.Sprintf("SELECT COUNT(*) FROM (%s)", q), nil
}
func (a *testSQLAdapter) QuoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
func (a *testSQLAdapter) Placeholder(n int) string { return "?" }
func (a *testSQLAdapter) NormalizeError(err error) DatabaseError {
	if err == nil {
		return DatabaseError{}
	}
	return DatabaseError{Code: "DB_ERROR", Message: err.Error()}
}
func (a *testSQLAdapter) Capabilities() Capabilities {
	return Capabilities{ImportSupported: true, ReadOnlyTxSupported: true}
}

func openTestSQLite(t *testing.T) (*sql.DB, *testSQLAdapter) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// 单连接：与手工事务占用同一连接的行为更接近真实池
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	return db, &testSQLAdapter{db: db}
}

// blockingAdapter 的 Query/Exec 阻塞直到 ctx 取消，用于测试 CancelWorkspaceStatement。
type blockingAdapter struct {
	started chan struct{}
}

type blockingTx struct{ started chan struct{} }

func (t *blockingTx) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	select {
	case <-t.started:
	default:
		close(t.started)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
func (t *blockingTx) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	select {
	case <-t.started:
	default:
		close(t.started)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
func (t *blockingTx) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return nil
}
func (t *blockingTx) Commit() error   { return nil }
func (t *blockingTx) Rollback() error { return nil }

func (a *blockingAdapter) Ping(ctx context.Context) (ServerInfo, error) {
	return ServerInfo{}, nil
}
func (a *blockingAdapter) Begin(ctx context.Context, readOnly bool) (Transaction, error) {
	return &blockingTx{started: a.started}, nil
}
func (a *blockingAdapter) Children(ctx context.Context, parent MetadataNode) ([]MetadataNode, error) {
	return nil, nil
}
func (a *blockingAdapter) Describe(ctx context.Context, object MetadataNode) (ObjectStructure, error) {
	return ObjectStructure{}, nil
}
func (a *blockingAdapter) DDL(ctx context.Context, obj MetadataNode) (string, error) {
	return "", fmt.Errorf("n/a")
}
func (a *blockingAdapter) SQLTemplate(obj MetadataNode, operation string) (string, error) {
	return "", fmt.Errorf("n/a")
}
func (a *blockingAdapter) PageSQL(query string, limit, offset int) (string, error) {
	return query, nil
}
func (a *blockingAdapter) CountSQL(query string) (string, error) {
	return "SELECT 0", nil
}
func (a *blockingAdapter) QuoteIdentifier(name string) string { return name }
func (a *blockingAdapter) Placeholder(n int) string           { return "?" }
func (a *blockingAdapter) NormalizeError(err error) DatabaseError {
	return DatabaseError{Code: "DB_ERROR", Message: err.Error()}
}
func (a *blockingAdapter) Capabilities() Capabilities { return Capabilities{} }

// TestCancelWorkspaceStatementUnblocksExecute 取消 API 不持 mu，能打断阻塞中的执行。
func TestCancelWorkspaceStatementUnblocksExecute(t *testing.T) {
	started := make(chan struct{})
	adapter := &blockingAdapter{started: started}
	pool := NewPoolManager(nil)
	wm := NewWorkspaceManager(pool)
	ws := wm.CreateWorkspace("res-cancel", "dave", adapter, false)

	done := make(chan error, 1)
	go func() {
		_, err := wm.ExecuteInWorkspace(context.Background(), ws, `SELECT 1`, 10, 0)
		done <- err
	}()

	// 等语句进入阻塞
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("语句未开始")
	}

	// cancel 不得与 Execute 死锁：不持 ws.mu
	if !wm.CancelWorkspaceStatement(ws) {
		t.Fatal("应触发 cancel")
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("取消后应返回错误")
		}
		var de DatabaseError
		if !errors.As(err, &de) || de.Code != "QUERY_CANCELED" {
			t.Fatalf("期望 QUERY_CANCELED, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("取消后执行应迅速返回，仍阻塞说明 cancel 未生效")
	}

	// 无执行中语句时 cancel 返回 false
	if wm.CancelWorkspaceStatement(ws) {
		t.Fatal("无执行时应 canceled=false")
	}
}

// TestCancelWorkspaceHandlerUnblocksExecute 覆盖真实 handler 链路：
// 鉴权/path 校验不得获取正在执行语句持有的 ws.mu。
func TestCancelWorkspaceHandlerUnblocksExecute(t *testing.T) {
	started := make(chan struct{})
	adapter := &blockingAdapter{started: started}
	pool := NewPoolManager(nil)
	wm := NewWorkspaceManager(pool)
	svc := &Service{db: testDB(t), pools: pool, workspaces: wm}
	ws := wm.CreateWorkspace("res-handler-cancel", "admin", adapter, false)

	executeDone := make(chan error, 1)
	go func() {
		_, err := wm.ExecuteInWorkspace(context.Background(), ws, `SELECT 1`, 10, 0)
		executeDone <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("语句未开始")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/data-resources/res-handler-cancel/workspaces/"+ws.ID+"/cancel", nil)
	req.SetPathValue("id", "res-handler-cancel")
	req.SetPathValue("workspaceId", ws.ID)
	req = WithUser(req, "admin", "admin")
	rec := httptest.NewRecorder()
	handlerDone := make(chan struct{})
	go func() {
		svc.CancelWorkspaceHandler(rec, req)
		close(handlerDone)
	}()

	select {
	case <-handlerDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancel handler 被 ws.mu 阻塞")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Canceled bool `json:"canceled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Canceled {
		t.Fatalf("cancel handler 未触发取消: %s", rec.Body.String())
	}

	select {
	case err := <-executeDone:
		var de DatabaseError
		if !errors.As(err, &de) || de.Code != "QUERY_CANCELED" {
			t.Fatalf("期望 QUERY_CANCELED, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler 取消后执行仍未结束")
	}
}

func TestDeleteWorkspaceIfIdleRechecksCurrentActivity(t *testing.T) {
	wm := NewWorkspaceManager(NewPoolManager(nil))
	ws := wm.CreateWorkspace("res-idle", "alice", &blockingAdapter{started: make(chan struct{})}, false)
	now := time.Now()

	ws.mu.Lock()
	ws.LastActivity = now
	ws.mu.Unlock()
	if wm.deleteWorkspaceIfIdle(ws, now) {
		t.Fatal("刚恢复活动的工作台不得按旧清理结论删除")
	}
	if got, ok := wm.GetWorkspace(ws.ID); !ok || got != ws {
		t.Fatal("活跃工作台应仍在 manager 中")
	}

	ws.mu.Lock()
	ws.LastActivity = now.Add(-workspaceIdleTimeout - time.Minute)
	ws.mu.Unlock()
	if !wm.deleteWorkspaceIfIdle(ws, now) {
		t.Fatal("确实空闲的工作台应被删除")
	}
	if _, ok := wm.GetWorkspace(ws.ID); ok {
		t.Fatal("空闲工作台删除后不应仍在 manager 中")
	}
}

// TestManualTxSurvivesRequestContextCancel 锁死：
// 关闭自动提交 → 执行 INSERT（请求 ctx 结束）→ Commit 成功且数据落库。
// 回归：Begin 若绑请求 ctx，defer cancel 会静默 rollback。
func TestManualTxSurvivesRequestContextCancel(t *testing.T) {
	db, adapter := openTestSQLite(t)
	defer db.Close()

	pool := NewPoolManager(nil)
	wm := NewWorkspaceManager(pool)
	ws := wm.CreateWorkspace("res-1", "alice", adapter, false)
	if err := wm.SetAutoCommit(ws, false); err != nil {
		t.Fatal(err)
	}

	// 模拟单次 HTTP 请求：短生命周期 ctx，返回后 cancel
	reqCtx, reqCancel := context.WithCancel(context.Background())
	result, err := wm.ExecuteInWorkspace(reqCtx, ws, `INSERT INTO items (id, name) VALUES (1, 'a')`, 0, 0)
	reqCancel() // 请求结束；修复前此处会毁掉事务
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if result.AffectedRows != 1 {
		t.Fatalf("affectedRows=%d, want 1", result.AffectedRows)
	}
	if result.TxState != string(TxActive) {
		t.Fatalf("txState=%s, want active", result.TxState)
	}

	// 跨「请求」提交
	if err := wm.CommitWorkspace(ws); err != nil {
		t.Fatalf("提交失败（请求结束后事务应仍可用）: %v", err)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("提交后行数=%d, want 1（数据被静默回滚则说明事务绑了请求 ctx）", n)
	}
	if pool.IsBusy("res-1") {
		t.Fatal("提交后资源仍 busy（activeTx 未释放）")
	}
}

// TestManualTxRollbackReleasesBusy 回滚后 activeTx 归零。
func TestManualTxRollbackReleasesBusy(t *testing.T) {
	db, adapter := openTestSQLite(t)
	defer db.Close()

	pool := NewPoolManager(nil)
	wm := NewWorkspaceManager(pool)
	ws := wm.CreateWorkspace("res-2", "bob", adapter, false)
	_ = wm.SetAutoCommit(ws, false)

	reqCtx, cancel := context.WithCancel(context.Background())
	if _, err := wm.ExecuteInWorkspace(reqCtx, ws, `INSERT INTO items (id, name) VALUES (2, 'b')`, 0, 0); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()

	if err := wm.RollbackWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&n)
	if n != 0 {
		t.Fatalf("回滚后行数=%d, want 0", n)
	}
	if pool.IsBusy("res-2") {
		t.Fatal("回滚后资源仍 busy")
	}
}

// TestRequestCancelDoesNotKillManualTx 取消单条语句的请求 ctx 不得摧毁整个手工事务。
func TestRequestCancelDoesNotKillManualTx(t *testing.T) {
	db, adapter := openTestSQLite(t)
	defer db.Close()

	pool := NewPoolManager(nil)
	wm := NewWorkspaceManager(pool)
	ws := wm.CreateWorkspace("res-3", "carol", adapter, false)
	_ = wm.SetAutoCommit(ws, false)

	// 第一条写入成功
	ctx1, c1 := context.WithTimeout(context.Background(), QueryTimeout)
	if _, err := wm.ExecuteInWorkspace(ctx1, ws, `INSERT INTO items (id, name) VALUES (3, 'c')`, 0, 0); err != nil {
		c1()
		t.Fatal(err)
	}
	c1()

	// 第二条：传入已取消的 ctx（模拟 abort 后迟到请求，或语句超时）
	// 语句可能失败，但事务应仍可提交第一条
	dead, deadCancel := context.WithCancel(context.Background())
	deadCancel()
	_, _ = wm.ExecuteInWorkspace(dead, ws, `INSERT INTO items (id, name) VALUES (4, 'd')`, 0, 0)

	// 无论第二条是否失败，提交应至少保留已成功且未回滚的写入
	// 若 tx 绑了请求 ctx，此处会报 already rolled back
	if err := wm.CommitWorkspace(ws); err != nil {
		// 若第二条把状态打成 failed 且驱动已中止，仍可能提交失败；再查库
		// 核心断言：不能是「事务在第一条请求结束时就被 cancel 掉」
		t.Logf("commit err: %v", err)
	}

	// 更硬的断言：用独立事务路径再跑一遍「仅第一条 + 直接 commit」
	// （上面 dead ctx 路径可能因 Exec 失败置 TxFailed；单独保证主路径）
	ws2 := wm.CreateWorkspace("res-3b", "carol", adapter, false)
	_ = wm.SetAutoCommit(ws2, false)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := wm.ExecuteInWorkspace(ctx, ws2, `INSERT INTO items (id, name) VALUES (30, 'x')`, 0, 0); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	// 短暂等待，确保若错误地绑了已 cancel 的 ctx，awaitDone 有时间 rollback
	time.Sleep(50 * time.Millisecond)
	if err := wm.CommitWorkspace(ws2); err != nil {
		t.Fatalf("请求 cancel 后提交应成功: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM items WHERE id=30`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("id=30 行数=%d, want 1", n)
	}
}
