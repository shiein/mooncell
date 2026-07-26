// 模块生命周期与依赖组装。
// serverops 只通过构造参数接收 *sql.DB、配置、审计回调与会话校验回调。
package serverops

import (
	"database/sql"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// SessionValidator 校验 Mooncell 登录会话是否仍有效（由 consoleapp 注入）。
// username 非空且 valid=true 表示 mc_sid 仍绑定该用户。
type SessionValidator func(username string) bool

// TouchSession 标记用户有主动活动（可节流后滑动续期）。
type TouchSession func(username string)

// Service 持有服务器运维模块的运行时依赖。
type Service struct {
	db    *sql.DB
	cfg   Config
	audit AuditFunc
	valid SessionValidator
	touch TouchSession
	sess  *SessionManager

	// 密码失败限速：(user+"\x00"+resource+"\x00"+ip) → 时间戳列表。
	authFailMu sync.Mutex
	authFails  map[string][]time.Time

	// 活动传输计数（内存），配合 SQLite 元数据。
	transferMu       sync.Mutex
	activeTransfers  map[string]string    // transferId/downloadId -> Mooncell username
	transferLastAct  map[string]time.Time // 槽位最后活动，用于释放僵尸内存槽
	uploadLocks      map[string]*sync.Mutex

	closed atomic.Bool
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// 内存传输槽无进展多久后释放（DB 记录保留，续传时 re-reserve）。
const transferSlotIdle = 5 * time.Minute

// NewService 创建服务。cfg 会 Normalize。不启动后台 goroutine（由 Start 负责）。
func NewService(db *sql.DB, cfg Config) *Service {
	cfg.Normalize()
	return &Service{
		db:              db,
		cfg:             cfg,
		sess:            newSessionManager(),
		authFails:       map[string][]time.Time{},
		activeTransfers: map[string]string{},
		transferLastAct: map[string]time.Time{},
		uploadLocks:     map[string]*sync.Mutex{},
		stopCh:          make(chan struct{}),
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
				s.reapIdleTransferSlots()
			case <-transferTicker.C:
				s.expireStaleTransfers()
			}
		}
	}()
}

// Close 关闭全部 SSH/SFTP 与后台任务。
func (s *Service) Close() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
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
	// uploading 过期 + completing 卡住（updated_at 超过 30 分钟）均回收。
	const completingStuckMs = int64(30 * 60 * 1000)
	rows, err := s.db.Query(
		`SELECT id, state FROM server_file_transfers
		 WHERE (state=? AND expires_at > 0 AND expires_at < ?)
		    OR (state=? AND updated_at > 0 AND updated_at < ?)`,
		TransferUploading, now,
		TransferCompleting, now-completingStuckMs)
	if err != nil {
		log.Printf("[serverops] 查询过期传输失败: %v", err)
		return
	}
	type item struct {
		id, state string
	}
	var ids []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.state); err != nil {
			rows.Close()
			return
		}
		ids = append(ids, it)
	}
	rows.Close()
	if len(ids) == 0 {
		return
	}
	for _, it := range ids {
		lock := s.uploadLock(it.id)
		lock.Lock()
		tr, found, err := getTransfer(s.db, it.id)
		if err != nil || !found {
			lock.Unlock()
			continue
		}
		from := tr.State
		if from != TransferUploading && from != TransferCompleting {
			lock.Unlock()
			continue
		}
		if from == TransferUploading && (tr.ExpiresAt <= 0 || tr.ExpiresAt >= now) {
			lock.Unlock()
			continue
		}
		if from == TransferCompleting && tr.UpdatedAt >= now-completingStuckMs {
			lock.Unlock()
			continue
		}
		next := TransferExpired
		if tr.RemoteTempPath != "" {
			next = TransferCleanupPending
		}
		updated, err := updateTransferStateFrom(s.db, it.id, from, next, now)
		if err != nil {
			log.Printf("[serverops] 标记过期传输失败 id=%s: %v", it.id, err)
		} else if updated {
			s.releaseTransfer(it.id)
		}
		lock.Unlock()
	}
	log.Printf("[serverops] 处理过期/卡住上传 %d 条", len(ids))
}

// reapIdleTransferSlots 释放长时间无活动的内存槽（DB 记录保留）。
func (s *Service) reapIdleTransferSlots() {
	s.transferMu.Lock()
	defer s.transferMu.Unlock()
	cut := time.Now().Add(-transferSlotIdle)
	for id, t := range s.transferLastAct {
		if t.Before(cut) {
			delete(s.activeTransfers, id)
			delete(s.transferLastAct, id)
			delete(s.uploadLocks, id)
		}
	}
}

// cleanupPendingTransfers 使用本次已认证 SSH session 精确清理由数据库记录的远端 part。
// 失败记录保持 cleanup_pending，供下一次成功连接继续重试。
func (s *Service) cleanupPendingTransfers(sess *Session) {
	if sess == nil || sess.closed.Load() {
		return
	}
	pending, err := listCleanupPending(s.db, sess.ResourceID)
	if err != nil || len(pending) == 0 {
		if err != nil {
			log.Printf("[serverops] 查询待清理上传失败 resource=%s: %v", sess.ResourceID, err)
		}
		return
	}
	sc, err := sess.SFTP()
	if err != nil {
		log.Printf("[serverops] 待清理上传无法打开 SFTP resource=%s", sess.ResourceID)
		return
	}
	for _, item := range pending {
		lock := s.uploadLock(item.ID)
		lock.Lock()
		current, found, err := getTransfer(s.db, item.ID)
		if err != nil || !found || current.ResourceID != sess.ResourceID ||
			current.State != TransferCleanupPending ||
			!isManagedUploadTemp(current.RemotePath, current.RemoteTempPath) {
			lock.Unlock()
			continue
		}
		if err := sc.Remove(current.RemoteTempPath); err != nil && !os.IsNotExist(err) {
			lock.Unlock()
			continue
		}
		updated, err := updateTransferStateFrom(
			s.db, current.ID, TransferCleanupPending, TransferCancelled, time.Now().UnixMilli())
		if err != nil {
			log.Printf("[serverops] 更新待清理上传状态失败 id=%s: %v", current.ID, err)
		} else if updated {
			s.releaseTransfer(current.ID)
		}
		lock.Unlock()
	}
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
	now := time.Now()
	cut := now.Add(-authFailWindow)
	// 顺带清理其它 key 的过期记录，避免 map 只增不减。
	for k, ts := range s.authFails {
		var kept []time.Time
		for _, t := range ts {
			if t.After(cut) {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(s.authFails, k)
		} else {
			s.authFails[k] = kept
		}
	}
	s.authFails[key] = append(s.authFails[key], now)
}

func (s *Service) clearAuthFail(user, resourceID, clientIP string) {
	key := user + "\x00" + resourceID + "\x00" + clientIP
	s.authFailMu.Lock()
	defer s.authFailMu.Unlock()
	delete(s.authFails, key)
}
