package etl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

// ---------------------------------------------------------------- 概览与目录

func (h *handlers) overview(w http.ResponseWriter, r *http.Request) {
	defs, err := h.s.st.ListDefinitions("")
	if err != nil {
		failErr(w, err)
		return
	}
	scheds, _ := h.s.st.ListSchedules("")
	conns, _ := h.s.st.ListConnections()
	since := time.Now().Add(-24 * time.Hour).UnixMilli()
	recent, _ := h.s.st.ListRuns(store.RunFilter{Limit: 200})
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
	if len(recent24) > 12 {
		recent24 = recent24[:12]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pipelines": pipelines, "workflows": workflows, "connections": len(conns), "schedulesEnabled": enabled,
		"runs24h": byStatus, "rowsWritten24h": rows, "active": h.s.runs.activeNames(), "recentRuns": recent24, "upcoming": upcoming,
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

func (h *handlers) listDefinitions(w http.ResponseWriter, r *http.Request) {
	list, err := h.s.st.ListDefinitions(r.URL.Query().Get("kind"))
	if err != nil {
		failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"definitions": list})
}

func (h *handlers) getDefinition(w http.ResponseWriter, r *http.Request) {
	d, err := h.s.st.GetDefinition(r.PathValue("id"))
	if err != nil {
		failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *handlers) createDefinition(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, d)
}

func (h *handlers) deleteDefinition(w http.ResponseWriter, r *http.Request) {
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
	if err := h.s.st.DeleteDefinition(id); err != nil {
		failErr(w, err)
		return
	}
	h.s.audit(userOf(r), "删除"+kindLabel(d.Kind), "数据集成·"+d.Name, "成功")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *handlers) duplicateDefinition(w http.ResponseWriter, r *http.Request) {
	d, err := h.s.st.GetDefinition(r.PathValue("id"))
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
		cp, err = h.s.st.CreateDefinition(store.Definition{Kind: d.Kind, Name: name, Folder: d.Folder, Description: d.Description, Doc: d.Doc}, userOf(r))
		if err == nil || !strings.Contains(err.Error(), "同名") {
			break
		}
	}
	if err != nil {
		failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cp)
}

func (h *handlers) listVersions(w http.ResponseWriter, r *http.Request) {
	list, err := h.s.st.ListVersions(r.PathValue("id"))
	if err != nil {
		failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": list})
}

func (h *handlers) getVersion(w http.ResponseWriter, r *http.Request) {
	rev, _ := strconv.Atoi(r.PathValue("rev"))
	v, err := h.s.st.GetVersion(r.PathValue("id"), rev)
	if err != nil {
		failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *handlers) restoreVersion(w http.ResponseWriter, r *http.Request) {
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
	list, err := h.s.st.ListRuns(store.RunFilter{DefinitionID: q.Get("definitionId"), Status: q.Get("status"), Before: before, Limit: limit})
	if err != nil {
		failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": list})
}

func (h *handlers) getRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
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
	if err := h.s.Cancel(r.PathValue("id"), userOf(r)); err != nil {
		failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *handlers) resumeRun(w http.ResponseWriter, r *http.Request) {
	run, err := h.s.Trigger("", TriggerOptions{Trigger: "resume", ResumeOf: r.PathValue("id"), User: userOf(r)})
	if err != nil {
		failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// ---------------------------------------------------------------- 调度

func (h *handlers) listSchedules(w http.ResponseWriter, r *http.Request) {
	list, err := h.s.st.ListSchedules(r.URL.Query().Get("targetId"))
	if err != nil {
		failErr(w, err)
		return
	}
	type out struct {
		store.Schedule
		Description string `json:"description"`
	}
	res := make([]out, len(list))
	for i, sc := range list {
		res[i] = out{Schedule: sc, Description: schedule.Describe(sc.Cron)}
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": res})
}

func (h *handlers) saveSchedule(w http.ResponseWriter, r *http.Request) {
	var in store.Schedule
	if err := decode(r, &in); err != nil {
		failErr(w, err)
		return
	}
	in.ID = r.PathValue("id")
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
