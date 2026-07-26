// 资源、授权、传输元数据 CRUD。
// SQLite MaxOpenConns=1：任何 Query 必须在发起下一条前关闭 rows。
package serverops

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// --- 校验 ---

// validateResourceInput 校验 name/host/port/username。
// host 只接受合法 IP 或规范化主机名；不接受 URL、路径、控制字符。
func validateResourceInput(in *ResourceInput) error {
	in.Name = strings.TrimSpace(in.Name)
	in.Host = strings.TrimSpace(in.Host)
	in.Username = strings.TrimSpace(in.Username)

	if in.Name == "" {
		return apiErr(CodeValidation, "服务器名称不能为空", false)
	}
	if len(in.Name) > 128 {
		return apiErr(CodeValidation, "服务器名称过长", false)
	}
	if hasControl(in.Name) {
		return apiErr(CodeValidation, "服务器名称含非法字符", false)
	}
	if err := validateHost(in.Host); err != nil {
		return err
	}
	if in.Port < 1 || in.Port > 65535 {
		return apiErr(CodeValidation, "端口必须在 1–65535", false)
	}
	if in.Username == "" {
		return apiErr(CodeValidation, "SSH 用户名不能为空", false)
	}
	if len(in.Username) > 64 {
		return apiErr(CodeValidation, "SSH 用户名过长", false)
	}
	if hasControl(in.Username) || strings.ContainsAny(in.Username, " \t\r\n") {
		return apiErr(CodeValidation, "SSH 用户名含非法字符", false)
	}
	return nil
}

func validateHost(host string) error {
	if host == "" {
		return apiErr(CodeValidation, "主机地址不能为空", false)
	}
	if len(host) > 253 {
		return apiErr(CodeValidation, "主机地址过长", false)
	}
	// 拒绝 URL/路径/用户信息。
	if strings.ContainsAny(host, "/\\@:?#") {
		return apiErr(CodeValidation, "主机地址不能包含 URL 或路径字符", false)
	}
	if hasControl(host) {
		return apiErr(CodeValidation, "主机地址含非法字符", false)
	}
	// IPv6 字面量允许方括号外的裸 IPv6（JoinHostPort 会处理）。
	// 简单 hostname 规则：字母数字、点、连字符；或 IPv4/IPv6。
	for _, r := range host {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == ':' || r == '[' || r == ']' {
			continue
		}
		return apiErr(CodeValidation, "主机地址含非法字符", false)
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 32 || r == 127 {
			return true
		}
	}
	return false
}

// --- 资源 CRUD ---

func createResource(db *sql.DB, r ServerResource) error {
	_, err := db.Exec(`INSERT INTO server_resources
		(id, name, host, port, username, host_key_algorithm, host_key_sha256, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Name, r.Host, r.Port, r.Username,
		r.HostKeyAlgorithm, r.HostKeySHA256, r.CreatedBy, r.CreatedAt, r.UpdatedAt)
	if err != nil && isUniqueConstraint(err) {
		return apiErr(CodeNameDuplicate, "服务器名称已存在", false)
	}
	return err
}

func getResource(db *sql.DB, id string) (ServerResource, bool, error) {
	var r ServerResource
	err := db.QueryRow(`SELECT id, name, host, port, username, host_key_algorithm, host_key_sha256,
		created_by, created_at, updated_at FROM server_resources WHERE id = ?`, id).Scan(
		&r.ID, &r.Name, &r.Host, &r.Port, &r.Username, &r.HostKeyAlgorithm, &r.HostKeySHA256,
		&r.CreatedBy, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

func listAllResources(db *sql.DB) ([]ServerResource, error) {
	rows, err := db.Query(`SELECT id, name, host, port, username, host_key_algorithm, host_key_sha256,
		created_by, created_at, updated_at FROM server_resources ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanResources(rows)
}

func listGrantedResources(db *sql.DB, username string) ([]ServerResource, error) {
	rows, err := db.Query(`SELECT r.id, r.name, r.host, r.port, r.username, r.host_key_algorithm, r.host_key_sha256,
		r.created_by, r.created_at, r.updated_at
		FROM server_resources r
		INNER JOIN server_resource_grants g ON g.resource_id = r.id AND g.username = ?
		ORDER BY r.created_at`, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanResources(rows)
}

func scanResources(rows *sql.Rows) ([]ServerResource, error) {
	var out []ServerResource
	for rows.Next() {
		var r ServerResource
		if err := rows.Scan(&r.ID, &r.Name, &r.Host, &r.Port, &r.Username, &r.HostKeyAlgorithm, &r.HostKeySHA256,
			&r.CreatedBy, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// updateResource 更新连接字段。host/port/username 变更时清空 host key（须重新确认）。
// expectedUpdatedAt 用于 CAS：不一致返回 RESOURCE_CHANGED。
func updateResource(db *sql.DB, id string, in ResourceInput, clearHostKey bool, expectedUpdatedAt int64, now int64) error {
	var res sql.Result
	var err error
	if clearHostKey {
		res, err = db.Exec(`UPDATE server_resources SET name=?, host=?, port=?, username=?,
			host_key_algorithm='', host_key_sha256='', updated_at=?
			WHERE id=? AND updated_at=?`,
			in.Name, in.Host, in.Port, in.Username, now, id, expectedUpdatedAt)
	} else {
		res, err = db.Exec(`UPDATE server_resources SET name=?, host=?, port=?, username=?, updated_at=?
			WHERE id=? AND updated_at=?`,
			in.Name, in.Host, in.Port, in.Username, now, id, expectedUpdatedAt)
	}
	if err != nil {
		if isUniqueConstraint(err) {
			return apiErr(CodeNameDuplicate, "服务器名称已存在", false)
		}
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return apiErr(CodeResourceChanged, "资源配置已变更，请刷新后重试", true)
	}
	return nil
}

// confirmHostKey CAS 写入指纹：expectedUpdatedAt 必须匹配。
func confirmHostKey(db *sql.DB, id, algo, sha256 string, expectedUpdatedAt, now int64) error {
	res, err := db.Exec(`UPDATE server_resources SET host_key_algorithm=?, host_key_sha256=?, updated_at=?
		WHERE id=? AND updated_at=?`, algo, sha256, now, id, expectedUpdatedAt)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return apiErr(CodeResourceChanged, "资源配置已变更，请重新探测后确认", true)
	}
	return nil
}

func deleteResource(db *sql.DB, id string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`DELETE FROM server_file_transfer_chunks
		 WHERE transfer_id IN (SELECT id FROM server_file_transfers WHERE resource_id = ?)`, id); err != nil {
		return err
	}
	for _, q := range []string{
		"DELETE FROM server_resource_grants WHERE resource_id = ?",
		"DELETE FROM server_file_transfers WHERE resource_id = ?",
		"DELETE FROM server_resources WHERE id = ?",
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// --- 授权 ---

// SetUserGrantsTx 在事务中全量替换用户的服务器运维授权。
// 供 consoleapp 用户管理与密码/应用/数据资源授权同事务提交。
func SetUserGrantsTx(tx *sql.Tx, username string, grants []ServerResourceGrant, grantedBy string) error {
	if _, err := tx.Exec("DELETE FROM server_resource_grants WHERE username = ?", username); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	seen := map[string]bool{}
	for _, g := range grants {
		rid := strings.TrimSpace(g.ResourceID)
		if rid == "" || seen[rid] {
			continue
		}
		seen[rid] = true
		// 资源必须存在。
		var n int
		if err := tx.QueryRow("SELECT COUNT(*) FROM server_resources WHERE id = ?", rid).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("服务器资源不存在: %s", rid)
		}
		if _, err := tx.Exec(
			`INSERT INTO server_resource_grants (resource_id, username, granted_by, granted_at) VALUES (?, ?, ?, ?)`,
			rid, username, grantedBy, now); err != nil {
			return err
		}
	}
	return nil
}

// UserGrants 返回某用户全部服务器授权。
func UserGrants(db *sql.DB, username string) ([]ServerResourceGrant, error) {
	rows, err := db.Query(
		`SELECT resource_id, username, granted_by, granted_at FROM server_resource_grants WHERE username = ? ORDER BY resource_id`,
		username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServerResourceGrant
	for rows.Next() {
		var g ServerResourceGrant
		if err := rows.Scan(&g.ResourceID, &g.Username, &g.GrantedBy, &g.GrantedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// AllGrantsByUser 批量返回所有用户的服务器授权（用户管理列表用）。
func AllGrantsByUser(db *sql.DB) (map[string][]ServerResourceGrant, error) {
	rows, err := db.Query(
		`SELECT resource_id, username, granted_by, granted_at FROM server_resource_grants ORDER BY username, resource_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]ServerResourceGrant{}
	for rows.Next() {
		var g ServerResourceGrant
		if err := rows.Scan(&g.ResourceID, &g.Username, &g.GrantedBy, &g.GrantedAt); err != nil {
			return nil, err
		}
		out[g.Username] = append(out[g.Username], g)
	}
	return out, rows.Err()
}

// UserHasGrant 判断普通用户是否有该资源授权。admin 调用方应自行短路。
func UserHasGrant(db *sql.DB, username, resourceID string) (bool, error) {
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM server_resource_grants WHERE username = ? AND resource_id = ?`,
		username, resourceID).Scan(&n)
	return n > 0, err
}

// ListResourceIDs 返回全部资源 ID（用户管理选择器用）。
func ListResourceIDs(db *sql.DB) ([]ServerResource, error) {
	return listAllResources(db)
}

// --- 传输元数据 ---

func insertTransfer(db *sql.DB, t FileTransfer) error {
	ow := 0
	if t.Overwrite {
		ow = 1
	}
	_, err := db.Exec(`INSERT INTO server_file_transfers
		(id, resource_id, username, direction, remote_path, remote_temp_path,
		 expected_size, transferred_size, overwrite, state, created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ResourceID, t.Username, t.Direction, t.RemotePath, t.RemoteTempPath,
		t.ExpectedSize, t.TransferredSize, ow, t.State, t.CreatedAt, t.UpdatedAt, t.ExpiresAt)
	return err
}

func scanTransfer(scanner interface {
	Scan(dest ...any) error
}) (FileTransfer, error) {
	var t FileTransfer
	var ow int
	err := scanner.Scan(
		&t.ID, &t.ResourceID, &t.Username, &t.Direction, &t.RemotePath, &t.RemoteTempPath,
		&t.ExpectedSize, &t.TransferredSize, &ow, &t.State, &t.CreatedAt, &t.UpdatedAt, &t.ExpiresAt)
	t.Overwrite = ow != 0
	return t, err
}

const transferSelectCols = `id, resource_id, username, direction, remote_path, remote_temp_path,
		expected_size, transferred_size, overwrite, state, created_at, updated_at, expires_at`

func getTransfer(db *sql.DB, id string) (FileTransfer, bool, error) {
	row := db.QueryRow(`SELECT `+transferSelectCols+` FROM server_file_transfers WHERE id = ?`, id)
	t, err := scanTransfer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return t, false, nil
	}
	return t, err == nil, err
}

// recordTransferChunk 在同一事务中持久化块身份并推进权威 offset。
// 只有远端完整写入后才能调用；事务失败时调用方须 truncate 远端 part 回旧 offset。
func recordTransferChunk(db *sql.DB, id string, offset, size int64, sha256 string, transferred, now int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO server_file_transfer_chunks(transfer_id, chunk_offset, chunk_size, sha256)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(transfer_id, chunk_offset)
		 DO UPDATE SET chunk_size=excluded.chunk_size, sha256=excluded.sha256`,
		id, offset, size, sha256); err != nil {
		return err
	}
	res, err := tx.Exec(
		`UPDATE server_file_transfers
		 SET transferred_size=?, updated_at=?
		 WHERE id=? AND state=? AND transferred_size=?`,
		transferred, now, id, TransferUploading, offset)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return apiErr(CodeResourceChanged, "传输状态或 offset 已变化", true)
	}
	return tx.Commit()
}

func listTransferChunks(db *sql.DB, transferID string) ([]UploadChunkProof, error) {
	rows, err := db.Query(
		`SELECT chunk_offset, chunk_size, sha256
		 FROM server_file_transfer_chunks
		 WHERE transfer_id=?
		 ORDER BY chunk_offset`, transferID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UploadChunkProof
	for rows.Next() {
		var p UploadChunkProof
		if err := rows.Scan(&p.Offset, &p.Size, &p.SHA256); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func updateTransferState(db *sql.DB, id, state string, now int64) error {
	_, err := db.Exec(`UPDATE server_file_transfers SET state=?, updated_at=? WHERE id=?`, state, now, id)
	return err
}

func updateTransferStateFrom(db *sql.DB, id, from, to string, now int64) (bool, error) {
	res, err := db.Exec(
		`UPDATE server_file_transfers SET state=?, updated_at=? WHERE id=? AND state=?`,
		to, now, id, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// listActiveTransfers 列出用户在资源上可续传的 uploading 记录（不含 cleanup_pending）。
func listActiveTransfers(db *sql.DB, username, resourceID string) ([]FileTransfer, error) {
	rows, err := db.Query(`SELECT `+transferSelectCols+`
		FROM server_file_transfers
		WHERE username=? AND resource_id=? AND state=? AND expires_at > ?
		ORDER BY created_at`,
		username, resourceID, TransferUploading, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileTransfer
	for rows.Next() {
		t, err := scanTransfer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// listRecoverableTransfers 列出可在已认证 SFTP 会话中清理或核对的记录。
func listRecoverableTransfers(db *sql.DB, resourceID string) ([]FileTransfer, error) {
	rows, err := db.Query(`SELECT `+transferSelectCols+`
		FROM server_file_transfers
		WHERE resource_id=? AND state IN (?, ?) ORDER BY created_at`,
		resourceID, TransferCleanupPending, TransferCompleting)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileTransfer
	for rows.Next() {
		t, err := scanTransfer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func countCleanupPending(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM server_file_transfers WHERE state = ?`, TransferCleanupPending).Scan(&n)
	return n, err
}

func isUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	// 仅 UNIQUE 冲突视为重名；不可把 CHECK 等 constraint 误报成名称重复。
	return strings.Contains(s, "unique")
}
