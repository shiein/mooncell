package consoleapp

import (
	"encoding/json"
	"net/http"
	"strings"

	"mooncell/console/internal/dataresource"
	"mooncell/console/internal/serverops"
)

// listUsers 处理 GET /api/users(admin):列出全部用户(不含口令,含授权应用)。
func (a *api) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.store.listUsers()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取用户失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

// createUser 处理 POST /api/users(admin):新建普通用户(用户名、密码、授权应用)。
// 角色固定为 user;管理员仅由 config 种入或库内既有 admin,不经此接口再造。
func (a *api) createUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username             string                           `json:"username"`
		Password             string                           `json:"password"`
		AppIDs               []string                         `json:"appIds"`
		DataResourceGrants   []dataresource.DataResourceGrant `json:"dataResourceGrants"`
		ServerResourceGrants []serverops.ServerResourceGrant  `json:"serverResourceGrants"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if body.Username == "" || body.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "用户名或密码不能为空"})
		return
	}
	for i := range body.DataResourceGrants {
		body.DataResourceGrants[i].Username = body.Username
	}
	for i := range body.ServerResourceGrants {
		body.ServerResourceGrants[i].Username = body.Username
	}
	if err := a.store.createUserBundle(
		body.Username,
		body.Password,
		"user",
		normalizeAppIDs(body.AppIDs),
		body.DataResourceGrants,
		body.ServerResourceGrants,
		a.sessionUser(r),
	); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "创建失败:用户名可能已存在或授权无效"})
		return
	}
	a.store.appendAudit(a.sessionUser(r), "创建用户", body.Username, "成功")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// updateUser 处理 PUT /api/users/{username}(admin):更新口令(可选)与应用授权。
// 不可改角色。admin 账号的 appIds 写入无意义但允许(前端通常不展示)。
func (a *api) updateUser(w http.ResponseWriter, r *http.Request) {
	target := strings.TrimSpace(r.PathValue("username"))
	if target == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少用户名"})
		return
	}
	if a.store.userRole(target) == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "用户不存在"})
		return
	}
	var body struct {
		Password             string                            `json:"password"`             // 非空则改密
		AppIDs               *[]string                         `json:"appIds"`               // 非 nil 则全量替换授权
		DataResourceGrants   *[]dataresource.DataResourceGrant `json:"dataResourceGrants"`   // 非 nil 则全量替换数据资源授权
		ServerResourceGrants *[]serverops.ServerResourceGrant  `json:"serverResourceGrants"` // 非 nil 则全量替换服务器运维授权
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	// 密码 / 应用 / 数据资源 / 服务器授权必须同一事务，避免半成品。
	var appIDs *[]string
	if body.AppIDs != nil {
		n := normalizeAppIDs(*body.AppIDs)
		appIDs = &n
	}
	var grants *[]dataresource.DataResourceGrant
	var oldGrants []dataresource.DataResourceGrant
	if body.DataResourceGrants != nil {
		g := *body.DataResourceGrants
		for i := range g {
			g[i].Username = target
		}
		// 写库前快照旧授权，供撤权失效工作台
		var err error
		oldGrants, err = dataresource.UserGrants(a.store.db, target)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取数据资源授权失败"})
			return
		}
		grants = &g
	}
	var serverGrants *[]serverops.ServerResourceGrant
	var oldServerGrants []serverops.ServerResourceGrant
	if body.ServerResourceGrants != nil {
		g := *body.ServerResourceGrants
		for i := range g {
			g[i].Username = target
		}
		var err error
		oldServerGrants, err = serverops.UserGrants(a.store.db, target)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取服务器运维授权失败"})
			return
		}
		serverGrants = &g
	}
	var beforeCommit func()
	if grants != nil || serverGrants != nil {
		beforeCommit = func() {
			// 所有授权校验和写入已成功后、事务提交前，使旧授权的在途写/SSH 失效。
			if grants != nil {
				a.rollbackRevokedDataResourceTx(target, oldGrants, *grants)
			}
			if serverGrants != nil {
				a.invalidateRevokedServerOps(target, oldServerGrants, *serverGrants)
			}
		}
	}
	if err := a.store.updateUserBundle(target, body.Password, appIDs, grants, serverGrants, a.sessionUser(r), beforeCommit); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "更新用户失败: " + err.Error()})
		return
	}
	a.store.appendAudit(a.sessionUser(r), "更新用户", target, "成功")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// deleteUser 处理 DELETE /api/users/{username}(admin):删除用户。
// 防呆:不能删当前登录账号,不能删最后一个管理员。
func (a *api) deleteUser(w http.ResponseWriter, r *http.Request) {
	target := r.PathValue("username")
	me, _, _ := a.currentUser(r)
	if target == me {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "不能删除当前登录账号"})
		return
	}
	if a.store.userRole(target) == "admin" && a.store.countAdmins() <= 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "不能删除最后一个管理员"})
		return
	}
	// 删除前先回滚并清理该用户全部数据资源工作台与 SSH 会话。
	if a.dataResSvc != nil {
		a.dataResSvc.InvalidateAllForUser(target)
	}
	if a.serverOps != nil {
		a.serverOps.InvalidateUser(target)
	}
	// 原子删除:即便上面的预检因并发互删而失效,deleteUser 内末位 admin 守卫也会拦下。
	deleted, err := a.store.deleteUser(target)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "删除失败"})
		return
	}
	if !deleted {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "并发删除:已是最后一个管理员或用户不存在,拒绝删除"})
		return
	}
	a.store.appendAudit(me, "删除用户", target, "成功")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func normalizeAppIDs(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// rollbackRevokedDataResourceTx 比较写库前的旧授权与新授权：
//   - 资源被完全撤销：回滚事务并删除工作台
//   - access_mode 任意变化（write→read 降级或 read→write 升级）：同样失效
//     降级避免可写事务残留；升级避免工作台创建时固化的 ReadOnly 粘滞导致已授权写仍 403
//
// 设计文档：撤销或降级授权后，立即回滚该用户在对应资源上的活动事务并使工作台失效。
func (a *api) rollbackRevokedDataResourceTx(username string, oldGrants, newGrants []dataresource.DataResourceGrant) {
	if a.dataResSvc == nil {
		return
	}
	newModes := map[string]string{}
	for _, g := range newGrants {
		newModes[g.ResourceID] = g.AccessMode
	}
	for _, old := range oldGrants {
		newMode, still := newModes[old.ResourceID]
		if !still || old.AccessMode != newMode {
			a.dataResSvc.InvalidateUserResource(username, old.ResourceID)
		}
	}
}

// invalidateRevokedServerOps 比较旧/新服务器授权：被撤销的资源立即终止活动 SSH/SFTP。
// 审计记在 updateUser 成功路径；此处只负责运行时失效。
func (a *api) invalidateRevokedServerOps(username string, oldGrants, newGrants []serverops.ServerResourceGrant) {
	if a.serverOps == nil {
		return
	}
	still := map[string]bool{}
	for _, g := range newGrants {
		still[g.ResourceID] = true
	}
	for _, old := range oldGrants {
		if !still[old.ResourceID] {
			a.serverOps.InvalidateUserResource(username, old.ResourceID)
		}
	}
}
