package main

import "encoding/json"

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

// loadEntities 按 kind 分组返回全部实体(原始 JSON),按插入顺序(seq)排列。
func (s *Store) loadEntities() (map[string][]json.RawMessage, error) {
	rows, err := s.db.Query("SELECT kind, data FROM entities ORDER BY seq")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]json.RawMessage{}
	for rows.Next() {
		var kind, data string
		if err := rows.Scan(&kind, &data); err != nil {
			return nil, err
		}
		out[kind] = append(out[kind], json.RawMessage(data))
	}
	return out, rows.Err()
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
