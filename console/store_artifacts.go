package main

import (
	"database/sql"
)

// ArtifactRow 是制品仓库条目(对外形态,不含二进制本身——二进制按 id 落盘在 artifactDir)。
// AppID/Source/Pinned 支撑「部署成功自动归档 + ⭐标记重要保留」:
//   - Source=auto 表示由真实部署成功后自动沉淀(AppID=来源应用);manual=手动上传(AppID 空)。
//   - Pinned=true 的条目豁免每应用滚动淘汰,永久保留。
type ArtifactRow struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Sha256    string `json:"sha256"`
	Size      int64  `json:"size"`
	Uploader  string `json:"uploader"`
	CreatedAt int64  `json:"createdAt"`
	AppID     string `json:"appId"`
	Source    string `json:"source"` // manual | auto
	Pinned    bool   `json:"pinned"`
}

const artifactCols = "id, name, version, sha256, size, uploader, created_at, app_id, source, pinned"

func scanArtifact(sc interface{ Scan(...any) error }) (ArtifactRow, error) {
	var a ArtifactRow
	err := sc.Scan(&a.ID, &a.Name, &a.Version, &a.Sha256, &a.Size, &a.Uploader, &a.CreatedAt, &a.AppID, &a.Source, &a.Pinned)
	return a, err
}

func (s *Store) listArtifacts() ([]ArtifactRow, error) {
	// ⭐ 重要置顶,其余按时间倒序——重要节点不会被自动条目淹没。
	rows, err := s.db.Query("SELECT " + artifactCols + " FROM artifacts ORDER BY pinned DESC, created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ArtifactRow{}
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) getArtifact(id string) (ArtifactRow, bool) {
	a, err := scanArtifact(s.db.QueryRow("SELECT "+artifactCols+" FROM artifacts WHERE id = ?", id))
	if err != nil {
		return ArtifactRow{}, false
	}
	return a, true
}

func (s *Store) addArtifact(a ArtifactRow) error {
	if a.Source == "" {
		a.Source = "manual"
	}
	_, err := s.db.Exec(
		"INSERT INTO artifacts ("+artifactCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		a.ID, a.Name, a.Version, a.Sha256, a.Size, a.Uploader, a.CreatedAt, a.AppID, a.Source, a.Pinned,
	)
	return err
}

func (s *Store) deleteArtifact(id string) error {
	_, err := s.db.Exec("DELETE FROM artifacts WHERE id = ?", id)
	return err
}

// setArtifactPinned 标记/取消⭐重要(豁免滚动淘汰)。
func (s *Store) setArtifactPinned(id string, pinned bool) error {
	_, err := s.db.Exec("UPDATE artifacts SET pinned = ? WHERE id = ?", pinned, id)
	return err
}

// artifactBySha 按 sha256 查一条制品(内容去重用);不存在返回 false。
func (s *Store) artifactBySha(sha string) (ArtifactRow, bool) {
	a, err := scanArtifact(s.db.QueryRow("SELECT "+artifactCols+" FROM artifacts WHERE sha256 = ?", sha))
	if err != nil {
		if err == sql.ErrNoRows {
			return ArtifactRow{}, false
		}
		return ArtifactRow{}, false
	}
	return a, true
}

// evictableAutoArtifacts 返回某应用「自动归档且未⭐」中、超出保留上限 keep 的旧条目(新→旧排序后跳过前 keep 份)。
// 调用方据此删元数据 + 落盘字节。keep<=0 表示不保留任何自动条目(全部可淘汰);手动/⭐ 永不在内。
func (s *Store) evictableAutoArtifacts(appID string, keep int) ([]ArtifactRow, error) {
	if keep < 0 {
		keep = 0
	}
	rows, err := s.db.Query(
		"SELECT "+artifactCols+" FROM artifacts WHERE app_id = ? AND source = 'auto' AND pinned = 0 "+
			"ORDER BY created_at DESC LIMIT -1 OFFSET ?", appID, keep)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ArtifactRow{}
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
