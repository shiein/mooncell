package store

import (
	"database/sql"
	"encoding/json"
	"errors"

	"mooncell/console/internal/etl/core"
)

// Schedule 是调度记录。
type Schedule struct {
	ID         string                `json:"id"`
	TargetID   string                `json:"targetId"`
	TargetName string                `json:"targetName,omitempty"`
	TargetKind string                `json:"targetKind,omitempty"`
	Name       string                `json:"name"`
	Cron       string                `json:"cron"`
	Timezone   string                `json:"timezone"`
	Enabled    bool                  `json:"enabled"`
	Overlap    string                `json:"overlap"` // skip | queue | parallel
	Misfire    string                `json:"misfire"` // skip | run_once
	Params     map[string]core.Value `json:"params"`
	NextFireAt int64                 `json:"nextFireAt"`
	LastFireAt int64                 `json:"lastFireAt"`
	CreatedBy  string                `json:"createdBy"`
	CreatedAt  int64                 `json:"createdAt"`
	UpdatedAt  int64                 `json:"updatedAt"`
}

const schedCols = `s.id, s.target_id, COALESCE(d.name, ''), COALESCE(d.kind, ''), s.name, s.cron, s.timezone, s.enabled, s.overlap, s.misfire,
	s.params_json, s.next_fire_at, s.last_fire_at, s.created_by, s.created_at, s.updated_at`

func scanSchedule(sc interface{ Scan(...any) error }) (Schedule, error) {
	var s Schedule
	var en int
	var params string
	err := sc.Scan(&s.ID, &s.TargetID, &s.TargetName, &s.TargetKind, &s.Name, &s.Cron, &s.Timezone, &en, &s.Overlap, &s.Misfire,
		&params, &s.NextFireAt, &s.LastFireAt, &s.CreatedBy, &s.CreatedAt, &s.UpdatedAt)
	s.Enabled = en == 1
	_ = json.Unmarshal([]byte(params), &s.Params)
	if s.Params == nil {
		s.Params = map[string]core.Value{}
	}
	return s, err
}

// ListSchedules 返回调度（targetID 为空则全部）。
func (s *Store) ListSchedules(targetID string) ([]Schedule, error) {
	rows, err := s.db.Query(`SELECT `+schedCols+` FROM etl_schedules s LEFT JOIN etl_definitions d ON d.id = s.target_id
		WHERE (? = '' OR s.target_id = ?) ORDER BY s.created_at`, targetID, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Schedule{}
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// GetSchedule 取单个调度。
func (s *Store) GetSchedule(id string) (Schedule, error) {
	sc, err := scanSchedule(s.db.QueryRow(`SELECT `+schedCols+` FROM etl_schedules s LEFT JOIN etl_definitions d ON d.id = s.target_id WHERE s.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return sc, ErrNotFound
	}
	return sc, err
}

// SaveSchedule 新建或更新。
func (s *Store) SaveSchedule(sc Schedule, user string) (Schedule, error) {
	params, _ := json.Marshal(sc.Params)
	ts := now()
	if sc.ID == "" {
		sc.ID = NewID("sch_")
		_, err := s.db.Exec(`INSERT INTO etl_schedules (id, target_id, name, cron, timezone, enabled, overlap, misfire, params_json,
			next_fire_at, last_fire_at, created_by, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,0,?,?,?)`,
			sc.ID, sc.TargetID, sc.Name, sc.Cron, sc.Timezone, boolInt(sc.Enabled), sc.Overlap, sc.Misfire, string(params), sc.NextFireAt, user, ts, ts)
		if err != nil {
			return sc, err
		}
		return s.GetSchedule(sc.ID)
	}
	res, err := s.db.Exec(`UPDATE etl_schedules SET target_id=?, name=?, cron=?, timezone=?, enabled=?, overlap=?, misfire=?, params_json=?,
		next_fire_at=?, updated_at=? WHERE id=?`, sc.TargetID, sc.Name, sc.Cron, sc.Timezone, boolInt(sc.Enabled), sc.Overlap, sc.Misfire,
		string(params), sc.NextFireAt, ts, sc.ID)
	if err != nil {
		return sc, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sc, ErrNotFound
	}
	return s.GetSchedule(sc.ID)
}

// DeleteSchedule 删除调度。
func (s *Store) DeleteSchedule(id string) error {
	res, err := s.db.Exec(`DELETE FROM etl_schedules WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkFired 记录一次触发并写入下次触发时间（CAS：next_fire_at 必须仍是 expectNext，防止重复触发）。
func (s *Store) MarkFired(id string, expectNext, firedAt, next int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE etl_schedules SET last_fire_at=?, next_fire_at=? WHERE id=? AND next_fire_at=?`, firedAt, next, id, expectNext)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ---------------------------------------------------------------- 持久状态

// GetState 读状态值。
func (s *Store) GetState(scope, key string) (core.Value, bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT value_json FROM etl_state WHERE scope=? AND key=?`, scope, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Value{}, false, nil
	}
	if err != nil {
		return core.Value{}, false, err
	}
	var v core.Value
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return core.Value{}, false, err
	}
	return v, true, nil
}

// SetState 写状态值。
func (s *Store) SetState(scope, key string, v core.Value) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO etl_state (scope, key, value_json, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(scope, key) DO UPDATE SET value_json=excluded.value_json, updated_at=excluded.updated_at`, scope, key, string(raw), now())
	return err
}
