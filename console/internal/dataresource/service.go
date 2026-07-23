// Service 是数据资源模块的业务服务层，持有 SQLite 句柄和凭据密钥。
// 由 consoleapp 在启动时创建，handler 方法注册到 HTTP mux。
package dataresource

import (
	"database/sql"
)

// Service 持有数据资源模块的运行时依赖。
type Service struct {
	db         *sql.DB
	credKey    *CredentialKey
	pools      *PoolManager
	workspaces *WorkspaceManager
}

// NewService 创建数据资源服务。credKey 不可为 nil（由 consoleapp 启动时保证）。
func NewService(db *sql.DB, credKey *CredentialKey) *Service {
	pool := NewPoolManager(credKey)
	return &Service{
		db:         db,
		credKey:    credKey,
		pools:      pool,
		workspaces: NewWorkspaceManager(pool),
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
