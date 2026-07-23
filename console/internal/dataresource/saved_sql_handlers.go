// 保存 SQL API handlers：个人 SQL 的增删改查。
//
// 设计文档第四节「保存 SQL」：
//   GET    /api/data-resources/{id}/saved-sql
//   POST   /api/data-resources/{id}/saved-sql
//   PUT    /api/data-resources/{id}/saved-sql/{sqlId}
//   DELETE /api/data-resources/{id}/saved-sql/{sqlId}
//
// 服务端始终以当前登录用户作为所有者，不接受请求体中的用户名。
// 每个用户只能查看、修改、删除自己的 SQL。
package dataresource

import (
	"database/sql"
	"net/http"
	"strings"
	"time"
)

// ListSavedSQLHandler 处理 GET /api/data-resources/{id}/saved-sql
func (s *Service) ListSavedSQLHandler(w http.ResponseWriter, r *http.Request) {
	user, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "未登录")
		return
	}
	id := r.PathValue("id")
	// 校验资源访问权限
	mode, _ := UserAccessMode(s.db, user, role, id)
	if mode == "" {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "无权访问该资源")
		return
	}
	list, err := ListSavedSQL(s.db, user, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "读取保存 SQL 失败")
		return
	}
	if list == nil {
		list = []SavedSQL{}
	}
	writeOK(w, map[string]any{"savedSql": list})
}

// CreateSavedSQLHandler 处理 POST /api/data-resources/{id}/saved-sql
func (s *Service) CreateSavedSQLHandler(w http.ResponseWriter, r *http.Request) {
	user, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "未登录")
		return
	}
	id := r.PathValue("id")
	mode, _ := UserAccessMode(s.db, user, role, id)
	if mode == "" {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "无权访问该资源")
		return
	}
	var body struct {
		Name    string `json:"name"`
		SQLText string `json:"sqlText"`
	}
	if err := jsonDecodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", "名称不能为空")
		return
	}
	if strings.TrimSpace(body.SQLText) == "" {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", "SQL 内容不能为空")
		return
	}
	now := time.Now().UnixMilli()
	rec := SavedSQL{
		ID: newID(), Username: user, ResourceID: id,
		Name: body.Name, SQLText: body.SQLText,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := CreateSavedSQL(s.db, rec); err != nil {
		if isUniqueConstraint(err) {
			writeErr(w, http.StatusConflict, "NAME_DUPLICATE", "同一资源下 SQL 名称已存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "保存 SQL 失败")
		return
	}
	writeOK(w, rec)
}

// UpdateSavedSQLHandler 处理 PUT /api/data-resources/{id}/saved-sql/{sqlId}
func (s *Service) UpdateSavedSQLHandler(w http.ResponseWriter, r *http.Request) {
	user, _, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "未登录")
		return
	}
	sqlID := r.PathValue("sqlId")
	var body struct {
		Name    string `json:"name"`
		SQLText string `json:"sqlText"`
	}
	if err := jsonDecodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", "名称不能为空")
		return
	}
	if strings.TrimSpace(body.SQLText) == "" {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", "SQL 内容不能为空")
		return
	}
	if err := UpdateSavedSQL(s.db, sqlID, user, body.Name, body.SQLText); err != nil {
		if err == sql.ErrNoRows {
			writeErr(w, http.StatusNotFound, "NOT_FOUND", "SQL 不存在或无权修改")
			return
		}
		if isUniqueConstraint(err) {
			writeErr(w, http.StatusConflict, "NAME_DUPLICATE", "同一资源下 SQL 名称已存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "更新 SQL 失败")
		return
	}
	writeOK(w, map[string]bool{"ok": true})
}

// DeleteSavedSQLHandler 处理 DELETE /api/data-resources/{id}/saved-sql/{sqlId}
func (s *Service) DeleteSavedSQLHandler(w http.ResponseWriter, r *http.Request) {
	user, _, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "未登录")
		return
	}
	sqlID := r.PathValue("sqlId")
	if err := DeleteSavedSQL(s.db, sqlID, user); err != nil {
		if err == sql.ErrNoRows {
			writeErr(w, http.StatusNotFound, "NOT_FOUND", "SQL 不存在或无权删除")
			return
		}
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "删除 SQL 失败")
		return
	}
	writeOK(w, map[string]bool{"ok": true})
}
