// HTTP handlers：数据资源管理 API。
//
// 设计文档第三节「公共 API」：
//   GET    /api/data-resources
//   POST   /api/data-resources
//   GET    /api/data-resources/{id}
//   PUT    /api/data-resources/{id}
//   DELETE /api/data-resources/{id}
//   POST   /api/data-resources/test
//   POST   /api/data-resources/{id}/test
//
// 统一错误结构：
//   {"error": "用户可读错误", "code": "STABLE_ERROR_CODE", "txState": "none|active|failed"}
package dataresource

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// APIError 是统一的错误响应结构。同时实现 error 接口供内部传递。
type APIError struct {
	Message string `json:"error"`
	Code    string `json:"code,omitempty"`
	TxState string `json:"txState,omitempty"`
}

// Error 实现 error 接口。
func (e *APIError) Error() string { return e.Message }

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(APIError{Message: msg, Code: code, TxState: "none"})
}

func writeOK(w http.ResponseWriter, v any) {
	writeJSON(w, http.StatusOK, v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// newID 生成资源/SQL 的唯一 ID。
func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// toOut 将内部 DataResource 转为对外形态（不含密码，含 hasPassword 和 accessMode）。
func toOut(r DataResource, accessMode string) DataResourceOut {
	return DataResourceOut{
		DataResource:  r,
		HasPassword:   r.CredentialCipher != "",
		AccessMode:    accessMode,
		LastTestInfo:  formatTestInfo(r.LastTestStatus, r.LastTestAt),
	}
}

func formatTestInfo(status string, at int64) string {
	if status == "" || at == 0 {
		return ""
	}
	t := time.UnixMilli(at).Format("2006-01-02 15:04")
	if status == "ok" {
		return "成功 · " + t
	}
	return "失败 · " + t
}

// --- 资源 CRUD ---

// ListResources 处理 GET /api/data-resources。
// admin 返回全部；普通用户仅返回授权资源。
func (s *Service) ListResources(w http.ResponseWriter, r *http.Request) {
	user, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "未登录")
		return
	}
	resources, err := VisibleResources(s.db, user, role)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "读取资源失败")
		return
	}
	out := make([]DataResourceOut, 0, len(resources))
	for _, res := range resources {
		mode, _ := UserAccessMode(s.db, user, role, res.ID)
		out = append(out, toOut(res, mode))
	}
	writeOK(w, map[string]any{"resources": out})
}

// CreateResource 处理 POST /api/data-resources（仅 admin）。
func (s *Service) CreateResource(w http.ResponseWriter, r *http.Request) {
	user, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "未登录")
		return
	}
	if role != "admin" {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "仅管理员可创建数据资源")
		return
	}
	var input DataResourceInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	if err := ValidateInput(input); err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	if input.Password == "" {
		writeErr(w, http.StatusBadRequest, "PASSWORD_REQUIRED", "创建资源时密码不能为空")
		return
	}
	cipher, err := s.credKey.Encrypt(input.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ENCRYPT_ERROR", "凭据加密失败")
		return
	}
	now := time.Now().UnixMilli()
	res := DataResource{
		ID: newID(), Name: strings.TrimSpace(input.Name), DBType: input.DBType,
		Host: input.Host, Port: input.Port, DatabaseName: input.DatabaseName,
		DefaultSchema: input.DefaultSchema, Username: input.Username,
		CredentialCipher: cipher, SSLMode: input.SSLMode,
		CreatedBy: user, CreatedAt: now, UpdatedAt: now,
	}
	if err := CreateDataResource(s.db, res); err != nil {
		if isUniqueConstraint(err) {
			writeErr(w, http.StatusConflict, "NAME_DUPLICATE", "资源名称已存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "创建资源失败")
		return
	}
	writeOK(w, toOut(res, "admin"))
}

// GetResource 处理 GET /api/data-resources/{id}。
func (s *Service) GetResource(w http.ResponseWriter, r *http.Request) {
	user, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "未登录")
		return
	}
	id := r.PathValue("id")
	res, found, err := GetDataResource(s.db, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "读取资源失败")
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "资源不存在")
		return
	}
	mode, _ := UserAccessMode(s.db, user, role, id)
	if mode == "" {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "无权访问该资源")
		return
	}
	writeOK(w, toOut(res, mode))
}

// UpdateResource 处理 PUT /api/data-resources/{id}（仅 admin）。
func (s *Service) UpdateResource(w http.ResponseWriter, r *http.Request) {
	_, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "未登录")
		return
	}
	if role != "admin" {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "仅管理员可编辑数据资源")
		return
	}
	id := r.PathValue("id")
	_, found, err := GetDataResource(s.db, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "读取资源失败")
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "资源不存在")
		return
	}
	var input DataResourceInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	if err := ValidateInput(input); err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	// 空密码表示保留原密码
	var cipher string
	if input.Password != "" {
		cipher, err = s.credKey.Encrypt(input.Password)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "ENCRYPT_ERROR", "凭据加密失败")
			return
		}
	}
	if err := UpdateDataResource(s.db, id, input, cipher); err != nil {
		if isUniqueConstraint(err) {
			writeErr(w, http.StatusConflict, "NAME_DUPLICATE", "资源名称已存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "更新资源失败")
		return
	}
	res, _, _ := GetDataResource(s.db, id)
	writeOK(w, toOut(res, "admin"))
}

// DeleteResource 处理 DELETE /api/data-resources/{id}（仅 admin）。
// 要求请求体中 name 与资源名称匹配（防误删）。
func (s *Service) DeleteResource(w http.ResponseWriter, r *http.Request) {
	_, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "未登录")
		return
	}
	if role != "admin" {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "仅管理员可删除数据资源")
		return
	}
	id := r.PathValue("id")
	res, found, err := GetDataResource(s.db, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "读取资源失败")
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "资源不存在")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	if strings.TrimSpace(body.Name) != res.Name {
		writeErr(w, http.StatusBadRequest, "NAME_MISMATCH", "输入的资源名称不匹配,无法删除")
		return
	}
	if err := DeleteDataResource(s.db, id); err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "删除资源失败")
		return
	}
	writeOK(w, map[string]bool{"ok": true})
}

// TestConnectionHandler 处理 POST /api/data-resources/test（仅 admin，用请求体中的配置测试）。
func (s *Service) TestConnectionHandler(w http.ResponseWriter, r *http.Request) {
	_, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "未登录")
		return
	}
	if role != "admin" {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "仅管理员可测试连接")
		return
	}
	var input DataResourceInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	if err := ValidateInput(input); err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	if input.Password == "" {
		writeErr(w, http.StatusBadRequest, "PASSWORD_REQUIRED", "测试连接时密码不能为空")
		return
	}
	res := DataResource{
		DBType: input.DBType, Host: input.Host, Port: input.Port,
		DatabaseName: input.DatabaseName, Username: input.Username, SSLMode: input.SSLMode,
	}
	result := TestConnection(res, input.Password)
	if !result.OK {
		writeOK(w, map[string]any{
			"ok":        false,
			"latencyMs": result.LatencyMs,
			"errorCode": result.ErrorCode,
			"error":     ErrorDescription(result.ErrorCode),
		})
		return
	}
	writeOK(w, result)
}

// TestExistingConnection 处理 POST /api/data-resources/{id}/test（仅 admin，用已保存的配置测试）。
func (s *Service) TestExistingConnection(w http.ResponseWriter, r *http.Request) {
	_, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "未登录")
		return
	}
	if role != "admin" {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "仅管理员可测试连接")
		return
	}
	id := r.PathValue("id")
	res, found, err := GetDataResource(s.db, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "读取资源失败")
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "资源不存在")
		return
	}
	password, err := s.credKey.Decrypt(res.CredentialCipher)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DECRYPT_ERROR", "凭据解密失败")
		return
	}
	result := TestConnection(res, password)
	UpdateTestStatus(s.db, id, boolToStatus(result.OK))
	if !result.OK {
		writeOK(w, map[string]any{
			"ok":        false,
			"latencyMs": result.LatencyMs,
			"errorCode": result.ErrorCode,
			"error":     ErrorDescription(result.ErrorCode),
		})
		return
	}
	writeOK(w, result)
}

func boolToStatus(ok bool) string {
	if ok {
		return "ok"
	}
	return "fail"
}

// isUniqueConstraint 检查是否为 SQLite UNIQUE 约束冲突。
func isUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || errors.Is(err, sql.ErrNoRows)
}
