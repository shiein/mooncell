package etl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"mooncell/console/internal/etl/actions"
	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/expr"
	"mooncell/console/internal/etl/pipeline"
	"mooncell/console/internal/etl/schedule"
	"mooncell/console/internal/etl/store"
	"mooncell/console/internal/etl/workflow"
)

type handlers struct{ s *Service }

const maxDocBytes = 4 << 20

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type apiError struct {
	Error    string        `json:"error"`
	Code     string        `json:"code,omitempty"`
	Problems core.Problems `json:"problems,omitempty"`
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiError{Error: msg, Code: code})
}

func failErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		fail(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, store.ErrConflict):
		fail(w, http.StatusConflict, "conflict", err.Error())
	default:
		fail(w, http.StatusBadRequest, "bad_request", err.Error())
	}
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxDocBytes))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("请求格式错误：%w", err)
	}
	return nil
}

// ---------------------------------------------------------------- 定义级授权（P3.16）

// modeRank 把授权模式映射为可比较的级别（"" < use < edit < admin）。
func modeRank(m string) int {
	switch m {
	case store.ModeAdmin:
		return 3
	case store.ModeEdit:
		return 2
	case store.ModeUse:
		return 1
	}
	return 0
}

func modeLabel(m string) string {
	if m == store.ModeEdit {
		return "编辑"
	}
	return "使用"
}

// defMode 返回请求者对 defID 的有效模式。
func (h *handlers) defMode(r *http.Request, defID string) string {
	if isAdmin(r) {
		return store.ModeAdmin
	}
	m, err := h.s.st.AccessMode(userOf(r), defID)
	if err != nil {
		return ""
	}
	return m
}

// requireDef 校验请求者对 defID 至少持有 min 模式；不满足写 403 并返回 false。
func (h *handlers) requireDef(w http.ResponseWriter, r *http.Request, defID, min string) bool {
	if modeRank(h.defMode(r, defID)) >= modeRank(min) {
		return true
	}
	fail(w, http.StatusForbidden, "forbidden", "无权访问该定义（需要"+modeLabel(min)+"授权）")
	return false
}

// requireAdmin 仅限管理员（连接 CRUD/测试、新建/导入定义、调度目标之外的模块级写）。
func (h *handlers) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if isAdmin(r) {
		return true
	}
	fail(w, http.StatusForbidden, "forbidden", "该操作需要管理员权限")
	return false
}

// grantScope 返回非管理员的授权映射 definition_id→mode；admin 返回 nil（不过滤）。
func (h *handlers) grantScope(r *http.Request) map[string]string {
	if isAdmin(r) {
		return nil
	}
	m, err := h.s.st.GrantedModes(userOf(r))
	if err != nil {
		return map[string]string{}
	}
	return m
}

// ---------------------------------------------------------------- 概览与目录

func (h *handlers) overview(w http.ResponseWriter, r *http.Request) {
	defs, err := h.s.st.ListDefinitions("")
	if err != nil {
		failErr(w, err)
		return
	}
	scope := h.grantScope(r)
	if scope != nil {
		granted := defs[:0]
		for _, d := range defs {
			if _, ok := scope[d.ID]; ok {
				granted = append(granted, d)
			}
		}
		defs = granted
	}
	scheds, _ := h.s.st.ListSchedules("")
	conns, _ := h.s.st.ListConnections()
	since := time.Now().Add(-24 * time.Hour).UnixMilli()
	recentFilter := store.RunFilter{Limit: 200}
	if scope != nil {
		recentFilter.GrantedUser = userOf(r) // 授权过滤进 SQL、先于 LIMIT（R1-M3）
	}
	recent, _ := h.s.st.ListRuns(recentFilter)
	byStatus := map[string]int{}
	var rows int64
	var recent24 []store.Run
	for _, run := range recent {
		if run.QueuedAt >= since {
			byStatus[run.Status]++
			rows += run.RowsWritten
			recent24 = append(recent24, run)
		}
	}
	enabled := 0
	var upcoming []store.Schedule
	for _, sc := range scheds {
		if scope != nil {
			if _, ok := scope[sc.TargetID]; !ok {
				continue
			}
		}
		if sc.Enabled {
			enabled++
			upcoming = append(upcoming, sc)
		}
	}
	pipelines, workflows := 0, 0
	for _, d := range defs {
		if d.Kind == "pipeline" {
			pipelines++
		} else {
			workflows++
		}
	}
	// 在途运行同样按授权过滤（R1-m4）：非管理员只见授权定义的活动运行
	active := []string{}
	for _, a := range h.s.runs.activeSummaries() {
		if scope != nil {
			if _, ok := scope[a.DefinitionID]; !ok {
				continue
			}
		}
		active = append(active, a.DefinitionName)
	}
	sort.Strings(active)
	connCount := len(conns)
	if scope != nil {
		connCount = 0 // 连接清单是管理员面，非管理员不暴露数量
	}
	if len(recent24) > 12 {
		recent24 = recent24[:12]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pipelines": pipelines, "workflows": workflows, "connections": connCount, "schedulesEnabled": enabled,
		"runs24h": byStatus, "rowsWritten24h": rows, "active": active, "recentRuns": recent24, "upcoming": upcoming,
	})
}

func (h *handlers) catalog(w http.ResponseWriter, r *http.Request) {
	type item struct {
		core.Meta
		Defaults json.RawMessage `json:"defaults"`
	}
	var steps, acts []item
	for _, m := range core.Steps.Metas() {
		t, _ := core.Steps.Get(m.ID)
		steps = append(steps, item{Meta: m, Defaults: t.DefaultConfig()})
	}
	for _, m := range workflow.Actions.Metas() {
		t, _ := workflow.Actions.Get(m.ID)
		acts = append(acts, item{Meta: m, Defaults: t.DefaultConfig()})
	}
	kinds := []map[string]string{}
	for _, k := range core.AllKinds() {
		kinds = append(kinds, map[string]string{"id": string(k), "label": k.Label()})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"steps": steps, "actions": acts, "functions": expr.Catalog(), "functionCategories": expr.Categories(),
		"dialects": conn.Catalog(), "kinds": kinds,
	})
}

// ---------------------------------------------------------------- 连接

type connInput struct {
	conn.Config
	Description string  `json:"description"`
	Password    *string `json:"password"` // nil / 空串 = 不修改（编辑时）
}

func (in connInput) validate() error {
	if strings.TrimSpace(in.Name) == "" {
		return errors.New("请填写连接名称")
	}
	d, ok := conn.Get(in.DBType)
	if !ok || d.Internal() {
		return fmt.Errorf("不支持的数据库类型 %q", in.DBType)
	}
	if strings.TrimSpace(in.Host) == "" {
		return errors.New("请填写主机地址")
	}
	if in.Port <= 0 || in.Port > 65535 {
		return errors.New("端口无效")
	}
	if strings.TrimSpace(in.Username) == "" {
		return errors.New("请填写用户名")
	}
	if in.SSLMode != "" && in.SSLMode != "disable" && in.SSLMode != "require" {
		return errors.New("SSL 模式无效")
	}
	if in.Options.TimeZone != "" {
		if _, err := time.LoadLocation(in.Options.TimeZone); err != nil {
			return errors.New("时区无效")
		}
	}
	return nil
}

func (h *handlers) listConnections(w http.ResponseWriter, r *http.Request) {
	list, err := h.s.st.ListConnections()
	if err != nil {
		failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connections": list})
}

func (h *handlers) getConnection(w http.ResponseWriter, r *http.Request) {
	c, err := h.s.st.GetConnection(r.PathValue("id"))
	if err != nil {
		failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *handlers) saveConnection(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var in connInput
	if err := decode(r, &in); err != nil {
		failErr(w, err)
		return
	}
	id := r.PathValue("id")
	in.ID = id
	if err := in.validate(); err != nil {
		failErr(w, err)
		return
	}
	if in.SSLMode == "" {
		in.SSLMode = "disable"
	}
	var cipher *string
	if in.Password != nil && *in.Password != "" {
		if id == "" { // 新建：先分配 ID，密文 AAD 绑定它
			in.ID = store.NewID("conn_")
		}
		ct, err := h.s.box.Seal(*in.Password, in.ID)
		if err != nil {
			failErr(w, err)
			return
		}
		cipher = &ct
	}
	rec := store.Connection{Config: in.Config, Description: in.Description}
	var saved store.Connection
	var err error
	if id == "" {
		saved, err = h.s.st.CreateConnectionWithID(rec, cipher, userOf(r))
	} else {
		saved, err = h.s.st.SaveConnection(rec, cipher, userOf(r))
	}
	if err != nil {
		failErr(w, err)
		return
	}
	h.s.pool.Invalidate(saved.ID)
	conn.ForgetProbes()
	action := "新建连接"
	if id != "" {
		action = "修改连接"
	}
	h.s.audit(userOf(r), action, "数据集成·"+saved.Name, "成功")
	writeJSON(w, http.StatusOK, saved)
}

func (h *handlers) deleteConnection(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	c, err := h.s.st.GetConnection(id)
	if err != nil {
		failErr(w, err)
		return
	}
	if uses, _ := h.s.st.References(id); len(uses) > 0 && r.URL.Query().Get("force") != "1" {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "连接仍被引用：" + strings.Join(uses, "、"), "code": "in_use", "usages": uses})
		return
	}
	if err := h.s.st.DeleteConnection(id); err != nil {
		failErr(w, err)
		return
	}
	h.s.pool.Invalidate(id)
	h.s.audit(userOf(r), "删除连接", "数据集成·"+c.Name, "成功")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *handlers) testDraftConnection(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var in connInput
	if err := decode(r, &in); err != nil {
		failErr(w, err)
		return
	}
	if err := in.validate(); err != nil {
		failErr(w, err)
		return
	}
	pw := ""
	if in.Password != nil {
		pw = *in.Password
	}
	if pw == "" && in.ID != "" { // 编辑态未改密码：用已保存的
		if _, saved, err := h.s.resolveConnection(r.Context(), in.ID); err == nil {
			pw = saved
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, conn.Test(ctx, in.Config, pw))
}

func (h *handlers) testConnection(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	cfg, pw, err := h.s.resolveConnection(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusOK, conn.TestResult{Error: err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	res := conn.Test(ctx, cfg, pw)
	msg := res.Version
	if !res.OK {
		msg = res.Error
	}
	_ = h.s.st.SetConnectionTest(id, res.OK, msg)
	writeJSON(w, http.StatusOK, res)
}

func (h *handlers) handle(w http.ResponseWriter, r *http.Request) (*conn.Handle, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	hd, err := h.s.pool.Get(ctx, r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusBadGateway, "connection", err.Error())
		return nil, false
	}
	return hd, true
}

func (h *handlers) listSchemas(w http.ResponseWriter, r *http.Request) {
	hd, ok := h.handle(w, r)
	if !ok {
		return
	}
	list, err := hd.Dialect.ListSchemas(r.Context(), hd.DB, hd.Config)
	if err != nil {
		fail(w, http.StatusBadGateway, "metadata", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schemas": list, "default": conn.EffectiveSchema(hd.Config, "")})
}

func (h *handlers) listTables(w http.ResponseWriter, r *http.Request) {
	hd, ok := h.handle(w, r)
	if !ok {
		return
	}
	schema := conn.EffectiveSchema(hd.Config, r.URL.Query().Get("schema"))
	list, err := hd.Dialect.ListTables(r.Context(), hd.DB, schema)
	if err != nil {
		fail(w, http.StatusBadGateway, "metadata", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema": schema, "tables": list})
}

func (h *handlers) listColumns(w http.ResponseWriter, r *http.Request) {
	hd, ok := h.handle(w, r)
	if !ok {
		return
	}
	t := conn.TableRef{Schema: conn.EffectiveSchema(hd.Config, r.URL.Query().Get("schema")), Name: r.URL.Query().Get("table")}
	cols, err := hd.Dialect.Columns(r.Context(), hd.DB, t)
	if err != nil {
		fail(w, http.StatusBadGateway, "metadata", err.Error())
		return
	}
	type colOut struct {
		conn.ColumnMeta
		Kind core.Kind `json:"kind"`
	}
	out := make([]colOut, len(cols))
	for i, c := range cols {
		out[i] = colOut{ColumnMeta: c, Kind: hd.Dialect.FieldFor(c).Kind}
	}
	idx, _ := hd.Dialect.Indexes(r.Context(), hd.DB, t)
	writeJSON(w, http.StatusOK, map[string]any{"table": t, "columns": out, "indexes": idx})
}

// ---------------------------------------------------------------- 定义

func emptyDoc(kind string) json.RawMessage {
	if kind == "workflow" {
		b, _ := json.Marshal(workflow.Definition{Format: workflow.FormatVersion, Kind: "workflow", Params: []core.ParamDecl{},
			Nodes: []workflow.Node{{ID: "start", Type: "start", Name: "开始", Version: 1, Config: json.RawMessage(`{}`), UI: json.RawMessage(`{"x":80,"y":160}`)}},
			Hops:  []workflow.Hop{}})
		return b
	}
	b, _ := json.Marshal(pipeline.Definition{Format: pipeline.FormatVersion, Kind: "pipeline", Params: []core.ParamDecl{}, Nodes: []pipeline.Node{}, Hops: []pipeline.Hop{}})
	return b
}

// checkDoc 只做格式校验（允许保存有问题的草稿）。
func checkDoc(kind string, doc json.RawMessage) error {
	if len(doc) > maxDocBytes {
		return errors.New("定义过大")
	}
	var err error
	switch kind {
	case "pipeline":
		_, err = pipeline.Decode(doc)
	case "workflow":
		_, err = workflow.Decode(doc)
	default:
		err = fmt.Errorf("未知类型 %q", kind)
	}
	return err
}

type defInput struct {
	Kind        string          `json:"kind"`
	Name        string          `json:"name"`
	Folder      string          `json:"folder"`
	Description string          `json:"description"`
	Doc         json.RawMessage `json:"doc"`
	Revision    int             `json:"revision"`
}

// sealDocSecrets 把作业文档中 notify 作业项的明文凭据密封为密文再入库。
// AAD = actions.NotifySecretAADPrefix + defID + "/" + nodeID，并把该后缀写回配置（运行侧开密用）。
func (h *handlers) sealDocSecrets(defID, kind string, doc json.RawMessage) (json.RawMessage, error) {
	if kind != "workflow" || h.s.box == nil {
		return doc, nil
	}
	def, err := workflow.Decode(doc)
	if err != nil {
		return doc, nil // checkDoc 已校验过格式；这里防御性放行
	}
	changed := false
	for i := range def.Nodes {
		n := &def.Nodes[i]
		if n.Type != "notify" {
			continue
		}
		var cfg map[string]json.RawMessage
		if err := json.Unmarshal(n.Config, &cfg); err != nil {
			continue
		}
		var secret string
		rawSecret, submitted := cfg["secret"]
		_ = json.Unmarshal(rawSecret, &secret)
		delete(cfg, "secret")
		dirty := false
		if secret != "" {
			ct, err := h.s.box.Seal(secret, actions.NotifySecretAADPrefix+defID+"/"+n.ID)
			if err != nil {
				return nil, fmt.Errorf("密封通知凭据失败：%w", err)
			}
			cfg["secretCipher"], _ = json.Marshal(ct)
			dirty = true
		} else if submitted {
			// 显式提交空凭据 = 清除
			delete(cfg, "secretCipher")
			delete(cfg, "aadSuffix")
			dirty = true
		}
		if _, ok := cfg["secretCipher"]; ok {
			sfx, _ := json.Marshal(defID + "/" + n.ID)
			if string(cfg["aadSuffix"]) != string(sfx) {
				cfg["aadSuffix"] = sfx
				dirty = true
			}
		}
		if dirty {
			n.Config, _ = json.Marshal(cfg)
			changed = true
		}
	}
	if !changed {
		return doc, nil
	}
	return json.Marshal(def)
}

// rekeyDocSecrets 复制场景：把 doc 中按源定义 ID 密封的 notify 凭据解密后
// 按新定义 ID 重密封，aadSuffix 同步指向新 ID。无密钥箱（box 为空）时剥离
// 凭据字段——与导出包行为一致，由用户在新定义里重新录入。
func (h *handlers) rekeyDocSecrets(oldDefID, newDefID, kind string, doc json.RawMessage) (json.RawMessage, error) {
	if kind != "workflow" {
		return doc, nil
	}
	def, err := workflow.Decode(doc)
	if err != nil {
		return doc, nil
	}
	changed := false
	for i := range def.Nodes {
		n := &def.Nodes[i]
		if n.Type != "notify" {
			continue
		}
		var cfg map[string]json.RawMessage
		if err := json.Unmarshal(n.Config, &cfg); err != nil {
			continue
		}
		ct, ok := cfg["secretCipher"]
		if !ok {
			continue
		}
		if h.s.box == nil {
			delete(cfg, "secretCipher")
			delete(cfg, "aadSuffix")
			n.Config, _ = json.Marshal(cfg)
			changed = true
			continue
		}
		var sealed string
		if err := json.Unmarshal(ct, &sealed); err != nil || sealed == "" {
			continue
		}
		// 开密 AAD 优先取文档记录的 aadSuffix（sealDocSecrets 写入），缺省按源定义推导
		var suffix string
		_ = json.Unmarshal(cfg["aadSuffix"], &suffix)
		if suffix == "" {
			suffix = oldDefID + "/" + n.ID
		}
		plain, err := h.s.box.Open(sealed, actions.NotifySecretAADPrefix+suffix)
		if err != nil {
			return nil, fmt.Errorf("复制通知凭据失败：%w", err)
		}
		newCT, err := h.s.box.Seal(plain, actions.NotifySecretAADPrefix+newDefID+"/"+n.ID)
		if err != nil {
			return nil, fmt.Errorf("重密封通知凭据失败：%w", err)
		}
		cfg["secretCipher"], _ = json.Marshal(newCT)
		cfg["aadSuffix"], _ = json.Marshal(newDefID + "/" + n.ID)
		n.Config, _ = json.Marshal(cfg)
		changed = true
	}
	if !changed {
		return doc, nil
	}
	return json.Marshal(def)
}

func (h *handlers) listDefinitions(w http.ResponseWriter, r *http.Request) {
	list, err := h.s.st.ListDefinitions(r.URL.Query().Get("kind"))
	if err != nil {
		failErr(w, err)
		return
	}
	if scope := h.grantScope(r); scope != nil {
		out := make([]store.DefinitionSummary, 0, len(list))
		for _, d := range list {
			if mode, ok := scope[d.ID]; ok {
				d.Access = mode
				out = append(out, d)
			}
		}
		list = out
	} else {
		for i := range list {
			list[i].Access = store.ModeAdmin
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"definitions": list})
}

func (h *handlers) getDefinition(w http.ResponseWriter, r *http.Request) {
	if !h.requireDef(w, r, r.PathValue("id"), store.ModeUse) {
		return
	}
	d, err := h.s.st.GetDefinition(r.PathValue("id"))
	if err != nil {
		failErr(w, err)
		return
	}
	d.Access = h.defMode(r, d.ID)
	writeJSON(w, http.StatusOK, d)
}

func (h *handlers) createDefinition(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var in defInput
	if err := decode(r, &in); err != nil {
		failErr(w, err)
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		failErr(w, errors.New("请填写名称"))
		return
	}
	if len(in.Doc) == 0 {
		in.Doc = emptyDoc(in.Kind)
	}
	if err := checkDoc(in.Kind, in.Doc); err != nil {
		failErr(w, err)
		return
	}
	// 预生成 ID：notify 凭据密封的 AAD 依赖 defID
	newID := store.NewID(map[string]string{"pipeline": "pl_", "workflow": "wf_"}[in.Kind])
	doc, err := h.sealDocSecrets(newID, in.Kind, in.Doc)
	if err != nil {
		failErr(w, err)
		return
	}
	d, err := h.s.st.CreateDefinition(store.Definition{ID: newID, Kind: in.Kind, Name: strings.TrimSpace(in.Name), Folder: in.Folder, Description: in.Description, Doc: doc}, userOf(r))
	if err != nil {
		failErr(w, err)
		return
	}
	h.s.audit(userOf(r), "新建"+kindLabel(d.Kind), "数据集成·"+d.Name, "成功")
	writeJSON(w, http.StatusOK, d)
}

func (h *handlers) updateDefinition(w http.ResponseWriter, r *http.Request) {
	if !h.requireDef(w, r, r.PathValue("id"), store.ModeEdit) {
		return
	}
	var in defInput
	if err := decode(r, &in); err != nil {
		failErr(w, err)
		return
	}
	cur, err := h.s.st.GetDefinition(r.PathValue("id"))
	if err != nil {
		failErr(w, err)
		return
	}
	if in.Doc == nil {
		in.Doc = cur.Doc
	}
	if in.Name == "" {
		in.Name = cur.Name
	}
	if err := checkDoc(cur.Kind, in.Doc); err != nil {
		failErr(w, err)
		return
	}
	// 非管理员保存作业：引用的转换/子作业须至少持 use（R1-m4），
	// 否则可借保存把未授权定义拉进自己的作业执行。
	if cur.Kind == "workflow" && !isAdmin(r) {
		if missing := h.ungrantedRefs(r, in.Doc); len(missing) > 0 {
			fail(w, http.StatusForbidden, "forbidden", "作业引用了未授权定义："+strings.Join(missing, "、"))
			return
		}
	}
	doc, err := h.sealDocSecrets(cur.ID, cur.Kind, in.Doc)
	if err != nil {
		failErr(w, err)
		return
	}
	d, err := h.s.st.UpdateDefinition(store.Definition{ID: cur.ID, Kind: cur.Kind, Name: strings.TrimSpace(in.Name), Folder: in.Folder, Description: in.Description, Doc: doc}, in.Revision, userOf(r))
	if err != nil {
		failErr(w, err)
		return
	}
	h.s.audit(userOf(r), "修改"+kindLabel(d.Kind), "数据集成·"+d.Name, "成功")
	writeJSON(w, http.StatusOK, d)
}

// ungrantedRefs 返回作业文档中引用的、当前用户不持任何授权的定义名。
// 解码失败返回 nil（checkDoc 已拦下格式问题，此处尽力而为）。
func (h *handlers) ungrantedRefs(r *http.Request, doc json.RawMessage) []string {
	def, err := workflow.Decode(doc)
	if err != nil {
		return nil
	}
	var missing []string
	for _, ref := range workflow.CollectRefs(def) {
		if modeRank(h.defMode(r, ref.ID)) < modeRank(store.ModeUse) {
			name := ref.ID
			if d, err := h.s.st.GetDefinition(ref.ID); err == nil {
				name = d.Name
			}
			missing = append(missing, name)
		}
	}
	return missing
}

func (h *handlers) deleteDefinition(w http.ResponseWriter, r *http.Request) {
	if !h.requireDef(w, r, r.PathValue("id"), store.ModeEdit) {
		return
	}
	id := r.PathValue("id")
	d, err := h.s.st.GetDefinition(id)
	if err != nil {
		failErr(w, err)
		return
	}
	if uses, _ := h.s.st.References(id); len(uses) > 0 && r.URL.Query().Get("force") != "1" {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "仍被作业引用：" + strings.Join(uses, "、"), "code": "in_use", "usages": uses})
		return
	}
	// force 删除会连带影响全部引用方：非管理员须对每个引用方都持 edit（R1-m4）
	if r.URL.Query().Get("force") == "1" && !isAdmin(r) {
		if refIDs, err := h.s.st.ReferenceIDs(id); err == nil {
			for _, refID := range refIDs {
				if modeRank(h.defMode(r, refID)) < modeRank(store.ModeEdit) {
					fail(w, http.StatusForbidden, "forbidden", "该定义仍被引用，需对全部引用方持有编辑授权")
					return
				}
			}
		}
	}
	if err := h.s.st.DeleteDefinition(id); err != nil {
		failErr(w, err)
		return
	}
	h.s.audit(userOf(r), "删除"+kindLabel(d.Kind), "数据集成·"+d.Name, "成功")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *handlers) duplicateDefinition(w http.ResponseWriter, r *http.Request) {
	if !h.requireDef(w, r, r.PathValue("id"), store.ModeEdit) {
		return
	}
	d, err := h.s.st.GetDefinition(r.PathValue("id"))
	if err != nil {
		failErr(w, err)
		return
	}
	// 先分配副本 ID：notify 凭据密文须按新 ID 重加密（AAD 绑定定义 ID）
	newID := store.NewID(map[string]string{"pipeline": "pl_", "workflow": "wf_"}[d.Kind])
	doc, err := h.rekeyDocSecrets(d.ID, newID, d.Kind, d.Doc)
	if err != nil {
		failErr(w, err)
		return
	}
	var cp store.Definition
	for i := 1; i <= 50; i++ {
		name := fmt.Sprintf("%s 副本", d.Name)
		if i > 1 {
			name = fmt.Sprintf("%s 副本 %d", d.Name, i)
		}
		cp, err = h.s.st.CreateDefinition(store.Definition{ID: newID, Kind: d.Kind, Name: name, Folder: d.Folder, Description: d.Description, Doc: doc}, userOf(r))
		if err == nil || !strings.Contains(err.Error(), "同名") {
			break
		}
	}
	if err != nil {
		failErr(w, err)
		return
	}
	// 非管理员复制出的副本自动归属本人（edit），否则复制完立刻无权查看
	if !isAdmin(r) {
		if err := h.s.st.Grant(userOf(r), cp.ID, store.ModeEdit, userOf(r)); err != nil {
			failErr(w, err)
			return
		}
		cp.Access = store.ModeEdit
	}
	h.s.audit(userOf(r), "复制"+kindLabel(cp.Kind), "数据集成·"+cp.Name, "成功")
	writeJSON(w, http.StatusOK, cp)
}

func (h *handlers) listVersions(w http.ResponseWriter, r *http.Request) {
	if !h.requireDef(w, r, r.PathValue("id"), store.ModeUse) {
		return
	}
	list, err := h.s.st.ListVersions(r.PathValue("id"))
	if err != nil {
		failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": list})
}

func (h *handlers) getVersion(w http.ResponseWriter, r *http.Request) {
	if !h.requireDef(w, r, r.PathValue("id"), store.ModeUse) {
		return
	}
	rev, _ := strconv.Atoi(r.PathValue("rev"))
	v, err := h.s.st.GetVersion(r.PathValue("id"), rev)
	if err != nil {
		failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *handlers) restoreVersion(w http.ResponseWriter, r *http.Request) {
	if !h.requireDef(w, r, r.PathValue("id"), store.ModeEdit) {
		return
	}
	rev, _ := strconv.Atoi(r.PathValue("rev"))
	cur, err := h.s.st.GetDefinition(r.PathValue("id"))
	if err != nil {
		failErr(w, err)
		return
	}
	v, err := h.s.st.GetVersion(cur.ID, rev)
	if err != nil {
		failErr(w, err)
		return
	}
	// 与普通保存同一授权口径（R1-m4）：旧版本可能引用了操作者已无 use 授权的定义，
	// 恢复即等于把它们重新拉进作业。
	if cur.Kind == "workflow" && !isAdmin(r) {
		if missing := h.ungrantedRefs(r, v.Doc); len(missing) > 0 {
			fail(w, http.StatusForbidden, "forbidden", "该版本引用了未授权定义："+strings.Join(missing, "、"))
			return
		}
	}
	cur.Doc = v.Doc
	d, err := h.s.st.UpdateDefinition(cur, cur.Revision, userOf(r))
	if err != nil {
		failErr(w, err)
		return
	}
	h.s.audit(userOf(r), "恢复版本", fmt.Sprintf("数据集成·%s（r%d）", d.Name, rev), "成功")
	writeJSON(w, http.StatusOK, d)
}

// validate 编译定义：问题 + 每条连线的字段 + 可见变量（设计器实时调用）。
func (h *handlers) validate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind string          `json:"kind"`
		Doc  json.RawMessage `json:"doc"`
	}
	if err := decode(r, &in); err != nil {
		failErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	switch in.Kind {
	case "pipeline":
		def, err := pipeline.Decode(in.Doc)
		if err != nil {
			failErr(w, err)
			return
		}
		plan := pipeline.Compile(ctx, def, pipeline.CompileOptions{Env: h.s.env(nil)})
		hops := map[string][]core.Field{}
		for id, s := range plan.HopSchemas {
			hops[id] = s.Fields()
		}
		nodes := map[string]any{}
		for id, pn := range plan.ByID {
			ins, outs := map[string][]core.Field{}, map[string][]core.Field{}
			for p, s := range pn.Inputs {
				ins[p] = s.Fields()
			}
			for p, s := range pn.Outputs {
				outs[p] = s.Fields()
			}
			nodes[id] = map[string]any{"inputs": ins, "outputs": outs, "blocked": pn.Blocked}
		}
		writeJSON(w, http.StatusOK, map[string]any{"problems": nonNil(plan.Problems), "hops": hops, "nodes": nodes, "vars": plan.VarKinds})
	case "workflow":
		def, err := workflow.Decode(in.Doc)
		if err != nil {
			failErr(w, err)
			return
		}
		host := &pipelineHost{s: h.s}
		plan := workflow.Compile(ctx, def, workflow.CompileOptions{Env: h.s.env(host)})
		writeJSON(w, http.StatusOK, map[string]any{"problems": nonNil(plan.Problems), "vars": plan.VarKinds})
	default:
		failErr(w, fmt.Errorf("未知类型 %q", in.Kind))
	}
}

func nonNil(p core.Problems) core.Problems {
	if p == nil {
		return core.Problems{}
	}
	return p
}

func (h *handlers) preview(w http.ResponseWriter, r *http.Request) {
	// 预览按未落库文档真实执行读操作，等价于任意连接的数据访问：
	// 非管理员须持有任一「编辑」授权（可修改定义者即视为数据集成开发者）。
	if !isAdmin(r) {
		ok, err := h.s.st.HasAnyGrant(userOf(r), store.ModeEdit)
		if err != nil {
			failErr(w, err)
			return
		}
		if !ok {
			fail(w, http.StatusForbidden, "forbidden", "数据预览需要任一定义的编辑授权")
			return
		}
	}
	var in struct {
		Doc    json.RawMessage       `json:"doc"`
		NodeID string                `json:"nodeId"`
		Limit  int                   `json:"limit"`
		Params map[string]core.Value `json:"params"`
	}
	if err := decode(r, &in); err != nil {
		failErr(w, err)
		return
	}
	def, err := pipeline.Decode(in.Doc)
	if err != nil {
		failErr(w, err)
		return
	}
	vars := core.NewVars(nil)
	vars.Set("RUN_ID", core.NewValue("preview"))
	vars.Set("RUN_START", core.NewValue(time.Now()))
	vars.Set("TRIGGER", core.NewValue("preview"))
	if err := applyParams(vars, def.Params, in.Params); err != nil {
		failErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	e := h.s.env(nil)
	res := pipeline.Preview(ctx, def, in.NodeID, in.Limit, pipeline.CompileOptions{Env: e}, pipeline.RunOptions{Env: e, Vars: vars, TempRoot: h.s.cfg.TempDir})
	writeJSON(w, http.StatusOK, res)
}

func (h *handlers) runDefinition(w http.ResponseWriter, r *http.Request) {
	if !h.requireDef(w, r, r.PathValue("id"), store.ModeUse) {
		return
	}
	var in struct {
		Params map[string]core.Value `json:"params"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &in); err != nil {
			failErr(w, err)
			return
		}
	}
	run, err := h.s.Trigger(r.PathValue("id"), TriggerOptions{Trigger: "manual", Params: in.Params, User: userOf(r)})
	if err != nil {
		failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// ---------------------------------------------------------------- 运行

func (h *handlers) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	f := store.RunFilter{DefinitionID: q.Get("definitionId"), Status: q.Get("status"), Before: before, Limit: limit}
	if scope := h.grantScope(r); scope != nil {
		f.GrantedUser = userOf(r) // 授权过滤进 SQL、先于 LIMIT（R1-M3）
	}
	list, err := h.s.st.ListRuns(f)
	if err != nil {
		failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": list})
}

// runDefID 取运行所属定义 ID（供授权校验）。
func (h *handlers) runDefID(id string) (string, error) {
	if a := h.s.runs.get(id); a != nil {
		return a.run.DefinitionID, nil
	}
	run, err := h.s.st.GetRun(id)
	if err != nil {
		return "", err
	}
	return run.DefinitionID, nil
}

func (h *handlers) getRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	defID, err := h.runDefID(id)
	if err != nil {
		failErr(w, err)
		return
	}
	if !h.requireDef(w, r, defID, store.ModeUse) {
		return
	}
	run, err := h.s.st.GetRun(id)
	if err != nil {
		failErr(w, err)
		return
	}
	nodes, err := h.s.st.ListRunNodes(id)
	if err != nil {
		failErr(w, err)
		return
	}
	// 在途运行：用内存中的实时度量覆盖
	if a := h.s.runs.get(id); a != nil {
		a.mu.Lock()
		live := map[string]store.RunNode{}
		for k, n := range a.nodes {
			live[k] = n
		}
		a.mu.Unlock()
		for i, n := range nodes {
			if l, ok := live[nodeKey(n.ParentNode, n.NodeID)]; ok {
				nodes[i] = l
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "nodes": nodes, "live": h.s.runs.get(id) != nil})
}

func (h *handlers) runLogs(w http.ResponseWriter, r *http.Request) {
	defID, err := h.runDefID(r.PathValue("id"))
	if err != nil {
		failErr(w, err)
		return
	}
	if !h.requireDef(w, r, defID, store.ModeUse) {
		return
	}
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.s.st.ListLogs(r.PathValue("id"), after, limit, q.Get("level"), q.Get("node"))
	if err != nil {
		failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": list})
}

// runEvents 是 SSE：status / node / metrics / log 事件，运行结束发送 end 后关闭。
func (h *handlers) runEvents(w http.ResponseWriter, r *http.Request) {
	defID, err := h.runDefID(r.PathValue("id"))
	if err != nil {
		failErr(w, err)
		return
	}
	if !h.requireDef(w, r, defID, store.ModeUse) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, "sse", "不支持流式响应")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	a := h.s.runs.get(r.PathValue("id"))
	send := func(e event) {
		b, _ := json.Marshal(e.Data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, b)
		flusher.Flush()
	}
	if a == nil {
		send(event{Type: "end", Data: map[string]any{}})
		return
	}
	ch, unsub := a.hub.subscribe()
	defer unsub()
	send(event{Type: "status", Data: a.run})
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				send(event{Type: "end", Data: map[string]any{}})
				return
			}
			send(e)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (h *handlers) cancelRun(w http.ResponseWriter, r *http.Request) {
	defID, err := h.runDefID(r.PathValue("id"))
	if err != nil {
		failErr(w, err)
		return
	}
	if !h.requireDef(w, r, defID, store.ModeUse) {
		return
	}
	if err := h.s.Cancel(r.PathValue("id"), userOf(r)); err != nil {
		failErr(w, err)
		return
	}
	if run, err := h.s.st.GetRun(r.PathValue("id")); err == nil {
		h.s.audit(userOf(r), "取消运行", "数据集成·"+run.DefinitionName, "成功")
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *handlers) resumeRun(w http.ResponseWriter, r *http.Request) {
	defID, err := h.runDefID(r.PathValue("id"))
	if err != nil {
		failErr(w, err)
		return
	}
	if !h.requireDef(w, r, defID, store.ModeUse) {
		return
	}
	run, err := h.s.Trigger("", TriggerOptions{Trigger: "resume", ResumeOf: r.PathValue("id"), User: userOf(r)})
	if err != nil {
		failErr(w, err)
		return
	}
	h.s.audit(userOf(r), "续跑运行", "数据集成·"+run.DefinitionName, "成功")
	writeJSON(w, http.StatusOK, run)
}

// ---------------------------------------------------------------- 调度

func (h *handlers) listSchedules(w http.ResponseWriter, r *http.Request) {
	list, err := h.s.st.ListSchedules(r.URL.Query().Get("targetId"))
	if err != nil {
		failErr(w, err)
		return
	}
	if scope := h.grantScope(r); scope != nil {
		out := list[:0]
		for _, sc := range list {
			if _, ok := scope[sc.TargetID]; ok {
				out = append(out, sc)
			}
		}
		list = out
	}
	type out struct {
		store.Schedule
		Description string `json:"description"`
		Access      string `json:"access,omitempty"`
	}
	res := make([]out, len(list))
	for i, sc := range list {
		res[i] = out{Schedule: sc, Description: schedule.Describe(sc.Cron), Access: h.defMode(r, sc.TargetID)}
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": res})
}

func (h *handlers) saveSchedule(w http.ResponseWriter, r *http.Request) {
	var in store.Schedule
	if err := decode(r, &in); err != nil {
		failErr(w, err)
		return
	}
	if !h.requireDef(w, r, in.TargetID, store.ModeEdit) {
		return
	}
	in.ID = r.PathValue("id")
	if in.ID != "" {
		// 编辑既有调度：原目标也须 edit，否则可把别人的调度改挂到自己的定义上
		if cur, err := h.s.st.GetSchedule(in.ID); err == nil && cur.TargetID != in.TargetID {
			if !h.requireDef(w, r, cur.TargetID, store.ModeEdit) {
				return
			}
		}
	}
	if _, err := h.s.st.GetDefinition(in.TargetID); err != nil {
		failErr(w, fmt.Errorf("调度目标无效：%w", err))
		return
	}
	if _, _, err := schedule.Parse(in.Cron, in.Timezone); err != nil {
		failErr(w, err)
		return
	}
	if in.Overlap == "" {
		in.Overlap = "skip"
	}
	if in.Misfire == "" {
		in.Misfire = "skip"
	}
	if !oneOf(in.Overlap, "skip", "queue", "parallel") || !oneOf(in.Misfire, "skip", "run_once") {
		failErr(w, errors.New("重叠或错过策略无效"))
		return
	}
	in.NextFireAt = 0
	if in.Enabled {
		next, err := schedule.Next(in.Cron, in.Timezone, time.Now())
		if err != nil {
			failErr(w, err)
			return
		}
		in.NextFireAt = next
	}
	saved, err := h.s.st.SaveSchedule(in, userOf(r))
	if err != nil {
		failErr(w, err)
		return
	}
	h.s.audit(userOf(r), "保存调度", "数据集成·"+saved.TargetName+"（"+saved.Cron+"）", "成功")
	writeJSON(w, http.StatusOK, saved)
}

func oneOf(v string, opts ...string) bool {
	for _, o := range opts {
		if v == o {
			return true
		}
	}
	return false
}

func (h *handlers) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	sc, err := h.s.st.GetSchedule(r.PathValue("id"))
	if err != nil {
		failErr(w, err)
		return
	}
	if !h.requireDef(w, r, sc.TargetID, store.ModeEdit) {
		return
	}
	if err := h.s.st.DeleteSchedule(sc.ID); err != nil {
		failErr(w, err)
		return
	}
	h.s.audit(userOf(r), "删除调度", "数据集成·"+sc.TargetName, "成功")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *handlers) previewSchedule(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	times, err := schedule.NextN(q.Get("cron"), q.Get("tz"), time.Now(), 5)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"valid": false, "error": err.Error()})
		return
	}
	ms := make([]int64, len(times))
	for i, t := range times {
		ms[i] = t.UnixMilli()
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true, "description": schedule.Describe(q.Get("cron")), "next": ms})
}
