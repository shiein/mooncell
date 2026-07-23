// 工作台 API handlers：创建工作台、执行 SQL、提交/回滚、自动提交开关。
//
// 设计文档第三节「工作台」：
//   POST   /api/data-resources/{id}/workspaces
//   PATCH  /api/data-resources/{id}/workspaces/{workspaceId}/auto-commit
//   POST   /api/data-resources/{id}/workspaces/{workspaceId}/execute
//   POST   /api/data-resources/{id}/workspaces/{workspaceId}/commit
//   POST   /api/data-resources/{id}/workspaces/{workspaceId}/rollback
//   POST   /api/data-resources/{id}/workspaces/{workspaceId}/export
//   DELETE /api/data-resources/{id}/workspaces/{workspaceId}
package dataresource

import (
	"fmt"
	"net/http"
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
	return ws, mode, true
}

// CreateWorkspace 处理 POST /api/data-resources/{id}/workspaces
func (s *Service) CreateWorkspaceHandler(w http.ResponseWriter, r *http.Request) {
	adapter, _, ok := s.getAdapterForRequest(w, r)
	if !ok {
		return
	}
	user, _, _ := userFromCtx(r)
	id := r.PathValue("id")
	ws := s.workspaces.CreateWorkspace(id, user, adapter)
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
		writeErr(w, http.StatusForbidden, "DATA_RESOURCE_READ_ONLY", "只读授权不允许执行写操作")
		return
	}

	// 危险 SQL 二次确认：TRUNCATE/DROP/无 WHERE 的 DELETE/UPDATE
	if stmtType.IsDangerousWrite(body.SQL) && !body.Confirmed {
		writeErr(w, http.StatusForbidden, "DANGEROUS_SQL", "危险操作需二次确认：TRUNCATE/DROP/无 WHERE 的 DELETE 或 UPDATE")
		return
	}

	result, err := s.workspaces.ExecuteInWorkspace(r.Context(), ws, body.SQL, body.Limit)
	if err != nil {
		if apiErr, isAPIErr := err.(*APIError); isAPIErr {
			writeErr(w, http.StatusForbidden, apiErr.Code, apiErr.Message)
			return
		}
		writeErr(w, http.StatusBadRequest, "EXEC_ERROR", err.Error())
		return
	}
	// 审计：只记录写操作（DML/DDL）和只读拦截，不记录普通 SELECT。
	if stmtType.IsWrite() || stmtType.IsDDLorDCL() || stmtType == StmtTruncate || stmtType == StmtCall {
		auditResult := "成功"
		if result.AffectedRows >= 0 {
			auditResult = fmt.Sprintf("成功·%d行", result.AffectedRows)
		}
		s.auditLog(user, "执行"+string(stmtType), id, auditResult)
	}
	writeOK(w, result)
}

// CommitWorkspaceHandler 处理 POST /api/data-resources/{id}/workspaces/{workspaceId}/commit
func (s *Service) CommitWorkspaceHandler(w http.ResponseWriter, r *http.Request) {
	ws, mode, ok := s.resolveWorkspaceForRequest(w, r)
	if !ok {
		return
	}
	if !canWriteAccess(mode) {
		// 只读用户不应持有可写事务；提交写事务需 write/admin
		writeErr(w, http.StatusForbidden, "DATA_RESOURCE_READ_ONLY", "只读授权不允许提交写事务")
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
		SQL    string `json:"sql"`    // 指定导出 SQL；空则用最近一次成功查询
		Format string `json:"format"` // csv 或 xlsx
	}
	if err := jsonDecodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	sqlText := body.SQL
	if sqlText == "" {
		sqlText = ws.LastSQL
	}
	if sqlText == "" {
		writeErr(w, http.StatusBadRequest, "NO_QUERY", "无可导出的查询")
		return
	}
	if body.Format == "" {
		body.Format = "csv"
	}

	stmtType := ClassifySQL(sqlText)
	if !canWriteAccess(mode) && !stmtType.IsReadOnly() {
		writeErr(w, http.StatusForbidden, "DATA_RESOURCE_READ_ONLY", "只读授权不允许导出写操作结果")
		return
	}

	switch body.Format {
	case "csv":
		ExportCSV(r.Context(), ws.Adapter, sqlText, w)
	case "xlsx":
		ExportXLSX(r.Context(), ws.Adapter, sqlText, w)
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
