// 定义级授权（P3.16）：etl_grants 表与用户管理页共用模型（参照 dataresource）。
// access_mode：use = 查看/运行/查看运行记录；edit = use 之上再加修改、删除、复制、调度。
// admin 隐式全量（代码中记为 "admin"），不写表。
package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// AccessMode 取值。
const (
	ModeUse   = "use"
	ModeEdit  = "edit"
	ModeAdmin = "admin" // 不落库：表示管理员隐式全量
)

// DefinitionGrant 是一条定义级授权记录。
type DefinitionGrant struct {
	DefinitionID string `json:"definitionId"`
	Username     string `json:"username"`
	AccessMode   string `json:"accessMode"`
	GrantedBy    string `json:"grantedBy"`
	GrantedAt    int64  `json:"grantedAt"`
}

func validMode(m string) bool { return m == ModeUse || m == ModeEdit }

// SetUserGrants 全量替换用户的定义授权（事务性，失败回滚不留半成品）。
func SetUserGrants(db *sql.DB, username string, grants []DefinitionGrant, grantedBy string) error {
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

// SetUserGrantsTx 在已有事务中替换定义授权（供与用户其它授权同事务提交）。
func SetUserGrantsTx(tx *sql.Tx, username string, grants []DefinitionGrant, grantedBy string) error {
	if _, err := tx.Exec(`DELETE FROM etl_grants WHERE username = ?`, username); err != nil {
		return err
	}
	ts := now()
	for _, g := range grants {
		if !validMode(g.AccessMode) {
			return fmt.Errorf("无效授权模式: %s", g.AccessMode)
		}
		// 校验定义存在，避免孤儿授权
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM etl_definitions WHERE id = ?`, g.DefinitionID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("定义不存在，无法授权: %s", g.DefinitionID)
		}
		if _, err := tx.Exec(`INSERT INTO etl_grants (username, definition_id, access_mode, granted_by, granted_at)
			VALUES (?,?,?,?,?)`, username, g.DefinitionID, g.AccessMode, grantedBy, ts); err != nil {
			return err
		}
	}
	return nil
}

// UserGrants 返回某用户的全部定义授权。
func UserGrants(db *sql.DB, username string) ([]DefinitionGrant, error) {
	rows, err := db.Query(`SELECT definition_id, username, access_mode, granted_by, granted_at
		FROM etl_grants WHERE username = ? ORDER BY definition_id`, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DefinitionGrant{}
	for rows.Next() {
		var g DefinitionGrant
		if err := rows.Scan(&g.DefinitionID, &g.Username, &g.AccessMode, &g.GrantedBy, &g.GrantedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// AllGrantsByUser 批量返回所有用户的定义授权（admin 用户管理页用）。
func AllGrantsByUser(db *sql.DB) (map[string][]DefinitionGrant, error) {
	rows, err := db.Query(`SELECT definition_id, username, access_mode, granted_by, granted_at
		FROM etl_grants ORDER BY username, definition_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]DefinitionGrant{}
	for rows.Next() {
		var g DefinitionGrant
		if err := rows.Scan(&g.DefinitionID, &g.Username, &g.AccessMode, &g.GrantedBy, &g.GrantedAt); err != nil {
			return nil, err
		}
		out[g.Username] = append(out[g.Username], g)
	}
	return out, rows.Err()
}

// HasAnyGrant 返回用户是否持有任一定义授权（Console 功能开关下发用；admin 隐式全量不走这里）。
func HasAnyGrant(db *sql.DB, username string) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM etl_grants WHERE username = ?`, username).Scan(&n)
	return n > 0, err
}

// AccessMode 返回该用户在某定义上的授权模式；无授权返回空串（不区分 admin——调用方先判角色）。
func (s *Store) AccessMode(username, defID string) (string, error) {
	var mode string
	err := s.db.QueryRow(`SELECT access_mode FROM etl_grants WHERE username = ? AND definition_id = ?`,
		username, defID).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return mode, err
}

// GrantedModes 返回该用户全部定义授权：definition_id → mode。
func (s *Store) GrantedModes(username string) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT definition_id, access_mode FROM etl_grants WHERE username = ?`, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, mode string
		if err := rows.Scan(&id, &mode); err != nil {
			return nil, err
		}
		out[id] = mode
	}
	return out, rows.Err()
}

// HasAnyGrant 返回用户是否持有任一授权（含指定模式过滤；mode 为空则任意模式）。
func (s *Store) HasAnyGrant(username, mode string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM etl_grants WHERE username = ? AND (? = '' OR access_mode = ?)`,
		username, mode, mode).Scan(&n)
	return n > 0, err
}

// Grant 给用户补一条定义授权（复制/导入时新定义归属用）；已存在则按新模式覆盖。
func (s *Store) Grant(username, defID, mode, grantedBy string) error {
	_, err := s.db.Exec(`INSERT INTO etl_grants (username, definition_id, access_mode, granted_by, granted_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(username, definition_id) DO UPDATE SET access_mode=excluded.access_mode`,
		username, defID, mode, grantedBy, now())
	return err
}
