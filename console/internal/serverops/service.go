// 模块生命周期与依赖组装。
// serverops 只通过构造参数接收 *sql.DB、配置、审计回调与会话校验回调。
package serverops

import (
	"database/sql"
	"log"
	"sync"
	"time"
)

// SessionValidator 校验 Mooncell 登录会话是否仍有效（由 consoleapp 注入）。
// username 非空且 valid=true 表示 mc_sid 仍绑定该用户。
type SessionValidator func(username string) bool

// TouchSession 标记用户有主动活动（可节流后滑动续期）。
type TouchSession func(username string)

// Service 持有服务器运维模块的运行时依赖。
type Service struct {
	db     *sql.DB
	cfg    Config
	audit  AuditFunc
	valid  SessionValidator
	touch  TouchSession
	sess   *SessionManager

	// 密码失败限速：(user+"\x00"+resource+"\x00"+ip) → 时间戳列表。
	authFailMu sync.Mutex
	authFails  map[string][]time.Time

	// 活动传输计数（内存），配合 SQLite 元数据。
	transferMu    sync.Mutex
	activeUploads map[string]struct{} // transferId
	uploadLocks   map[string]*sync.Mutex

	closed bool
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewService 创建服务。cfg 会 Normalize。不启动后台 goroutine（由 Start 负责）。
func NewService(db *sql.DB, cfg Config) *Service {
	cfg.Normalize()
	return &Service{
		db:            db,
		cfg:           cfg,
		sess:          newSessionManager(),
		authFails:     map[string][]time.Time{},
		activeUploads: map[string]struct{}{},
		uploadLocks:   map[string]*sync.Mutex{},
		stopCh:        make(chan struct{}),
	}
}

// SetAuditFunc 注入审计回调。
func (s *Service) SetAuditFunc(fn AuditFunc) { s.audit = fn }

// SetSessionValidator 注入 Mooncell 会话校验。
func (s *Service) SetSessionValidator(fn SessionValidator) { s.valid = fn }

// SetTouchSession 注入会话滑动续期回调（主动用户动作时调用）。
func (s *Service) SetTouchSession(fn TouchSession) { s.touch = fn }

// Config 返回当前配置副本。
func (s *Service) Config() Config { return s.cfg }

// Enabled 功能是否开启。
func (s *Service) Enabled() bool { return s.cfg.Enabled }

// DB 返回底层 SQLite 句柄（用户管理授权写入）。
func (s *Service) DB() *sql.DB { return s.db }

// Sessions 返回会话管理器（consoleapp 撤权/logout 联动）。
func (s *Service) Sessions() *SessionManager { return s.sess }

// Start 在 enabled 时启动后台清理：会话 idle/绝对过期回收 + 传输 TTL。
func (s *Service) Start() {
	if !s.cfg.Enabled {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// 会话回收需较勤，避免浏览器崩溃后占满限额。
		sessionTicker := time.NewTicker(30 * time.Second)
		transferTicker := time.NewTicker(10 * time.Minute)
		defer sessionTicker.Stop()
		defer transferTicker.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case <-sessionTicker.C:
				if n := s.sess.ReapTimedOut(); n > 0 {
					log.Printf("[serverops] 回收超时 SSH 会话 %d 个", n)
				}
			case <-transferTicker.C:
				s.expireStaleTransfers()
			}
		}
	}()
}

// Close 关闭全部 SSH/SFTP 与后台任务。
func (s *Service) Close() {
	if s.closed {
		return
	}
	s.closed = true
	close(s.stopCh)
	s.sess.CloseAll()
	s.wg.Wait()
}

// InvalidateUser 终止用户全部 SSH/SFTP 会话（logout / 删用户 / Session 过期）。
func (s *Service) InvalidateUser(username string) {
	if username == "" {
		return
	}
	s.sess.BumpUser(username)
}

// InvalidateUserResource 撤权后终止该用户在该资源上的会话。
func (s *Service) InvalidateUserResource(username, resourceID string) {
	s.sess.BumpGrant(username, resourceID)
}

// InvalidateResource 资源删除或连接参数变更后终止全部旧会话。
func (s *Service) InvalidateResource(resourceID string) {
	s.sess.BumpResource(resourceID)
}

func (s *Service) expireStaleTransfers() {
	now := time.Now().UnixMilli()
	// 仅将 uploading 标为 expired，并移出内存 activeUploads。
	// cleanup_pending 必须保留，等待下次成功连接后按记录精确删远端 part，不得改成 expired。
	rows, err := s.db.Query(
		`SELECT id FROM server_file_transfers WHERE state=? AND expires_at > 0 AND expires_at < ?`,
		TransferUploading, now)
	if err != nil {
		log.Printf("[serverops] 查询过期传输失败: %v", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) == 0 {
		return
	}
	for _, id := range ids {
		if _, err := s.db.Exec(
			`UPDATE server_file_transfers SET state=?, updated_at=? WHERE id=? AND state=?`,
			TransferExpired, now, id, TransferUploading); err != nil {
			log.Printf("[serverops] 标记过期传输失败 id=%s: %v", id, err)
			continue
		}
		// 过期后远端 part 无法无密码清理 → 转为 cleanup_pending 等待精确清理
		_, _ = s.db.Exec(
			`UPDATE server_file_transfers SET state=? WHERE id=? AND state=? AND remote_temp_path != ''`,
			TransferCleanupPending, id, TransferExpired)
		s.untrackUpload(id)
	}
	log.Printf("[serverops] 处理过期上传 %d 条", len(ids))
}

// checkAuthRateLimit 密码失败限速；超过返回错误。
func (s *Service) checkAuthRateLimit(user, resourceID, clientIP string) error {
	key := user + "\x00" + resourceID + "\x00" + clientIP
	s.authFailMu.Lock()
	defer s.authFailMu.Unlock()
	now := time.Now()
	cut := now.Add(-authFailWindow)
	var kept []time.Time
	for _, t := range s.authFails[key] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	s.authFails[key] = kept
	if len(kept) >= authFailLimit {
		return apiErr(CodeSSHAuthRateLimited, "密码尝试过于频繁，请稍后再试", true)
	}
	return nil
}

func (s *Service) recordAuthFail(user, resourceID, clientIP string) {
	key := user + "\x00" + resourceID + "\x00" + clientIP
	s.authFailMu.Lock()
	defer s.authFailMu.Unlock()
	s.authFails[key] = append(s.authFails[key], time.Now())
}

func (s *Service) clearAuthFail(user, resourceID, clientIP string) {
	key := user + "\x00" + resourceID + "\x00" + clientIP
	s.authFailMu.Lock()
	defer s.authFailMu.Unlock()
	delete(s.authFails, key)
}
