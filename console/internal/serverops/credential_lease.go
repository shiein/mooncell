package serverops

import "sync"

// credentialLeaseManager 只在 Console 进程内保存 SSH 密码副本，并按精确 Mooncell 登录会话隔离。
// 退出、过期、撤权、资源变更或进程退出时清零并删除；从不写数据库、日志或响应。
type credentialLeaseManager struct {
	mu     sync.Mutex
	leases map[string]*credentialLease
	next   uint64
}

type credentialLease struct {
	username       string
	resourceID     string
	loginSessionID string
	password       []byte
	version        uint64
}

func newCredentialLeaseManager() *credentialLeaseManager {
	return &credentialLeaseManager{leases: map[string]*credentialLease{}}
}

func credentialLeaseKey(loginSessionID, resourceID string) string {
	return loginSessionID + "\x00" + resourceID
}

func cloneBytes(src []byte) []byte {
	return append([]byte(nil), src...)
}

func wipeBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func (m *credentialLeaseManager) Store(username, loginSessionID, resourceID string, password []byte) uint64 {
	if loginSessionID == "" || resourceID == "" || len(password) == 0 {
		return 0
	}
	key := credentialLeaseKey(loginSessionID, resourceID)
	m.mu.Lock()
	if old := m.leases[key]; old != nil {
		wipeBytes(old.password)
	}
	m.next++
	version := m.next
	m.leases[key] = &credentialLease{
		username: username, loginSessionID: loginSessionID, resourceID: resourceID,
		password: cloneBytes(password), version: version,
	}
	m.mu.Unlock()
	return version
}

func (m *credentialLeaseManager) Get(loginSessionID, resourceID string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lease := m.leases[credentialLeaseKey(loginSessionID, resourceID)]
	if lease == nil || len(lease.password) == 0 {
		return nil, false
	}
	return cloneBytes(lease.password), true
}

func (m *credentialLeaseManager) deleteMatching(match func(*credentialLease) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, lease := range m.leases {
		if match(lease) {
			wipeBytes(lease.password)
			delete(m.leases, key)
		}
	}
}

func (m *credentialLeaseManager) DeleteLoginSession(loginSessionID string) {
	m.deleteMatching(func(lease *credentialLease) bool { return lease.loginSessionID == loginSessionID })
}

func (m *credentialLeaseManager) DeleteLoginSessionResource(loginSessionID, resourceID string) {
	m.deleteMatching(func(lease *credentialLease) bool {
		return lease.loginSessionID == loginSessionID && lease.resourceID == resourceID
	})
}

// DeleteIfVersion 只回收本次握手发布的租约，不会误删并发成功连接写入的更新版本。
func (m *credentialLeaseManager) DeleteIfVersion(loginSessionID, resourceID string, version uint64) {
	if version == 0 {
		return
	}
	key := credentialLeaseKey(loginSessionID, resourceID)
	m.mu.Lock()
	lease := m.leases[key]
	if lease != nil && lease.version == version {
		wipeBytes(lease.password)
		delete(m.leases, key)
	}
	m.mu.Unlock()
}

func (m *credentialLeaseManager) DeleteUser(username string) {
	m.deleteMatching(func(lease *credentialLease) bool { return lease.username == username })
}

func (m *credentialLeaseManager) DeleteUserResource(username, resourceID string) {
	m.deleteMatching(func(lease *credentialLease) bool {
		return lease.username == username && lease.resourceID == resourceID
	})
}

func (m *credentialLeaseManager) DeleteResource(resourceID string) {
	m.deleteMatching(func(lease *credentialLease) bool { return lease.resourceID == resourceID })
}

func (m *credentialLeaseManager) Close() {
	m.deleteMatching(func(*credentialLease) bool { return true })
}
