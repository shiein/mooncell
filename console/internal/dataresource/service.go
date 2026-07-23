// Service 是数据资源模块的业务服务层，持有 SQLite 句柄和凭据密钥。
// 由 consoleapp 在启动时创建，handler 方法注册到 HTTP mux。
package dataresource

import (
	"database/sql"
)

// Service 持有数据资源模块的运行时依赖。
type Service struct {
	db      *sql.DB
	credKey *CredentialKey
	pools   *PoolManager
}

// NewService 创建数据资源服务。credKey 不可为 nil（由 consoleapp 启动时保证）。
func NewService(db *sql.DB, credKey *CredentialKey) *Service {
	return &Service{
		db:      db,
		credKey: credKey,
		pools:   NewPoolManager(credKey),
	}
}

// DB 返回底层 SQLite 句柄（供 consoleapp 用户管理调用授权函数）。
func (s *Service) DB() *sql.DB { return s.db }

// Close 关闭所有外部数据库连接池（Console 退出时调用）。
func (s *Service) Close() {
	if s.pools != nil {
		s.pools.CloseAll()
	}
}
