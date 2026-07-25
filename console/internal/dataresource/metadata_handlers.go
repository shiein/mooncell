// 元数据 API handlers：元数据树、表结构、DDL、SQL 模板。
//
// 设计文档第三节「元数据」：
//   GET  /api/data-resources/{id}/metadata/children?parentId=
//   GET  /api/data-resources/{id}/metadata/structure?nodeId=
//   GET  /api/data-resources/{id}/metadata/ddl?nodeId=
//   POST /api/data-resources/{id}/metadata/sql-template
//
// nodeId 视为不可信输入，服务端解码后仍重新校验资源、类型和标识符。
package dataresource

import (
	"encoding/json"
	"net/http"
)

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
	if err != nil || !found {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "资源不存在")
		return nil, "", false
	}
	mode, _ := UserAccessMode(s.db, user, role, id)
	if mode == "" {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "无权访问该资源")
		return nil, "", false
	}
	// 获取或创建连接池
	db, err := s.pools.GetDB(res)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "POOL_ERROR", "获取数据库连接失败")
		return nil, "", false
	}
	adapter, err := NewAdapter(db, res.DBType, BoundSchema(res))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ADAPTER_ERROR", "创建适配器失败")
		return nil, "", false
	}
	return adapter, mode, true
}

// MetadataChildren 处理 GET /api/data-resources/{id}/metadata/children?parentId=
func (s *Service) MetadataChildren(w http.ResponseWriter, r *http.Request) {
	adapter, _, ok := s.getAdapterForRequest(w, r)
	if !ok {
		return
	}
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
	children, err := adapter.Children(r.Context(), parent)
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
	adapter, _, ok := s.getAdapterForRequest(w, r)
	if !ok {
		return
	}
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
	structure, err := adapter.Describe(r.Context(), node)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "METADATA_ERROR", sanitizeErrMsg(err.Error()))
		return
	}
	writeOK(w, structure)
}

// MetadataDDL 处理 GET /api/data-resources/{id}/metadata/ddl?nodeId=
func (s *Service) MetadataDDL(w http.ResponseWriter, r *http.Request) {
	adapter, _, ok := s.getAdapterForRequest(w, r)
	if !ok {
		return
	}
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
	ddl, err := adapter.DDL(r.Context(), node)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DDL_ERROR", sanitizeErrMsg(err.Error()))
		return
	}
	writeOK(w, map[string]string{"ddl": ddl})
}

// MetadataSQLTemplate 处理 POST /api/data-resources/{id}/metadata/sql-template
func (s *Service) MetadataSQLTemplate(w http.ResponseWriter, r *http.Request) {
	adapter, mode, ok := s.getAdapterForRequest(w, r)
	if !ok {
		return
	}
	var body struct {
		NodeID    string `json:"nodeId"`
		Operation string `json:"operation"` // SELECT/INSERT/UPDATE/DELETE
	}
	if err := jsonDecodeBody(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
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

// jsonBodyMaxBytes JSON 请求体上限（含导出快照 rows、行编辑数组等）。
// 与 consoleapp 上传路径一致：传输层截断，避免无界内存占用。
const jsonBodyMaxBytes = 16 << 20 // 16MB

// jsonDecodeBody 解码 JSON 请求体（套 MaxBytesReader）。
func jsonDecodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, jsonBodyMaxBytes)
	dec := json.NewDecoder(r.Body)
	return dec.Decode(v)
}
