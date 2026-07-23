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
	wsID := r.PathValue("workspaceId")
	ws, ok := s.workspaces.GetWorkspace(wsID)
	if !ok {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "工作台不存在")
		return
	}
	// 校验工作台归属
	user, _, _ := userFromCtx(r)
	if ws.Username != user {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "无权操作他人工作台")
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
	wsID := r.PathValue("workspaceId")
	ws, ok := s.workspaces.GetWorkspace(wsID)
	if !ok {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "工作台不存在")
		return
	}
	user, _, _ := userFromCtx(r)
	if ws.Username != user {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "无权操作他人工作台")
		return
	}
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

	// 权限：只读用户只能执行 SELECT
	_, role, _ := userFromCtx(r)
	id := r.PathValue("id")
	mode, _ := UserAccessMode(s.db, user, role, id)
	stmtType := ClassifySQL(body.SQL)
	if mode == AccessRead && !stmtType.IsReadOnly() {
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
	wsID := r.PathValue("workspaceId")
	ws, ok := s.workspaces.GetWorkspace(wsID)
	if !ok {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "工作台不存在")
		return
	}
	user, _, _ := userFromCtx(r)
	if ws.Username != user {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "无权操作他人工作台")
		return
	}
	if err := s.workspaces.CommitWorkspace(ws); err != nil {
		writeErr(w, http.StatusConflict, "COMMIT_FAILED", err.Error())
		return
	}
	s.auditLog(user, "提交事务", ws.ResourceID, "成功")
	writeOK(w, map[string]any{
		"txState":   string(ws.TxState),
		"autoCommit": ws.AutoCommit,
	})
}

// RollbackWorkspaceHandler 处理 POST /api/data-resources/{id}/workspaces/{workspaceId}/rollback
func (s *Service) RollbackWorkspaceHandler(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("workspaceId")
	ws, ok := s.workspaces.GetWorkspace(wsID)
	if !ok {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "工作台不存在")
		return
	}
	user, _, _ := userFromCtx(r)
	if ws.Username != user {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "无权操作他人工作台")
		return
	}
	if err := s.workspaces.RollbackWorkspace(ws); err != nil {
		writeErr(w, http.StatusInternalServerError, "ROLLBACK_FAILED", err.Error())
		return
	}
	s.auditLog(user, "回滚事务", ws.ResourceID, "成功")
	writeOK(w, map[string]any{
		"txState":   string(ws.TxState),
		"autoCommit": ws.AutoCommit,
	})
}

// ExportFromWorkspace 处理 POST /api/data-resources/{id}/workspaces/{workspaceId}/export
// 全量导出重新执行最近一次成功的查询。
func (s *Service) ExportFromWorkspace(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("workspaceId")
	ws, ok := s.workspaces.GetWorkspace(wsID)
	if !ok {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "工作台不存在")
		return
	}
	user, _, _ := userFromCtx(r)
	if ws.Username != user {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "无权操作他人工作台")
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

	// 只读用户只能导出 SELECT
	stmtType := ClassifySQL(sqlText)
	_, role, _ := userFromCtx(r)
	id := r.PathValue("id")
	mode, _ := UserAccessMode(s.db, user, role, id)
	if mode == AccessRead && !stmtType.IsReadOnly() {
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
	wsID := r.PathValue("workspaceId")
	ws, ok := s.workspaces.GetWorkspace(wsID)
	if !ok {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "工作台不存在")
		return
	}
	user, _, _ := userFromCtx(r)
	if ws.Username != user {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "无权操作他人工作台")
		return
	}
	s.workspaces.DeleteWorkspace(wsID)
	writeOK(w, map[string]bool{"ok": true})
}
