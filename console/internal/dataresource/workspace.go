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
	"fmt"
	"sync"
	"time"
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
	ID           string
	ResourceID   string
	Username     string
	AutoCommit   bool
	ReadOnly     bool // 用户对该资源为 read 授权时为 true；手工事务必须以只读事务创建
	TxState      TxState
	Tx           Transaction // 活动事务（nil 表示无）
	// txCtx/txCancel：手工事务生命周期与单次 HTTP 请求解耦。
	// database/sql 的 BeginTx(ctx) 在 ctx 取消时会主动 rollback；若用请求 ctx 开启事务，
	// 请求结束后 defer cancel 会静默毁掉 ws.Tx，导致「执行成功但提交已回滚」。
	txCtx        context.Context
	txCancel     context.CancelFunc
	Adapter      DataSourceAdapter
	LastSQL      string     // 最近一次成功的查询（供全量导出重放）
	LastActivity time.Time  // 最近活动时间（用于超时回滚）
	closed       bool       // 已失效/删除：持指针的迟到请求必须拒绝
	mu           sync.Mutex // 串行化同一工作台的请求
}

// beginManualTxLocked 开启工作台级手工事务（调用方须已持 ws.mu）。
// Begin 使用独立于请求的 txCtx；单条语句仍用调用方传入的短超时 ctx。
func (ws *Workspace) beginManualTxLocked(readOnly bool) (Transaction, error) {
	// 与空闲回滚窗口对齐，防止 goroutine/连接无限挂起
	ctx, cancel := context.WithTimeout(context.Background(), txIdleTimeout)
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

func (ws *Workspace) ensureOpenLocked() error {
	if ws.closed {
		return errWorkspaceClosed
	}
	return nil
}

// WorkspaceManager 管理所有工作台。
type WorkspaceManager struct {
	mu         sync.Mutex
	workspaces map[string]*Workspace
	pool       *PoolManager
}

// 事务超时时间。
const txIdleTimeout = 15 * time.Minute

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
// 先标记 closed，再从 map 移除，最后在 ws.mu 下回滚，避免已取指针的请求在失效后仍 BeginTx。
func (wm *WorkspaceManager) DeleteWorkspace(id string) {
	wm.mu.Lock()
	ws, ok := wm.workspaces[id]
	if !ok {
		wm.mu.Unlock()
		return
	}
	delete(wm.workspaces, id)
	wm.mu.Unlock()

	ws.mu.Lock()
	ws.closed = true
	if ws.Tx != nil {
		_ = ws.Tx.Rollback()
		ws.clearTxLocked(true, wm.pool)
	} else {
		ws.clearTxLocked(false, nil)
	}
	ws.mu.Unlock()
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
	wm.mu.Lock()
	var ids []string
	for id, ws := range wm.workspaces {
		if ws.Username == username && ws.ResourceID == resourceID {
			ids = append(ids, id)
		}
	}
	wm.mu.Unlock()
	for _, id := range ids {
		wm.DeleteWorkspace(id)
	}
}

// InvalidateAllForUser 回滚并删除某用户的全部工作台（退出登录/删用户/会话失效时调用）。
// 设计：退出登录、会话失效不得提交未完成事务。
func (wm *WorkspaceManager) InvalidateAllForUser(username string) {
	wm.mu.Lock()
	var ids []string
	for id, ws := range wm.workspaces {
		if ws.Username == username {
			ids = append(ids, id)
		}
	}
	wm.mu.Unlock()
	for _, id := range ids {
		wm.DeleteWorkspace(id)
	}
}

// InvalidateResource 回滚并删除某资源上的全部工作台（资源更新/删除时调用）。
func (wm *WorkspaceManager) InvalidateResource(resourceID string) {
	wm.mu.Lock()
	var ids []string
	for id, ws := range wm.workspaces {
		if ws.ResourceID == resourceID {
			ids = append(ids, id)
		}
	}
	wm.mu.Unlock()
	for _, id := range ids {
		wm.DeleteWorkspace(id)
	}
}

// CleanupIdle 回滚超时的手工事务。由后台定期调用。
func (wm *WorkspaceManager) CleanupIdle() int {
	wm.mu.Lock()
	candidates := make([]*Workspace, 0, len(wm.workspaces))
	for _, ws := range wm.workspaces {
		candidates = append(candidates, ws)
	}
	wm.mu.Unlock()

	now := time.Now()
	rolledBack := 0
	for _, ws := range candidates {
		ws.mu.Lock()
		if ws.Tx != nil && now.Sub(ws.LastActivity) > txIdleTimeout {
			_ = ws.Tx.Rollback()
			ws.clearTxLocked(true, wm.pool)
			rolledBack++
		}
		ws.mu.Unlock()
	}
	return rolledBack
}

// CloseAll 关闭所有工作台（Console 退出时）。
func (wm *WorkspaceManager) CloseAll() {
	wm.mu.Lock()
	list := make([]*Workspace, 0, len(wm.workspaces))
	for _, ws := range wm.workspaces {
		list = append(list, ws)
	}
	wm.workspaces = map[string]*Workspace{}
	wm.mu.Unlock()
	for _, ws := range list {
		ws.mu.Lock()
		ws.closed = true
		if ws.Tx != nil {
			_ = ws.Tx.Rollback()
			ws.clearTxLocked(true, wm.pool)
		} else {
			ws.clearTxLocked(false, nil)
		}
		ws.mu.Unlock()
	}
}

// ExecuteInWorkspace 在工作台中执行 SQL，处理自动提交和手工事务逻辑。
// limit/offset 仅对 SELECT 生效：服务端隐式分页，不改写编辑器中的 SQL 文本。
func (wm *WorkspaceManager) ExecuteInWorkspace(ctx context.Context, ws *Workspace, sqlText string, limit, offset int) (*ExecutionResult, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if err := ws.ensureOpenLocked(); err != nil {
		return nil, &APIError{Code: "WORKSPACE_CLOSED", Message: err.Error()}
	}
	ws.LastActivity = time.Now()

	// 服务端执行期限，防止慢查询占满连接池
	qctx, cancel := context.WithTimeout(ctx, QueryTimeout)
	defer cancel()
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
			return fmt.Errorf("开启只读事务失败: %w", err)
		}
		defer tx.Rollback()

		pageSQL, err := ws.Adapter.PageSQL(sqlText, limit, offset)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, pageSQL)
		if err != nil {
			return ws.Adapter.NormalizeError(err).toError()
		}
		defer rows.Close()
		if err := fillResult(rows, result, limit); err != nil {
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

	pageSQL, err := ws.Adapter.PageSQL(sqlText, limit, offset)
	if err != nil {
		return err
	}
	rows, err := ws.Tx.Query(ctx, pageSQL)
	if err != nil {
		ws.TxState = TxFailed
		return ws.Adapter.NormalizeError(err).toError()
	}
	defer rows.Close()
	if err := fillResult(rows, result, limit); err != nil {
		ws.TxState = TxFailed
		return err
	}
	// 手工事务中不自动统计总数（避免副作用函数被调用两次）
	result.TotalStatus = "unavailable"
	result.HasMore = result.ReturnedRows >= limit
	return nil
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
			return fmt.Errorf("开启事务失败: %w", err)
		}
		res, err := tx.Exec(ctx, sqlText)
		if err != nil {
			tx.Rollback()
			return ws.Adapter.NormalizeError(err).toError()
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
		return ws.Adapter.NormalizeError(err).toError()
	}
	n, _ := res.RowsAffected()
	result.AffectedRows = n
	return nil
}

// CommitWorkspace 提交工作台的活动事务。
func (wm *WorkspaceManager) CommitWorkspace(ws *Workspace) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if err := ws.ensureOpenLocked(); err != nil {
		return err
	}
	ws.LastActivity = time.Now()
	if ws.Tx == nil {
		return fmt.Errorf("无活动事务可提交")
	}
	err := ws.Tx.Commit()
	// database/sql：Commit 返回后事务已结束（成功或失败），必须释放计数与 cancel，避免 activeTx 泄漏
	ws.clearTxLocked(true, wm.pool)
	if err != nil {
		return fmt.Errorf("提交失败: %w", err)
	}
	return nil
}

// RollbackWorkspace 回滚工作台的活动事务。
func (wm *WorkspaceManager) RollbackWorkspace(ws *Workspace) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if err := ws.ensureOpenLocked(); err != nil {
		return err
	}
	ws.LastActivity = time.Now()
	if ws.Tx == nil {
		return nil // 无事务可回滚，幂等
	}
	_ = ws.Tx.Rollback() // 回滚失败也清除状态（连接可能已断开）
	ws.clearTxLocked(true, wm.pool)
	return nil
}

// SetAutoCommit 设置自动提交模式。
// 活动事务未处理前不能重新开启自动提交。
func (wm *WorkspaceManager) SetAutoCommit(ws *Workspace, autoCommit bool) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if err := ws.ensureOpenLocked(); err != nil {
		return err
	}
	ws.LastActivity = time.Now()
	if autoCommit && ws.Tx != nil {
		return fmt.Errorf("存在活动事务，不能开启自动提交，请先提交或回滚")
	}
	ws.AutoCommit = autoCommit
	return nil
}

// fillResult 从 rows 填充 ExecutionResult。
// 优先使用 *sql.Rows 的 ColumnTypes 区分 TEXT 与 BLOB。
func fillResult(rows interface {
	Columns() ([]string, error)
	Next() bool
	Scan(...any) error
	Err() error
}, result *ExecutionResult, limit int) error {
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	result.Columns = cols
	typeNames := make([]string, len(cols))
	if sr, ok := rows.(*sql.Rows); ok {
		typeNames = columnTypeNames(sr, len(cols))
	}
	for rows.Next() {
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
	result.HasMore = result.ReturnedRows == limit
	return nil
}
