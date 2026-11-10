package store

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// Definition 是转换/作业定义记录。Doc 是完整 JSON 文档（pipeline.Definition / workflow.Definition）。
type Definition struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Name        string          `json:"name"`
	Folder      string          `json:"folder"`
	Description string          `json:"description"`
	Doc         json.RawMessage `json:"doc,omitempty"`
	Revision    int             `json:"revision"`
	CreatedBy   string          `json:"createdBy"`
	CreatedAt   int64           `json:"createdAt"`
	UpdatedBy   string          `json:"updatedBy"`
	UpdatedAt   int64           `json:"updatedAt"`
}

// DefinitionSummary 是列表项：不含文档，带最近运行与调度信息。
type DefinitionSummary struct {
	Definition
	NodeCount     int    `json:"nodeCount"`
	LastRunID     string `json:"lastRunId,omitempty"`
	LastRunStatus string `json:"lastRunStatus,omitempty"`
	LastRunAt     int64  `json:"lastRunAt,omitempty"`
	ScheduleCount int    `json:"scheduleCount"`
	NextFireAt    int64  `json:"nextFireAt,omitempty"`
}

// keepVersions 是每个定义保留的历史版本数。
const keepVersions = 30

// ListDefinitions 返回定义摘要（kind 为空则全部）。
func (s *Store) ListDefinitions(kind string) ([]DefinitionSummary, error) {
	rows, err := s.db.Query(`
		SELECT d.id, d.kind, d.name, d.folder, d.description, d.revision, d.created_by, d.created_at, d.updated_by, d.updated_at,
		       COALESCE(json_array_length(d.doc_json, '$.nodes'), 0),
		       COALESCE(r.id, ''), COALESCE(r.status, ''), COALESCE(r.queued_at, 0),
		       (SELECT COUNT(*) FROM etl_schedules sc WHERE sc.target_id = d.id),
		       COALESCE((SELECT MIN(next_fire_at) FROM etl_schedules sc WHERE sc.target_id = d.id AND sc.enabled = 1 AND next_fire_at > 0), 0)
		FROM etl_definitions d
		LEFT JOIN etl_runs r ON r.id = (SELECT id FROM etl_runs x WHERE x.definition_id = d.id ORDER BY x.queued_at DESC LIMIT 1)
		WHERE (? = '' OR d.kind = ?)
		ORDER BY d.updated_at DESC`, kind, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DefinitionSummary{}
	for rows.Next() {
		var d DefinitionSummary
		if err := rows.Scan(&d.ID, &d.Kind, &d.Name, &d.Folder, &d.Description, &d.Revision, &d.CreatedBy, &d.CreatedAt, &d.UpdatedBy, &d.UpdatedAt,
			&d.NodeCount, &d.LastRunID, &d.LastRunStatus, &d.LastRunAt, &d.ScheduleCount, &d.NextFireAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDefinition 取完整定义。
func (s *Store) GetDefinition(id string) (Definition, error) {
	var d Definition
	var doc string
	err := s.db.QueryRow(`SELECT id, kind, name, folder, description, doc_json, revision, created_by, created_at, updated_by, updated_at
		FROM etl_definitions WHERE id = ?`, id).
		Scan(&d.ID, &d.Kind, &d.Name, &d.Folder, &d.Description, &doc, &d.Revision, &d.CreatedBy, &d.CreatedAt, &d.UpdatedBy, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	d.Doc = json.RawMessage(doc)
	return d, err
}

// CreateDefinition 新建定义（revision=1）并记录首个版本。
func (s *Store) CreateDefinition(d Definition, user string) (Definition, error) {
	d.ID = NewID(map[string]string{"pipeline": "pl_", "workflow": "wf_"}[d.Kind])
	ts := now()
	tx, err := s.db.Begin()
	if err != nil {
		return d, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO etl_definitions (id, kind, name, folder, description, doc_json, revision, created_by, created_at, updated_by, updated_at)
		VALUES (?,?,?,?,?,?,1,?,?,?,?)`, d.ID, d.Kind, d.Name, d.Folder, d.Description, string(d.Doc), user, ts, user, ts); err != nil {
		return d, uniqueErr(err, "同类定义中已有同名项")
	}
	if _, err := tx.Exec(`INSERT INTO etl_definition_versions (definition_id, revision, doc_json, saved_by, saved_at) VALUES (?,1,?,?,?)`,
		d.ID, string(d.Doc), user, ts); err != nil {
		return d, err
	}
	if err := tx.Commit(); err != nil {
		return d, err
	}
	return s.GetDefinition(d.ID)
}

// UpdateDefinition 以乐观锁保存：baseRevision 必须等于当前 revision，否则 ErrConflict。
func (s *Store) UpdateDefinition(d Definition, baseRevision int, user string) (Definition, error) {
	ts := now()
	tx, err := s.db.Begin()
	if err != nil {
		return d, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE etl_definitions SET name=?, folder=?, description=?, doc_json=?, revision=revision+1, updated_by=?, updated_at=?
		WHERE id=? AND revision=?`, d.Name, d.Folder, d.Description, string(d.Doc), user, ts, d.ID, baseRevision)
	if err != nil {
		return d, uniqueErr(err, "同类定义中已有同名项")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var exists int
		_ = tx.QueryRow(`SELECT COUNT(*) FROM etl_definitions WHERE id=?`, d.ID).Scan(&exists)
		if exists == 0 {
			return d, ErrNotFound
		}
		return d, ErrConflict
	}
	rev := baseRevision + 1
	if _, err := tx.Exec(`INSERT INTO etl_definition_versions (definition_id, revision, doc_json, saved_by, saved_at) VALUES (?,?,?,?,?)`,
		d.ID, rev, string(d.Doc), user, ts); err != nil {
		return d, err
	}
	if _, err := tx.Exec(`DELETE FROM etl_definition_versions WHERE definition_id=? AND revision <= ?`, d.ID, rev-keepVersions); err != nil {
		return d, err
	}
	if err := tx.Commit(); err != nil {
		return d, err
	}
	return s.GetDefinition(d.ID)
}

// DeleteDefinition 删除定义、其版本与调度（运行记录保留，便于审计）。
func (s *Store) DeleteDefinition(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM etl_definitions WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(`DELETE FROM etl_definition_versions WHERE definition_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM etl_schedules WHERE target_id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// Version 是历史版本摘要。
type Version struct {
	Revision int             `json:"revision"`
	SavedBy  string          `json:"savedBy"`
	SavedAt  int64           `json:"savedAt"`
	Doc      json.RawMessage `json:"doc,omitempty"`
}

// ListVersions 返回历史版本（新→旧，不含文档）。
func (s *Store) ListVersions(id string) ([]Version, error) {
	rows, err := s.db.Query(`SELECT revision, saved_by, saved_at FROM etl_definition_versions WHERE definition_id=? ORDER BY revision DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Version{}
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.Revision, &v.SavedBy, &v.SavedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetVersion 取某个历史版本文档。
func (s *Store) GetVersion(id string, rev int) (Version, error) {
	var v Version
	var doc string
	err := s.db.QueryRow(`SELECT revision, saved_by, saved_at, doc_json FROM etl_definition_versions WHERE definition_id=? AND revision=?`, id, rev).
		Scan(&v.Revision, &v.SavedBy, &v.SavedAt, &doc)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	v.Doc = json.RawMessage(doc)
	return v, err
}
