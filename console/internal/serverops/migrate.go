// 模块表迁移：幂等 CREATE TABLE IF NOT EXISTS。
// 必须在 Console 启动时无条件执行（早于用户管理接口），feature flag 只控制路由与运行时。
package serverops

import "database/sql"

// Migrate 创建 server_resources / grants / transfers 三张表。
// 表结构明确不含 password、private_key 等凭据列。
func Migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS server_resources (
			id                  TEXT    PRIMARY KEY,
			name                TEXT    NOT NULL,
			host                TEXT    NOT NULL,
			port                INTEGER NOT NULL,
			username            TEXT    NOT NULL,
			host_key_algorithm  TEXT    NOT NULL DEFAULT '',
			host_key_sha256     TEXT    NOT NULL DEFAULT '',
			created_by          TEXT    NOT NULL,
			created_at          INTEGER NOT NULL,
			updated_at          INTEGER NOT NULL,
			CHECK (port >= 1 AND port <= 65535)
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_server_resources_name_lower
			ON server_resources(LOWER(name));

		CREATE TABLE IF NOT EXISTS server_resource_grants (
			resource_id  TEXT    NOT NULL,
			username     TEXT    NOT NULL,
			granted_by   TEXT    NOT NULL,
			granted_at   INTEGER NOT NULL,
			PRIMARY KEY (resource_id, username)
		);
		CREATE INDEX IF NOT EXISTS idx_server_resource_grants_username
			ON server_resource_grants(username);

		CREATE TABLE IF NOT EXISTS server_file_transfers (
			id                TEXT    PRIMARY KEY,
			resource_id       TEXT    NOT NULL,
			username          TEXT    NOT NULL,
			direction         TEXT    NOT NULL,
			remote_path       TEXT    NOT NULL,
			remote_temp_path  TEXT    NOT NULL DEFAULT '',
			expected_size     INTEGER NOT NULL,
			transferred_size  INTEGER NOT NULL DEFAULT 0,
			state             TEXT    NOT NULL,
			created_at        INTEGER NOT NULL,
			updated_at        INTEGER NOT NULL,
			expires_at        INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_server_transfers_owner
			ON server_file_transfers(username, resource_id, state);
	`)
	return err
}
