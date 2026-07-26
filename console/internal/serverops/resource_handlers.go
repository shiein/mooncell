// 资源列表与 admin CRUD、host key 探测/确认。
package serverops

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"
)

// ListResources GET /api/server-resources
func (s *Service) ListResources(w http.ResponseWriter, r *http.Request) {
	user, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "未登录", false)
		return
	}
	resources, err := VisibleResources(s.db, user, role)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeDBError, "读取服务器资源失败", true)
		return
	}
	out := make([]ResourceOut, 0, len(resources))
	for _, res := range resources {
		mode, err := AccessModeFor(s.db, user, role, res.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, CodeDBError, "读取授权失败", true)
			return
		}
		out = append(out, toResourceOut(res, mode))
	}
	// 附带 cleanup_pending 计数（admin 可见，便于运维）。
	resp := map[string]any{"resources": out}
	if isAdmin(role) {
		if n, err := countCleanupPending(s.db); err == nil {
			resp["cleanupPending"] = n
		}
	}
	writeOK(w, resp)
}

// GetResource GET /api/server-resources/{id}
func (s *Service) GetResource(w http.ResponseWriter, r *http.Request) {
	user, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "未登录", false)
		return
	}
	id := r.PathValue("id")
	res, err := RequireAccess(s.db, user, role, id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	mode, _ := AccessModeFor(s.db, user, role, id)
	writeOK(w, toResourceOut(res, mode))
}

// CreateResource POST /api/server-resources（admin）
func (s *Service) CreateResource(w http.ResponseWriter, r *http.Request) {
	user, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "未登录", false)
		return
	}
	if !isAdmin(role) {
		writeErr(w, http.StatusForbidden, CodeForbidden, "仅管理员可创建服务器资源", false)
		return
	}
	var in ResourceInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "请求格式错误", false)
		return
	}
	if err := validateResourceInput(&in); err != nil {
		writeAPIError(w, err)
		return
	}
	now := time.Now().UnixMilli()
	res := ServerResource{
		ID:        newID("srv"),
		Name:      in.Name,
		Host:      in.Host,
		Port:      in.Port,
		Username:  in.Username,
		CreatedBy: user,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := createResource(s.db, res); err != nil {
		if ae, ok := err.(*APIError); ok {
			writeAPIError(w, ae)
			return
		}
		writeErr(w, http.StatusInternalServerError, CodeDBError, "创建资源失败", true)
		return
	}
	s.auditLog(user, "创建服务器资源", res.Name, "成功")
	writeOK(w, toResourceOut(res, AccessAdmin))
}

// UpdateResource PUT /api/server-resources/{id}（admin）
// 请求体可带 updatedAt 做 CAS；host/port/username 变更会清空 host key 并失效会话。
func (s *Service) UpdateResource(w http.ResponseWriter, r *http.Request) {
	user, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "未登录", false)
		return
	}
	if !isAdmin(role) {
		writeErr(w, http.StatusForbidden, CodeForbidden, "仅管理员可修改服务器资源", false)
		return
	}
	id := r.PathValue("id")
	var body struct {
		ResourceInput
		UpdatedAt int64 `json:"updatedAt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "请求格式错误", false)
		return
	}
	if err := validateResourceInput(&body.ResourceInput); err != nil {
		writeAPIError(w, err)
		return
	}
	cur, found, err := getResource(s.db, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeDBError, "读取资源失败", true)
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, CodeNotFound, "服务器资源不存在", false)
		return
	}
	expected := body.UpdatedAt
	if expected == 0 {
		expected = cur.UpdatedAt
	}
	connChanged := cur.Host != body.Host || cur.Port != body.Port || cur.Username != body.Username
	now := time.Now().UnixMilli()
	if err := updateResource(s.db, id, body.ResourceInput, connChanged, expected, now); err != nil {
		writeAPIError(w, err)
		return
	}
	// 连接参数或名称变更均递增代际，终止旧会话（host key 清空后必须重新连接）。
	if connChanged || cur.Name != body.Name {
		s.InvalidateResource(id)
	}
	s.auditLog(user, "更新服务器资源", body.Name, "成功")
	updated, _, _ := getResource(s.db, id)
	writeOK(w, toResourceOut(updated, AccessAdmin))
}

// DeleteResource DELETE /api/server-resources/{id}（admin）
func (s *Service) DeleteResource(w http.ResponseWriter, r *http.Request) {
	user, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "未登录", false)
		return
	}
	if !isAdmin(role) {
		writeErr(w, http.StatusForbidden, CodeForbidden, "仅管理员可删除服务器资源", false)
		return
	}
	id := r.PathValue("id")
	cur, found, err := getResource(s.db, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeDBError, "读取资源失败", true)
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, CodeNotFound, "服务器资源不存在", false)
		return
	}
	s.InvalidateResource(id)
	if err := deleteResource(s.db, id); err != nil {
		writeErr(w, http.StatusInternalServerError, CodeDBError, "删除资源失败", true)
		return
	}
	s.auditLog(user, "删除服务器资源", cur.Name, "成功")
	writeOK(w, map[string]bool{"ok": true})
}

// ProbeHostKey POST /api/server-resources/host-key/probe（admin）
// 探测草稿指纹，不写库。
func (s *Service) ProbeHostKey(w http.ResponseWriter, r *http.Request) {
	_, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "未登录", false)
		return
	}
	if !isAdmin(role) {
		writeErr(w, http.StatusForbidden, CodeForbidden, "仅管理员可探测主机指纹", false)
		return
	}
	var body struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "请求格式错误", false)
		return
	}
	body.Host = trimSpace(body.Host)
	if body.Port == 0 {
		body.Port = 22
	}
	if err := validateHost(body.Host); err != nil {
		writeAPIError(w, err)
		return
	}
	result, err := ProbeHostKey(body.Host, body.Port, s.cfg.connectTimeout())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	user, _, _ := userFromCtx(r)
	s.auditLog(user, "探测主机指纹", body.Host, "成功")
	writeOK(w, result)
}

// ConfirmHostKey POST /api/server-resources/{id}/host-key/confirm（admin）
// CAS：必须带 updatedAt，避免把旧地址指纹写到新地址。
func (s *Service) ConfirmHostKey(w http.ResponseWriter, r *http.Request) {
	user, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "未登录", false)
		return
	}
	if !isAdmin(role) {
		writeErr(w, http.StatusForbidden, CodeForbidden, "仅管理员可确认主机指纹", false)
		return
	}
	id := r.PathValue("id")
	var body struct {
		Algorithm string `json:"algorithm"`
		SHA256    string `json:"sha256"`
		UpdatedAt int64  `json:"updatedAt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "请求格式错误", false)
		return
	}
	if body.SHA256 == "" || body.Algorithm == "" {
		writeErr(w, http.StatusBadRequest, CodeValidation, "算法与指纹不能为空", false)
		return
	}
	cur, found, err := getResource(s.db, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeDBError, "读取资源失败", true)
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, CodeNotFound, "服务器资源不存在", false)
		return
	}
	expected := body.UpdatedAt
	if expected == 0 {
		expected = cur.UpdatedAt
	}
	now := time.Now().UnixMilli()
	if err := confirmHostKey(s.db, id, body.Algorithm, body.SHA256, expected, now); err != nil {
		writeAPIError(w, err)
		return
	}
	// 指纹变更终止旧会话。
	s.InvalidateResource(id)
	s.auditLog(user, "确认主机指纹", cur.Name, "成功")
	updated, _, _ := getResource(s.db, id)
	writeOK(w, toResourceOut(updated, AccessAdmin))
}

func newID(prefix string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

func trimSpace(s string) string {
	// 避免 handlers 额外依赖 strings 过多；用简单实现。
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 {
		c := s[len(s)-1]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}
