package consoleapp

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
)

// 旧制品文件由服务端固定生成 art<UnixNano>，分块中间态以 .part 结尾。
// 清理目录时只匹配这一受控命名，绝不递归删除用户配置目录或其中的其它文件。
var legacyArtifactFilename = regexp.MustCompile(`^art[0-9]+(?:\.part)?$`)

// removeLegacyArtifacts 完成已下线制品仓库的一次性升级迁移：
//  1. 删除旧 artifacts 表中可验证 id 对应的落盘文件；
//  2. 清理专用目录内遗留的受控命名孤儿文件；
//  3. 最后 DROP artifacts 表。
//
// 文件删除失败时拒绝丢弃元数据并返回错误，避免升级后留下无法定位、无法管理的制品字节。
// 重复执行安全：表/目录已不存在时直接成功。
func (s *Store) removeLegacyArtifacts(dir string) (int, error) {
	if dir == "" {
		dir = "artifacts"
	}

	var tableExists int
	if err := s.db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'artifacts'",
	).Scan(&tableExists); err != nil {
		return 0, fmt.Errorf("检查 artifacts 表: %w", err)
	}

	ids := []string{}
	if tableExists > 0 {
		rows, err := s.db.Query("SELECT id FROM artifacts")
		if err != nil {
			return 0, fmt.Errorf("读取旧制品索引: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return 0, fmt.Errorf("读取旧制品 id: %w", err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return 0, fmt.Errorf("遍历旧制品索引: %w", err)
		}
		rows.Close() // SQLite 单连接：删文件/落表前先释放查询连接。
	}

	removed := 0
	seen := map[string]bool{}
	removeFile := func(name string) error {
		if seen[name] || !legacyArtifactFilename.MatchString(name) {
			return nil
		}
		seen[name] = true
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("删除旧制品 %s: %w", path, err)
		}
		removed++
		return nil
	}

	for _, id := range ids {
		// 旧实现只生成受控 id；异常/手工数据不用于拼接路径，避免目录穿越。
		if id == filepath.Base(id) {
			if err := removeFile(id); err != nil {
				return removed, err
			}
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return removed, fmt.Errorf("扫描旧制品目录 %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := removeFile(entry.Name()); err != nil {
			return removed, err
		}
	}

	if tableExists > 0 {
		if _, err := s.db.Exec("DROP TABLE artifacts"); err != nil {
			return removed, fmt.Errorf("删除 artifacts 表: %w", err)
		}
	}
	// 仅删除空目录；其中若有非制品文件则保留并明确记录，不做递归破坏。
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
		log.Printf("[artifact-migration] 旧目录非空或无法移除，已保留非制品内容: %s (%v)", dir, err)
	}
	return removed, nil
}
