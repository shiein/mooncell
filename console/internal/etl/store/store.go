// Package store 是数据集成的持久化：与 Console 共用 SQLite 文件，表名统一 etl_ 前缀。
//
// 约束：Console 的 *sql.DB 只有 1 个连接（SQLite 写串行）。本包任何查询必须读完并关闭 rows
// 之后才能发起下一条语句，否则会自锁。
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound 表示记录不存在。
var ErrNotFound = errors.New("记录不存在")

// ErrConflict 表示乐观锁冲突（别人已保存了更新的版本）。
var ErrConflict = errors.New("内容已被其他人修改，请刷新后再保存")

// Store 封装 etl_* 表。
type Store struct{ db *sql.DB }

// New 包装数据库句柄（迁移见 Migrate）。
func New(db *sql.DB) *Store { return &Store{db: db} }

// DB 返回底层句柄。
func (s *Store) DB() *sql.DB { return s.db }

// NewID 生成带前缀的随机 ID。
func NewID(prefix string) string {
	b := make([]byte, 10)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func now() int64 { return time.Now().UnixMilli() }

// Migrate 建表（幂等）。schema 无条件迁移；功能开关只控制路由。
func Migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS etl_connections (
			id             TEXT PRIMARY KEY,
			name           TEXT NOT NULL,
			db_type        TEXT NOT NULL,
			host           TEXT NOT NULL DEFAULT '',
			port           INTEGER NOT NULL DEFAULT 0,
			database_name  TEXT NOT NULL DEFAULT '',
			default_schema TEXT NOT NULL DEFAULT '',
			username       TEXT NOT NULL DEFAULT '',
			ssl_mode       TEXT NOT NULL DEFAULT 'disable',
			options_json   TEXT NOT NULL DEFAULT '{}',
			secret_cipher  TEXT NOT NULL DEFAULT '',
			description    TEXT NOT NULL DEFAULT '',
			created_by     TEXT NOT NULL,
			created_at     INTEGER NOT NULL,
			updated_at     INTEGER NOT NULL,
			last_test_ok   INTEGER NOT NULL DEFAULT 0,
			last_test_msg  TEXT NOT NULL DEFAULT '',
			last_test_at   INTEGER NOT NULL DEFAULT 0
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_etl_connections_name ON etl_connections(LOWER(name));

		CREATE TABLE IF NOT EXISTS etl_grants (
			username      TEXT NOT NULL,
			definition_id TEXT NOT NULL,
			access_mode   TEXT NOT NULL CHECK(access_mode IN ('use','edit')),
			granted_by    TEXT NOT NULL DEFAULT '',
			granted_at    INTEGER NOT NULL,
			PRIMARY KEY (username, definition_id)
		);
		CREATE INDEX IF NOT EXISTS idx_etl_grants_def ON etl_grants(definition_id);

		CREATE TABLE IF NOT EXISTS etl_definitions (
			id          TEXT PRIMARY KEY,
			kind        TEXT NOT NULL,          -- pipeline | workflow
			name        TEXT NOT NULL,
			folder      TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			doc_json    TEXT NOT NULL,
			revision    INTEGER NOT NULL DEFAULT 1,
			created_by  TEXT NOT NULL,
			created_at  INTEGER NOT NULL,
			updated_by  TEXT NOT NULL,
			updated_at  INTEGER NOT NULL
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_etl_definitions_name ON etl_definitions(kind, LOWER(name));

		CREATE TABLE IF NOT EXISTS etl_definition_versions (
			definition_id TEXT NOT NULL,
			revision      INTEGER NOT NULL,
			doc_json      TEXT NOT NULL,
			saved_by      TEXT NOT NULL,
			saved_at      INTEGER NOT NULL,
			PRIMARY KEY (definition_id, revision)
		);

		CREATE TABLE IF NOT EXISTS etl_schedules (
			id           TEXT PRIMARY KEY,
			target_id    TEXT NOT NULL,
			name         TEXT NOT NULL DEFAULT '',
			cron         TEXT NOT NULL,
			timezone     TEXT NOT NULL DEFAULT '',
			enabled      INTEGER NOT NULL DEFAULT 1,
			overlap      TEXT NOT NULL DEFAULT 'skip',  -- skip | queue | parallel
			misfire      TEXT NOT NULL DEFAULT 'skip',  -- skip | run_once
			params_json  TEXT NOT NULL DEFAULT '{}',
			next_fire_at INTEGER NOT NULL DEFAULT 0,
			last_fire_at INTEGER NOT NULL DEFAULT 0,
			created_by   TEXT NOT NULL,
			created_at   INTEGER NOT NULL,
			updated_at   INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_etl_schedules_target ON etl_schedules(target_id);

		CREATE TABLE IF NOT EXISTS etl_runs (
			id                  TEXT PRIMARY KEY,
			kind                TEXT NOT NULL,
			definition_id       TEXT NOT NULL,
			definition_name     TEXT NOT NULL,
			revision            INTEGER NOT NULL,
			trigger             TEXT NOT NULL,          -- manual | schedule | resume | preview
			schedule_id         TEXT NOT NULL DEFAULT '',
			resume_of           TEXT NOT NULL DEFAULT '',
			status              TEXT NOT NULL,          -- queued | running | succeeded | failed | canceled | interrupted
			params_json         TEXT NOT NULL DEFAULT '{}',
			snapshot_json       TEXT NOT NULL,
			error               TEXT NOT NULL DEFAULT '',
			triggered_by        TEXT NOT NULL DEFAULT '',
			rows_read           INTEGER NOT NULL DEFAULT 0,
			rows_written        INTEGER NOT NULL DEFAULT 0,
			log_count           INTEGER NOT NULL DEFAULT 0,
			log_dropped         INTEGER NOT NULL DEFAULT 0,
			queued_at           INTEGER NOT NULL,
			started_at          INTEGER NOT NULL DEFAULT 0,
			finished_at         INTEGER NOT NULL DEFAULT 0,
			refs_json           TEXT NOT NULL DEFAULT '{}'  -- 运行快照引用的定义文档（作业→转换）
		);
		CREATE INDEX IF NOT EXISTS idx_etl_runs_def ON etl_runs(definition_id, queued_at DESC);
		CREATE INDEX IF NOT EXISTS idx_etl_runs_status ON etl_runs(status);
		CREATE INDEX IF NOT EXISTS idx_etl_runs_queued ON etl_runs(queued_at DESC);

		CREATE TABLE IF NOT EXISTS etl_run_nodes (
			run_id           TEXT NOT NULL,
			node_id          TEXT NOT NULL,
			node_name        TEXT NOT NULL DEFAULT '',
			node_type        TEXT NOT NULL DEFAULT '',
			parent_node      TEXT NOT NULL DEFAULT '',  -- 作业中「运行转换」节点下的步骤：其所属作业项 ID
			status           TEXT NOT NULL,
			attempt          INTEGER NOT NULL DEFAULT 1,
			metrics_json     TEXT NOT NULL DEFAULT '{}',
			output_vars_json TEXT NOT NULL DEFAULT '{}',
			port             TEXT NOT NULL DEFAULT '',  -- 作业项结束时激活的输出端口
			error            TEXT NOT NULL DEFAULT '',
			samples_json     TEXT NOT NULL DEFAULT '{}', -- error 端口拒绝行采样（前 N 条）
			started_at       INTEGER NOT NULL DEFAULT 0,
			finished_at      INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (run_id, parent_node, node_id)
		);

		CREATE TABLE IF NOT EXISTS etl_run_logs (
			run_id  TEXT NOT NULL,
			seq     INTEGER NOT NULL,
			ts      INTEGER NOT NULL,
			level   TEXT NOT NULL,
			node_id TEXT NOT NULL DEFAULT '',
			message TEXT NOT NULL,
			PRIMARY KEY (run_id, seq)
		);

		CREATE TABLE IF NOT EXISTS etl_state (
			scope      TEXT NOT NULL,
			key        TEXT NOT NULL,
			value_json TEXT NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (scope, key)
		);
	`)
	if err != nil {
		return err
	}
	// 旧库可能缺少的列：幂等补齐。
	if err := ensureColumn(db, "etl_runs", "refs_json",
		`ALTER TABLE etl_runs ADD COLUMN refs_json TEXT NOT NULL DEFAULT '{}'`); err != nil {
		return err
	}
	return ensureColumn(db, "etl_run_nodes", "samples_json",
		`ALTER TABLE etl_run_nodes ADD COLUMN samples_json TEXT NOT NULL DEFAULT '{}'`)
}

// ensureColumn 幂等补齐旧库缺失的列。
func ensureColumn(db *sql.DB, table, column, alterSQL string) error {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(alterSQL)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
