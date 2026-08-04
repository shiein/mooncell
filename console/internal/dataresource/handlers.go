// HTTP handlers：数据资源管理 API。
//
// 设计文档第三节「公共 API」：
//
//	GET    /api/data-resources
//	POST   /api/data-resources
//	GET    /api/data-resources/{id}
//	PUT    /api/data-resources/{id}
//	DELETE /api/data-resources/{id}
//	POST   /api/data-resources/test
//	POST   /api/data-resources/{id}/test
//
// 统一错误结构：
//
//	{"error": "用户可读错误", "code": "STABLE_ERROR_CODE", "txState": "none|active|failed"}
package dataresource

import (
	"crypto/rand"
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
	writeErrTx(w, status, code, msg, "none")
}

// writeErrTx 写出错误并附带工作台事务状态（取消/执行失败时前端需同步，不得固定 none）。
func writeErrTx(w http.ResponseWriter, status int, code, msg, txState string) {
	if txState == "" {
		txState = "none"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(APIError{Message: msg, Code: code, TxState: txState})
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

// toOut 将内部 DataResource 转为对外形态。密码从不持久化，hasPassword 固定为 false。
func toOut(r DataResource, accessMode string) DataResourceOut {
	return DataResourceOut{
		DataResource: r,
		HasPassword:  false,
		AccessMode:   accessMode,
		LastTestInfo: formatTestInfo(r.LastTestStatus, r.LastTestAt),
	}
}

func formatTestInfo(status string, at int64) string {
	if status == "" || at == 0 {
		return ""
	}
	t := time.UnixMilli(at).Format("2006-01-02 15:04")
	switch status {
	case TestStatusOK:
		return "成功 · " + t
	case TestStatusOKNoRO:
		return "成功(只读事务不可用) · " + t
	default:
		return "失败 · " + t
	}
}

// --- 资源 CRUD ---

// ListDrivers 处理 GET /api/data-resources/drivers：返回可选驱动及实验性标记。
func (s *Service) ListDrivers(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := userFromCtx(r); !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "未登录")
		return
	}
	writeOK(w, map[string]any{"drivers": DriverCatalog()})
}

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
		mode, err := UserAccessMode(s.db, user, role, res.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "DB_ERROR", "读取资源授权失败")
			return
		}
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
	if err := ValidateInput(&input); err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	now := time.Now().UnixMilli()
	res := DataResource{
		ID: newID(), Name: input.Name, DBType: input.DBType,
		Host: input.Host, Port: input.Port, DatabaseName: input.DatabaseName,
		DefaultSchema: input.DefaultSchema, Username: input.Username,
		CredentialCipher: "", SSLMode: input.SSLMode,
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
	s.auditLog(user, "创建数据资源", res.Name, "成功")
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
	mode, err := UserAccessMode(s.db, user, role, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "读取资源授权失败")
		return
	}
	if mode == "" {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "无权访问该资源")
		return
	}
	writeOK(w, toOut(res, mode))
}

// UpdateResource 处理 PUT /api/data-resources/{id}（仅 admin）。
func (s *Service) UpdateResource(w http.ResponseWriter, r *http.Request) {
	user, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "未登录")
		return
	}
	if role != "admin" {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "仅管理员可编辑数据资源")
		return
	}
	id := r.PathValue("id")
	prev, found, err := GetDataResource(s.db, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "读取资源失败")
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "资源不存在")
		return
	}
	// 与导入/手工事务互斥：同一锁下占用 exclusive，避免 TOCTOU
	if !s.pools.TryBeginExclusive(id) {
		writeErr(w, http.StatusConflict, "RESOURCE_BUSY", "资源正在执行 SQL、导入或存在活动事务，请稍后再更新")
		return
	}
	defer s.pools.EndExclusive(id)

	var input DataResourceInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	if err := ValidateInput(&input); err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	poolChanged := poolAffectingChanged(prev, input)
	if err := UpdateDataResource(s.db, id, input, prev); err != nil {
		if isUniqueConstraint(err) {
			writeErr(w, http.StatusConflict, "NAME_DUPLICATE", "资源名称已存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "更新资源失败")
		return
	}
	// defaultSchema（含达梦 DSN）变化也必须关池；仅 auth 变化才清测试态/重测
	if poolChanged {
		s.InvalidateResource(id)
		s.pools.CloseDB(id)
	}
	res, found, err := GetDataResource(s.db, id)
	if err != nil || !found {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "更新后读取资源失败")
		return
	}
	s.auditLog(user, "更新数据资源", res.Name, "成功")
	writeOK(w, toOut(res, "admin"))
}

// DeleteResource 处理 DELETE /api/data-resources/{id}（仅 admin）。
// 要求请求体中 name 与资源名称匹配（防误删）。
func (s *Service) DeleteResource(w http.ResponseWriter, r *http.Request) {
	user, role, ok := userFromCtx(r)
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
	// 与导入/手工事务互斥
	if !s.pools.TryBeginExclusive(id) {
		writeErr(w, http.StatusConflict, "RESOURCE_BUSY", "资源正在执行 SQL、导入或存在活动事务，请稍后再删除")
		return
	}
	defer s.pools.EndExclusive(id)

	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	// 名称唯一索引按 LOWER(name)；删除确认用大小写不敏感比较
	if !strings.EqualFold(strings.TrimSpace(body.Name), res.Name) {
		writeErr(w, http.StatusBadRequest, "NAME_MISMATCH", "输入的资源名称不匹配,无法删除")
		return
	}
	if err := DeleteDataResource(s.db, id); err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "删除资源失败")
		return
	}
	// 失效工作台并关闭连接池
	s.InvalidateResource(id)
	s.pools.CloseDB(id)
	s.auditLog(user, "删除数据资源", res.Name, "成功")
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
	if err := ValidateInput(&input); err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	if input.Password == "" {
		writeErr(w, http.StatusBadRequest, "PASSWORD_REQUIRED", "测试连接时密码不能为空")
		return
	}
	res := DataResource{
		DBType: input.DBType, Host: input.Host, Port: input.Port,
		DatabaseName: input.DatabaseName, DefaultSchema: input.DefaultSchema,
		Username: input.Username, SSLMode: input.SSLMode,
	}
	result := TestConnection(res, input.Password)
	writeOK(w, testResultResponse(result))
}

// TestResourceDraftHandler 处理 POST /api/data-resources/{id}/test-draft（仅 admin）。
// 用请求体中的连接字段测试；密码必须由用户本次输入，且不保存。
// 用于编辑对话框：改了主机/库等但未改密码时，必须测「新配置 + 旧密码」，不能测整条旧记录。
// 不写 last_test_status（草稿测试不污染已保存资源的测试状态）。
func (s *Service) TestResourceDraftHandler(w http.ResponseWriter, r *http.Request) {
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
	if err := ValidateInput(&input); err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	if input.Password == "" {
		writeErr(w, http.StatusBadRequest, "PASSWORD_REQUIRED", "测试连接时密码不能为空")
		return
	}
	res := DataResource{
		DBType: input.DBType, Host: input.Host, Port: input.Port,
		DatabaseName: input.DatabaseName, DefaultSchema: input.DefaultSchema,
		Username: input.Username, SSLMode: input.SSLMode,
	}
	result := TestConnection(res, input.Password)
	writeOK(w, testResultResponse(result))
}

// TestExistingConnection 处理 POST /api/data-resources/{id}/test（仅 admin，用本次输入密码测试）。
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
	// 快照配置版本：测试可能长达 10s，写回时用 CAS 防止旧结果覆盖新配置
	configVersion := res.UpdatedAt
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Password == "" {
		writeErr(w, http.StatusBadRequest, "PASSWORD_REQUIRED", "测试连接时密码不能为空")
		return
	}
	result := TestConnection(res, body.Password)
	status := result.PersistStatus()
	revoked, err := UpdateTestStatusAndRevokeRead(s.db, id, status, configVersion)
	if err != nil {
		if errors.Is(err, ErrConfigChanged) {
			writeErr(w, http.StatusConflict, "CONFIG_CHANGED", "测试期间资源配置已变更，请使用新配置重新测试")
			return
		}
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "保存连接测试结果失败")
		return
	}
	for _, username := range revoked {
		s.InvalidateUserResource(username, id)
	}
	user, _, _ := userFromCtx(r)
	s.auditLog(user, "测试连接", res.Name, status)
	writeOK(w, testResultResponse(result))
}

// testResultResponse 统一连接测试响应：失败时带可读 error（含脱敏驱动细节）。
func testResultResponse(result TestResult) any {
	if result.OK {
		return result
	}
	errMsg := result.Error
	if errMsg == "" {
		errMsg = ErrorDescription(result.ErrorCode)
	}
	return map[string]any{
		"ok":        false,
		"latencyMs": result.LatencyMs,
		"errorCode": result.ErrorCode,
		"error":     errMsg,
	}
}

// isUniqueConstraint 检查是否为 SQLite UNIQUE 约束冲突。
// 注意：sql.ErrNoRows 表示未命中，不是唯一约束冲突，不得并入本判断。
func isUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
