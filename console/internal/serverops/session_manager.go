// 活动 SSH 会话、授权代际与失效。
// 代际保证：撤权 / 改配置 / 删用户 / logout 后，在途慢连接与已注册会话均会关闭。
package serverops

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Session 是一次用户到远端的运行时 SSH 连接（含可选 SFTP 子系统）。
// 密码在握手后不再持有。
type Session struct {
	ID           string
	ResourceID   string
	Username     string // Mooncell 用户
	SSHUser      string
	Host         string
	Port         int
	CreatedAt    time.Time
	ExpiresAt    time.Time // 绝对过期（max session hours）
	IdleTimeout  time.Duration
	ResourceGen  uint64 // 注册时资源代际
	UserGrantGen uint64 // 注册时 (user, resource) 授权代际

	// lastActivityUnix 最后主动活动（输入/resize/SFTP/创建）；idle 回收依据。
	lastActivityUnix atomic.Int64

	client   *ssh.Client
	sftp     *sftp.Client
	sftpOnce sync.Once
	sftpErr  error

	// PTY 会话（终端 WebSocket 持有；关闭时一并释放）
	sshSession *ssh.Session

	cancel context.CancelFunc
	ctx    context.Context

	// 所属管理器，Close 时自动注销。
	mgr *SessionManager

	// 传输绑定计数：>0 时有活动上传持有本 session。
	transferRefs int32

	closed atomic.Bool
	mu     sync.Mutex
}

// SessionManager 管理活动会话与代际计数。
type SessionManager struct {
	mu sync.Mutex

	// sessions[sessionId]
	sessions map[string]*Session

	// resourceGen[resourceId]：资源 host/port/username/hostkey 变更时递增。
	resourceGen map[string]uint64

	// grantGen[username+"\x00"+resourceId]：撤权时递增。
	grantGen map[string]uint64

	// userGen[username]：logout / 删用户时递增，使该用户全部会话失效。
	userGen map[string]uint64

	// 会话创建时快照的用户代际。
	sessionUserGen map[string]uint64

	// SSH 握手可能持续数秒，握手前先占位，避免并发请求同时越过会话上限。
	pendingTotal  int
	pendingByUser map[string]int
}

func newSessionManager() *SessionManager {
	return &SessionManager{
		sessions:       map[string]*Session{},
		resourceGen:    map[string]uint64{},
		grantGen:       map[string]uint64{},
		userGen:        map[string]uint64{},
		sessionUserGen: map[string]uint64{},
		pendingByUser:  map[string]int{},
	}
}

func grantKey(username, resourceID string) string {
	return username + "\x00" + resourceID
}

// ResourceGeneration 返回当前资源代际。
func (m *SessionManager) ResourceGeneration(resourceID string) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resourceGen[resourceID]
}

// GrantGeneration 返回当前授权代际。
func (m *SessionManager) GrantGeneration(username, resourceID string) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.grantGen[grantKey(username, resourceID)]
}

// UserGeneration 返回用户代际。
func (m *SessionManager) UserGeneration(username string) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.userGen[username]
}

// BumpResource 递增资源代际并关闭该资源全部活动会话。
func (m *SessionManager) BumpResource(resourceID string) {
	m.mu.Lock()
	m.resourceGen[resourceID]++
	var toClose []*Session
	for _, s := range m.sessions {
		if s.ResourceID == resourceID {
			toClose = append(toClose, s)
		}
	}
	m.mu.Unlock()
	for _, s := range toClose {
		s.Close()
	}
}

// BumpGrant 递增 (user, resource) 代际并关闭该用户在该资源上的会话。
func (m *SessionManager) BumpGrant(username, resourceID string) {
	m.mu.Lock()
	m.grantGen[grantKey(username, resourceID)]++
	var toClose []*Session
	for _, s := range m.sessions {
		if s.Username == username && s.ResourceID == resourceID {
			toClose = append(toClose, s)
		}
	}
	m.mu.Unlock()
	for _, s := range toClose {
		s.Close()
	}
}

// BumpUser 递增用户代际并关闭其全部会话（logout / 删用户）。
func (m *SessionManager) BumpUser(username string) {
	m.mu.Lock()
	m.userGen[username]++
	var toClose []*Session
	for _, s := range m.sessions {
		if s.Username == username {
			toClose = append(toClose, s)
		}
	}
	m.mu.Unlock()
	for _, s := range toClose {
		s.Close()
	}
}

// Count 返回当前会话总数。
func (m *SessionManager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// CountUser 返回用户会话数。
func (m *SessionManager) CountUser(username string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.sessions {
		if s.Username == username {
			n++
		}
	}
	return n
}

// Reserve 在 SSH 握手前原子占用会话槽。
func (m *SessionManager) Reserve(username string, maxTotal, maxPerUser int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions)+m.pendingTotal >= maxTotal {
		return apiErr(CodeSessionLimit, "全局 SSH 会话数已达上限", true)
	}
	activeUser := 0
	for _, s := range m.sessions {
		if s.Username == username {
			activeUser++
		}
	}
	if activeUser+m.pendingByUser[username] >= maxPerUser {
		return apiErr(CodeSessionLimit, "您的 SSH 会话数已达上限", true)
	}
	m.pendingTotal++
	m.pendingByUser[username]++
	return nil
}

// ReleaseReservation 释放失败或取消的握手占位。
func (m *SessionManager) ReleaseReservation(username string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releaseReservationLocked(username)
}

func (m *SessionManager) releaseReservationLocked(username string) {
	if m.pendingByUser[username] <= 0 {
		return
	}
	m.pendingByUser[username]--
	if m.pendingByUser[username] == 0 {
		delete(m.pendingByUser, username)
	}
	m.pendingTotal--
}

// RegisterReserved 将握手占位原子转换为活动会话。
func (m *SessionManager) RegisterReserved(s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releaseReservationLocked(s.Username)
	s.mgr = m
	if s.lastActivityUnix.Load() == 0 {
		s.lastActivityUnix.Store(time.Now().Unix())
	}
	m.sessions[s.ID] = s
	m.sessionUserGen[s.ID] = m.userGen[s.Username]
}

// Register 仅供无需握手占位的内部测试使用。
func (m *SessionManager) Register(s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.mgr = m
	if s.lastActivityUnix.Load() == 0 {
		s.lastActivityUnix.Store(time.Now().Unix())
	}
	m.sessions[s.ID] = s
	m.sessionUserGen[s.ID] = m.userGen[s.Username]
}

// Unregister 从管理器移除（Close 内部调用）。
func (m *SessionManager) Unregister(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	delete(m.sessionUserGen, id)
}

// Get 按 ID 取会话；校验 owner、代际、绝对过期与 idle；不一致则关闭并返回 nil。
func (m *SessionManager) Get(sessionID, username, resourceID string) *Session {
	m.mu.Lock()
	s := m.sessions[sessionID]
	if s == nil {
		m.mu.Unlock()
		return nil
	}
	if s.Username != username || s.ResourceID != resourceID {
		m.mu.Unlock()
		return nil
	}
	// 代际校验。
	rg := m.resourceGen[resourceID]
	gg := m.grantGen[grantKey(username, resourceID)]
	ug := m.userGen[username]
	sug := m.sessionUserGen[sessionID]
	stale := s.ResourceGen != rg || s.UserGrantGen != gg || sug != ug || s.closed.Load()
	if stale {
		m.mu.Unlock()
		s.Close()
		return nil
	}
	if s.isTimedOut(time.Now()) {
		m.mu.Unlock()
		s.Close()
		return nil
	}
	m.mu.Unlock()
	return s
}

// ReapTimedOut 关闭所有绝对过期或 idle 超时的会话，返回关闭数量。
// 由 Service 后台周期调用，不依赖后续 API 访问才触发回收。
func (m *SessionManager) ReapTimedOut() int {
	now := time.Now()
	m.mu.Lock()
	var toClose []*Session
	for _, s := range m.sessions {
		if s.isTimedOut(now) {
			toClose = append(toClose, s)
		}
	}
	m.mu.Unlock()
	for _, s := range toClose {
		s.Close()
	}
	return len(toClose)
}

// isTimedOut 判断绝对过期或 idle 超时（持锁外可读原子字段）。
func (s *Session) isTimedOut(now time.Time) bool {
	if now.After(s.ExpiresAt) {
		return true
	}
	if s.IdleTimeout > 0 {
		last := s.lastActivityUnix.Load()
		if last > 0 && now.Unix()-last > int64(s.IdleTimeout.Seconds()) {
			return true
		}
	}
	return false
}

// Touch 记录主动活动，滑动 idle 窗口（不延长绝对 ExpiresAt）。
func (s *Session) Touch() {
	if s == nil || s.closed.Load() {
		return
	}
	s.lastActivityUnix.Store(time.Now().Unix())
}

// CloseAll 关闭全部会话（Service.Close / Console 退出）。
func (m *SessionManager) CloseAll() {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		s.Close()
	}
}

// Close 关闭 SSH/SFTP 并注销。可重复调用。
func (s *Session) Close() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Lock()
	if s.sshSession != nil {
		_ = s.sshSession.Close()
		s.sshSession = nil
	}
	if s.sftp != nil {
		_ = s.sftp.Close()
		s.sftp = nil
	}
	client := s.client
	s.client = nil
	mgr := s.mgr
	id := s.ID
	s.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
	if mgr != nil {
		mgr.Unregister(id)
	}
}

// SFTP 懒打开 SFTP 子系统（同一 session 复用）。
func (s *Session) SFTP() (*sftp.Client, error) {
	if s.closed.Load() {
		return nil, apiErr(CodeSessionClosed, "会话已结束", false)
	}
	s.sftpOnce.Do(func() {
		s.mu.Lock()
		client := s.client
		s.mu.Unlock()
		if client == nil {
			s.sftpErr = apiErr(CodeSessionClosed, "会话已结束", false)
			return
		}
		c, err := sftp.NewClient(client)
		if err != nil {
			s.sftpErr = apiErr(CodeSSHConnectionFailed, "打开 SFTP 失败", true)
			return
		}
		s.mu.Lock()
		s.sftp = c
		s.mu.Unlock()
	})
	if s.sftpErr != nil {
		return nil, s.sftpErr
	}
	s.mu.Lock()
	c := s.sftp
	s.mu.Unlock()
	if c == nil {
		return nil, apiErr(CodeSessionClosed, "会话已结束", false)
	}
	return c, nil
}

// Client 返回底层 SSH client（PTY 创建用）。
func (s *Session) Client() *ssh.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}
