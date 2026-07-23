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
	"fmt"
	"net/http"
	"strings"
	"time"
)

// canWriteAccess 返回当前 access mode 是否允许写操作（admin 隐式 write）。
func canWriteAccess(mode string) bool {
	return mode == "admin" || mode == AccessWrite
}

// resolveWorkspaceForRequest 校验工作台存在、归属当前用户、path 资源 id 与工作台一致，
// 且用户对该资源仍有授权（mode != ""）。撤权后 mode 为空，所有工作台操作立即拒绝。
func (s *Service) resolveWorkspaceForRequest(w http.ResponseWriter, r *http.Request) (*Workspace, string, bool) {
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
	if ws.Username != user {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "无权操作他人工作台")
		return nil, "", false
	}
	id := r.PathValue("id")
	if id != ws.ResourceID {
		// path 与工作台资源不一致：按不存在处理，避免跨资源误操作
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "工作台不存在")
		return nil, "", false
	}
	mode, _ := UserAccessMode(s.db, user, role, id)
	if mode == "" {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "无权访问该资源")
		return nil, "", false
	}
	// 以最新授权刷新只读标志，避免授权升级后仍沿用创建时的 ReadOnly 快照。
	// 授权变更主路径会 Invalidate 工作台；此处为防御性同步。
	wantRO := !canWriteAccess(mode)
	ws.mu.Lock()
	ws.ReadOnly = wantRO
	ws.mu.Unlock()
	return ws, mode, true
}

// CreateWorkspace 处理 POST /api/data-resources/{id}/workspaces
func (s *Service) CreateWorkspaceHandler(w http.ResponseWriter, r *http.Request) {
	adapter, mode, ok := s.getAdapterForRequest(w, r)
	if !ok {
		return
	}
	user, _, _ := userFromCtx(r)
	id := r.PathValue("id")
	// 仅 AccessRead 标记为只读工作台；admin/write 可写
	ws := s.workspaces.CreateWorkspace(id, user, adapter, mode == AccessRead)
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
	if err := jsonDecodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	if err := s.workspaces.SetAutoCommit(ws, body.AutoCommit); err != nil {
		writeErr(w, http.StatusConflict, "TX_ACTIVE", err.Error())
		return
	}
	writeOK(w, map[string]any{
		"autoCommit": ws.AutoCommit,
		"txState":    string(ws.TxState),
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
		Confirmed bool   `json:"confirmed,omitempty"` // 危险 SQL 二次确认
	}
	if err := jsonDecodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
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

	result, err := s.workspaces.ExecuteInWorkspace(r.Context(), ws, body.SQL, body.Limit)
	if err != nil {
		if apiErr, isAPIErr := err.(*APIError); isAPIErr {
			if apiErr.Code == "DATA_RESOURCE_READ_ONLY" {
				s.auditSQL(user, "只读写入被拒绝", id, stmtType, body.SQL, "拒绝·READ_ONLY", 0)
				writeErr(w, http.StatusForbidden, apiErr.Code, apiErr.Message)
				return
			}
			if apiErr.Code == "IMPORT_ACTIVE" {
				writeErr(w, http.StatusConflict, apiErr.Code, apiErr.Message)
				return
			}
			writeErr(w, http.StatusForbidden, apiErr.Code, apiErr.Message)
			return
		}
		s.auditSQL(user, "执行"+string(stmtType), id, stmtType, body.SQL, "失败", 0)
		writeErr(w, http.StatusBadRequest, "EXEC_ERROR", err.Error())
		return
	}
	// 读写用户：单表 SELECT 且有主键时附带 editable 元数据，供结果区就地改删
	if canWriteAccess(mode) && result != nil && stmtType.IsReadOnly() {
		s.attachEditableMeta(r.Context(), ws, body.SQL, result)
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

// attachEditableMeta 为单表 SELECT 结果附加就地编辑元数据（仅表、需主键）。
func (s *Service) attachEditableMeta(ctx context.Context, ws *Workspace, sqlText string, result *ExecutionResult) {
	target, ok := DetectEditableSelect(sqlText)
	if !ok {
		return
	}
	schema := target.Schema
	if schema == "" {
		if res, found, _ := GetDataResource(s.db, ws.ResourceID); found {
			schema = res.DefaultSchema
			if schema == "" {
				schema = res.DatabaseName
			}
		}
	}
	obj := MetadataNode{Kind: NodeTable, Schema: schema, Name: target.Table}
	structure, err := ws.Adapter.Describe(ctx, obj)
	if err != nil {
		// 可能是视图或对象不存在：不开放编辑
		return
	}
	pks := primaryKeyColumns(structure)
	info := &EditableInfo{Schema: schema, Table: target.Table}
	if len(pks) == 0 {
		info.Reason = "该表没有主键，无法安全地修改或删除行（需主键定位目标行，与 Navicat 等工具一致）"
	} else {
		info.PrimaryKeys = pks
	}
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
	if err := jsonDecodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	body.Table = strings.TrimSpace(body.Table)
	body.Schema = strings.TrimSpace(body.Schema)
	if body.Table == "" {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", "表名不能为空")
		return
	}

	// 重新读取主键，不信任客户端传入的 PK 列表作为权威
	obj := MetadataNode{Kind: NodeTable, Schema: body.Schema, Name: body.Table}
	structure, err := ws.Adapter.Describe(r.Context(), obj)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "NOT_A_TABLE", "无法读取表结构，仅支持单表结果编辑")
		return
	}
	pks := primaryKeyColumns(structure)
	if len(pks) == 0 {
		writeErr(w, http.StatusBadRequest, "NO_PRIMARY_KEY", "该表没有主键，无法安全地修改或删除行")
		return
	}

	ws.mu.Lock()
	defer ws.mu.Unlock()
	// 有活动手工事务时禁止就地编辑，避免与用户事务交错
	if ws.TxState == TxActive || ws.TxState == TxFailed {
		writeErr(w, http.StatusConflict, "TX_ACTIVE", "存在活动手工事务，请先提交或回滚后再就地编辑")
		return
	}
	ws.LastActivity = time.Now()

	result, err := ApplyRowEdits(r.Context(), ws.Adapter, body.Schema, body.Table, pks, body)
	if err != nil {
		s.auditLog(user, "就地编辑", body.Schema+"."+body.Table, "失败·"+err.Error())
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
	if err := s.workspaces.CommitWorkspace(ws); err != nil {
		writeErr(w, http.StatusConflict, "COMMIT_FAILED", err.Error())
		return
	}
	s.auditLog(user, "提交事务", ws.ResourceID, "成功")
	writeOK(w, map[string]any{
		"txState":    string(ws.TxState),
		"autoCommit": ws.AutoCommit,
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
	if err := s.workspaces.RollbackWorkspace(ws); err != nil {
		writeErr(w, http.StatusInternalServerError, "ROLLBACK_FAILED", err.Error())
		return
	}
	s.auditLog(user, "回滚事务", ws.ResourceID, "成功")
	writeOK(w, map[string]any{
		"txState":    string(ws.TxState),
		"autoCommit": ws.AutoCommit,
	})
}

// ExportFromWorkspace 处理 POST /api/data-resources/{id}/workspaces/{workspaceId}/export
// 全量导出重新执行最近一次成功的查询。
func (s *Service) ExportFromWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, mode, ok := s.resolveWorkspaceForRequest(w, r)
	if !ok {
		return
	}
	var body struct {
		SQL     string   `json:"sql"`     // 指定导出 SQL；空则用最近一次成功查询
		Format  string   `json:"format"`  // csv 或 xlsx
		Scope   string   `json:"scope"`   // all 或 current
		Columns []string `json:"columns"` // current 快照
		Rows    [][]any  `json:"rows"`    // current 快照，最多 MaxLimit 行
	}
	if err := jsonDecodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	if body.Format == "" {
		body.Format = "csv"
	}
	_ = mode // 授权已在 resolve 校验；SQL 已限制为只读

	// 同一工作台单飞：导出全程持 ws.mu，与 Execute/Commit/Rollback 互斥。
	// 全量导出在适配器上独立执行只读查询，不读取手工事务未提交数据。
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.LastActivity = time.Now()

	if body.Scope == "current" {
		var err error
		switch body.Format {
		case "csv":
			err = ExportSnapshotCSV(body.Columns, body.Rows, w)
		case "xlsx":
			err = ExportSnapshotXLSX(body.Columns, body.Rows, w)
		default:
			writeErr(w, http.StatusBadRequest, "BAD_FORMAT", "不支持的导出格式: "+body.Format)
			return
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, "EXPORT_ERROR", err.Error())
		}
		return
	}

	sqlText := body.SQL
	if sqlText == "" {
		sqlText = ws.LastSQL
	}
	adapter := ws.Adapter
	if sqlText == "" {
		writeErr(w, http.StatusBadRequest, "NO_QUERY", "无可导出的查询")
		return
	}
	if err := prepareExportSQL(sqlText); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_SQL", err.Error())
		return
	}

	switch body.Format {
	case "csv":
		if err := ExportCSV(r.Context(), adapter, sqlText, w); err != nil {
			if _, ok := err.(*ErrExportLimit); ok {
				// CSV 可能已在流中写入截断提示
				return
			}
			// 流未开始时尽量返回错误
			return
		}
	case "xlsx":
		if err := ExportXLSX(r.Context(), adapter, sqlText, w); err != nil {
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
	if ws.Username != user {
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
