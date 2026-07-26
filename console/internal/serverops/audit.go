// 审计回调封装。由 consoleapp 注入 appendAudit，不直接依赖 consoleapp 包。
package serverops

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
)

// AuditFunc 审计回调：user / action / target / result。
type AuditFunc func(user, action, target, result string)

// auditLog 安全写入审计；不记录密码、完整路径、终端内容或 session ID。
func (s *Service) auditLog(user, action, target, result string) {
	if s.audit != nil {
		s.audit(user, action, target, result)
	}
}

// auditFileTarget 构造文件传输审计目标（含 basename 与路径哈希，不含完整路径）。
func auditFileTarget(resourceName, direction, remotePath string, size int64) string {
	base := path.Base(remotePath)
	if base == "." || base == "/" {
		base = "file"
	}
	sum := sha256.Sum256([]byte(remotePath))
	hash := hex.EncodeToString(sum[:4])
	return fmt.Sprintf("服务器运维·%s·%s·%s·%d·pathHash=%s", resourceName, direction, base, size, hash)
}
