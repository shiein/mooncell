// 连接池管理器：每个数据资源维护一个懒加载 *sql.DB。
//
// 设计文档第三节：
//   - 默认 MaxOpenConns=5、MaxIdleConns=2、连接最大生命周期 30 分钟。
//   - 更新资源配置后关闭旧连接池。
//   - 存在活动手工事务时禁止更新或删除资源并返回 409。
//   - 外部数据库连接池与 Mooncell SQLite 的单连接池完全分离。
package dataresource

import (
	"database/sql"
	"fmt"
	"sync"
	"time"
)

// PoolManager 管理各数据资源的 *sql.DB 连接池。
type PoolManager struct {
	mu    sync.Mutex
	pools map[string]*sql.DB // resourceID → *sql.DB
	// activeTx 记录每个资源是否有活动手工事务（ref count）。
	activeTx   map[string]int
	credKey    *CredentialKey
}

// NewPoolManager 创建连接池管理器。
func NewPoolManager(credKey *CredentialKey) *PoolManager {
	return &PoolManager{
		pools:    map[string]*sql.DB{},
		activeTx: map[string]int{},
		credKey:  credKey,
	}
}

const (
	defaultMaxOpenConns    = 5
	defaultMaxIdleConns    = 2
	defaultConnMaxLifetime = 30 * time.Minute
)

// GetDB 获取或创建资源的 *sql.DB。password 从凭据密文解密。
func (pm *PoolManager) GetDB(r DataResource) (*sql.DB, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if db, ok := pm.pools[r.ID]; ok {
		return db, nil
	}

	password, err := pm.credKey.Decrypt(r.CredentialCipher)
	if err != nil {
		return nil, fmt.Errorf("凭据解密失败")
	}

	driverName := DriverName(r.DBType)
	dsn := BuildDSN(r, password)
	if dsn == "" {
		return nil, fmt.Errorf("不支持的数据库类型: %s", r.DBType)
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("打开连接失败")
	}
	db.SetMaxOpenConns(defaultMaxOpenConns)
	db.SetMaxIdleConns(defaultMaxIdleConns)
	db.SetConnMaxLifetime(defaultConnMaxLifetime)

	pm.pools[r.ID] = db
	return db, nil
}

// CloseDB 关闭并移除资源的连接池。用于资源更新或删除时。
func (pm *PoolManager) CloseDB(resourceID string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if db, ok := pm.pools[resourceID]; ok {
		db.Close()
		delete(pm.pools, resourceID)
	}
}

// HasActiveTx 检查资源是否有活动手工事务。
func (pm *PoolManager) HasActiveTx(resourceID string) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.activeTx[resourceID] > 0
}

// BeginTx 增加活动事务计数。
func (pm *PoolManager) BeginTx(resourceID string) {
	pm.mu.Lock()
	pm.activeTx[resourceID]++
	pm.mu.Unlock()
}

// EndTx 减少活动事务计数。
func (pm *PoolManager) EndTx(resourceID string) {
	pm.mu.Lock()
	if pm.activeTx[resourceID] > 0 {
		pm.activeTx[resourceID]--
	}
	pm.mu.Unlock()
}

// CloseAll 关闭所有连接池（Console 退出时）。
func (pm *PoolManager) CloseAll() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for _, db := range pm.pools {
		db.Close()
	}
	pm.pools = map[string]*sql.DB{}
}
