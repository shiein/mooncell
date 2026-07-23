package consoleapp

import (
	"encoding/json"
	"net/http"
	"strings"

	"mooncell/console/internal/dataresource"
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
		Username           string                             `json:"username"`
		Password           string                             `json:"password"`
		AppIDs             []string                           `json:"appIds"`
		DataResourceGrants []dataresource.DataResourceGrant   `json:"dataResourceGrants"`
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
	if err := a.store.createUser(body.Username, body.Password, "user"); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "创建失败:用户名可能已存在"})
		return
	}
	if err := a.store.setUserApps(body.Username, normalizeAppIDs(body.AppIDs)); err != nil {
		// 用户已建但授权失败:回滚用户,避免半成品账号。
		if deleted, derr := a.store.deleteUser(body.Username); !deleted || derr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "写入应用授权失败,且回滚用户未完成,请管理员清理账号: " + body.Username,
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "写入应用授权失败"})
		return
	}
	// 数据资源授权:与应用授权一起事务性写入。
	for i := range body.DataResourceGrants {
		body.DataResourceGrants[i].Username = body.Username
	}
	if err := dataresource.SetUserGrants(a.store.db, body.Username, body.DataResourceGrants, a.sessionUser(r)); err != nil {
		// 授权失败:回滚用户和应用授权,避免半成品。
		a.store.deleteUser(body.Username)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "写入数据资源授权失败"})
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
		Password           string                           `json:"password"`           // 非空则改密
		AppIDs             *[]string                        `json:"appIds"`             // 非 nil 则全量替换授权
		DataResourceGrants *[]dataresource.DataResourceGrant `json:"dataResourceGrants"` // 非 nil 则全量替换数据资源授权
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	// 密码 / 应用授权 / 数据资源授权必须同一事务，避免半成品。
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
	if err := a.store.updateUserBundle(target, body.Password, appIDs, grants, a.sessionUser(r)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "更新用户失败: " + err.Error()})
		return
	}
	if grants != nil {
		a.rollbackRevokedDataResourceTx(target, oldGrants, *grants)
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
	// 删除前先回滚并清理该用户全部数据资源工作台事务。
	if a.dataResSvc != nil {
		a.dataResSvc.InvalidateAllForUser(target)
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
// - 资源被完全撤销：回滚事务并删除工作台
// - write → read 降级：同样失效工作台（可能持有可写事务）
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
		if !still {
			a.dataResSvc.InvalidateUserResource(username, old.ResourceID)
			continue
		}
		if old.AccessMode == dataresource.AccessWrite && newMode == dataresource.AccessRead {
			a.dataResSvc.InvalidateUserResource(username, old.ResourceID)
		}
	}
}
