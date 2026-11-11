package etl

// 定义导入/导出（P2.15）：单定义 JSON 包，连接与被引定义按名称重映射，
// 缺失项由用户在导入预览中选择目标或保留原引用（草稿态，校验会提示）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"mooncell/console/internal/etl/store"
)

// exportFormat/exportVersion 是导出包标识；未知格式或更高版本一律拒绝导入。
const (
	exportFormat  = "mooncell-etl-export"
	exportVersion = 1
)

type exportDoc struct {
	Format     string     `json:"format"`
	Version    int        `json:"version"`
	Definition exportDef  `json:"definition"`
	References exportRefs `json:"references"`
}

type exportDef struct {
	Kind        string          `json:"kind"`
	Name        string          `json:"name"`
	Folder      string          `json:"folder"`
	Description string          `json:"description"`
	Doc         json.RawMessage `json:"doc"`
}

type exportRefs struct {
	Connections []exportConn `json:"connections"`
	Definitions []exportRef  `json:"definitions"`
}

type exportConn struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	DBType string `json:"dbType"`
}

type exportRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// docRefs 扫描 doc.nodes[].config 的引用字段（connection / pipeline / workflow），
// 返回去重后的连接 ID 与定义 ID（保持出现顺序）。
func docRefs(doc json.RawMessage) (connIDs, defIDs []string) {
	var d struct {
		Nodes []struct {
			Config map[string]json.RawMessage `json:"config"`
		} `json:"nodes"`
	}
	_ = json.Unmarshal(doc, &d)
	seen := map[string]bool{}
	str := func(raw json.RawMessage) string {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	for _, n := range d.Nodes {
		if id := str(n.Config["connection"]); id != "" && !seen["c:"+id] {
			seen["c:"+id] = true
			connIDs = append(connIDs, id)
		}
		for _, k := range []string{"pipeline", "workflow"} {
			if id := str(n.Config[k]); id != "" && !seen["d:"+id] {
				seen["d:"+id] = true
				defIDs = append(defIDs, id)
			}
		}
	}
	return connIDs, defIDs
}

// stripExportSecrets 移除 notify 凭据密文：密文 AAD 绑定原定义 ID，
// 导出后无论到哪里都无法解密，带着只会造成「静默失效」的错觉。
// 返回处理后的文档与是否有改动。
func stripExportSecrets(doc json.RawMessage) (json.RawMessage, bool) {
	var d map[string]any
	if err := json.Unmarshal(doc, &d); err != nil {
		return doc, false
	}
	nodes, _ := d["nodes"].([]any)
	changed := false
	for _, raw := range nodes {
		n, _ := raw.(map[string]any)
		cfg, _ := n["config"].(map[string]any)
		for _, k := range []string{"secretCipher", "aadSuffix", "secret"} {
			if _, ok := cfg[k]; ok {
				delete(cfg, k)
				changed = true
			}
		}
	}
	if !changed {
		return doc, false
	}
	b, err := json.Marshal(d)
	if err != nil {
		return doc, false
	}
	return b, true
}

// parseExport 校验导出包格式/版本并返回定义载荷。
func parseExport(raw json.RawMessage) (exportDoc, error) {
	var ex exportDoc
	if err := json.Unmarshal(raw, &ex); err != nil {
		return ex, errors.New("文件不是合法 JSON")
	}
	if ex.Format != exportFormat {
		return ex, errors.New("不是数据集成导出文件（format 标识缺失或不匹配）")
	}
	if ex.Version != exportVersion {
		return ex, fmt.Errorf("导出文件版本 v%d 与本系统支持的 v%d 不一致", ex.Version, exportVersion)
	}
	if ex.Definition.Kind != "pipeline" && ex.Definition.Kind != "workflow" {
		return ex, fmt.Errorf("未知定义类型 %q", ex.Definition.Kind)
	}
	if err := checkDoc(ex.Definition.Kind, ex.Definition.Doc); err != nil {
		return ex, fmt.Errorf("文档校验失败：%w", err)
	}
	return ex, nil
}

// remapDoc 按映射表替换 doc.nodes[].config 中的引用值；未映射项保留原值（草稿态）。
func remapDoc(doc json.RawMessage, connMap, defMap map[string]string) (json.RawMessage, error) {
	var d map[string]any
	if err := json.Unmarshal(doc, &d); err != nil {
		return nil, err
	}
	nodes, _ := d["nodes"].([]any)
	for _, raw := range nodes {
		n, _ := raw.(map[string]any)
		cfg, _ := n["config"].(map[string]any)
		if cfg == nil {
			continue
		}
		if v, ok := cfg["connection"].(string); ok {
			if nv, mapped := connMap[v]; mapped {
				cfg["connection"] = nv
			}
		}
		for _, k := range []string{"pipeline", "workflow"} {
			if v, ok := cfg[k].(string); ok {
				if nv, mapped := defMap[v]; mapped {
					cfg[k] = nv
				}
			}
		}
	}
	return json.Marshal(d)
}

// exportDefinition 导出单个定义为 JSON 包（GET /api/etl/definitions/{id}/export）。
func (h *handlers) exportDefinition(w http.ResponseWriter, r *http.Request) {
	if !h.requireDef(w, r, r.PathValue("id"), store.ModeUse) {
		return
	}
	d, err := h.s.st.GetDefinition(r.PathValue("id"))
	if err != nil {
		failErr(w, err)
		return
	}
	connIDs, defIDs := docRefs(d.Doc)
	conns, err := h.s.st.ListConnections()
	if err != nil {
		failErr(w, err)
		return
	}
	connByID := map[string]store.Connection{}
	for _, c := range conns {
		connByID[c.ID] = c
	}
	doc, stripped := stripExportSecrets(d.Doc)
	out := exportDoc{
		Format:  exportFormat,
		Version: exportVersion,
		Definition: exportDef{Kind: d.Kind, Name: d.Name, Folder: d.Folder,
			Description: d.Description, Doc: doc},
	}
	for _, id := range connIDs {
		if c, ok := connByID[id]; ok {
			out.References.Connections = append(out.References.Connections, exportConn{ID: id, Name: c.Name, DBType: c.DBType})
		} else {
			out.References.Connections = append(out.References.Connections, exportConn{ID: id})
		}
	}
	for _, id := range defIDs {
		if rd, err := h.s.st.GetDefinition(id); err == nil {
			out.References.Definitions = append(out.References.Definitions, exportRef{ID: id, Name: rd.Name, Kind: rd.Kind})
		} else {
			out.References.Definitions = append(out.References.Definitions, exportRef{ID: id})
		}
	}
	if stripped {
		h.s.audit(userOf(r), "导出"+kindLabel(d.Kind), "数据集成·"+d.Name, "成功（通知凭据未随包迁移）")
	} else {
		h.s.audit(userOf(r), "导出"+kindLabel(d.Kind), "数据集成·"+d.Name, "成功")
	}
	writeJSON(w, http.StatusOK, out)
}

// importMatch 是按名称匹配到的现存目标；nil 表示缺失需用户选择。
type importMatch struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// importItem 是导入预览中一条待映射引用。
type importItem struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Kind    string       `json:"kind,omitempty"`   // 仅 definitions
	DBType  string       `json:"dbType,omitempty"` // 仅 connections
	Matched *importMatch `json:"matched"`
}

// importPreview 解析导出包并报告引用匹配结果（不创建任何记录）。
func (h *handlers) importPreview(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var in struct {
		Document json.RawMessage `json:"document"`
	}
	if err := decode(r, &in); err != nil || len(in.Document) == 0 {
		failErr(w, errors.New("缺少导入文件内容"))
		return
	}
	ex, err := parseExport(in.Document)
	if err != nil {
		failErr(w, err)
		return
	}
	p, err := h.analyzeImport(ex)
	if err != nil {
		failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// analyzeImport 计算导出包在当前库的引用匹配与目标名称。
func (h *handlers) analyzeImport(ex exportDoc) (map[string]any, error) {
	conns, err := h.s.st.ListConnections()
	if err != nil {
		return nil, err
	}
	defs, err := h.s.st.ListDefinitions("")
	if err != nil {
		return nil, err
	}
	connByName := map[string]store.Connection{}
	for _, c := range conns {
		connByName[strings.ToLower(c.Name)] = c
	}
	defByKey := map[string]store.DefinitionSummary{}
	for _, d := range defs {
		defByKey[d.Kind+"/"+strings.ToLower(d.Name)] = d
	}
	connItems := []importItem{}
	for _, c := range ex.References.Connections {
		it := importItem{ID: c.ID, Name: c.Name, DBType: c.DBType}
		if c.Name != "" {
			if m, ok := connByName[strings.ToLower(c.Name)]; ok {
				it.Matched = &importMatch{ID: m.ID, Name: m.Name}
			}
		}
		connItems = append(connItems, it)
	}
	defItems := []importItem{}
	for _, dr := range ex.References.Definitions {
		it := importItem{ID: dr.ID, Name: dr.Name, Kind: dr.Kind}
		if dr.Name != "" {
			if m, ok := defByKey[dr.Kind+"/"+strings.ToLower(dr.Name)]; ok {
				it.Matched = &importMatch{ID: m.ID, Name: m.Name}
			}
		}
		defItems = append(defItems, it)
	}
	warnings := []string{}
	for _, it := range defItems {
		if it.Matched == nil && it.Name == "" {
			warnings = append(warnings, fmt.Sprintf("引用的定义 %s 在导出时已失效，导入后需手动修复", it.ID))
		}
	}
	for _, it := range connItems {
		if it.Matched == nil && it.Name == "" {
			warnings = append(warnings, fmt.Sprintf("引用的连接 %s 在导出时已失效，导入后需手动修复", it.ID))
		}
	}
	return map[string]any{
		"kind":        ex.Definition.Kind,
		"name":        ex.Definition.Name,
		"finalName":   importName(defs, ex.Definition.Kind, ex.Definition.Name),
		"folder":      ex.Definition.Folder,
		"description": ex.Definition.Description,
		"connections": connItems,
		"definitions": defItems,
		"warnings":    warnings,
	}, nil
}

// importName 在同类定义中找可用名：冲突时追加「 (导入)」「 (导入 N)」。
func importName(existing []store.DefinitionSummary, kind, name string) string {
	taken := map[string]bool{}
	for _, d := range existing {
		if d.Kind == kind {
			taken[strings.ToLower(d.Name)] = true
		}
	}
	if !taken[strings.ToLower(name)] {
		return name
	}
	for i := 1; i <= 50; i++ {
		cand := fmt.Sprintf("%s (导入)", name)
		if i > 1 {
			cand = fmt.Sprintf("%s (导入 %d)", name, i)
		}
		if !taken[strings.ToLower(cand)] {
			return cand
		}
	}
	return name // 50 个后缀都用尽——交给存储层唯一约束报错
}

// importDefinition 按用户确认的连接/定义映射创建定义（POST /api/etl/definitions/import）。
func (h *handlers) importDefinition(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var in struct {
		Document      json.RawMessage   `json:"document"`
		ConnectionMap map[string]string `json:"connectionMap"`
		DefinitionMap map[string]string `json:"definitionMap"`
	}
	if err := decode(r, &in); err != nil || len(in.Document) == 0 {
		failErr(w, errors.New("缺少导入文件内容"))
		return
	}
	ex, err := parseExport(in.Document)
	if err != nil {
		failErr(w, err)
		return
	}
	// 映射目标校验 + 构建有效映射：未显式指定的引用按名自动匹配（显式优先）。
	conns, err := h.s.st.ListConnections()
	if err != nil {
		failErr(w, err)
		return
	}
	connByID := map[string]store.Connection{}
	connByName := map[string]store.Connection{}
	for _, c := range conns {
		connByID[c.ID] = c
		connByName[strings.ToLower(c.Name)] = c
	}
	connMap := map[string]string{}
	for _, c := range ex.References.Connections {
		if c.Name != "" {
			if m, ok := connByName[strings.ToLower(c.Name)]; ok {
				connMap[c.ID] = m.ID
			}
		}
	}
	for _, to := range in.ConnectionMap {
		if _, ok := connByID[to]; !ok {
			failErr(w, fmt.Errorf("映射的目标连接不存在：%s", to))
			return
		}
	}
	for k, v := range in.ConnectionMap {
		connMap[k] = v
	}
	defs, err := h.s.st.ListDefinitions("")
	if err != nil {
		failErr(w, err)
		return
	}
	defByID := map[string]bool{}
	defByKey := map[string]string{}
	for _, d := range defs {
		defByID[d.ID] = true
		defByKey[d.Kind+"/"+strings.ToLower(d.Name)] = d.ID
	}
	defMap := map[string]string{}
	for _, dr := range ex.References.Definitions {
		if dr.Name != "" {
			if id, ok := defByKey[dr.Kind+"/"+strings.ToLower(dr.Name)]; ok {
				defMap[dr.ID] = id
			}
		}
	}
	for _, to := range in.DefinitionMap {
		if !defByID[to] {
			failErr(w, fmt.Errorf("映射的目标定义不存在：%s", to))
			return
		}
	}
	for k, v := range in.DefinitionMap {
		defMap[k] = v
	}
	doc, err := remapDoc(ex.Definition.Doc, connMap, defMap)
	if err != nil {
		failErr(w, fmt.Errorf("引用重映射失败：%w", err))
		return
	}
	if err := checkDoc(ex.Definition.Kind, doc); err != nil {
		failErr(w, fmt.Errorf("文档校验失败：%w", err))
		return
	}
	kindDefs, err := h.s.st.ListDefinitions(ex.Definition.Kind)
	if err != nil {
		failErr(w, err)
		return
	}
	newID := store.NewID(map[string]string{"pipeline": "pl_", "workflow": "wf_"}[ex.Definition.Kind])
	doc, err = h.sealDocSecrets(newID, ex.Definition.Kind, doc)
	if err != nil {
		failErr(w, err)
		return
	}
	d, err := h.s.st.CreateDefinition(store.Definition{
		ID: newID, Kind: ex.Definition.Kind,
		Name:        importName(kindDefs, ex.Definition.Kind, ex.Definition.Name),
		Folder:      ex.Definition.Folder,
		Description: ex.Definition.Description,
		Doc:         doc,
	}, userOf(r))
	if err != nil {
		failErr(w, err)
		return
	}
	h.s.audit(userOf(r), "导入"+kindLabel(d.Kind), "数据集成·"+d.Name, "成功")
	writeJSON(w, http.StatusOK, d)
}
