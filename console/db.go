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
	`); err != nil {
		log.Fatalf("[db] 建表失败: %v", err)
	}

	return &Store{db: db, ttl: time.Duration(cfg.Session.TTLHours) * time.Hour}
}

func (s *Store) Close() error { return s.db.Close() }

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
	log.Printf("[db] 已创建默认管理员: %s / %s", username, password)
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

// userByToken 返回会话用户名;顺手清理过期会话。
func (s *Store) userByToken(token string) (string, bool) {
	var username string
	var exp int64
	if err := s.db.QueryRow("SELECT username, expires_at FROM sessions WHERE token = ?", token).Scan(&username, &exp); err != nil {
		return "", false
	}
	if exp < time.Now().UnixMilli() {
		s.db.Exec("DELETE FROM sessions WHERE token = ?", token)
		return "", false
	}
	return username, true
}

func (s *Store) deleteSession(token string) {
	s.db.Exec("DELETE FROM sessions WHERE token = ?", token)
}
