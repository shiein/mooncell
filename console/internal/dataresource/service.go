// Service 是数据资源模块的业务服务层。数据库凭据仅存在于当前 Mooncell 登录会话的内存连接中。
// 由 consoleapp 在启动时创建，handler 方法注册到 HTTP mux。
package dataresource

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sync"
)

// AuditFunc 是审计回调类型，由 consoleapp 注入，复用现有审计系统。
// 设计文档：SQL 审计只保存资源、用户、语句类型、SQL 哈希、结果和耗时，
// 不保存完整 SQL、参数值或结果数据。
type AuditFunc func(user, action, target, result string)

type SessionValidator func(loginSessionID string) bool

type accessGeneration struct {
	login      uint64
	user       uint64
	grant      uint64
	credential uint64
}

// Service 持有数据资源模块的运行时依赖。
type Service struct {
	db             *sql.DB
	pools          *PoolManager
	workspaces     *WorkspaceManager
	importMu       sync.Mutex
	importSessions map[string]*ImportSession
	importMaxMB    int
	audit          AuditFunc
	valid          SessionValidator
	generationMu   sync.Mutex
	loginGen       map[string]uint64
	userGen        map[string]uint64
	grantGen       map[string]uint64
	credentialGen  map[string]uint64
}

// NewService 创建数据资源服务。
func NewService(db *sql.DB) *Service {
	pool := NewPoolManager()
	return &Service{
		db:             db,
		pools:          pool,
		workspaces:     NewWorkspaceManager(pool),
		importSessions: map[string]*ImportSession{},
		importMaxMB:    100,
		loginGen:       map[string]uint64{},
		userGen:        map[string]uint64{},
		grantGen:       map[string]uint64{},
		credentialGen:  map[string]uint64{},
	}
}

// SetAuditFunc 设置审计回调（由 consoleapp 注入）。
func (s *Service) SetAuditFunc(fn AuditFunc) {
	s.audit = fn
}

func (s *Service) SetSessionValidator(fn SessionValidator) { s.valid = fn }

func dataResourceGrantKey(username, resourceID string) string {
	return username + "\x00" + resourceID
}

func (s *Service) snapshotAccessGeneration(username, loginSessionID, resourceID string) accessGeneration {
	s.generationMu.Lock()
	defer s.generationMu.Unlock()
	return accessGeneration{
		login:      s.loginGen[loginSessionID],
		user:       s.userGen[username],
		grant:      s.grantGen[dataResourceGrantKey(username, resourceID)],
		credential: s.credentialGen[dataResourceGrantKey(loginSessionID, resourceID)],
	}
}

func (s *Service) accessGenerationCurrent(want accessGeneration, username, loginSessionID, resourceID string) bool {
	s.generationMu.Lock()
	defer s.generationMu.Unlock()
	return s.loginGen[loginSessionID] == want.login &&
		s.userGen[username] == want.user &&
		s.grantGen[dataResourceGrantKey(username, resourceID)] == want.grant &&
		s.credentialGen[dataResourceGrantKey(loginSessionID, resourceID)] == want.credential
}

func (s *Service) createWorkspaceIfCurrent(
	want accessGeneration, resourceID, username, loginSessionID string,
	adapter DataSourceAdapter, readOnly bool,
) (*Workspace, bool) {
	s.generationMu.Lock()
	defer s.generationMu.Unlock()
	if s.loginGen[loginSessionID] != want.login ||
		s.userGen[username] != want.user ||
		s.grantGen[dataResourceGrantKey(username, resourceID)] != want.grant ||
		s.credentialGen[dataResourceGrantKey(loginSessionID, resourceID)] != want.credential {
		return nil, false
	}
	return s.workspaces.CreateWorkspaceForSession(resourceID, username, loginSessionID, adapter, readOnly), true
}

func (s *Service) bumpLoginGeneration(loginSessionID string) {
	s.generationMu.Lock()
	if s.loginGen == nil {
		s.loginGen = map[string]uint64{}
	}
	s.loginGen[loginSessionID]++
	s.generationMu.Unlock()
}

func (s *Service) bumpUserGeneration(username string) {
	s.generationMu.Lock()
	if s.userGen == nil {
		s.userGen = map[string]uint64{}
	}
	s.userGen[username]++
	s.generationMu.Unlock()
}

func (s *Service) bumpGrantGeneration(username, resourceID string) {
	s.generationMu.Lock()
	if s.grantGen == nil {
		s.grantGen = map[string]uint64{}
	}
	s.grantGen[dataResourceGrantKey(username, resourceID)]++
	s.generationMu.Unlock()
}

func (s *Service) bumpCredentialGeneration(loginSessionID, resourceID string) {
	s.generationMu.Lock()
	if s.credentialGen == nil {
		s.credentialGen = map[string]uint64{}
	}
	s.credentialGen[dataResourceGrantKey(loginSessionID, resourceID)]++
	s.generationMu.Unlock()
}

// auditLog 记录审计（如果回调已设置）。
func (s *Service) auditLog(user, action, target, result string) {
	if s.audit != nil {
		s.audit(user, action, target, result)
	}
}

// sqlHashShort 返回 SQL 的短哈希（SHA-256 前 8 字节 hex），不保存完整 SQL。
func sqlHashShort(sqlText string) string {
	sum := sha256.Sum256([]byte(sqlText))
	return hex.EncodeToString(sum[:8])
}

// auditSQL 记录 SQL 相关审计：target=资源·语句类型·哈希，result 含结果与耗时。
// 设计：只保存资源、用户、语句类型、SQL 哈希、结果和耗时。
func (s *Service) auditSQL(user, action, resourceID string, stmtType StatementType, sqlText, result string, durationMs int64) {
	target := fmt.Sprintf("%s·%s·%s", resourceID, string(stmtType), sqlHashShort(sqlText))
	if durationMs >= 0 {
		result = fmt.Sprintf("%s·%dms", result, durationMs)
	}
	s.auditLog(user, action, target, result)
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
	s.cleanupAllImportSessions()
	if s.workspaces != nil {
		s.workspaces.CloseAll()
	}
	if s.pools != nil {
		s.pools.CloseAll()
	}
}

// CleanupOrphanImportsOnStart 启动时清理遗留导入临时文件（重启后 map 为空）。
func (s *Service) CleanupOrphanImportsOnStart() {
	cleanupOrphanImportFiles(ImportTempTimeout)
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

// InvalidateUserResource 回滚并删除用户在某资源上的全部工作台（撤销/降级授权时调用）。
func (s *Service) InvalidateUserResource(username, resourceID string) {
	s.bumpGrantGeneration(username, resourceID)
	s.invalidateImports(func(session *ImportSession) bool {
		return session.Username == username && session.ResourceID == resourceID
	})
	if s.workspaces != nil {
		s.workspaces.InvalidateUserResource(username, resourceID)
	}
	if s.pools != nil {
		s.pools.CloseUserResourceDBs(username, resourceID)
	}
}

// InvalidateAllForUser 回滚并删除某用户的全部工作台（退出登录/删用户时调用）。
func (s *Service) InvalidateAllForUser(username string) {
	s.bumpUserGeneration(username)
	s.invalidateImports(func(session *ImportSession) bool {
		return session.Username == username
	})
	if s.workspaces != nil {
		s.workspaces.InvalidateAllForUser(username)
	}
	if s.pools != nil {
		s.pools.CloseUserDBs(username)
	}
}

// InvalidateLoginSession 只清理一个 Mooncell 登录会话持有的工作台、导入与数据库连接租约。
func (s *Service) InvalidateLoginSession(loginSessionID string) {
	if loginSessionID == "" {
		return
	}
	s.bumpLoginGeneration(loginSessionID)
	s.invalidateImports(func(session *ImportSession) bool {
		return session.LoginSessionID == loginSessionID
	})
	if s.workspaces != nil {
		s.workspaces.InvalidateLoginSession(loginSessionID)
	}
	if s.pools != nil {
		s.pools.CloseLoginSessionDBs(loginSessionID)
	}
}

// InvalidateLoginSessionResource 用于同一登录会话显式输入新密码重连。
func (s *Service) InvalidateLoginSessionResource(loginSessionID, resourceID string) {
	s.bumpCredentialGeneration(loginSessionID, resourceID)
	s.invalidateImports(func(session *ImportSession) bool {
		return session.LoginSessionID == loginSessionID && session.ResourceID == resourceID
	})
	if s.workspaces != nil {
		s.workspaces.InvalidateLoginSessionResource(loginSessionID, resourceID)
	}
	if s.pools != nil {
		s.pools.CloseSessionDB(resourceID, loginSessionID)
	}
}

// InvalidateResource 回滚并删除某资源上的全部工作台（资源更新/删除时调用）。
func (s *Service) InvalidateResource(resourceID string) {
	s.invalidateImports(func(session *ImportSession) bool {
		return session.ResourceID == resourceID
	})
	if s.workspaces != nil {
		s.workspaces.InvalidateResource(resourceID)
	}
	if s.pools != nil {
		s.pools.CloseDB(resourceID)
	}
}

// CleanupExpiredImports 清理超时的导入会话和临时文件。
func (s *Service) CleanupExpiredImports() {
	s.cleanupExpiredImports()
}
