package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 业务实体以 JSON 文档形式持久化(kind + id + data),前端对象形态即存储形态。
// 这一阶段刻意不建强类型表:当前对象多为模拟产物,等真实部署(Agent 驱动)落地再建 schema。
var entityKinds = map[string]bool{
	"app": true, "release": true, "backup": true, "cabinet": true, "audit": true,
}

type seedItem struct {
	id   string
	data json.RawMessage
}

func (s *Store) countEntities() (int, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM entities").Scan(&n)
	return n, err
}

// loadEntities 按 kind 分组返回实体(原始 JSON),按插入顺序(seq)排列。
// 非 audit 实体全量返回(数量有界:app 按数量、cabinet 有 TTL 清理、backup 受 backupKeep 限制);
// audit 为 append-only 无限增长,只取最近 auditLimit 条(更早记录经 GET /api/audit 分页查)。
// auditLimit<=0 时退化为全量(供清理/统计等内部调用)。
func (s *Store) loadEntities(auditLimit int) (map[string][]json.RawMessage, error) {
	out := map[string][]json.RawMessage{}
	// 每条查询用闭包包住 defer Close:MaxOpenConns=1 下必须先释放连接再发下一条查询。
	collect := func(query string, args ...any) error {
		rows, err := s.db.Query(query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var kind, data string
			if err := rows.Scan(&kind, &data); err != nil {
				return err
			}
			out[kind] = append(out[kind], json.RawMessage(data))
		}
		return rows.Err()
	}
	if err := collect("SELECT kind, data FROM entities WHERE kind != 'audit' ORDER BY seq"); err != nil {
		return nil, err
	}
	if auditLimit > 0 {
		if err := collect("SELECT 'audit' AS kind, data FROM entities WHERE kind = 'audit' ORDER BY seq DESC LIMIT ?", auditLimit); err != nil {
			return nil, err
		}
	} else if err := collect("SELECT 'audit' AS kind, data FROM entities WHERE kind = 'audit' ORDER BY seq"); err != nil {
		return nil, err
	}
	return out, nil
}

// appsRaw 返回全部 app 实体原始 JSON(按 seq),供健康巡检遍历。
func (s *Store) appsRaw() ([]json.RawMessage, error) {
	rows, err := s.db.Query("SELECT data FROM entities WHERE kind = 'app' ORDER BY seq")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(data))
	}
	return out, rows.Err()
}

// MetricPoint 是某 Agent 某时刻的资源水位(供总览画真实历史曲线)。
type MetricPoint struct {
	Ts   int64   `json:"ts"`
	Cpu  float64 `json:"cpu"`
	Mem  float64 `json:"mem"`
	Disk float64 `json:"disk"`
}

func (s *Store) insertMetric(agentID string, ts int64, cpu, mem, disk float64) {
	s.db.Exec("INSERT INTO metrics (agent_id, ts, cpu, mem, disk) VALUES (?, ?, ?, ?, ?)", agentID, ts, cpu, mem, disk)
}

// listMetrics 返回某 Agent 自 since(含)起的指标时序(按时间升序)。
func (s *Store) listMetrics(agentID string, since int64) ([]MetricPoint, error) {
	rows, err := s.db.Query("SELECT ts, cpu, mem, disk FROM metrics WHERE agent_id = ? AND ts >= ? ORDER BY ts", agentID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MetricPoint{}
	for rows.Next() {
		var p MetricPoint
		if err := rows.Scan(&p.Ts, &p.Cpu, &p.Mem, &p.Disk); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// trimMetrics 删除早于 before 的指标点(保留窗口外),返回删除条数。
func (s *Store) trimMetrics(before int64) int {
	res, err := s.db.Exec("DELETE FROM metrics WHERE ts < ?", before)
	if err != nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}

// pageAudit 倒序(最近在前)分页返回审计实体的原始 JSON,并附总条数(供前端判断是否还有更早记录)。
func (s *Store) pageAudit(offset, limit int) ([]json.RawMessage, int, error) {
	var total int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM entities WHERE kind = 'audit'").Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query("SELECT data FROM entities WHERE kind = 'audit' ORDER BY seq DESC LIMIT ? OFFSET ?", limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []json.RawMessage{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, 0, err
		}
		items = append(items, json.RawMessage(data))
	}
	return items, total, rows.Err()
}

// trimAudit 裁剪审计:只保留最近 keep 条(按 seq 降序),删除更早的;返回删除条数。keep<=0 不裁剪。
func (s *Store) trimAudit(keep int) int {
	if keep <= 0 {
		return 0
	}
	res, err := s.db.Exec(
		`DELETE FROM entities WHERE kind = 'audit' AND seq NOT IN (
			SELECT seq FROM entities WHERE kind = 'audit' ORDER BY seq DESC LIMIT ?
		)`, keep)
	if err != nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}

// seedEntities 仅在实体表为空时按种子批量插入(单事务);返回是否实际种入。
func (s *Store) seedEntities(seed map[string][]seedItem) (bool, error) {
	n, err := s.countEntities()
	if err != nil || n > 0 {
		return false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare("INSERT INTO entities (kind, id, data) VALUES (?, ?, ?)")
	if err != nil {
		return false, err
	}
	defer stmt.Close()
	for kind, items := range seed {
		for _, it := range items {
			if _, err := stmt.Exec(kind, it.id, string(it.data)); err != nil {
				return false, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) putEntity(kind, id string, data json.RawMessage) error {
	_, err := s.db.Exec(
		`INSERT INTO entities (kind, id, data) VALUES (?, ?, ?)
		 ON CONFLICT(kind, id) DO UPDATE SET data = excluded.data`,
		kind, id, string(data),
	)
	return err
}

func (s *Store) deleteEntity(kind, id string) error {
	_, err := s.db.Exec("DELETE FROM entities WHERE kind = ? AND id = ?", kind, id)
	return err
}

func (s *Store) getEntity(kind, id string) (json.RawMessage, bool) {
	var data string
	if err := s.db.QueryRow("SELECT data FROM entities WHERE kind = ? AND id = ?", kind, id).Scan(&data); err != nil {
		return nil, false
	}
	return json.RawMessage(data), true
}

// expiredCabinet 返回已过期(expires < nowMs)的文件柜条目 id,供后台清理。
func (s *Store) expiredCabinet(nowMs int64) []string {
	rows, err := s.db.Query("SELECT id, data FROM entities WHERE kind = 'cabinet'")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id, data string
		if rows.Scan(&id, &data) != nil {
			continue
		}
		var m struct {
			Expires int64 `json:"expires"`
		}
		if json.Unmarshal([]byte(data), &m) == nil && m.Expires > 0 && m.Expires < nowMs {
			ids = append(ids, id)
		}
	}
	return ids
}

// cabinetByCode 按提取码查找文件柜元数据(全表扫 cabinet 实体,数量小可接受)。
func (s *Store) cabinetByCode(code string) (map[string]any, bool) {
	rows, err := s.db.Query("SELECT data FROM entities WHERE kind = 'cabinet'")
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	for rows.Next() {
		var data string
		if rows.Scan(&data) != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(data), &m) != nil {
			continue
		}
		if c, _ := m["code"].(string); strings.EqualFold(c, code) {
			return m, true
		}
	}
	return nil, false
}

// appendRelease 服务端权威写一条发布记录:真实部署/还原完成后,Console 据 Agent 实际结果落库,
// 不依赖前端伪造。status ∈ success|rolledback|failed,source=agent 标识权威记录。
func (s *Store) appendRelease(appID, version, status, operator string) error {
	id := fmt.Sprintf("r%d", time.Now().UnixNano())
	rec := map[string]any{
		"id": id, "appId": appID, "version": version, "status": status,
		"time": time.Now().UnixMilli(), "operator": operator,
		"duration": "", "size": "—", "source": "agent",
	}
	b, _ := json.Marshal(rec)
	return s.putEntity("release", id, b)
}

// appendAudit 服务端权威写一条审计实体:真实操作(经 Agent 的部署/还原)由 Console 据会话与
// Agent 实际结果落库,不依赖前端乐观上报。source=agent 标识其为权威记录(区别于前端模拟操作)。
func (s *Store) appendAudit(user, action, target, result string) error {
	id := fmt.Sprintf("a%d", time.Now().UnixNano())
	rec := map[string]any{
		"id": id, "time": time.Now().UnixMilli(),
		"user": user, "action": action, "target": target,
		"result": result, "ip": "", "source": "agent",
	}
	b, _ := json.Marshal(rec)
	return s.putEntity("audit", id, b)
}
