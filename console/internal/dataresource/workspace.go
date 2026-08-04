// 工作台管理器：维护内存中的工作台状态和手工事务。
//
// 设计文档第三节「事务状态」：
//   - 默认 autoCommit=true，每条写 SQL 独立提交。
//   - 用户关闭自动提交后，第一条 SQL 才创建事务。
//   - 只有 autoCommit=false && txState=active 时"提交"按钮可用。
//   - 提交或回滚后事务结束；自动提交仍关闭时，下一条 SQL 再创建新事务。
//   - 活动事务未处理前不能重新开启自动提交。
//   - 同一工作台同一时间只执行一个请求。
//   - 手工事务空闲 15 分钟自动回滚。
//   - 退出登录、会话失效、授权撤销、资源删除、Console 重启均不得提交未完成事务。
package dataresource

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"
)

// TxState 事务状态。
type TxState string

const (
	TxNone   TxState = "none"   // 无活动事务
	TxActive TxState = "active" // 活动事务
	TxFailed TxState = "failed" // 事务失败（需回滚）
)

// Workspace 是一个工作台的内存状态。
type Workspace struct {
	ID         string
	ResourceID string
	Username   string
	AutoCommit bool
	ReadOnly   bool // 用户对该资源为 read 授权时为 true；手工事务必须以只读事务创建
	TxState    TxState
	Tx         Transaction // 活动事务（nil 表示无）
	// txCtx/txCancel：手工事务生命周期与单次 HTTP 请求解耦。
	// database/sql 的 BeginTx(ctx) 在 ctx 取消时会主动 rollback；若用请求 ctx 开启事务，
	// 请求结束后 defer cancel 会静默毁掉 ws.Tx，导致「执行成功但提交已回滚」。
	txCtx        context.Context
	txCancel     context.CancelFunc
	Adapter      DataSourceAdapter
	LastSQL      string    // 最近一次成功的查询（供全量导出重放）
	LastActivity time.Time // 最近活动时间（用于超时回滚）
	// closed 用 atomic：失效路径必须先标记 closed 并 cancel 语句，再等 ws.mu，
	// 否则在途 Execute 持锁时 Invalidate 会卡在 mu 上，执行完仍可能提交。
	closed atomic.Bool
	mu     sync.Mutex // 串行化同一工作台的请求（执行/提交/回滚）
	// stmtMu/stmtCancel：当前语句取消函数，与 mu 分离。
	// Cancel 接口不得获取 mu，否则与正在执行的 Execute 死锁。
	stmtMu     sync.Mutex
	stmtCancel context.CancelFunc
}

// setStmtCancel 登记当前语句的 cancel（调用方持有或不持有 ws.mu 均可）。
func (ws *Workspace) setStmtCancel(cancel context.CancelFunc) {
	ws.stmtMu.Lock()
	ws.stmtCancel = cancel
	ws.stmtMu.Unlock()
}

// clearStmtCancel 清除登记（执行结束时）。
func (ws *Workspace) clearStmtCancel() {
	ws.stmtMu.Lock()
	ws.stmtCancel = nil
	ws.stmtMu.Unlock()
}

// cancelRunningStmt 取消当前正在执行的语句。不获取 ws.mu，可被 cancel API 安全调用。
// 返回是否确实触发了 cancel（false 表示当前无执行中的语句）。
func (ws *Workspace) cancelRunningStmt() bool {
	ws.stmtMu.Lock()
	cancel := ws.stmtCancel
	ws.stmtMu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// beginManualTxLocked 开启工作台级手工事务（调用方须已持 ws.mu）。
// Begin 使用独立于请求的 txCtx（无绝对超时）；空闲回收由 CleanupIdle 按 LastActivity 判断。
// 单条语句仍用调用方传入的短超时 ctx。
func (ws *Workspace) beginManualTxLocked(readOnly bool) (Transaction, error) {
	// WithCancel 而非 WithTimeout：避免「活动事务在 15min 绝对上限被静默回滚，
	// 但 LastActivity 仍新鲜导致 CleanupIdle 永不回收、资源长期 RESOURCE_BUSY」。
	// cancel 仅在 Commit/Rollback/DeleteWorkspace/CleanupIdle/CloseAll 调用。
	ctx, cancel := context.WithCancel(context.Background())
	tx, err := ws.Adapter.Begin(ctx, readOnly)
	if err != nil {
		cancel()
		return nil, err
	}
	ws.txCtx = ctx
	ws.txCancel = cancel
	return tx, nil
}

// clearTxLocked 清理事务指针与 txCtx（调用方须已持 ws.mu）。
// endPool 为 true 时递减 pool.activeTx（事务曾成功占用计数时使用）。
func (ws *Workspace) clearTxLocked(endPool bool, pool *PoolManager) {
	if ws.txCancel != nil {
		ws.txCancel()
		ws.txCancel = nil
	}
	ws.txCtx = nil
	ws.Tx = nil
	ws.TxState = TxNone
	if endPool && pool != nil {
		pool.EndTx(ws.ResourceID)
	}
}

// errWorkspaceClosed 工作台已从管理器移除或被失效。
var errWorkspaceClosed = fmt.Errorf("工作台已关闭或失效")

func (ws *Workspace) isClosed() bool {
	return ws.closed.Load()
}

func (ws *Workspace) ensureOpenLocked() error {
	if ws.closed.Load() {
		return errWorkspaceClosed
	}
	return nil
}

// workspaceStateSnapshot 锁内快照，避免 handler 解锁后读 AutoCommit/TxState 的 data race。
type workspaceStateSnapshot struct {
	AutoCommit bool
	TxState    TxState
}

func (ws *Workspace) snapshotState() workspaceStateSnapshot {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return workspaceStateSnapshot{AutoCommit: ws.AutoCommit, TxState: ws.TxState}
}

// WorkspaceManager 管理所有工作台。
type WorkspaceManager struct {
	mu         sync.Mutex
	workspaces map[string]*Workspace
	pool       *PoolManager
}

// 事务空闲超时：超过则回滚手工事务。
const txIdleTimeout = 15 * time.Minute

// 工作台对象空闲超时：无活动事务且超过该时间则删除（防浏览器直接关闭导致泄漏）。
const workspaceIdleTimeout = 1 * time.Hour

// NewWorkspaceManager 创建工作台管理器。
func NewWorkspaceManager(pool *PoolManager) *WorkspaceManager {
	return &WorkspaceManager{
		workspaces: map[string]*Workspace{},
		pool:       pool,
	}
}

// CreateWorkspace 创建新工作台。readOnly 表示用户对该资源仅有 read 授权，
// 关闭自动提交后整段手工事务必须以数据库只读事务创建（设计第二层安全边界）。
func (wm *WorkspaceManager) CreateWorkspace(resourceID, username string, adapter DataSourceAdapter, readOnly bool) *Workspace {
	ws := &Workspace{
		ID:           newID(),
		ResourceID:   resourceID,
		Username:     username,
		AutoCommit:   true,
		ReadOnly:     readOnly,
		TxState:      TxNone,
		Adapter:      adapter,
		LastActivity: time.Now(),
	}
	wm.mu.Lock()
	wm.workspaces[ws.ID] = ws
	wm.mu.Unlock()
	return ws
}

// GetWorkspace 获取工作台。
func (wm *WorkspaceManager) GetWorkspace(id string) (*Workspace, bool) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	ws, ok := wm.workspaces[id]
	return ws, ok
}

// DeleteWorkspace 删除工作台。有活动事务时回滚。
// 顺序：锁内标记 closed 并移出 map → cancel 在途语句 → 再取 mu 回滚。
// 禁止「先等 mu 再 closed」，否则在途 Execute 持锁期间仍可完整提交。
func (wm *WorkspaceManager) DeleteWorkspace(id string) {
	wm.mu.Lock()
	ws, ok := wm.workspaces[id]
	if !ok {
		wm.mu.Unlock()
		return
	}
	ws.closed.Store(true)
	delete(wm.workspaces, id)
	wm.mu.Unlock()

	ws.cancelRunningStmt()
	wm.finishInvalidation(ws)
}

// finishInvalidation 等待在途请求退出并回滚手工事务。
func (wm *WorkspaceManager) finishInvalidation(ws *Workspace) {
	ws.mu.Lock()
	if ws.Tx != nil {
		_ = ws.Tx.Rollback()
		ws.clearTxLocked(true, wm.pool)
	} else {
		ws.clearTxLocked(false, nil)
	}
	ws.mu.Unlock()
	wm.pool.CloseUserDB(ws.ResourceID, ws.Username)
}

// invalidateMatching 在 manager 锁内一次性标记并移除匹配工作台，
// 避免逐个 DeleteWorkspace 留下尚未 closed 的可执行窗口。
func (wm *WorkspaceManager) invalidateMatching(match func(*Workspace) bool) {
	wm.mu.Lock()
	var list []*Workspace
	for id, ws := range wm.workspaces {
		if match(ws) {
			ws.closed.Store(true)
			delete(wm.workspaces, id)
			list = append(list, ws)
		}
	}
	wm.mu.Unlock()
	for _, ws := range list {
		ws.cancelRunningStmt()
	}
	for _, ws := range list {
		wm.finishInvalidation(ws)
	}
}

// RollbackUserTx 回滚用户在某资源上的所有活动事务（撤权/退出时调用）。
func (wm *WorkspaceManager) RollbackUserTx(username, resourceID string) {
	wm.mu.Lock()
	var toRollback []*Workspace
	for _, ws := range wm.workspaces {
		if ws.Username == username && ws.ResourceID == resourceID {
			toRollback = append(toRollback, ws)
		}
	}
	wm.mu.Unlock()
	for _, ws := range toRollback {
		ws.mu.Lock()
		if ws.Tx != nil {
			_ = ws.Tx.Rollback()
			ws.clearTxLocked(true, wm.pool)
		}
		ws.mu.Unlock()
	}
}

// InvalidateUserResource 回滚并删除用户在某资源上的全部工作台（撤销/降级授权时调用）。
// 设计：撤权后立即回滚活动事务并使工作台失效。
func (wm *WorkspaceManager) InvalidateUserResource(username, resourceID string) {
	wm.invalidateMatching(func(ws *Workspace) bool {
		return ws.Username == username && ws.ResourceID == resourceID
	})
}

// InvalidateAllForUser 回滚并删除某用户的全部工作台（退出登录/删用户/会话失效时调用）。
// 设计：退出登录、会话失效不得提交未完成事务。
func (wm *WorkspaceManager) InvalidateAllForUser(username string) {
	wm.invalidateMatching(func(ws *Workspace) bool {
		return ws.Username == username
	})
}

// InvalidateResource 回滚并删除某资源上的全部工作台（资源更新/删除时调用）。
func (wm *WorkspaceManager) InvalidateResource(resourceID string) {
	wm.invalidateMatching(func(ws *Workspace) bool {
		return ws.ResourceID == resourceID
	})
}

// CleanupIdle 回滚超时手工事务，并删除长期无活动的空工作台。由后台定期调用。
// 返回值：回滚的事务数 + 删除的空闲工作台数（便于日志观测）。
func (wm *WorkspaceManager) CleanupIdle() int {
	wm.mu.Lock()
	candidates := make([]*Workspace, 0, len(wm.workspaces))
	for _, ws := range wm.workspaces {
		candidates = append(candidates, ws)
	}
	wm.mu.Unlock()

	now := time.Now()
	n := 0
	for _, ws := range candidates {
		ws.mu.Lock()
		if ws.Tx != nil && now.Sub(ws.LastActivity) > txIdleTimeout {
			_ = ws.Tx.Rollback()
			ws.clearTxLocked(true, wm.pool)
			n++
		}
		ws.mu.Unlock()
		// 删除前在同一工作台锁下重新核对状态，避免检查后有请求恢复活动却仍被删除。
		if wm.deleteWorkspaceIfIdle(ws, now) {
			n++
		}
	}
	return n
}

// deleteWorkspaceIfIdle 仅在工作台仍是 map 中同一对象、无事务且确实空闲时删除。
// 先取 ws.mu：已拿到工作台指针但尚未执行的请求，要么先刷新 LastActivity，
// 要么在删除后通过 closed 检查失败，不能出现“执行成功后被旧清理结论删除”。
func (wm *WorkspaceManager) deleteWorkspaceIfIdle(ws *Workspace, now time.Time) bool {
	if ws == nil {
		return false
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.closed.Load() || ws.Tx != nil || now.Sub(ws.LastActivity) <= workspaceIdleTimeout {
		return false
	}

	wm.mu.Lock()
	defer wm.mu.Unlock()
	current, ok := wm.workspaces[ws.ID]
	if !ok || current != ws {
		return false
	}
	delete(wm.workspaces, ws.ID)
	ws.closed.Store(true)
	ws.clearTxLocked(false, nil)
	wm.pool.CloseUserDB(ws.ResourceID, ws.Username)
	return true
}

// CloseAll 关闭所有工作台（Console 退出时）。
func (wm *WorkspaceManager) CloseAll() {
	wm.mu.Lock()
	list := make([]*Workspace, 0, len(wm.workspaces))
	for _, ws := range wm.workspaces {
		ws.closed.Store(true)
		list = append(list, ws)
	}
	wm.workspaces = map[string]*Workspace{}
	wm.mu.Unlock()
	for _, ws := range list {
		ws.cancelRunningStmt()
	}
	for _, ws := range list {
		wm.finishInvalidation(ws)
	}
}

// CancelWorkspaceStatement 取消工作台上正在执行的语句（不持 ws.mu）。
func (wm *WorkspaceManager) CancelWorkspaceStatement(ws *Workspace) bool {
	if ws == nil {
		return false
	}
	return ws.cancelRunningStmt()
}

// mapStmtContextError 将语句 ctx 取消/超时映射为稳定 QUERY_CANCELED；其它错误走适配器归一化。
func mapStmtContextError(adapter DataSourceAdapter, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return DatabaseError{Code: "QUERY_CANCELED", Message: "查询已取消"}.toError()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return DatabaseError{Code: "QUERY_CANCELED", Message: "查询超时"}.toError()
	}
	if adapter != nil {
		return adapter.NormalizeError(err).toError()
	}
	return err
}

// ExecuteInWorkspace 在工作台中执行 SQL，处理自动提交和手工事务逻辑。
// limit/offset 仅对 SELECT 生效：服务端隐式分页，不改写编辑器中的 SQL 文本。
// 执行期间登记 stmtCancel，供 POST .../cancel 在不持 mu 的情况下取消语句。
func (wm *WorkspaceManager) ExecuteInWorkspace(ctx context.Context, ws *Workspace, sqlText string, limit, offset int) (*ExecutionResult, error) {
	if !wm.pool.TryBeginOperation(ws.ResourceID) {
		return nil, &APIError{Code: "RESOURCE_BUSY", Message: "资源正在导入或更新，请稍后再执行"}
	}
	defer wm.pool.EndOperation(ws.ResourceID)

	ws.mu.Lock()
	defer ws.mu.Unlock()
	if err := ws.ensureOpenLocked(); err != nil {
		return nil, &APIError{Code: "WORKSPACE_CLOSED", Message: err.Error()}
	}
	ws.LastActivity = time.Now()

	// 服务端执行期限 + 可被 CancelWorkspaceStatement 取消
	qctx, cancel := context.WithTimeout(ctx, QueryTimeout)
	ws.setStmtCancel(cancel)
	defer func() {
		ws.clearStmtCancel()
		cancel()
	}()
	ctx = qctx

	// 校验单语句
	if err := ValidateSingleStatement(sqlText); err != nil {
		return nil, err
	}

	stmtType := ClassifySQL(sqlText)
	if stmtType.IsTransactionControl() {
		return nil, fmt.Errorf("显式事务控制语句不允许，请使用工作台按钮")
	}
	// 无法可靠分类的语句 fail-closed，避免 UNKNOWN 走写路径破坏事务状态
	if stmtType == StmtUnknown {
		return nil, fmt.Errorf("无法识别的 SQL 类型，已拒绝执行")
	}

	// DDL/DCL/TRUNCATE/CALL 只允许自动提交模式
	if stmtType.IsAutoCommitOnly() && !ws.AutoCommit {
		return nil, fmt.Errorf("DDL/DCL/TRUNCATE/过程调用只允许自动提交模式，请先提交或回滚当前事务")
	}

	start := time.Now()
	result := &ExecutionResult{
		ExecutionID:   newID(),
		StatementType: stmtType,
		TxState:       string(ws.TxState),
	}

	if stmtType.IsReadOnly() {
		// SELECT：隐式分页
		if limit <= 0 {
			limit = DefaultLimit
		}
		if limit > MaxLimit {
			limit = MaxLimit
		}
		if offset < 0 {
			offset = 0
		}
		err := wm.executeQueryInWorkspace(ctx, ws, sqlText, limit, offset, result)
		result.DurationMs = time.Since(start).Milliseconds()
		result.TxState = string(ws.TxState)
		if err != nil {
			return result, err
		}
		ws.LastSQL = sqlText // 记录最近成功的查询供导出（无分页包装的原文）
	} else {
		// 写操作
		err := wm.executeWriteInWorkspace(ctx, ws, sqlText, result)
		result.DurationMs = time.Since(start).Milliseconds()
		result.TxState = string(ws.TxState)
		if err != nil {
			return result, err
		}
	}

	return result, nil
}

// executeQueryInWorkspace 执行 SELECT。
// 自动提交模式：在只读事务中执行查询和计数。
// 手工事务模式：在当前事务中执行（不自动统计总数，避免副作用函数被调用两次）。
func (wm *WorkspaceManager) executeQueryInWorkspace(ctx context.Context, ws *Workspace, sqlText string, limit, offset int, result *ExecutionResult) error {
	result.Limit = limit
	result.Offset = offset

	if ws.AutoCommit {
		// 自动提交：只读事务
		tx, err := ws.Adapter.Begin(ctx, true)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return mapStmtContextError(ws.Adapter, err)
			}
			return fmt.Errorf("开启只读事务失败: %w", err)
		}
		defer tx.Rollback()

		if err := queryPageIntoResult(ctx, ws.Adapter, tx, sqlText, limit, offset, result); err != nil {
			return err
		}

		// 计数：更短独立超时，失败不影响数据返回
		countSQL, err := ws.Adapter.CountSQL(sqlText)
		if err == nil {
			cctx, ccancel := context.WithTimeout(ctx, CountTimeout)
			var total int
			if err := tx.QueryRow(cctx, countSQL).Scan(&total); err == nil {
				result.Total = total
				result.TotalStatus = "available"
				result.HasMore = offset+result.ReturnedRows < total
			} else {
				result.TotalStatus = "unavailable"
				result.HasMore = result.ReturnedRows >= limit
			}
			ccancel()
		} else {
			result.TotalStatus = "unavailable"
			result.HasMore = result.ReturnedRows >= limit
		}
		return nil
	}

	// 手工事务：在当前事务中执行（Begin 用工作台 txCtx，语句用请求短超时 ctx）
	if ws.Tx == nil {
		// 第一条 SQL 创建事务；只读用户必须 Begin(readOnly=true)
		tx, err := ws.beginManualTxLocked(ws.ReadOnly)
		if err != nil {
			return fmt.Errorf("开启事务失败: %w", err)
		}
		if err := wm.pool.BeginTx(ws.ResourceID); err != nil {
			_ = tx.Rollback()
			ws.clearTxLocked(false, nil)
			return &APIError{Code: "IMPORT_ACTIVE", Message: err.Error()}
		}
		ws.Tx = tx
		ws.TxState = TxActive
	}

	if err := queryPageIntoResult(ctx, ws.Adapter, ws.Tx, sqlText, limit, offset, result); err != nil {
		ws.TxState = TxFailed
		return err
	}
	// 手工事务中不自动统计总数（避免副作用函数被调用两次）
	result.TotalStatus = "unavailable"
	result.HasMore = result.ReturnedRows >= limit
	return nil
}

// queryPageIntoResult 执行分页查询并填充 result。
// 优先 PageSQL。仅当识别为派生表重名列（MySQL 1060 等）且 offset==0 时流式兜底；
// 其它失败直接返回，避免语法/权限错误二次打库，或大 offset 拖死连接池。
func queryPageIntoResult(ctx context.Context, adapter DataSourceAdapter, q interface {
	Query(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, sqlText string, limit, offset int, result *ExecutionResult) error {
	pageSQL, err := adapter.PageSQL(sqlText, limit, offset)
	if err != nil {
		return err
	}
	rows, err := q.Query(ctx, pageSQL)
	if err == nil {
		defer rows.Close()
		if err := fillResult(rows, result, limit); err != nil {
			return mapStmtContextError(adapter, err)
		}
		return nil
	}
	pageErr := err
	if errors.Is(pageErr, context.Canceled) || errors.Is(pageErr, context.DeadlineExceeded) {
		return mapStmtContextError(adapter, pageErr)
	}
	if offset != 0 || !isDuplicateColumnPageError(pageErr) {
		// 用户自带 LIMIT/FETCH 且包装后重名列：给明确文案
		if isDuplicateColumnPageError(pageErr) {
			return DatabaseError{
				Code:    "DUPLICATE_COLUMN",
				Message: "该查询存在重复列名，无法自动分页；请显式指定列别名，或去掉 SQL 中的 LIMIT/FETCH/FOR UPDATE 后重试",
			}.toError()
		}
		return mapStmtContextError(adapter, pageErr)
	}
	// 仅 offset=0 的重名列：流式读 limit+1，避免全量拉取后丢弃
	raw := strings.TrimRight(strings.TrimSpace(sqlText), ";")
	rows2, err2 := q.Query(ctx, raw)
	if err2 != nil {
		return mapStmtContextError(adapter, pageErr)
	}
	defer rows2.Close()
	if err := fillResultStream(rows2, result, limit, 0); err != nil {
		return mapStmtContextError(adapter, err)
	}
	return nil
}

// isDuplicateColumnPageError 识别 MySQL 1060 / 派生表重名列等包装失败。
func isDuplicateColumnPageError(err error) bool {
	if err == nil {
		return false
	}
	// go-sql-driver/mysql 错误号
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) && myErr != nil && myErr.Number == 1060 {
		return true
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "duplicate column") || strings.Contains(lower, "error 1060") {
		return true
	}
	// 达梦/Oracle 常见「列名重复 / 歧义」
	if strings.Contains(lower, "ambiguous") {
		return true
	}
	if strings.Contains(msg, "列名") && (strings.Contains(msg, "重复") || strings.Contains(msg, "歧义")) {
		return true
	}
	return false
}

// executeWriteInWorkspace 执行写操作。
func (wm *WorkspaceManager) executeWriteInWorkspace(ctx context.Context, ws *Workspace, sqlText string, result *ExecutionResult) error {
	if ws.ReadOnly {
		// 第二层兜底：只读工作台不得执行写（handler 应已拦截）
		return &APIError{Code: "DATA_RESOURCE_READ_ONLY", Message: "只读授权不允许执行写操作"}
	}
	if ws.AutoCommit {
		// 自动提交：独立事务
		tx, err := ws.Adapter.Begin(ctx, false)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return mapStmtContextError(ws.Adapter, err)
			}
			return fmt.Errorf("开启事务失败: %w", err)
		}
		res, err := tx.Exec(ctx, sqlText)
		if err != nil {
			_ = tx.Rollback()
			return mapStmtContextError(ws.Adapter, err)
		}
		// 失效路径可能已 closed：禁止提交在途自动提交写
		if ws.isClosed() {
			_ = tx.Rollback()
			return &APIError{Code: "WORKSPACE_CLOSED", Message: errWorkspaceClosed.Error()}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("提交失败: %w", err)
		}
		n, _ := res.RowsAffected()
		result.AffectedRows = n
		return nil
	}

	// 手工事务（Begin 用工作台 txCtx，语句用请求短超时 ctx）
	if ws.Tx == nil {
		tx, err := ws.beginManualTxLocked(false)
		if err != nil {
			return fmt.Errorf("开启事务失败: %w", err)
		}
		if err := wm.pool.BeginTx(ws.ResourceID); err != nil {
			_ = tx.Rollback()
			ws.clearTxLocked(false, nil)
			return &APIError{Code: "IMPORT_ACTIVE", Message: err.Error()}
		}
		ws.Tx = tx
		ws.TxState = TxActive
	}
	res, err := ws.Tx.Exec(ctx, sqlText)
	if err != nil {
		ws.TxState = TxFailed
		return mapStmtContextError(ws.Adapter, err)
	}
	n, _ := res.RowsAffected()
	result.AffectedRows = n
	return nil
}

// CommitWorkspace 提交工作台的活动事务。返回锁内状态快照（供响应，避免解锁后读竞争）。
func (wm *WorkspaceManager) CommitWorkspace(ws *Workspace) (workspaceStateSnapshot, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	snap := workspaceStateSnapshot{AutoCommit: ws.AutoCommit, TxState: ws.TxState}
	if err := ws.ensureOpenLocked(); err != nil {
		return snap, err
	}
	ws.LastActivity = time.Now()
	if ws.Tx == nil {
		return snap, fmt.Errorf("无活动事务可提交")
	}
	err := ws.Tx.Commit()
	// database/sql：Commit 返回后事务已结束（成功或失败），必须释放计数与 cancel
	ws.clearTxLocked(true, wm.pool)
	snap = workspaceStateSnapshot{AutoCommit: ws.AutoCommit, TxState: ws.TxState}
	if err != nil {
		return snap, fmt.Errorf("提交失败: %w", err)
	}
	return snap, nil
}

// RollbackWorkspace 回滚工作台的活动事务。
func (wm *WorkspaceManager) RollbackWorkspace(ws *Workspace) (workspaceStateSnapshot, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	snap := workspaceStateSnapshot{AutoCommit: ws.AutoCommit, TxState: ws.TxState}
	if err := ws.ensureOpenLocked(); err != nil {
		return snap, err
	}
	ws.LastActivity = time.Now()
	if ws.Tx == nil {
		return snap, nil // 无事务可回滚，幂等
	}
	_ = ws.Tx.Rollback() // 回滚失败也清除状态（连接可能已断开）
	ws.clearTxLocked(true, wm.pool)
	return workspaceStateSnapshot{AutoCommit: ws.AutoCommit, TxState: ws.TxState}, nil
}

// SetAutoCommit 设置自动提交模式。
// 活动事务未处理前不能重新开启自动提交。
func (wm *WorkspaceManager) SetAutoCommit(ws *Workspace, autoCommit bool) (workspaceStateSnapshot, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	snap := workspaceStateSnapshot{AutoCommit: ws.AutoCommit, TxState: ws.TxState}
	if err := ws.ensureOpenLocked(); err != nil {
		return snap, err
	}
	ws.LastActivity = time.Now()
	if autoCommit && ws.Tx != nil {
		return snap, fmt.Errorf("存在活动事务，不能开启自动提交，请先提交或回滚")
	}
	ws.AutoCommit = autoCommit
	return workspaceStateSnapshot{AutoCommit: ws.AutoCommit, TxState: ws.TxState}, nil
}

// fillResult 从已分页的 rows 填充 ExecutionResult（PageSQL 已含 LIMIT/OFFSET）。
// HasMore 启发式：返回行数 == limit 时可能还有下一页。
func fillResult(rows interface {
	Columns() ([]string, error)
	Next() bool
	Scan(...any) error
	Err() error
}, result *ExecutionResult, limit int) error {
	if err := scanRowsInto(rows, result, limit); err != nil {
		return err
	}
	result.HasMore = result.ReturnedRows == limit
	return nil
}

// fillResultStream 对未分页结果：跳过 offset 后读取 limit+1 行以判断 HasMore。
func fillResultStream(rows interface {
	Columns() ([]string, error)
	Next() bool
	Scan(...any) error
	Err() error
}, result *ExecutionResult, limit, offset int) error {
	// 跳过 offset
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	for skipped := 0; skipped < offset; skipped++ {
		if !rows.Next() {
			result.Columns = cols
			result.ReturnedRows = 0
			result.HasMore = false
			return rows.Err()
		}
		discard := make([]any, len(cols))
		dptrs := make([]any, len(cols))
		for i := range discard {
			dptrs[i] = &discard[i]
		}
		if err := rows.Scan(dptrs...); err != nil {
			return err
		}
	}
	// 多读 1 行判断 hasMore
	if err := scanRowsInto(rows, result, limit+1); err != nil {
		return err
	}
	result.HasMore = result.ReturnedRows > limit
	if result.HasMore {
		result.Rows = result.Rows[:limit]
		result.ReturnedRows = limit
	}
	return nil
}

func scanRowsInto(rows interface {
	Columns() ([]string, error)
	Next() bool
	Scan(...any) error
	Err() error
}, result *ExecutionResult, maxRows int) error {
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	result.Columns = cols
	typeNames := make([]string, len(cols))
	if sr, ok := rows.(*sql.Rows); ok {
		typeNames = columnTypeNames(sr, len(cols))
	}
	for maxRows <= 0 || len(result.Rows) < maxRows {
		if !rows.Next() {
			break
		}
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		out := make([]any, len(cols))
		for i, v := range values {
			out[i] = normalizeValue(v, typeNames[i])
		}
		result.Rows = append(result.Rows, out)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	result.ReturnedRows = len(result.Rows)
	return nil
}
