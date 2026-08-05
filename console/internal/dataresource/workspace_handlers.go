// 工作台 API handlers：创建工作台、执行 SQL、提交/回滚、自动提交开关。
//
// 设计文档第三节「工作台」：
//
//	POST   /api/data-resources/{id}/workspaces
//	PATCH  /api/data-resources/{id}/workspaces/{workspaceId}/auto-commit
//	POST   /api/data-resources/{id}/workspaces/{workspaceId}/execute
//	POST   /api/data-resources/{id}/workspaces/{workspaceId}/commit
//	POST   /api/data-resources/{id}/workspaces/{workspaceId}/rollback
//	POST   /api/data-resources/{id}/workspaces/{workspaceId}/export
//	DELETE /api/data-resources/{id}/workspaces/{workspaceId}
package dataresource

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// canWriteAccess 返回当前 access mode 是否允许写操作（admin 隐式 write）。
func canWriteAccess(mode string) bool {
	return mode == "admin" || mode == AccessWrite
}

// resolveWorkspaceAccess 校验工作台存在、归属当前用户、path 资源 id 与工作台一致，
// 且用户对该资源仍有授权（mode != ""）。refreshReadOnly 仅供普通工作台操作使用；
// cancel 路径必须传 false，避免等待正在执行语句持有的 ws.mu。
func (s *Service) resolveWorkspaceAccess(w http.ResponseWriter, r *http.Request, refreshReadOnly bool) (*Workspace, string, bool) {
	wsID := r.PathValue("workspaceId")
	ws, ok := s.workspaces.GetWorkspace(wsID)
	if !ok {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "工作台不存在")
		return nil, "", false
	}
	user, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "未登录")
		return nil, "", false
	}
	loginSessionID, hasSession := loginSessionFromCtx(r)
	if !hasSession || ws.Username != user || ws.LoginSessionID != loginSessionID {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "无权操作他人工作台")
		return nil, "", false
	}
	id := r.PathValue("id")
	if id != ws.ResourceID {
		// path 与工作台资源不一致：按不存在处理，避免跨资源误操作
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "工作台不存在")
		return nil, "", false
	}
	mode, err := UserAccessMode(s.db, user, role, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "读取资源授权失败")
		return nil, "", false
	}
	if mode == "" {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "无权访问该资源")
		return nil, "", false
	}
	if refreshReadOnly {
		// 以最新授权刷新只读标志，避免授权升级后仍沿用创建时的 ReadOnly 快照。
		// 授权变更主路径会 Invalidate 工作台；此处为防御性同步。
		wantRO := !canWriteAccess(mode)
		ws.mu.Lock()
		ws.ReadOnly = wantRO
		ws.mu.Unlock()
	}
	return ws, mode, true
}

func (s *Service) resolveWorkspaceForRequest(w http.ResponseWriter, r *http.Request) (*Workspace, string, bool) {
	return s.resolveWorkspaceAccess(w, r, true)
}

// CreateWorkspace 处理 POST /api/data-resources/{id}/workspaces
func (s *Service) CreateWorkspaceHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	user, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "未登录")
		return
	}
	loginSessionID, ok := loginSessionFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "登录会话无效")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := jsonDecodeBody(w, r, &body); err != nil {
		writeJSONBodyError(w, err)
		return
	}
	res, found, err := GetDataResource(s.db, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "读取资源失败")
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "资源不存在")
		return
	}
	mode, err := UserAccessMode(s.db, user, role, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "读取资源授权失败")
		return
	}
	if mode == "" {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "无权访问该资源")
		return
	}
	if !s.pools.TryBeginOperation(id) {
		writeErr(w, http.StatusConflict, "RESOURCE_BUSY", "资源正在导入或更新，请稍后再试")
		return
	}
	defer s.pools.EndOperation(id)
	if body.Password != "" {
		// 显式输入新密码只替换当前登录会话的租约，不影响同账号其它浏览器。
		s.InvalidateLoginSessionResource(loginSessionID, id)
	}
	generation := s.snapshotAccessGeneration(user, loginSessionID, id)
	ctx, cancel := context.WithTimeout(r.Context(), testTimeout)
	defer cancel()
	var db *sql.DB
	if body.Password != "" {
		db, err = s.pools.ConnectSessionDBIf(ctx, res, user, loginSessionID, body.Password, func() bool {
			return s.accessGenerationCurrent(generation, user, loginSessionID, id)
		})
		if err != nil {
			if errors.Is(err, ErrConnectionLeaseStale) {
				writeErr(w, http.StatusConflict, "SESSION_STATE_CHANGED", err.Error())
				return
			}
			code := classifyConnError(err)
			writeErr(w, http.StatusBadRequest, code, formatConnError(code, err))
			return
		}
	} else {
		db, err = s.pools.GetSessionDB(id, loginSessionID)
		if err != nil {
			writeErr(w, http.StatusPreconditionRequired, "PASSWORD_REQUIRED", "请输入数据库密码")
			return
		}
		if err := db.PingContext(ctx); err != nil {
			s.pools.CloseSessionDB(id, loginSessionID)
			writeErr(w, http.StatusPreconditionRequired, "PASSWORD_REQUIRED", "数据库连接已失效，请重新输入密码")
			return
		}
	}
	adapter, err := NewAdapter(db, res.DBType, BoundSchema(res))
	if err != nil {
		s.pools.CloseSessionDB(id, loginSessionID)
		writeErr(w, http.StatusInternalServerError, "ADAPTER_ERROR", "创建适配器失败")
		return
	}
	if s.valid != nil && !s.valid(loginSessionID) {
		s.pools.CloseSessionDB(id, loginSessionID)
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "登录会话已失效")
		return
	}
	// 外部连接可能耗时数秒；发布工作台前重新读取授权并以代际锁封住 logout/撤权竞态。
	mode, err = UserAccessMode(s.db, user, role, id)
	if err != nil {
		s.pools.CloseSessionDB(id, loginSessionID)
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "读取资源授权失败")
		return
	}
	if mode == "" {
		s.pools.CloseSessionDB(id, loginSessionID)
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "无权访问该资源")
		return
	}
	// 仅 AccessRead 标记为只读工作台；admin/write 可写
	ws, registered := s.createWorkspaceIfCurrent(generation, id, user, loginSessionID, adapter, mode == AccessRead)
	if !registered {
		s.pools.CloseSessionDB(id, loginSessionID)
		writeErr(w, http.StatusConflict, "SESSION_STATE_CHANGED", "登录会话或授权已变更，请重试")
		return
	}
	writeOK(w, map[string]any{
		"workspaceId": ws.ID,
		"autoCommit":  ws.AutoCommit,
		"txState":     string(ws.TxState),
	})
}

// PatchAutoCommit 处理 PATCH /api/data-resources/{id}/workspaces/{workspaceId}/auto-commit
func (s *Service) PatchAutoCommit(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := s.resolveWorkspaceForRequest(w, r)
	if !ok {
		return
	}
	var body struct {
		AutoCommit bool `json:"autoCommit"`
	}
	if err := jsonDecodeBody(w, r, &body); err != nil {
		writeJSONBodyError(w, err)
		return
	}
	snap, err := s.workspaces.SetAutoCommit(ws, body.AutoCommit)
	if err != nil {
		writeErr(w, http.StatusConflict, "TX_ACTIVE", err.Error())
		return
	}
	writeOK(w, map[string]any{
		"autoCommit": snap.AutoCommit,
		"txState":    string(snap.TxState),
	})
}

// CancelWorkspaceHandler 处理 POST /api/data-resources/{id}/workspaces/{workspaceId}/cancel
// 取消当前正在执行的语句。不得获取 ws.mu（否则与 Execute 死锁）。
func (s *Service) CancelWorkspaceHandler(w http.ResponseWriter, r *http.Request) {
	// cancel 专用解析不得获取 ws.mu；ExecuteInWorkspace 在数据库调用期间持有该锁。
	ws, _, ok := s.resolveWorkspaceAccess(w, r, false)
	if !ok {
		return
	}
	canceled := s.workspaces.CancelWorkspaceStatement(ws)
	writeOK(w, map[string]any{
		"canceled": canceled,
		// 执行中无法安全读 TxState（需 mu）；取消后前端以 execute 返回的 QUERY_CANCELED / txState 为准
	})
}

// ExecuteInWorkspace 处理 POST /api/data-resources/{id}/workspaces/{workspaceId}/execute
func (s *Service) ExecuteInWorkspaceHandler(w http.ResponseWriter, r *http.Request) {
	ws, mode, ok := s.resolveWorkspaceForRequest(w, r)
	if !ok {
		return
	}
	user, _, _ := userFromCtx(r)
	id := r.PathValue("id")
	var body struct {
		SQL       string `json:"sql"`
		Limit     int    `json:"limit,omitempty"`
		Offset    int    `json:"offset,omitempty"`    // 隐式分页偏移，不改写 SQL
		Confirmed bool   `json:"confirmed,omitempty"` // 危险 SQL 二次确认
	}
	if err := jsonDecodeBody(w, r, &body); err != nil {
		writeJSONBodyError(w, err)
		return
	}
	if body.SQL == "" {
		writeErr(w, http.StatusBadRequest, "EMPTY_SQL", "SQL 不能为空")
		return
	}

	stmtType := ClassifySQL(body.SQL)
	// 无写权限（只读或其它非 write）一律拒绝写操作；mode=="" 已在 resolve 拦截
	if !canWriteAccess(mode) && !stmtType.IsReadOnly() {
		s.auditSQL(user, "只读写入被拒绝", id, stmtType, body.SQL, "拒绝·READ_ONLY", 0)
		writeErr(w, http.StatusForbidden, "DATA_RESOURCE_READ_ONLY", "只读授权不允许执行写操作")
		return
	}

	// 危险 SQL 二次确认：TRUNCATE/DROP/无 WHERE 的 DELETE/UPDATE
	if stmtType.IsDangerousWrite(body.SQL) && !body.Confirmed {
		s.auditSQL(user, "危险SQL被拒绝", id, stmtType, body.SQL, "拒绝·DANGEROUS", 0)
		writeErr(w, http.StatusForbidden, "DANGEROUS_SQL", "危险操作需二次确认：TRUNCATE/DROP/无 WHERE 的 DELETE 或 UPDATE")
		return
	}

	result, err := s.workspaces.ExecuteInWorkspace(r.Context(), ws, body.SQL, body.Limit, body.Offset)
	if err != nil {
		// 执行失败时 result 仍可能带有正确的 TxState（如取消后 failed），必须回传前端
		txState := "none"
		if result != nil && result.TxState != "" {
			txState = result.TxState
		}
		if apiErr, isAPIErr := err.(*APIError); isAPIErr {
			if apiErr.TxState != "" {
				txState = apiErr.TxState
			}
			if apiErr.Code == "DATA_RESOURCE_READ_ONLY" {
				s.auditSQL(user, "只读写入被拒绝", id, stmtType, body.SQL, "拒绝·READ_ONLY", 0)
				writeErrTx(w, http.StatusForbidden, apiErr.Code, apiErr.Message, txState)
				return
			}
			if apiErr.Code == "IMPORT_ACTIVE" || apiErr.Code == "RESOURCE_BUSY" {
				writeErrTx(w, http.StatusConflict, apiErr.Code, apiErr.Message, txState)
				return
			}
			if apiErr.Code == "WORKSPACE_CLOSED" {
				writeErrTx(w, http.StatusGone, apiErr.Code, apiErr.Message, txState)
				return
			}
			writeErrTx(w, http.StatusForbidden, apiErr.Code, apiErr.Message, txState)
			return
		}
		var de DatabaseError
		if errors.As(err, &de) && de.Code != "" {
			s.auditSQL(user, "执行"+string(stmtType), id, stmtType, body.SQL, "失败·"+de.Code, 0)
			status := http.StatusBadRequest
			switch de.Code {
			case "DATA_RESOURCE_READ_ONLY":
				status = http.StatusForbidden
			case "QUERY_CANCELED":
				// 用 400 而非 408：部分代理对 408 行为怪异；语义靠 code
				status = http.StatusBadRequest
			}
			writeErrTx(w, status, de.Code, de.Message, txState)
			return
		}
		s.auditSQL(user, "执行"+string(stmtType), id, stmtType, body.SQL, "失败", 0)
		writeErrTx(w, http.StatusBadRequest, "EXEC_ERROR", err.Error(), txState)
		return
	}
	// 读写用户：单表 SELECT 且有主键时附带 editable 元数据，供结果区就地改删
	if canWriteAccess(mode) && result != nil && stmtType.IsReadOnly() {
		s.attachEditableMeta(r.Context(), ws, body.SQL, result, ws.snapshotState().AutoCommit)
	}
	// 审计：写操作（DML/DDL）记录类型、哈希、行数、耗时；不记录普通 SELECT 全文。
	if stmtType.IsWrite() || stmtType.IsDDLorDCL() || stmtType == StmtTruncate || stmtType == StmtCall {
		auditResult := "成功"
		if result.AffectedRows >= 0 {
			auditResult = fmt.Sprintf("成功·%d行", result.AffectedRows)
		}
		s.auditSQL(user, "执行"+string(stmtType), id, stmtType, body.SQL, auditResult, result.DurationMs)
	}
	writeOK(w, result)
}

// attachEditableMeta 为 SELECT * 单表结果附加就地编辑元数据。
// autoCommit 须由调用方在锁内快照传入，禁止解锁后直接读 ws.AutoCommit。
func (s *Service) attachEditableMeta(ctx context.Context, ws *Workspace, sqlText string, result *ExecutionResult, autoCommit bool) {
	// 手工事务模式下禁用就地编辑，避免绕过自动提交开关与提交/回滚按钮
	if !autoCommit {
		result.Editable = &EditableInfo{Reason: "已关闭自动提交，请使用 SQL 编辑或先开启自动提交后再就地编辑"}
		return
	}
	if ws.isClosed() || !s.pools.TryBeginOperation(ws.ResourceID) {
		return
	}
	defer s.pools.EndOperation(ws.ResourceID)
	target, ok := DetectEditableSelect(sqlText)
	if !ok {
		return
	}
	schema := target.Schema
	if schema == "" {
		res, found, err := GetDataResource(s.db, ws.ResourceID)
		if err != nil {
			return
		}
		if found {
			schema = BoundSchema(res)
		}
	}
	obj := MetadataNode{Kind: NodeTable, Schema: schema, Name: target.Table}
	mctx, cancel := context.WithTimeout(ctx, MetadataTimeout)
	defer cancel()
	structure, err := ws.Adapter.Describe(mctx, obj)
	if err != nil {
		return
	}
	pks := primaryKeyColumns(structure)
	info := &EditableInfo{Schema: schema, Table: target.Table}
	if len(pks) == 0 {
		info.Reason = "该表没有主键，无法安全地修改或删除行"
		result.Editable = info
		return
	}
	// 结果列须包含全部主键（SELECT * 通常满足；双重校验）
	colLower := map[string]bool{}
	for _, c := range result.Columns {
		colLower[strings.ToLower(c)] = true
	}
	for _, pk := range pks {
		if !colLower[strings.ToLower(pk)] {
			info.Reason = "结果中缺少主键列，无法就地编辑"
			result.Editable = info
			return
		}
	}
	info.PrimaryKeys = pks
	result.Editable = info
}

// ApplyRowEditsHandler 处理 POST .../workspaces/{workspaceId}/row-edits
// 仅 write/admin；按主键批量 UPDATE/DELETE。
func (s *Service) ApplyRowEditsHandler(w http.ResponseWriter, r *http.Request) {
	ws, mode, ok := s.resolveWorkspaceForRequest(w, r)
	if !ok {
		return
	}
	if !canWriteAccess(mode) {
		writeErr(w, http.StatusForbidden, "DATA_RESOURCE_READ_ONLY", "只读授权不允许就地编辑")
		return
	}
	user, _, _ := userFromCtx(r)
	var body RowEditRequest
	if err := jsonDecodeBody(w, r, &body); err != nil {
		writeJSONBodyError(w, err)
		return
	}
	body.Table = strings.TrimSpace(body.Table)
	body.Schema = strings.TrimSpace(body.Schema)
	if body.Table == "" {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", "表名不能为空")
		return
	}

	ws.mu.Lock()
	defer ws.mu.Unlock()
	if err := ws.ensureOpenLocked(); err != nil {
		writeErr(w, http.StatusGone, "WORKSPACE_CLOSED", err.Error())
		return
	}
	if !ws.AutoCommit {
		writeErr(w, http.StatusConflict, "AUTO_COMMIT_REQUIRED", "已关闭自动提交时不可就地编辑，请开启自动提交或使用 SQL")
		return
	}
	// 有活动手工事务时禁止就地编辑，避免与用户事务交错
	if ws.TxState == TxActive || ws.TxState == TxFailed {
		writeErr(w, http.StatusConflict, "TX_ACTIVE", "存在活动手工事务，请先提交或回滚后再就地编辑")
		return
	}
	// 与导入/配置变更互斥：复用 importing 槽，堵住与 Import 的并发写
	if !s.pools.TryBeginImport(ws.ResourceID) {
		writeErr(w, http.StatusConflict, "RESOURCE_BUSY", "资源正在导入、配置变更或存在活动事务，请稍后再就地编辑")
		return
	}
	defer s.pools.EndImport(ws.ResourceID)

	ws.LastActivity = time.Now()

	ectx, cancel := context.WithTimeout(r.Context(), QueryTimeout)
	ws.setStmtCancel(cancel)
	defer func() {
		ws.clearStmtCancel()
		cancel()
	}()
	// invalidation 可能发生在第一次 closed 检查与 cancel 登记之间；登记后必须复核。
	if err := ws.ensureOpenLocked(); err != nil {
		writeErr(w, http.StatusGone, "WORKSPACE_CLOSED", err.Error())
		return
	}

	// 重新读取主键，不信任客户端传入的 PK 列表作为权威。
	obj := MetadataNode{Kind: NodeTable, Schema: body.Schema, Name: body.Table}
	structure, err := ws.Adapter.Describe(ectx, obj)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "NOT_A_TABLE", "无法读取表结构，仅支持单表结果编辑")
		return
	}
	pks := primaryKeyColumns(structure)
	if len(pks) == 0 {
		writeErr(w, http.StatusBadRequest, "NO_PRIMARY_KEY", "该表没有主键，无法安全地修改或删除行")
		return
	}

	result, err := applyRowEditsGuarded(ectx, ws.Adapter, body.Schema, body.Table, pks, body, ws.ensureOpenLocked)
	if err != nil {
		// 审计不落库原始错误文本
		s.auditLog(user, "就地编辑", body.Schema+"."+body.Table, "失败")
		writeErr(w, http.StatusBadRequest, "ROW_EDIT_ERROR", err.Error())
		return
	}
	s.auditLog(user, "就地编辑", body.Schema+"."+body.Table,
		fmt.Sprintf("成功·更新%d·删除%d", result.Updated, result.Deleted))
	writeOK(w, result)
}

// CommitWorkspaceHandler 处理 POST /api/data-resources/{id}/workspaces/{workspaceId}/commit
// 只读用户的手工事务本身是 DB 只读事务，允许提交以正常结束事务。
func (s *Service) CommitWorkspaceHandler(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := s.resolveWorkspaceForRequest(w, r)
	if !ok {
		return
	}
	user, _, _ := userFromCtx(r)
	snap, err := s.workspaces.CommitWorkspace(ws)
	if err != nil {
		writeErrTx(w, http.StatusConflict, "COMMIT_FAILED", err.Error(), string(snap.TxState))
		return
	}
	s.auditLog(user, "提交事务", ws.ResourceID, "成功")
	writeOK(w, map[string]any{
		"txState":    string(snap.TxState),
		"autoCommit": snap.AutoCommit,
	})
}

// RollbackWorkspaceHandler 处理 POST /api/data-resources/{id}/workspaces/{workspaceId}/rollback
// 回滚允许只读用户调用（只读事务也需要能主动结束）。
func (s *Service) RollbackWorkspaceHandler(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := s.resolveWorkspaceForRequest(w, r)
	if !ok {
		return
	}
	user, _, _ := userFromCtx(r)
	snap, err := s.workspaces.RollbackWorkspace(ws)
	if err != nil {
		writeErrTx(w, http.StatusInternalServerError, "ROLLBACK_FAILED", err.Error(), string(snap.TxState))
		return
	}
	s.auditLog(user, "回滚事务", ws.ResourceID, "成功")
	writeOK(w, map[string]any{
		"txState":    string(snap.TxState),
		"autoCommit": snap.AutoCommit,
	})
}

// ExportFromWorkspace 处理 POST /api/data-resources/{id}/workspaces/{workspaceId}/export
// 仅全量导出（重新执行最近成功查询）；当前页快照由前端本地生成 CSV，不再 POST 回环。
func (s *Service) ExportFromWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, mode, ok := s.resolveWorkspaceForRequest(w, r)
	if !ok {
		return
	}
	if !s.pools.TryBeginOperation(ws.ResourceID) {
		writeErr(w, http.StatusConflict, "RESOURCE_BUSY", "资源正在导入或更新，请稍后再导出")
		return
	}
	defer s.pools.EndOperation(ws.ResourceID)

	var body struct {
		SQL    string `json:"sql"`    // 指定导出 SQL；空则用最近一次成功查询
		Format string `json:"format"` // csv 或 xlsx
		Scope  string `json:"scope"`  // 仅 all；current 已废弃
	}
	if err := jsonDecodeBody(w, r, &body); err != nil {
		writeJSONBodyError(w, err)
		return
	}
	if body.Format == "" {
		body.Format = "csv"
	}
	if body.Scope == "current" {
		writeErr(w, http.StatusBadRequest, "BAD_SCOPE", "当前结果请在浏览器本地导出 CSV；服务端仅支持全量导出")
		return
	}
	_ = mode // 授权已在 resolve 校验；SQL 已限制为只读

	// 锁内仅快照 LastSQL/Adapter；导出走独立只读查询，不长时间占用 ws.mu
	ws.mu.Lock()
	ws.LastActivity = time.Now()
	sqlText := body.SQL
	if sqlText == "" {
		sqlText = ws.LastSQL
	}
	adapter := ws.Adapter
	ws.mu.Unlock()

	if sqlText == "" {
		writeErr(w, http.StatusBadRequest, "NO_QUERY", "无可导出的查询")
		return
	}
	if err := prepareExportSQL(sqlText); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_SQL", err.Error())
		return
	}

	ectx, cancel := context.WithTimeout(r.Context(), ExportTimeout)
	defer cancel()

	switch body.Format {
	case "csv":
		if err := ExportCSV(ectx, adapter, sqlText, w); err != nil {
			if lim, ok := err.(*ErrExportLimit); ok {
				// 临时文件路径：超限时尚未写响应体，可返回明确错误
				writeErr(w, http.StatusRequestEntityTooLarge, "EXPORT_LIMIT", lim.Reason)
				return
			}
			var bodyStarted *ErrExportBodyStarted
			if errors.As(err, &bodyStarted) {
				panic(http.ErrAbortHandler)
			}
			writeErr(w, http.StatusBadRequest, "EXPORT_ERROR", err.Error())
			return
		}
	case "xlsx":
		if err := ExportXLSX(ectx, adapter, sqlText, w); err != nil {
			if lim, ok := err.(*ErrExportLimit); ok {
				writeErr(w, http.StatusRequestEntityTooLarge, "EXPORT_LIMIT", lim.Reason)
				return
			}
			writeErr(w, http.StatusBadRequest, "EXPORT_ERROR", err.Error())
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "BAD_FORMAT", "不支持的导出格式: "+body.Format)
	}
}

// DeleteWorkspaceHandler 处理 DELETE /api/data-resources/{id}/workspaces/{workspaceId}
func (s *Service) DeleteWorkspaceHandler(w http.ResponseWriter, r *http.Request) {
	// 删除：校验归属与 path 一致即可；授权已撤时仍允许用户清理本地工作台状态
	wsID := r.PathValue("workspaceId")
	ws, ok := s.workspaces.GetWorkspace(wsID)
	if !ok {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "工作台不存在")
		return
	}
	user, _, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "未登录")
		return
	}
	loginSessionID, hasSession := loginSessionFromCtx(r)
	if !hasSession || ws.Username != user || ws.LoginSessionID != loginSessionID {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "无权操作他人工作台")
		return
	}
	if r.PathValue("id") != ws.ResourceID {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "工作台不存在")
		return
	}
	s.workspaces.DeleteWorkspace(wsID)
	writeOK(w, map[string]bool{"ok": true})
}
