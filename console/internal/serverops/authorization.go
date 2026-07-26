// admin / 普通用户资源权限判定。
// 普通用户访问未授权资源统一 404，避免通过 ID 枚举服务器信息。
package serverops

import "database/sql"

// CanAccess 判断用户是否可查看/运维该资源。admin 全放行；普通用户须有 grant。
func CanAccess(db *sql.DB, username, role, resourceID string) (bool, error) {
	if isAdmin(role) {
		return true, nil
	}
	return UserHasGrant(db, username, resourceID)
}

// AccessModeFor 返回对外 accessMode：admin | operate | ""。
func AccessModeFor(db *sql.DB, username, role, resourceID string) (string, error) {
	if isAdmin(role) {
		return AccessAdmin, nil
	}
	ok, err := UserHasGrant(db, username, resourceID)
	if err != nil || !ok {
		return "", err
	}
	return AccessOperate, nil
}

// VisibleResources 返回用户可见资源列表。
func VisibleResources(db *sql.DB, username, role string) ([]ServerResource, error) {
	if isAdmin(role) {
		return listAllResources(db)
	}
	return listGrantedResources(db, username)
}

// RequireAccess 加载资源并校验权限；未授权或缺失均返回 404 语义错误。
func RequireAccess(db *sql.DB, username, role, resourceID string) (ServerResource, error) {
	r, found, err := getResource(db, resourceID)
	if err != nil {
		return r, apiErr(CodeDBError, "读取资源失败", true)
	}
	if !found {
		return r, apiErr(CodeNotFound, "服务器资源不存在", false)
	}
	ok, err := CanAccess(db, username, role, resourceID)
	if err != nil {
		return r, apiErr(CodeDBError, "读取授权失败", true)
	}
	if !ok {
		return r, apiErr(CodeNotFound, "服务器资源不存在", false)
	}
	return r, nil
}
