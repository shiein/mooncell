package main

import (
	"database/sql"
	"log"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite" // 纯 Go sqlite 驱动,无 CGO,利于单二进制
)

type Store struct {
	db  *sql.DB
	ttl time.Duration
}

func openDB(cfg *Config) *Store {
	db, err := sql.Open("sqlite", cfg.Database.Path)
	if err != nil {
		log.Fatalf("[db] 打开数据库失败: %v", err)
	}
	// sqlite 写串行,限制连接数避免 "database is locked"
	db.SetMaxOpenConns(1)

	migrateDeploys(db) // 旧库 deploys(单列 release_id 主键)→ 复合主键前的一次性迁移

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			username      TEXT    UNIQUE NOT NULL,
			password_hash TEXT    NOT NULL,
			role          TEXT    NOT NULL DEFAULT 'admin',
			created_at    INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS sessions (
			token      TEXT    PRIMARY KEY,
			username   TEXT    NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);
		CREATE TABLE IF NOT EXISTS entities (
			seq  INTEGER PRIMARY KEY AUTOINCREMENT,
			kind TEXT NOT NULL,
			id   TEXT NOT NULL,
			data TEXT NOT NULL,
			UNIQUE(kind, id)
		);
		CREATE TABLE IF NOT EXISTS agents (
			id         TEXT    PRIMARY KEY,
			name       TEXT    NOT NULL,
			addr       TEXT    NOT NULL,
			token      TEXT    NOT NULL,
			created_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS deploys (
			op          TEXT    NOT NULL,
			app_id      TEXT    NOT NULL,
			release_id  TEXT    NOT NULL,
			result      TEXT    NOT NULL,
			fingerprint TEXT    NOT NULL DEFAULT '',
			created_at  INTEGER NOT NULL,
			PRIMARY KEY (op, app_id, release_id)
		);
		CREATE TABLE IF NOT EXISTS metrics (
			agent_id TEXT    NOT NULL,
			ts       INTEGER NOT NULL,
			cpu      REAL,
			mem      REAL,
			disk     REAL
		);
		CREATE INDEX IF NOT EXISTS idx_metrics_agent_ts ON metrics(agent_id, ts);
		CREATE TABLE IF NOT EXISTS artifacts (
			id         TEXT    PRIMARY KEY,
			name       TEXT    NOT NULL,
			version    TEXT    NOT NULL DEFAULT '',
			sha256     TEXT    NOT NULL,
			size       INTEGER NOT NULL DEFAULT 0,
			uploader   TEXT    NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_artifacts_sha ON artifacts(sha256);
	`); err != nil {
		log.Fatalf("[db] 建表失败: %v", err)
	}

	return &Store{db: db, ttl: time.Duration(cfg.Session.TTLHours) * time.Hour}
}

func (s *Store) Close() error { return s.db.Close() }

// AgentRow 是注册的远端 Agent(token 仅服务端用,列表对外时置空)。
type AgentRow struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Addr      string `json:"addr"`
	Token     string `json:"token,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}

func (s *Store) listAgents() ([]AgentRow, error) {
	rows, err := s.db.Query("SELECT id, name, addr, created_at FROM agents ORDER BY created_at")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentRow{}
	for rows.Next() {
		var a AgentRow
		if err := rows.Scan(&a.ID, &a.Name, &a.Addr, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a) // 不含 token
	}
	return out, rows.Err()
}

func (s *Store) getAgent(id string) (AgentRow, bool) {
	var a AgentRow
	if err := s.db.QueryRow("SELECT id, name, addr, token, created_at FROM agents WHERE id = ?", id).
		Scan(&a.ID, &a.Name, &a.Addr, &a.Token, &a.CreatedAt); err != nil {
		return AgentRow{}, false
	}
	return a, true
}

func (s *Store) addAgent(id, name, addr, token string) error {
	_, err := s.db.Exec(
		"INSERT INTO agents (id, name, addr, token, created_at) VALUES (?, ?, ?, ?, ?)",
		id, name, addr, token, time.Now().UnixMilli(),
	)
	return err
}

func (s *Store) deleteAgent(id string) error {
	_, err := s.db.Exec("DELETE FROM agents WHERE id = ?", id)
	return err
}

// migrateDeploys 把旧的「单列 release_id 主键」deploys 表迁移到复合主键前清理:
// 旧表无 op 列时直接 DROP(幂等去重记录非持久业务数据,丢弃可接受),由后续 CREATE 重建新结构。
func migrateDeploys(db *sql.DB) {
	rows, err := db.Query(`PRAGMA table_info(deploys)`)
	if err != nil {
		return // 表尚不存在(全新库):无需迁移
	}
	defer rows.Close()
	hasOp, hasFp, hasAny := false, false, false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return
		}
		hasAny = true
		switch name {
		case "op":
			hasOp = true
		case "fingerprint":
			hasFp = true
		}
	}
	if hasAny && !hasOp {
		db.Exec(`DROP TABLE deploys`)
		log.Printf("[db] 迁移 deploys → 复合主键(op,app_id,release_id),旧去重记录已清空")
		return
	}
	if hasAny && hasOp && !hasFp {
		// 已是复合主键但缺 fingerprint 列:补列(旧记录指纹为空,命中比对时视为不一致 → 放行给 Agent 裁决)。
		db.Exec(`ALTER TABLE deploys ADD COLUMN fingerprint TEXT NOT NULL DEFAULT ''`)
		log.Printf("[db] 迁移 deploys → 增加 fingerprint 列")
	}
}

// getDeploy 返回某 (op,appId,releaseId) 已记录的结果与指纹(幂等:同操作+同 app+同 release)。
// 按操作与应用隔离——部署/还原、不同 app 复用同一 releaseId 不会互相误命中。
// 调用方还需比对指纹:仅当指纹一致才短路,否则放行给 Agent 做最终裁决。
func (s *Store) getDeploy(op, appID, releaseID string) (result, fingerprint string, ok bool) {
	if err := s.db.QueryRow(
		"SELECT result, fingerprint FROM deploys WHERE op = ? AND app_id = ? AND release_id = ?",
		op, appID, releaseID,
	).Scan(&result, &fingerprint); err != nil {
		return "", "", false
	}
	return result, fingerprint, true
}

func (s *Store) putDeploy(op, appID, releaseID, result, fingerprint string) {
	s.db.Exec(
		`INSERT INTO deploys (op, app_id, release_id, result, fingerprint, created_at) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(op, app_id, release_id) DO UPDATE SET result = excluded.result, fingerprint = excluded.fingerprint, created_at = excluded.created_at`,
		op, appID, releaseID, result, fingerprint, time.Now().UnixMilli(),
	)
}

// seedAdmin 仅在用户表为空时种入默认管理员(bcrypt)。
func (s *Store) seedAdmin(username, password string) {
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&n); err != nil {
		log.Printf("[db] 统计用户失败: %v", err)
		return
	}
	if n > 0 {
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("[db] 口令哈希失败: %v", err)
		return
	}
	if _, err := s.db.Exec(
		"INSERT INTO users (username, password_hash, role, created_at) VALUES (?, ?, ?, ?)",
		username, string(hash), "admin", time.Now().UnixMilli(),
	); err != nil {
		log.Printf("[db] 种子管理员失败: %v", err)
		return
	}
	// 不打印明文口令:journal 会长期留存;初始密码由 install.sh 首次安装摘要一次性输出。
	log.Printf("[db] 已创建默认管理员: %s(初始口令见安装摘要 / config.toml)", username)
}

// UserInfo 是用户列表对外形态(不含口令哈希)。
type UserInfo struct {
	Username  string `json:"username"`
	Role      string `json:"role"`
	CreatedAt int64  `json:"createdAt"`
}

func (s *Store) userRole(username string) string {
	var role string
	if err := s.db.QueryRow("SELECT role FROM users WHERE username = ?", username).Scan(&role); err != nil {
		return ""
	}
	return role
}

func (s *Store) listUsers() ([]UserInfo, error) {
	rows, err := s.db.Query("SELECT username, role, created_at FROM users ORDER BY created_at")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserInfo{}
	for rows.Next() {
		var u UserInfo
		if err := rows.Scan(&u.Username, &u.Role, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) createUser(username, password, role string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		"INSERT INTO users (username, password_hash, role, created_at) VALUES (?, ?, ?, ?)",
		username, string(hash), role, time.Now().UnixMilli(),
	)
	return err // UNIQUE 冲突 → 用户名已存在
}

// deleteUser 删除用户并清其会话。
func (s *Store) deleteUser(username string) error {
	_, err := s.db.Exec("DELETE FROM users WHERE username = ?", username)
	s.db.Exec("DELETE FROM sessions WHERE username = ?", username)
	return err
}

// countAdmins 用于防止删掉最后一个管理员。
func (s *Store) countAdmins() int {
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM users WHERE role = 'admin'").Scan(&n)
	return n
}

func (s *Store) verifyUser(username, password string) bool {
	var hash string
	if err := s.db.QueryRow("SELECT password_hash FROM users WHERE username = ?", username).Scan(&hash); err != nil {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func (s *Store) createSession(username string) (string, time.Time) {
	token := randomToken()
	now := time.Now()
	exp := now.Add(s.ttl)
	if _, err := s.db.Exec(
		"INSERT INTO sessions (token, username, created_at, expires_at) VALUES (?, ?, ?, ?)",
		token, username, now.UnixMilli(), exp.UnixMilli(),
	); err != nil {
		log.Printf("[db] 写入会话失败: %v", err)
	}
	return token, exp
}

// sessionAbsoluteMax 是会话绝对最长生命周期:即便持续有动作滑动续期,从登录起超过即失效、需重新登录(纵深防御)。
const sessionAbsoluteMax = 12 * time.Hour

// userByToken 仅校验会话有效性(闲置过期 / 超绝对寿命即清理并失败),不做续期。
// 续期由 touchSession 单独负责,且只在"用户动作"请求上调用——避免后台轮询把闲置会话续命。
func (s *Store) userByToken(token string) (string, bool) {
	var username string
	var created, exp int64
	if err := s.db.QueryRow("SELECT username, created_at, expires_at FROM sessions WHERE token = ?", token).Scan(&username, &created, &exp); err != nil {
		return "", false
	}
	now := time.Now().UnixMilli()
	if exp < now || now-created > sessionAbsoluteMax.Milliseconds() {
		s.db.Exec("DELETE FROM sessions WHERE token = ?", token)
		return "", false
	}
	return username, true
}

// touchSession 滑动续期(idle timeout):把过期推到 now+ttl。仅供"用户动作"请求调用。
// 节流写在 SQL 里(距上次续期满 1 分钟才更新),避免高频请求写放大。
func (s *Store) touchSession(token string) {
	if s.ttl <= time.Minute {
		return
	}
	now := time.Now()
	s.db.Exec("UPDATE sessions SET expires_at = ? WHERE token = ? AND expires_at <= ?",
		now.Add(s.ttl).UnixMilli(), token, now.Add(s.ttl-time.Minute).UnixMilli())
}

func (s *Store) deleteSession(token string) {
	s.db.Exec("DELETE FROM sessions WHERE token = ?", token)
}
