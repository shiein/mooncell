// SQLite 存储层：数据资源、授权、保存 SQL 三张表的迁移与 CRUD。
//
// 沿用 consoleapp 的显式迁移方式（CREATE TABLE IF NOT EXISTS），不引入迁移框架。
// 所有 SQL 使用参数化绑定，标识符（表名/列名）来自受控白名单。
package dataresource

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MigrateDataResources 在 SQLite 中创建数据资源模块所需的三张表。
// 幂等：已存在则跳过。由 consoleapp.openDB 在启动时调用。
func MigrateDataResources(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS data_resources (
			id                TEXT    PRIMARY KEY,
			name              TEXT    NOT NULL,
			db_type           TEXT    NOT NULL,
			host              TEXT    NOT NULL,
			port              INTEGER NOT NULL,
			database_name     TEXT    NOT NULL,
			default_schema    TEXT    NOT NULL DEFAULT '',
			username          TEXT    NOT NULL,
			credential_cipher TEXT    NOT NULL DEFAULT '',
			ssl_mode          TEXT    NOT NULL DEFAULT 'disable',
			created_by        TEXT    NOT NULL,
			created_at        INTEGER NOT NULL,
			updated_at        INTEGER NOT NULL,
			last_test_status  TEXT    NOT NULL DEFAULT '',
			last_test_at      INTEGER NOT NULL DEFAULT 0
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_data_resources_name_lower
			ON data_resources(LOWER(name));

		CREATE TABLE IF NOT EXISTS data_resource_grants (
			resource_id  TEXT    NOT NULL,
			username     TEXT    NOT NULL,
			access_mode  TEXT    NOT NULL,
			granted_by   TEXT    NOT NULL,
			granted_at   INTEGER NOT NULL,
			PRIMARY KEY (resource_id, username)
		);

		CREATE TABLE IF NOT EXISTS saved_sql (
			id          TEXT    PRIMARY KEY,
			username    TEXT    NOT NULL,
			resource_id TEXT    NOT NULL,
			name        TEXT    NOT NULL,
			sql_text    TEXT    NOT NULL,
			created_at  INTEGER NOT NULL,
			updated_at  INTEGER NOT NULL,
			UNIQUE(username, resource_id, name)
		);
	`)
	return err
}

// DataResource 是数据资源记录的完整内部形态（含加密后的凭据）。
type DataResource struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	DBType           string `json:"dbType"`
	Host             string `json:"host"`
	Port             int    `json:"port"`
	DatabaseName     string `json:"databaseName"`
	DefaultSchema    string `json:"defaultSchema"`
	Username         string `json:"username"`
	CredentialCipher string `json:"-"` // 永不返回前端
	SSLMode          string `json:"sslMode"`
	CreatedBy        string `json:"createdBy"`
	CreatedAt        int64  `json:"createdAt"`
	UpdatedAt        int64  `json:"updatedAt"`
	LastTestStatus   string `json:"lastTestStatus"`
	LastTestAt       int64  `json:"lastTestAt"`
}

// DataResourceOut 是对外 API 形态：不含密码，只有 hasPassword 标记。
type DataResourceOut struct {
	DataResource
	HasPassword  bool   `json:"hasPassword"`
	AccessMode   string `json:"accessMode"`   // 当前用户对该资源的权限：read/write/admin
	LastTestInfo string `json:"lastTestInfo"` // 可读的最近测试结果摘要
}

// DataResourceInput 是创建/编辑资源时的请求体。Password 为空表示保留原密码。
type DataResourceInput struct {
	Name          string `json:"name"`
	DBType        string `json:"dbType"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	DatabaseName  string `json:"databaseName"`
	DefaultSchema string `json:"defaultSchema"`
	Username      string `json:"username"`
	Password      string `json:"password"` // 空表示保留原密码（编辑时）
	SSLMode       string `json:"sslMode"`
}

// AccessMode 值。
const (
	AccessRead  = "read"
	AccessWrite = "write"
)

// validDBType 校验 dbType 是否为当前构建中可用的驱动。
// Vastbase 官方本地 pq 未随仓库提供，按设计 fail-closed，不允许新建未认证资源。
func validDBType(dbType string) bool {
	switch dbType {
	case DriverPostgreSQL, DriverMySQL, DriverDM, DriverKingbase:
		return true
	}
	return false
}

// validSSLMode 校验 sslMode 是否合法。
func validSSLMode(mode string) bool {
	return mode == "disable" || mode == "require"
}

// HasExistingResources 检查是否已有数据资源记录（用于密钥生成决策）。
func HasExistingResources(db *sql.DB) (bool, error) {
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM data_resources").Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// CreateDataResource 插入一条数据资源。credentialCipher 为加密后的密码密文。
func CreateDataResource(db *sql.DB, r DataResource) error {
	_, err := db.Exec(`INSERT INTO data_resources
		(id, name, db_type, host, port, database_name, default_schema, username, credential_cipher, ssl_mode, created_by, created_at, updated_at, last_test_status, last_test_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Name, r.DBType, r.Host, r.Port, r.DatabaseName, r.DefaultSchema,
		r.Username, r.CredentialCipher, r.SSLMode, r.CreatedBy, r.CreatedAt, r.UpdatedAt,
		r.LastTestStatus, r.LastTestAt,
	)
	return err
}

// GetDataResource 按 ID 查询单条资源（含密文）。
func GetDataResource(db *sql.DB, id string) (DataResource, bool, error) {
	var r DataResource
	err := db.QueryRow(`SELECT id, name, db_type, host, port, database_name, default_schema,
		username, credential_cipher, ssl_mode, created_by, created_at, updated_at, last_test_status, last_test_at
		FROM data_resources WHERE id = ?`, id).
		Scan(&r.ID, &r.Name, &r.DBType, &r.Host, &r.Port, &r.DatabaseName, &r.DefaultSchema,
			&r.Username, &r.CredentialCipher, &r.SSLMode, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt,
			&r.LastTestStatus, &r.LastTestAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DataResource{}, false, nil
	}
	if err != nil {
		return DataResource{}, false, err
	}
	return r, true, nil
}

// ListDataResources 返回全部资源（admin 用），按创建时间升序。
func ListDataResources(db *sql.DB) ([]DataResource, error) {
	rows, err := db.Query(`SELECT id, name, db_type, host, port, database_name, default_schema,
		username, credential_cipher, ssl_mode, created_by, created_at, updated_at, last_test_status, last_test_at
		FROM data_resources ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DataResource
	for rows.Next() {
		var r DataResource
		if err := rows.Scan(&r.ID, &r.Name, &r.DBType, &r.Host, &r.Port, &r.DatabaseName, &r.DefaultSchema,
			&r.Username, &r.CredentialCipher, &r.SSLMode, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt,
			&r.LastTestStatus, &r.LastTestAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateDataResource 更新资源。credentialCipher 为空表示保留原密码。
// 配置变更后清空连接测试状态，并撤销该资源上所有 read 授权（须重新测试后才能再授只读）。
func UpdateDataResource(db *sql.DB, id string, input DataResourceInput, credentialCipher string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	if credentialCipher != "" {
		_, err = tx.Exec(`UPDATE data_resources SET
			name=?, db_type=?, host=?, port=?, database_name=?, default_schema=?, username=?,
			credential_cipher=?, ssl_mode=?, updated_at=?, last_test_status='', last_test_at=0
			WHERE id=?`,
			input.Name, input.DBType, input.Host, input.Port, input.DatabaseName, input.DefaultSchema,
			input.Username, credentialCipher, input.SSLMode, now, id)
	} else {
		_, err = tx.Exec(`UPDATE data_resources SET
			name=?, db_type=?, host=?, port=?, database_name=?, default_schema=?, username=?,
			ssl_mode=?, updated_at=?, last_test_status='', last_test_at=0
			WHERE id=?`,
			input.Name, input.DBType, input.Host, input.Port, input.DatabaseName, input.DefaultSchema,
			input.Username, input.SSLMode, now, id)
	}
	if err != nil {
		return err
	}
	// 配置可能已切到未认证的驱动/目标库：旧 read 授权不得沿用
	if _, err := tx.Exec(`DELETE FROM data_resource_grants WHERE resource_id = ? AND access_mode = ?`,
		id, AccessRead); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteDataResource 删除资源及其关联的授权和保存 SQL。
func DeleteDataResource(db *sql.DB, id string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM data_resource_grants WHERE resource_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM saved_sql WHERE resource_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM data_resources WHERE id = ?", id); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateTestStatus 更新资源的最近测试状态（无 CAS，仅供测试辅助）。
func UpdateTestStatus(db *sql.DB, id, status string) error {
	_, err := db.Exec("UPDATE data_resources SET last_test_status=?, last_test_at=? WHERE id=?",
		status, time.Now().UnixMilli(), id)
	return err
}

// ErrConfigChanged 表示测试期间资源配置已变更，测试结果不得写回。
var ErrConfigChanged = errors.New("CONFIG_CHANGED")

// UpdateTestStatusAndRevokeRead 更新连接测试状态；认证降级时在同一事务内撤销既有 read 授权。
// expectedUpdatedAt 为测试开始前读到的 updated_at；不一致则返回 ErrConfigChanged（CAS）。
// 返回被撤销的用户名，供调用方立即失效其内存工作台。
func UpdateTestStatusAndRevokeRead(db *sql.DB, id, status string, expectedUpdatedAt int64) ([]string, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var revoked []string
	if status != TestStatusOK {
		rows, err := tx.Query(
			"SELECT username FROM data_resource_grants WHERE resource_id = ? AND access_mode = ? ORDER BY username",
			id, AccessRead)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var username string
			if err := rows.Scan(&username); err != nil {
				rows.Close()
				return nil, err
			}
			revoked = append(revoked, username)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		if _, err := tx.Exec(
			"DELETE FROM data_resource_grants WHERE resource_id = ? AND access_mode = ?",
			id, AccessRead); err != nil {
			return nil, err
		}
	}
	res, err := tx.Exec(
		"UPDATE data_resources SET last_test_status=?, last_test_at=? WHERE id=? AND updated_at=?",
		status, time.Now().UnixMilli(), id, expectedUpdatedAt)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, ErrConfigChanged
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return revoked, nil
}

// --- 授权 ---

// DataResourceGrant 是一条授权记录。
type DataResourceGrant struct {
	ResourceID string `json:"resourceId"`
	Username   string `json:"username"`
	AccessMode string `json:"accessMode"`
	GrantedBy  string `json:"grantedBy"`
	GrantedAt  int64  `json:"grantedAt"`
}

// SetUserGrants 全量替换用户的数据资源授权（事务性，失败回滚不留半成品）。
// grants 中 AccessMode 必须是 read 或 write。
// 授予 read 时要求资源最近连接测试已通过只读事务认证（last_test_status=ok）。
func SetUserGrants(db *sql.DB, username string, grants []DataResourceGrant, grantedBy string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := SetUserGrantsTx(tx, username, grants, grantedBy); err != nil {
		return err
	}
	return tx.Commit()
}

// SetUserGrantsTx 在已有事务中替换数据资源授权（供与密码/应用授权同事务提交）。
func SetUserGrantsTx(tx *sql.Tx, username string, grants []DataResourceGrant, grantedBy string) error {
	if _, err := tx.Exec("DELETE FROM data_resource_grants WHERE username = ?", username); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for _, g := range grants {
		if g.AccessMode != AccessRead && g.AccessMode != AccessWrite {
			return fmt.Errorf("无效的 access_mode: %s", g.AccessMode)
		}
		// read/write 均校验资源存在，避免孤儿授权
		var testStatus string
		err := tx.QueryRow(`SELECT last_test_status FROM data_resources WHERE id = ?`, g.ResourceID).Scan(&testStatus)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("资源不存在，无法授权: %s", g.ResourceID)
		}
		if err != nil {
			return err
		}
		if g.AccessMode == AccessRead {
			if !SupportsReadGrant(testStatus) {
				return fmt.Errorf("资源未通过只读事务认证，不能授予只读权限（请先成功测试连接且 readOnlyTxSupported=true）: %s", g.ResourceID)
			}
		}
		if _, err := tx.Exec(`INSERT INTO data_resource_grants (resource_id, username, access_mode, granted_by, granted_at)
			VALUES (?, ?, ?, ?, ?)`,
			g.ResourceID, username, g.AccessMode, grantedBy, now); err != nil {
			return err
		}
	}
	return nil
}

// UserGrants 返回某用户的全部数据资源授权。
func UserGrants(db *sql.DB, username string) ([]DataResourceGrant, error) {
	rows, err := db.Query(`SELECT resource_id, username, access_mode, granted_by, granted_at
		FROM data_resource_grants WHERE username = ? ORDER BY resource_id`, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DataResourceGrant
	for rows.Next() {
		var g DataResourceGrant
		if err := rows.Scan(&g.ResourceID, &g.Username, &g.AccessMode, &g.GrantedBy, &g.GrantedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// UserAccessMode 返回用户对某资源的权限。admin 返回 "admin"（隐式 write）。
// 无授权返回空字符串。
func UserAccessMode(db *sql.DB, username, role, resourceID string) (string, error) {
	if role == "admin" {
		return "admin", nil
	}
	var mode string
	err := db.QueryRow("SELECT access_mode FROM data_resource_grants WHERE resource_id = ? AND username = ?",
		resourceID, username).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return mode, err
}

// VisibleResources 返回用户可见的资源列表。admin 返回全部；普通用户仅返回授权的资源。
func VisibleResources(db *sql.DB, username, role string) ([]DataResource, error) {
	if role == "admin" {
		return ListDataResources(db)
	}
	rows, err := db.Query(`SELECT r.id, r.name, r.db_type, r.host, r.port, r.database_name, r.default_schema,
		r.username, r.credential_cipher, r.ssl_mode, r.created_by, r.created_at, r.updated_at, r.last_test_status, r.last_test_at
		FROM data_resources r
		INNER JOIN data_resource_grants g ON g.resource_id = r.id AND g.username = ?
		ORDER BY r.created_at`, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DataResource
	for rows.Next() {
		var r DataResource
		if err := rows.Scan(&r.ID, &r.Name, &r.DBType, &r.Host, &r.Port, &r.DatabaseName, &r.DefaultSchema,
			&r.Username, &r.CredentialCipher, &r.SSLMode, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt,
			&r.LastTestStatus, &r.LastTestAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteUserGrants 删除用户的所有数据资源授权（用户删除时调用）。
func DeleteUserGrants(db *sql.DB, username string) error {
	_, err := db.Exec("DELETE FROM data_resource_grants WHERE username = ?", username)
	return err
}

// DeleteResourceGrants 删除资源的所有授权（资源删除时已在事务中处理，此处供单独调用）。
func DeleteResourceGrants(db *sql.DB, resourceID string) error {
	_, err := db.Exec("DELETE FROM data_resource_grants WHERE resource_id = ?", resourceID)
	return err
}

// AllGrantsByUser 批量返回所有用户的数据资源授权（admin 用户管理页用）。
// 返回 map[username][]DataResourceGrant。
func AllGrantsByUser(db *sql.DB) (map[string][]DataResourceGrant, error) {
	rows, err := db.Query(`SELECT resource_id, username, access_mode, granted_by, granted_at
		FROM data_resource_grants ORDER BY username, resource_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]DataResourceGrant{}
	for rows.Next() {
		var g DataResourceGrant
		if err := rows.Scan(&g.ResourceID, &g.Username, &g.AccessMode, &g.GrantedBy, &g.GrantedAt); err != nil {
			return nil, err
		}
		out[g.Username] = append(out[g.Username], g)
	}
	return out, rows.Err()
}

// --- 保存 SQL ---

// SavedSQL 是一条保存的 SQL 记录。
type SavedSQL struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	ResourceID string `json:"resourceId"`
	Name       string `json:"name"`
	SQLText    string `json:"sqlText"`
	CreatedAt  int64  `json:"createdAt"`
	UpdatedAt  int64  `json:"updatedAt"`
}

// ListSavedSQL 返回某用户在某资源下的保存 SQL。
func ListSavedSQL(db *sql.DB, username, resourceID string) ([]SavedSQL, error) {
	rows, err := db.Query(`SELECT id, username, resource_id, name, sql_text, created_at, updated_at
		FROM saved_sql WHERE username = ? AND resource_id = ? ORDER BY updated_at DESC`,
		username, resourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SavedSQL
	for rows.Next() {
		var s SavedSQL
		if err := rows.Scan(&s.ID, &s.Username, &s.ResourceID, &s.Name, &s.SQLText, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// CreateSavedSQL 插入一条保存 SQL。同一用户同一资源下名称唯一。
func CreateSavedSQL(db *sql.DB, s SavedSQL) error {
	_, err := db.Exec(`INSERT INTO saved_sql (id, username, resource_id, name, sql_text, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Username, s.ResourceID, s.Name, s.SQLText, s.CreatedAt, s.UpdatedAt)
	return err
}

// UpdateSavedSQL 更新一条保存 SQL。仅 owner 且属于指定 resource 时可更新。
func UpdateSavedSQL(db *sql.DB, id, username, resourceID, name, sqlText string) error {
	now := time.Now().UnixMilli()
	res, err := db.Exec(
		"UPDATE saved_sql SET name=?, sql_text=?, updated_at=? WHERE id=? AND username=? AND resource_id=?",
		name, sqlText, now, id, username, resourceID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteSavedSQL 删除一条保存 SQL。仅 owner 且属于指定 resource 时可删除。
func DeleteSavedSQL(db *sql.DB, id, username, resourceID string) error {
	res, err := db.Exec("DELETE FROM saved_sql WHERE id=? AND username=? AND resource_id=?", id, username, resourceID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// --- 校验工具 ---

// ValidateInput 校验 DataResourceInput 的基本合法性。
func ValidateInput(input DataResourceInput) error {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return errors.New("名称不能为空")
	}
	if !validDBType(input.DBType) {
		return fmt.Errorf("不支持的数据库类型: %s", input.DBType)
	}
	if strings.TrimSpace(input.Host) == "" {
		return errors.New("主机不能为空")
	}
	if input.Port <= 0 || input.Port > 65535 {
		return errors.New("端口必须在 1-65535 范围内")
	}
	if strings.TrimSpace(input.DatabaseName) == "" {
		return errors.New("数据库名不能为空")
	}
	if strings.TrimSpace(input.Username) == "" {
		return errors.New("用户名不能为空")
	}
	if !validSSLMode(input.SSLMode) {
		return errors.New("SSL 模式只能为 disable 或 require")
	}
	// 达梦 DSN 当前未接线 TLS：禁止 require，避免界面宣称加密实际明文连接
	if input.DBType == DriverDM && input.SSLMode == "require" {
		return errors.New("达梦驱动当前不支持 sslMode=require，请使用 disable")
	}
	return nil
}
