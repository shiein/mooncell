package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// listUsers 处理 GET /api/users(admin):列出全部用户(不含口令)。
func (a *api) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.store.listUsers()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取用户失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

// createUser 处理 POST /api/users(admin):新建用户。
func (a *api) createUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
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
	if !validRoles[body.Role] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "非法角色(admin/operator/viewer)"})
		return
	}
	if err := a.store.createUser(body.Username, body.Password, body.Role); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "创建失败:用户名可能已存在"})
		return
	}
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
	if err := a.store.deleteUser(target); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "删除失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
