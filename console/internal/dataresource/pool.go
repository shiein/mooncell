// 连接池管理器：每个 Mooncell 登录会话对每个数据资源维护一次内存连接。
//
// 设计文档第三节：
//   - 默认 MaxOpenConns=5、MaxIdleConns=2、连接最大生命周期 30 分钟。
//   - 更新资源配置后关闭旧连接池。
//   - 存在活动手工事务时禁止更新或删除资源并返回 409。
//   - 外部数据库连接池与 Mooncell SQLite 的单连接池完全分离。
//
// 资源级占用（同一把 mu）：
//   - activeTx：手工事务
//   - activeOps：正在执行的 SQL / 工作台创建
//   - importing：导入执行
//   - exclusive：配置更新/删除
//
// 导入、配置变更与 activeTx/activeOps 互斥；普通 activeOps 之间可并发。
// 所有占用在同一锁下竞争，避免 TOCTOU。
package dataresource

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var ErrConnectionLeaseStale = errors.New("登录会话或凭据租约已变更")

// PoolManager 管理各数据资源的 *sql.DB 连接池。
type PoolManager struct {
	mu    sync.Mutex
	pools map[string]poolEntry // resourceID + loginSessionID → 内存连接；密码不落盘
	// activeTx 记录每个资源是否有活动手工事务（ref count）。
	activeTx map[string]int
	// activeOps 记录资源上正在执行的普通操作（自动 SQL、工作台创建等）。
	activeOps map[string]int
	// importing 记录每个资源上正在执行的导入占用（ref count）。
	importing map[string]int
	// exclusive 配置变更占用（更新/删除资源），与事务、导入互斥。
	exclusive map[string]int
}

type poolEntry struct {
	db             *sql.DB
	resourceID     string
	username       string
	loginSessionID string
}

// NewPoolManager 创建连接池管理器。
func NewPoolManager() *PoolManager {
	return &PoolManager{
		pools:     map[string]poolEntry{},
		activeTx:  map[string]int{},
		activeOps: map[string]int{},
		importing: map[string]int{},
		exclusive: map[string]int{},
	}
}

const (
	defaultMaxOpenConns    = 5
	defaultMaxIdleConns    = 2
	defaultConnMaxLifetime = 30 * time.Minute
)

func sessionPoolKey(resourceID, loginSessionID string) string {
	return resourceID + "\x00" + loginSessionID
}

// ConnectUserDB 使用本次输入的密码建立并验证连接，再放入仅内存的用户连接池。
// 调用方须先使该用户在资源上的旧工作台失效，避免替换连接时影响在途操作。
func (pm *PoolManager) ConnectUserDB(ctx context.Context, r DataResource, username, password string) (*sql.DB, error) {
	return pm.ConnectSessionDB(ctx, r, username, "test:"+username, password)
}

// ConnectSessionDB 使用本次输入密码建立连接，并绑定到精确 Mooncell 登录会话。
func (pm *PoolManager) ConnectSessionDB(ctx context.Context, r DataResource, username, loginSessionID, password string) (*sql.DB, error) {
	return pm.ConnectSessionDBIf(ctx, r, username, loginSessionID, password, nil)
}

// ConnectSessionDBIf 仅在 accept 仍为 true 时发布已验证连接，阻止并发 logout/重连发布陈旧租约。
func (pm *PoolManager) ConnectSessionDBIf(
	ctx context.Context, r DataResource, username, loginSessionID, password string, accept func() bool,
) (*sql.DB, error) {
	if password == "" {
		return nil, fmt.Errorf("密码不能为空")
	}
	if loginSessionID == "" {
		return nil, fmt.Errorf("登录会话无效")
	}
	dsn := BuildDSN(r, password)
	if dsn == "" {
		return nil, fmt.Errorf("不支持的数据库类型: %s", r.DBType)
	}
	db, err := sql.Open(DriverName(r.DBType), dsn)
	if err != nil {
		return nil, fmt.Errorf("打开连接失败")
	}
	db.SetMaxOpenConns(defaultMaxOpenConns)
	db.SetMaxIdleConns(defaultMaxIdleConns)
	db.SetConnMaxLifetime(defaultConnMaxLifetime)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}

	key := sessionPoolKey(r.ID, loginSessionID)
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if accept != nil && !accept() {
		db.Close()
		return nil, ErrConnectionLeaseStale
	}
	if old, ok := pm.pools[key]; ok && old.db != nil {
		old.db.Close()
	}
	pm.pools[key] = poolEntry{db: db, resourceID: r.ID, username: username, loginSessionID: loginSessionID}
	return db, nil
}

// GetUserDB 获取用户已通过密码建立的当前连接。
func (pm *PoolManager) GetUserDB(resourceID, username string) (*sql.DB, error) {
	return pm.GetSessionDB(resourceID, "test:"+username)
}

func (pm *PoolManager) GetSessionDB(resourceID, loginSessionID string) (*sql.DB, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	entry, ok := pm.pools[sessionPoolKey(resourceID, loginSessionID)]
	if !ok || entry.db == nil {
		return nil, fmt.Errorf("请先输入密码连接数据库")
	}
	return entry.db, nil
}

func (pm *PoolManager) CloseUserDB(resourceID, username string) {
	pm.CloseSessionDB(resourceID, "test:"+username)
}

func (pm *PoolManager) CloseSessionDB(resourceID, loginSessionID string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	key := sessionPoolKey(resourceID, loginSessionID)
	if entry, ok := pm.pools[key]; ok {
		if entry.db != nil {
			entry.db.Close()
		}
		delete(pm.pools, key)
	}
}

// CloseDB 关闭并移除资源的连接池。用于资源更新或删除时。
func (pm *PoolManager) CloseDB(resourceID string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	prefix := resourceID + "\x00"
	for key, entry := range pm.pools {
		if strings.HasPrefix(key, prefix) {
			entry.db.Close()
			delete(pm.pools, key)
		}
	}
}

func (pm *PoolManager) CloseUserDBs(username string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for key, entry := range pm.pools {
		if entry.username == username {
			entry.db.Close()
			delete(pm.pools, key)
		}
	}
}

func (pm *PoolManager) CloseLoginSessionDBs(loginSessionID string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for key, entry := range pm.pools {
		if entry.loginSessionID == loginSessionID {
			entry.db.Close()
			delete(pm.pools, key)
		}
	}
}

func (pm *PoolManager) CloseUserResourceDBs(username, resourceID string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for key, entry := range pm.pools {
		if entry.username == username && entry.resourceID == resourceID {
			entry.db.Close()
			delete(pm.pools, key)
		}
	}
}

// HasActiveTx 检查资源是否有活动手工事务。
func (pm *PoolManager) HasActiveTx(resourceID string) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.activeTx[resourceID] > 0
}

// IsBusy 资源是否被事务/导入/配置变更占用。
func (pm *PoolManager) IsBusy(resourceID string) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.isBusyLocked(resourceID)
}

func (pm *PoolManager) isBusyLocked(resourceID string) bool {
	return pm.activeTx[resourceID] > 0 || pm.activeOps[resourceID] > 0 ||
		pm.importing[resourceID] > 0 || pm.exclusive[resourceID] > 0
}

// TryBeginOperation 占用普通资源操作槽。普通操作之间可并发，但不得与导入或配置变更交错。
func (pm *PoolManager) TryBeginOperation(resourceID string) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.importing[resourceID] > 0 || pm.exclusive[resourceID] > 0 {
		return false
	}
	pm.activeOps[resourceID]++
	return true
}

// EndOperation 释放普通资源操作槽。
func (pm *PoolManager) EndOperation(resourceID string) {
	pm.mu.Lock()
	if pm.activeOps[resourceID] > 0 {
		pm.activeOps[resourceID]--
		if pm.activeOps[resourceID] == 0 {
			delete(pm.activeOps, resourceID)
		}
	}
	pm.mu.Unlock()
}

// BeginTx 增加活动事务计数。若资源正在导入或配置变更则失败。
func (pm *PoolManager) BeginTx(resourceID string) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.importing[resourceID] > 0 {
		return fmt.Errorf("资源正在导入数据，请稍后再开启手工事务")
	}
	if pm.exclusive[resourceID] > 0 {
		return fmt.Errorf("资源正在更新或删除，请稍后再开启手工事务")
	}
	pm.activeTx[resourceID]++
	return nil
}

// EndTx 减少活动事务计数。
func (pm *PoolManager) EndTx(resourceID string) {
	pm.mu.Lock()
	if pm.activeTx[resourceID] > 0 {
		pm.activeTx[resourceID]--
	}
	pm.mu.Unlock()
}

// TryBeginImport 在无活动 SQL/手工事务、无其他导入且无配置变更时原子占用导入/写槽。
// 同一资源同时只允许一个导入或就地编辑占用（importing 计数），避免与写操作并发。
func (pm *PoolManager) TryBeginImport(resourceID string) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.isBusyLocked(resourceID) {
		return false
	}
	pm.importing[resourceID]++
	return true
}

// EndImport 释放导入占用。
func (pm *PoolManager) EndImport(resourceID string) {
	pm.mu.Lock()
	if pm.importing[resourceID] > 0 {
		pm.importing[resourceID]--
		if pm.importing[resourceID] == 0 {
			delete(pm.importing, resourceID)
		}
	}
	pm.mu.Unlock()
}

// TryBeginExclusive 配置更新/删除的互斥占用：要求无活动操作、事务、导入或其他 exclusive。
func (pm *PoolManager) TryBeginExclusive(resourceID string) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.isBusyLocked(resourceID) {
		return false
	}
	pm.exclusive[resourceID] = 1
	return true
}

// EndExclusive 释放配置变更占用。
func (pm *PoolManager) EndExclusive(resourceID string) {
	pm.mu.Lock()
	delete(pm.exclusive, resourceID)
	pm.mu.Unlock()
}

// CloseAll 关闭所有连接池（Console 退出时）。
func (pm *PoolManager) CloseAll() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for _, entry := range pm.pools {
		entry.db.Close()
	}
	pm.pools = map[string]poolEntry{}
}
