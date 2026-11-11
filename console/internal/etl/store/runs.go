package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"mooncell/console/internal/etl/core"
)

// 运行状态。
const (
	RunQueued      = "queued"
	RunRunning     = "running"
	RunSucceeded   = "succeeded"
	RunFailed      = "failed"
	RunCanceled    = "canceled"
	RunInterrupted = "interrupted"
)

// Run 是运行记录。
type Run struct {
	ID             string          `json:"id"`
	Kind           string          `json:"kind"`
	DefinitionID   string          `json:"definitionId"`
	DefinitionName string          `json:"definitionName"`
	Revision       int             `json:"revision"`
	Trigger        string          `json:"trigger"`
	ScheduleID     string          `json:"scheduleId,omitempty"`
	ResumeOf       string          `json:"resumeOf,omitempty"`
	Status         string          `json:"status"`
	Params         json.RawMessage `json:"params"`
	Snapshot       json.RawMessage `json:"snapshot,omitempty"`
	// Refs 是快照引用的定义文档（{id: {kind,name,revision,doc}}）；不进 API 响应。
	Refs        json.RawMessage `json:"-"`
	Error       string          `json:"error,omitempty"`
	TriggeredBy string          `json:"triggeredBy"`
	RowsRead    int64           `json:"rowsRead"`
	RowsWritten int64           `json:"rowsWritten"`
	LogCount    int64           `json:"logCount"`
	LogDropped  int64           `json:"logDropped"`
	QueuedAt    int64           `json:"queuedAt"`
	StartedAt   int64           `json:"startedAt"`
	FinishedAt  int64           `json:"finishedAt"`
}

// ErrorSample 是节点 error 端口拒绝行的采样（有上限，随运行记录保存）。
type ErrorSample struct {
	Fields []string `json:"fields"` // 错误行布局：基础字段 + _error_code/_error_message/_error_field/_error_step
	Rows   [][]any  `json:"rows"`   // 采样行（值经截断清洗）
	Total  int64    `json:"total"`  // 拒绝总数（采样只保留前几条）
}

// RunNode 是运行中一个节点（作业项或步骤）的状态。
type RunNode struct {
	RunID      string                `json:"runId"`
	NodeID     string                `json:"nodeId"`
	NodeName   string                `json:"nodeName"`
	NodeType   string                `json:"nodeType"`
	ParentNode string                `json:"parentNode,omitempty"`
	Status     string                `json:"status"`
	Attempt    int                   `json:"attempt"`
	Metrics    core.MetricsSnapshot  `json:"metrics"`
	OutputVars map[string]core.Value `json:"outputVars,omitempty"`
	Port       string                `json:"port,omitempty"`
	Error      string                `json:"error,omitempty"`
	Samples    *ErrorSample          `json:"samples,omitempty"`
	StartedAt  int64                 `json:"startedAt"`
	FinishedAt int64                 `json:"finishedAt"`
}

const runCols = `id, kind, definition_id, definition_name, revision, trigger, schedule_id, resume_of, status, params_json,
	error, triggered_by, rows_read, rows_written, log_count, log_dropped, queued_at, started_at, finished_at`

func scanRun(sc interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var params string
	err := sc.Scan(&r.ID, &r.Kind, &r.DefinitionID, &r.DefinitionName, &r.Revision, &r.Trigger, &r.ScheduleID, &r.ResumeOf, &r.Status, &params,
		&r.Error, &r.TriggeredBy, &r.RowsRead, &r.RowsWritten, &r.LogCount, &r.LogDropped, &r.QueuedAt, &r.StartedAt, &r.FinishedAt)
	r.Params = json.RawMessage(params)
	return r, err
}

// CreateRun 新建 queued 状态的运行。
func (s *Store) CreateRun(r Run) (Run, error) {
	if r.ID == "" {
		r.ID = NewID("run_")
	}
	r.Status = RunQueued
	r.QueuedAt = now()
	if len(r.Params) == 0 {
		r.Params = json.RawMessage("{}")
	}
	if len(r.Refs) == 0 {
		r.Refs = json.RawMessage("{}")
	}
	_, err := s.db.Exec(`INSERT INTO etl_runs (id, kind, definition_id, definition_name, revision, trigger, schedule_id, resume_of, status,
		params_json, snapshot_json, refs_json, triggered_by, queued_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Kind, r.DefinitionID, r.DefinitionName, r.Revision, r.Trigger, r.ScheduleID, r.ResumeOf, r.Status,
		string(r.Params), string(r.Snapshot), string(r.Refs), r.TriggeredBy, r.QueuedAt)
	return r, err
}

// StartRun 标记 running。
func (s *Store) StartRun(id string) error {
	_, err := s.db.Exec(`UPDATE etl_runs SET status=?, started_at=? WHERE id=? AND status=?`, RunRunning, now(), id, RunQueued)
	return err
}

// FinishRun 写入终态。
func (s *Store) FinishRun(id, status, errMsg string, read, written, logCount, logDropped int64) error {
	_, err := s.db.Exec(`UPDATE etl_runs SET status=?, error=?, rows_read=?, rows_written=?, log_count=?, log_dropped=?, finished_at=?
		WHERE id=?`, status, errMsg, read, written, logCount, logDropped, now(), id)
	return err
}

// MarkInterrupted 把遗留的 queued/running 运行标为 interrupted（Console 启动时调用），返回条数。
func (s *Store) MarkInterrupted() (int64, error) {
	res, err := s.db.Exec(`UPDATE etl_runs SET status=?, error=CASE WHEN error='' THEN 'Console 重启，运行被中断' ELSE error END, finished_at=?
		WHERE status IN (?, ?)`, RunInterrupted, now(), RunQueued, RunRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetRun 取运行（含定义快照）。
func (s *Store) GetRun(id string) (Run, error) {
	var snap, refs string
	row := s.db.QueryRow(`SELECT `+runCols+`, snapshot_json, refs_json FROM etl_runs WHERE id=?`, id)
	var r Run
	var params string
	err := row.Scan(&r.ID, &r.Kind, &r.DefinitionID, &r.DefinitionName, &r.Revision, &r.Trigger, &r.ScheduleID, &r.ResumeOf, &r.Status, &params,
		&r.Error, &r.TriggeredBy, &r.RowsRead, &r.RowsWritten, &r.LogCount, &r.LogDropped, &r.QueuedAt, &r.StartedAt, &r.FinishedAt, &snap, &refs)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	r.Params, r.Snapshot, r.Refs = json.RawMessage(params), json.RawMessage(snap), json.RawMessage(refs)
	return r, err
}

// RunFilter 是运行列表筛选。
type RunFilter struct {
	DefinitionID string
	Status       string // 可逗号分隔多个
	Before       int64  // queued_at < Before（翻页游标）
	Limit        int
}

// ListRuns 返回运行（新→旧，不含快照）。
func (s *Store) ListRuns(f RunFilter) ([]Run, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	q := `SELECT ` + runCols + ` FROM etl_runs WHERE 1=1`
	var args []any
	if f.DefinitionID != "" {
		q += ` AND definition_id = ?`
		args = append(args, f.DefinitionID)
	}
	if f.Status != "" {
		sts := strings.Split(f.Status, ",")
		q += ` AND status IN (` + strings.TrimSuffix(strings.Repeat("?,", len(sts)), ",") + `)`
		for _, st := range sts {
			args = append(args, strings.TrimSpace(st))
		}
	}
	if f.Before > 0 {
		q += ` AND queued_at < ?`
		args = append(args, f.Before)
	}
	q += ` ORDER BY queued_at DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertRunNode 写入/更新节点状态。
func (s *Store) UpsertRunNode(n RunNode) error {
	m, _ := json.Marshal(n.Metrics)
	if n.OutputVars == nil {
		n.OutputVars = map[string]core.Value{}
	}
	v, _ := json.Marshal(n.OutputVars)
	if n.Attempt == 0 {
		n.Attempt = 1
	}
	smp := "{}"
	if n.Samples != nil {
		b, _ := json.Marshal(n.Samples)
		smp = string(b)
	}
	_, err := s.db.Exec(`INSERT INTO etl_run_nodes (run_id, node_id, node_name, node_type, parent_node, status, attempt, metrics_json,
			output_vars_json, port, error, samples_json, started_at, finished_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(run_id, parent_node, node_id) DO UPDATE SET node_name=excluded.node_name, node_type=excluded.node_type,
			status=excluded.status, attempt=excluded.attempt, metrics_json=excluded.metrics_json, output_vars_json=excluded.output_vars_json,
			port=excluded.port, error=excluded.error, samples_json=excluded.samples_json, started_at=excluded.started_at, finished_at=excluded.finished_at`,
		n.RunID, n.NodeID, n.NodeName, n.NodeType, n.ParentNode, n.Status, n.Attempt, string(m), string(v), n.Port, n.Error, smp, n.StartedAt, n.FinishedAt)
	return err
}

// ListRunNodes 返回运行的全部节点。
func (s *Store) ListRunNodes(runID string) ([]RunNode, error) {
	rows, err := s.db.Query(`SELECT run_id, node_id, node_name, node_type, parent_node, status, attempt, metrics_json, output_vars_json,
		port, error, samples_json, started_at, finished_at FROM etl_run_nodes WHERE run_id=? ORDER BY parent_node, started_at, node_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunNode{}
	for rows.Next() {
		var n RunNode
		var m, v, smp string
		if err := rows.Scan(&n.RunID, &n.NodeID, &n.NodeName, &n.NodeType, &n.ParentNode, &n.Status, &n.Attempt, &m, &v,
			&n.Port, &n.Error, &smp, &n.StartedAt, &n.FinishedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(m), &n.Metrics)
		_ = json.Unmarshal([]byte(v), &n.OutputVars)
		var es ErrorSample
		if err := json.Unmarshal([]byte(smp), &es); err == nil && len(es.Rows) > 0 {
			n.Samples = &es
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// AppendLogs 批量写入日志。
func (s *Store) AppendLogs(runID string, entries []core.LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO etl_run_logs (run_id, seq, ts, level, node_id, message) VALUES (?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range entries {
		if _, err := stmt.Exec(runID, e.Seq, e.TS, e.Level, e.NodeID, e.Message); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListLogs 返回 seq > after 的日志（最多 limit 条）；level/node 可选过滤。
func (s *Store) ListLogs(runID string, after int64, limit int, level, nodeID string) ([]core.LogEntry, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	q := `SELECT seq, ts, level, node_id, message FROM etl_run_logs WHERE run_id=? AND seq > ?`
	args := []any{runID, after}
	if level != "" {
		lv := core.ParseLevel(level)
		var allowed []string
		for l := core.LevelError; l <= lv; l++ {
			allowed = append(allowed, l.String())
		}
		q += ` AND level IN (` + strings.TrimSuffix(strings.Repeat("?,", len(allowed)), ",") + `)`
		for _, a := range allowed {
			args = append(args, a)
		}
	}
	if nodeID != "" {
		q += ` AND node_id = ?`
		args = append(args, nodeID)
	}
	q += ` ORDER BY seq LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.LogEntry{}
	for rows.Next() {
		var e core.LogEntry
		if err := rows.Scan(&e.Seq, &e.TS, &e.Level, &e.NodeID, &e.Message); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// TrimRuns 按保留策略删除旧运行（含节点与日志）：早于 cutoff 的，或超出每定义保留数的。
func (s *Store) TrimRuns(cutoff int64, keepPerDef int) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	doomed := `SELECT id FROM etl_runs WHERE status NOT IN ('queued','running') AND (queued_at < ? OR id IN (
		SELECT id FROM (SELECT id, ROW_NUMBER() OVER (PARTITION BY definition_id ORDER BY queued_at DESC) AS rn FROM etl_runs) WHERE rn > ?))`
	for _, t := range []string{"etl_run_logs", "etl_run_nodes"} {
		if _, err := tx.Exec(`DELETE FROM `+t+` WHERE run_id IN (`+doomed+`)`, cutoff, keepPerDef); err != nil {
			return 0, err
		}
	}
	res, err := tx.Exec(`DELETE FROM etl_runs WHERE id IN (`+doomed+`)`, cutoff, keepPerDef)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, tx.Commit()
}

// ActiveRunCount 返回某定义正在排队/运行的数量（重叠策略用）。
func (s *Store) ActiveRunCount(defID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM etl_runs WHERE definition_id=? AND status IN (?,?)`, defID, RunQueued, RunRunning).Scan(&n)
	return n, err
}
