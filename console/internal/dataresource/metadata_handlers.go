// 元数据 API handlers：元数据树、表结构、DDL、SQL 模板。
//
// 设计文档第三节「元数据」：
//
//	GET  /api/data-resources/{id}/metadata/children?parentId=
//	GET  /api/data-resources/{id}/metadata/structure?nodeId=
//	GET  /api/data-resources/{id}/metadata/ddl?nodeId=
//	POST /api/data-resources/{id}/metadata/sql-template
//
// nodeId 视为不可信输入，服务端解码后仍重新校验资源、类型和标识符。
package dataresource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// ResourceCapabilities 处理 GET /api/data-resources/{id}/capabilities
// 供前端关闭不支持的菜单（DDL/导入等），避免空结果伪装成功。
func (s *Service) ResourceCapabilities(w http.ResponseWriter, r *http.Request) {
	adapter, _, release, ok := s.getAdapterForOperation(w, r)
	if !ok {
		return
	}
	defer release()
	writeOK(w, adapter.Capabilities())
}

// getAdapterForRequest 从请求中获取资源 ID，创建适配器。
// 复用权限校验逻辑：admin 全放行，普通用户须有授权。
func (s *Service) getAdapterForRequest(w http.ResponseWriter, r *http.Request) (DataSourceAdapter, string, bool) {
	user, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "未登录")
		return nil, "", false
	}
	id := r.PathValue("id")
	res, found, err := GetDataResource(s.db, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", "读取资源失败")
		return nil, "", false
	}
	if !found {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "资源不存在")
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
	loginSessionID, ok := loginSessionFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "登录会话无效")
		return nil, "", false
	}
	// 仅使用当前 Mooncell 登录会话持有的内存连接租约。
	db, err := s.pools.GetSessionDB(res.ID, loginSessionID)
	if err != nil {
		writeErr(w, http.StatusPreconditionRequired, "PASSWORD_REQUIRED", "请先输入密码连接数据库")
		return nil, "", false
	}
	adapter, err := NewAdapter(db, res.DBType, BoundSchema(res))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ADAPTER_ERROR", "创建适配器失败")
		return nil, "", false
	}
	return adapter, mode, true
}

// getAdapterForOperation 在适配器使用期间持有普通资源操作槽，
// 防止配置更新/删除关闭正在使用的连接池。
func (s *Service) getAdapterForOperation(w http.ResponseWriter, r *http.Request) (DataSourceAdapter, string, func(), bool) {
	id := r.PathValue("id")
	if !s.pools.TryBeginOperation(id) {
		writeErr(w, http.StatusConflict, "RESOURCE_BUSY", "资源正在导入或更新，请稍后再试")
		return nil, "", nil, false
	}
	adapter, mode, ok := s.getAdapterForRequest(w, r)
	if !ok {
		s.pools.EndOperation(id)
		return nil, "", nil, false
	}
	return adapter, mode, func() { s.pools.EndOperation(id) }, true
}

// MetadataChildren 处理 GET /api/data-resources/{id}/metadata/children?parentId=
func (s *Service) MetadataChildren(w http.ResponseWriter, r *http.Request) {
	adapter, _, release, ok := s.getAdapterForOperation(w, r)
	if !ok {
		return
	}
	defer release()
	parentID := r.URL.Query().Get("parentId")
	var parent MetadataNode
	if parentID == "" {
		parent = MetadataNode{Kind: NodeRoot}
	} else {
		var found bool
		parent, found = DecodeID(parentID)
		if !found {
			writeErr(w, http.StatusBadRequest, "BAD_NODE_ID", "无效的节点 ID")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), MetadataTimeout)
	defer cancel()
	children, err := adapter.Children(ctx, parent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "METADATA_ERROR", sanitizeErrMsg(err.Error()))
		return
	}
	if children == nil {
		children = []MetadataNode{}
	}
	writeOK(w, map[string]any{"children": children})
}

// MetadataStructure 处理 GET /api/data-resources/{id}/metadata/structure?nodeId=
func (s *Service) MetadataStructure(w http.ResponseWriter, r *http.Request) {
	adapter, _, release, ok := s.getAdapterForOperation(w, r)
	if !ok {
		return
	}
	defer release()
	nodeID := r.URL.Query().Get("nodeId")
	if nodeID == "" {
		writeErr(w, http.StatusBadRequest, "BAD_NODE_ID", "缺少 nodeId")
		return
	}
	node, found := DecodeID(nodeID)
	if !found {
		writeErr(w, http.StatusBadRequest, "BAD_NODE_ID", "无效的节点 ID")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), MetadataTimeout)
	defer cancel()
	structure, err := adapter.Describe(ctx, node)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "METADATA_ERROR", sanitizeErrMsg(err.Error()))
		return
	}
	writeOK(w, structure)
}

// MetadataDDL 处理 GET /api/data-resources/{id}/metadata/ddl?nodeId=
func (s *Service) MetadataDDL(w http.ResponseWriter, r *http.Request) {
	adapter, _, release, ok := s.getAdapterForOperation(w, r)
	if !ok {
		return
	}
	defer release()
	nodeID := r.URL.Query().Get("nodeId")
	if nodeID == "" {
		writeErr(w, http.StatusBadRequest, "BAD_NODE_ID", "缺少 nodeId")
		return
	}
	node, found := DecodeID(nodeID)
	if !found {
		writeErr(w, http.StatusBadRequest, "BAD_NODE_ID", "无效的节点 ID")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), MetadataTimeout)
	defer cancel()
	ddl, err := adapter.DDL(ctx, node)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DDL_ERROR", sanitizeErrMsg(err.Error()))
		return
	}
	writeOK(w, map[string]string{"ddl": ddl})
}

// MetadataSQLTemplate 处理 POST /api/data-resources/{id}/metadata/sql-template
func (s *Service) MetadataSQLTemplate(w http.ResponseWriter, r *http.Request) {
	adapter, mode, release, ok := s.getAdapterForOperation(w, r)
	if !ok {
		return
	}
	defer release()
	var body struct {
		NodeID    string `json:"nodeId"`
		Operation string `json:"operation"` // SELECT/INSERT/UPDATE/DELETE
	}
	if err := jsonDecodeBody(w, r, &body); err != nil {
		writeJSONBodyError(w, err)
		return
	}
	node, found := DecodeID(body.NodeID)
	if !found {
		writeErr(w, http.StatusBadRequest, "BAD_NODE_ID", "无效的节点 ID")
		return
	}
	// INSERT/UPDATE/DELETE 仅 write 可用
	if body.Operation != "SELECT" && mode != "admin" && mode != AccessWrite {
		writeErr(w, http.StatusForbidden, "READ_ONLY", "只读授权不可生成写操作模板")
		return
	}
	tpl, err := adapter.SQLTemplate(node, body.Operation)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "TEMPLATE_ERROR", sanitizeErrMsg(err.Error()))
		return
	}
	writeOK(w, map[string]string{"sql": tpl})
}

// jsonBodyMaxBytes JSON 请求体上限（行编辑等）。
// 与 consoleapp 上传路径一致：传输层截断，避免无界内存占用。
const jsonBodyMaxBytes = 16 << 20 // 16MB

// ErrRequestBodyTooLarge 请求体超过 MaxBytesReader 上限。
var ErrRequestBodyTooLarge = errors.New("REQUEST_BODY_TOO_LARGE")

// jsonDecodeBody 解码 JSON 请求体（套 MaxBytesReader）。
// 超限返回 ErrRequestBodyTooLarge，调用方应回 413。
func jsonDecodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, jsonBodyMaxBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return ErrRequestBodyTooLarge
		}
		return err
	}
	return nil
}

// writeJSONBodyError 将 jsonDecodeBody 错误写成 HTTP 响应；返回 true 表示已写出。
func writeJSONBodyError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrRequestBodyTooLarge) {
		writeErr(w, http.StatusRequestEntityTooLarge, "REQUEST_BODY_TOO_LARGE",
			fmt.Sprintf("请求体过大，上限 %d MB", jsonBodyMaxBytes>>20))
		return true
	}
	writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
	return true
}
