package main

import (
	"database/sql"
)

// ArtifactRow 是制品仓库条目(对外形态,不含二进制本身——二进制按 id 落盘在 artifactDir)。
type ArtifactRow struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Sha256    string `json:"sha256"`
	Size      int64  `json:"size"`
	Uploader  string `json:"uploader"`
	CreatedAt int64  `json:"createdAt"`
}

func (s *Store) listArtifacts() ([]ArtifactRow, error) {
	rows, err := s.db.Query("SELECT id, name, version, sha256, size, uploader, created_at FROM artifacts ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ArtifactRow{}
	for rows.Next() {
		var a ArtifactRow
		if err := rows.Scan(&a.ID, &a.Name, &a.Version, &a.Sha256, &a.Size, &a.Uploader, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) getArtifact(id string) (ArtifactRow, bool) {
	var a ArtifactRow
	if err := s.db.QueryRow("SELECT id, name, version, sha256, size, uploader, created_at FROM artifacts WHERE id = ?", id).
		Scan(&a.ID, &a.Name, &a.Version, &a.Sha256, &a.Size, &a.Uploader, &a.CreatedAt); err != nil {
		return ArtifactRow{}, false
	}
	return a, true
}

func (s *Store) addArtifact(a ArtifactRow) error {
	_, err := s.db.Exec(
		"INSERT INTO artifacts (id, name, version, sha256, size, uploader, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		a.ID, a.Name, a.Version, a.Sha256, a.Size, a.Uploader, a.CreatedAt,
	)
	return err
}

func (s *Store) deleteArtifact(id string) error {
	_, err := s.db.Exec("DELETE FROM artifacts WHERE id = ?", id)
	return err
}

// sha256OfArtifact 按 sha256 查一条制品(重名内容去重提示用);不存在返回 false。
func (s *Store) artifactBySha(sha string) (ArtifactRow, bool) {
	var a ArtifactRow
	if err := s.db.QueryRow("SELECT id, name, version, sha256, size, uploader, created_at FROM artifacts WHERE sha256 = ?", sha).
		Scan(&a.ID, &a.Name, &a.Version, &a.Sha256, &a.Size, &a.Uploader, &a.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return ArtifactRow{}, false
		}
		return ArtifactRow{}, false
	}
	return a, true
}
