package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"mooncell/console/internal/etl/conn"
)

// Connection 是连接记录。SecretCipher 永不出现在 API 响应中。
type Connection struct {
	conn.Config
	Description  string `json:"description"`
	SecretCipher string `json:"-"`
	HasPassword  bool   `json:"hasPassword"`
	CreatedBy    string `json:"createdBy"`
	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
	LastTestOK   bool   `json:"lastTestOk"`
	LastTestMsg  string `json:"lastTestMsg"`
	LastTestAt   int64  `json:"lastTestAt"`
}

const connCols = `id, name, db_type, host, port, database_name, default_schema, username, ssl_mode, options_json,
	secret_cipher, description, created_by, created_at, updated_at, last_test_ok, last_test_msg, last_test_at`

func scanConn(sc interface{ Scan(...any) error }) (Connection, error) {
	var c Connection
	var opts string
	var ok int
	err := sc.Scan(&c.ID, &c.Name, &c.DBType, &c.Host, &c.Port, &c.Database, &c.DefaultSchema, &c.Username, &c.SSLMode, &opts,
		&c.SecretCipher, &c.Description, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt, &ok, &c.LastTestMsg, &c.LastTestAt)
	if err != nil {
		return c, err
	}
	_ = json.Unmarshal([]byte(opts), &c.Options)
	c.LastTestOK = ok == 1
	c.HasPassword = c.SecretCipher != ""
	return c, nil
}

// ListConnections 按名称返回全部连接。
func (s *Store) ListConnections() ([]Connection, error) {
	rows, err := s.db.Query(`SELECT ` + connCols + ` FROM etl_connections ORDER BY LOWER(name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Connection{}
	for rows.Next() {
		c, err := scanConn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetConnection 取单个连接。
func (s *Store) GetConnection(id string) (Connection, error) {
	c, err := scanConn(s.db.QueryRow(`SELECT `+connCols+` FROM etl_connections WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// CreateConnectionWithID 新建连接；c.ID 非空时沿用（密文的 AAD 已绑定该 ID）。
func (s *Store) CreateConnectionWithID(c Connection, secretCipher *string, user string) (Connection, error) {
	if c.ID == "" {
		c.ID = NewID("conn_")
	}
	opts, _ := json.Marshal(c.Options)
	ts := now()
	cipher := ""
	if secretCipher != nil {
		cipher = *secretCipher
	}
	_, err := s.db.Exec(`INSERT INTO etl_connections (`+connCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,'',0)`,
		c.ID, c.Name, c.DBType, c.Host, c.Port, c.Database, c.DefaultSchema, c.Username, c.SSLMode, string(opts),
		cipher, c.Description, user, ts, ts)
	if err != nil {
		return c, uniqueErr(err, "连接名称已存在")
	}
	return s.GetConnection(c.ID)
}

// SaveConnection 更新已有连接。secretCipher 为 nil 表示保持原密码。
func (s *Store) SaveConnection(c Connection, secretCipher *string, user string) (Connection, error) {
	if c.ID == "" {
		return s.CreateConnectionWithID(c, secretCipher, user)
	}
	opts, _ := json.Marshal(c.Options)
	ts := now()
	q := `UPDATE etl_connections SET name=?, db_type=?, host=?, port=?, database_name=?, default_schema=?, username=?,
		ssl_mode=?, options_json=?, description=?, updated_at=?, last_test_ok=0, last_test_msg='', last_test_at=0`
	args := []any{c.Name, c.DBType, c.Host, c.Port, c.Database, c.DefaultSchema, c.Username, c.SSLMode, string(opts), c.Description, ts}
	if secretCipher != nil {
		q += `, secret_cipher=?`
		args = append(args, *secretCipher)
	}
	q += ` WHERE id=?`
	args = append(args, c.ID)
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return c, uniqueErr(err, "连接名称已存在")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return c, ErrNotFound
	}
	return s.GetConnection(c.ID)
}

// SetConnectionTest 记录测试结果。
func (s *Store) SetConnectionTest(id string, ok bool, msg string) error {
	_, err := s.db.Exec(`UPDATE etl_connections SET last_test_ok=?, last_test_msg=?, last_test_at=? WHERE id=?`, boolInt(ok), msg, now(), id)
	return err
}

// DeleteConnection 删除连接。
func (s *Store) DeleteConnection(id string) error {
	res, err := s.db.Exec(`DELETE FROM etl_connections WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// References 返回文档中引用了该 ID（连接或转换）的定义名，删除前提示用。按 JSON 文本匹配。
func (s *Store) References(id string) ([]string, error) {
	return s.referenceCol(id, "name")
}

// ReferenceIDs 返回引用方定义 ID（force 删除的授权校验用）。
func (s *Store) ReferenceIDs(id string) ([]string, error) {
	return s.referenceCol(id, "id")
}

func (s *Store) referenceCol(id, col string) ([]string, error) {
	rows, err := s.db.Query(`SELECT `+col+` FROM etl_definitions WHERE doc_json LIKE ? ORDER BY name`, `%"`+strings.ReplaceAll(id, `%`, ``)+`"%`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func uniqueErr(err error, msg string) error {
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
		return errors.New(msg)
	}
	return err
}
