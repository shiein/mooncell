// Service 是数据资源模块的业务服务层，持有 SQLite 句柄和凭据密钥。
// 由 consoleapp 在启动时创建，handler 方法注册到 HTTP mux。
package dataresource

import (
	"database/sql"
	"sync"
)

// AuditFunc 是审计回调类型，由 consoleapp 注入，复用现有审计系统。
// 设计文档：SQL 审计只保存资源、用户、语句类型、SQL 哈希、结果和耗时，
// 不保存完整 SQL、参数值或结果数据。
type AuditFunc func(user, action, target, result string)

// Service 持有数据资源模块的运行时依赖。
type Service struct {
	db            *sql.DB
	credKey       *CredentialKey
	pools         *PoolManager
	workspaces    *WorkspaceManager
	importMu      sync.Mutex
	importSessions map[string]*ImportSession
	importMaxMB   int
	audit         AuditFunc
}

// NewService 创建数据资源服务。credKey 不可为 nil（由 consoleapp 启动时保证）。
func NewService(db *sql.DB, credKey *CredentialKey) *Service {
	pool := NewPoolManager(credKey)
	return &Service{
		db:             db,
		credKey:        credKey,
		pools:          pool,
		workspaces:     NewWorkspaceManager(pool),
		importSessions: map[string]*ImportSession{},
		importMaxMB:    100,
	}
}

// SetAuditFunc 设置审计回调（由 consoleapp 注入）。
func (s *Service) SetAuditFunc(fn AuditFunc) {
	s.audit = fn
}

// auditLog 记录审计（如果回调已设置）。
func (s *Service) auditLog(user, action, target, result string) {
	if s.audit != nil {
		s.audit(user, action, target, result)
	}
}

// SetImportMaxMB 设置导入文件大小上限（MB）。
func (s *Service) SetImportMaxMB(mb int) {
	if mb > 0 {
		s.importMaxMB = mb
	}
}

// DB 返回底层 SQLite 句柄（供 consoleapp 用户管理调用授权函数）。
func (s *Service) DB() *sql.DB { return s.db }

// Close 关闭所有工作台和外部数据库连接池（Console 退出时调用）。
func (s *Service) Close() {
	if s.workspaces != nil {
		s.workspaces.CloseAll()
	}
	if s.pools != nil {
		s.pools.CloseAll()
	}
}

// CleanupIdle 回滚超时的手工事务。由后台定期调用。
func (s *Service) CleanupIdle() int {
	if s.workspaces == nil {
		return 0
	}
	return s.workspaces.CleanupIdle()
}

// RollbackUserTx 回滚用户在某资源上的所有活动事务（撤权/退出时调用）。
func (s *Service) RollbackUserTx(username, resourceID string) {
	if s.workspaces != nil {
		s.workspaces.RollbackUserTx(username, resourceID)
	}
}

// CleanupExpiredImports 清理超时的导入会话和临时文件。
func (s *Service) CleanupExpiredImports() {
	s.cleanupExpiredImports()
}
