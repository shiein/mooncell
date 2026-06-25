package main

import (
	"encoding/json"
	"net/http"
)

// 前端复数键 ↔ 实体 kind(单数)
var kindOfKey = map[string]string{
	"apps": "app", "releases": "release", "backups": "backup", "cabinet": "cabinet", "audit": "audit",
}
var keyOfKind = map[string]string{
	"app": "apps", "release": "releases", "backup": "backups", "cabinet": "cabinet", "audit": "audit",
}

// serverOnlyKinds:权威实体,只由服务端在真实操作时写(审计 append、发布记录 appendRelease),
// 禁止前端经通用 PUT/DELETE 直写伪造。
var serverOnlyKinds = map[string]bool{"audit": true, "release": true}

// hydrate 处理 POST /api/data:body 为前端 INITIAL_* 种子(仅库为空时使用),
// 始终返回库中当前全部业务数据。前端据此首启种子、后续重载取持久化数据。
func (a *api) hydrate(w http.ResponseWriter, r *http.Request) {
	var seed map[string][]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&seed); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	// 仅 demo 模式才把前端 mock 种子写库;生产(默认)空库即全真实,不污染权威数据。
	if a.demoSeed {
		items := map[string][]seedItem{}
		for key, arr := range seed {
			kind, ok := kindOfKey[key]
			if !ok {
				continue
			}
			for _, raw := range arr {
				var probe struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(raw, &probe); err != nil || probe.ID == "" {
					continue
				}
				items[kind] = append(items[kind], seedItem{id: probe.ID, data: raw})
			}
		}
		if _, err := a.store.seedEntities(items); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "种子写入失败"})
			return
		}
	}

	grouped, err := a.store.loadEntities()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取数据失败"})
		return
	}
	out := map[string][]json.RawMessage{
		"apps": {}, "releases": {}, "backups": {}, "cabinet": {}, "audit": {},
	}
	for kind, arr := range grouped {
		if key, ok := keyOfKind[kind]; ok {
			out[key] = arr
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// putEntity 处理 PUT /api/data/{kind}/{id}:upsert 单条业务实体。
func (a *api) putEntity(w http.ResponseWriter, r *http.Request) {
	kind, id := r.PathValue("kind"), r.PathValue("id")
	if !entityKinds[kind] || id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "非法 kind 或 id"})
		return
	}
	// 审计/发布记录为权威记录,只允许服务端在真实操作时写,禁止前端直接写入伪造。
	if serverOnlyKinds[kind] {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "该实体为服务端权威记录,不可前端写入"})
		return
	}
	// 应用配置不走通用 JSON 写入口:必须经 PUT /api/apps/{id}/config 做服务端 schema/能力/范围校验,
	// 否则可绕过配置页预检写入坏 path/runner/agentId 等。
	if kind == "app" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "应用配置请用 PUT /api/apps/{id}/config(带服务端校验)"})
		return
	}
	var data json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	if err := a.store.putEntity(kind, id, data); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "写入失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// deleteEntity 处理 DELETE /api/data/{kind}/{id}。
func (a *api) deleteEntity(w http.ResponseWriter, r *http.Request) {
	kind, id := r.PathValue("kind"), r.PathValue("id")
	if !entityKinds[kind] || id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "非法 kind 或 id"})
		return
	}
	if serverOnlyKinds[kind] {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "服务端权威记录不可前端删除"})
		return
	}
	// 应用不允许走通用元数据删除:只删 Console 元数据会在 Agent 侧残留 systemd/pm2/bind mount 资源,
	// 且审计「删除应用」名不副实。真正删除须先 DELETE /api/agent/apps/{id} 下线,再删元数据。
	if kind == "app" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "应用不能仅删元数据;请先经 Agent 下线(DELETE /api/agent/apps/{id})再删除"})
		return
	}
	if err := a.store.deleteEntity(kind, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "删除失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
