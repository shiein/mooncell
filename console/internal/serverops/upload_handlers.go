// SFTP 分块上传：初始化、分块、完成、取消、恢复。
// Console 不缓存完整文件；远端同目录临时 part，完成后 rename。
package serverops

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// InitUpload POST .../sessions/{sid}/uploads
func (s *Service) InitUpload(w http.ResponseWriter, r *http.Request) {
	user, _, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "未登录", false)
		return
	}
	resourceID := r.PathValue("id")
	sessionID := r.PathValue("sid")
	sess := s.sess.Get(sessionID, user, resourceID)
	if sess == nil {
		writeErr(w, http.StatusNotFound, CodeSessionClosed, "会话不存在或已结束", false)
		return
	}
	sc, err := sess.SFTP()
	if err != nil {
		writeAPIError(w, err)
		return
	}

	var body struct {
		Directory string `json:"directory"`
		Filename  string `json:"filename"`
		Size      int64  `json:"size"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "请求格式错误", false)
		return
	}
	if body.Size <= 0 || body.Size > s.cfg.maxUploadBytes() {
		writeErr(w, http.StatusRequestEntityTooLarge, CodeTransferTooLarge, "文件大小超出上传上限", false)
		return
	}
	if err := validateFilename(body.Filename); err != nil {
		writeAPIError(w, err)
		return
	}
	dir, err := cleanRemotePath(body.Directory)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if dir == "." {
		if wd, err := sc.Getwd(); err == nil {
			dir = wd
		}
	}

	// 并发传输限额
	if err := s.checkTransferLimits(user); err != nil {
		writeAPIError(w, err)
		return
	}

	remotePath := joinRemote(dir, body.Filename)
	// 目标已存在且不允许覆盖
	if st, err := sc.Stat(remotePath); err == nil && st != nil && !body.Overwrite {
		writeErr(w, http.StatusConflict, CodeRemoteTargetExists, "目标文件已存在", false)
		return
	}

	randB := make([]byte, 8)
	_, _ = rand.Read(randB)
	tempName := uploadTempName(body.Filename, hex.EncodeToString(randB))
	tempPath := path.Join(dir, tempName)

	// 创建远端 part（同目录，完成后 rename 不跨文件系统）
	f, err := sc.OpenFile(tempPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		writeErr(w, http.StatusBadGateway, CodeSSHConnectionFailed, "创建远端临时文件失败", true)
		return
	}
	_ = f.Close()

	now := time.Now().UnixMilli()
	tid := newID("tx")
	tr := FileTransfer{
		ID:              tid,
		ResourceID:      resourceID,
		Username:        user,
		Direction:       DirectionUpload,
		RemotePath:      remotePath,
		RemoteTempPath:  tempPath,
		ExpectedSize:    body.Size,
		TransferredSize: 0,
		State:           TransferUploading,
		CreatedAt:       now,
		UpdatedAt:       now,
		ExpiresAt:       now + s.cfg.transferResumeTTL().Milliseconds(),
	}
	if err := insertTransfer(s.db, tr); err != nil {
		_ = sc.Remove(tempPath)
		writeErr(w, http.StatusInternalServerError, CodeDBError, "保存传输记录失败", true)
		return
	}
	s.trackUpload(tid)

	resName := resourceID
	if res, found, _ := getResource(s.db, resourceID); found {
		resName = res.Name
	}
	s.auditLog(user, "SFTP 上传开始", auditFileTarget(resName, "upload", remotePath, body.Size), "开始")
	if s.touch != nil {
		s.touch(user)
	}

	writeOK(w, UploadInitResponse{
		TransferID: tid,
		ChunkSize:  ChunkSize,
		NextOffset: 0,
		ExpiresAt:  tr.ExpiresAt,
	})
}

// UploadChunk PUT .../uploads/{tid}?offset=
func (s *Service) UploadChunk(w http.ResponseWriter, r *http.Request) {
	user, _, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "未登录", false)
		return
	}
	resourceID := r.PathValue("id")
	sessionID := r.PathValue("sid")
	tid := r.PathValue("tid")

	sess := s.sess.Get(sessionID, user, resourceID)
	if sess == nil {
		writeErr(w, http.StatusNotFound, CodeSessionClosed, "会话不存在或已结束", false)
		return
	}
	tr, found, err := getTransfer(s.db, tid)
	if err != nil || !found {
		writeErr(w, http.StatusNotFound, CodeNotFound, "传输不存在", false)
		return
	}
	if tr.Username != user || tr.ResourceID != resourceID {
		writeErr(w, http.StatusNotFound, CodeNotFound, "传输不存在", false)
		return
	}
	if tr.State != TransferUploading {
		writeErr(w, http.StatusConflict, CodeResourceChanged, "传输状态不可写入", false)
		return
	}

	offsetStr := r.URL.Query().Get("offset")
	offset, err := strconv.ParseInt(offsetStr, 10, 64)
	if err != nil || offset < 0 {
		writeErr(w, http.StatusBadRequest, CodeValidation, "offset 无效", false)
		return
	}

	lock := s.uploadLock(tid)
	lock.Lock()
	defer lock.Unlock()

	// 重读权威进度
	tr, _, err = getTransfer(s.db, tid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeDBError, "读取传输失败", true)
		return
	}
	if offset != tr.TransferredSize {
		// 重复已确认块：幂等返回当前进度
		if offset < tr.TransferredSize {
			writeOK(w, map[string]any{"nextOffset": tr.TransferredSize, "transferredSize": tr.TransferredSize})
			return
		}
		writeErrOffset(w, http.StatusConflict, CodeChunkOffsetMismatch, "分块 offset 不一致", tr.TransferredSize)
		return
	}

	// 单块硬限制 8 MiB
	r.Body = http.MaxBytesReader(w, r.Body, ChunkSize+1)
	buf := make([]byte, ChunkSize)
	n, readErr := io.ReadFull(r.Body, buf)
	if readErr == io.ErrUnexpectedEOF || readErr == io.EOF {
		// 最后一块可小于 ChunkSize
		buf = buf[:n]
		readErr = nil
	}
	if readErr != nil {
		if strings.Contains(readErr.Error(), "request body too large") {
			writeErr(w, http.StatusRequestEntityTooLarge, CodeTransferTooLarge, "分块过大", false)
			return
		}
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "读取分块失败", true)
		return
	}
	if int64(len(buf)) == 0 {
		writeErr(w, http.StatusBadRequest, CodeValidation, "空分块", false)
		return
	}
	if tr.TransferredSize+int64(len(buf)) > tr.ExpectedSize {
		writeErr(w, http.StatusBadRequest, CodeValidation, "分块超出声明大小", false)
		return
	}

	// 可选块校验
	if want := r.Header.Get("X-Chunk-SHA256"); want != "" {
		sum := sha256.Sum256(buf)
		got := hex.EncodeToString(sum[:])
		if !strings.EqualFold(got, want) {
			writeErr(w, http.StatusBadRequest, CodeValidation, "分块校验失败", true)
			return
		}
	}

	sc, err := sess.SFTP()
	if err != nil {
		writeAPIError(w, err)
		return
	}
	f, err := sc.OpenFile(tr.RemoteTempPath, os.O_WRONLY)
	if err != nil {
		writeErr(w, http.StatusBadGateway, CodeSSHConnectionFailed, "打开远端临时文件失败", true)
		return
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		writeErr(w, http.StatusBadGateway, CodeSSHConnectionFailed, "定位远端临时文件失败", true)
		return
	}
	if _, err := f.Write(buf); err != nil {
		// 写失败：truncate 回块前 offset
		_ = f.Truncate(offset)
		writeErr(w, http.StatusBadGateway, CodeSSHConnectionFailed, "写入远端失败", true)
		return
	}

	now := time.Now().UnixMilli()
	newSize := offset + int64(len(buf))
	if err := updateTransferProgress(s.db, tid, newSize, now); err != nil {
		_ = f.Truncate(offset)
		writeErr(w, http.StatusInternalServerError, CodeDBError, "更新进度失败", true)
		return
	}

	if s.touch != nil {
		s.touch(user)
	}
	writeOK(w, map[string]any{
		"nextOffset":      newSize,
		"transferredSize": newSize,
		"complete":        newSize >= tr.ExpectedSize,
	})
}

// GetUploadStatus GET /api/server-resources/{id}/uploads/{tid}
func (s *Service) GetUploadStatus(w http.ResponseWriter, r *http.Request) {
	user, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "未登录", false)
		return
	}
	resourceID := r.PathValue("id")
	tid := r.PathValue("tid")
	if _, err := RequireAccess(s.db, user, role, resourceID); err != nil {
		writeAPIError(w, err)
		return
	}
	tr, found, err := getTransfer(s.db, tid)
	if err != nil || !found || tr.ResourceID != resourceID {
		writeErr(w, http.StatusNotFound, CodeNotFound, "传输不存在", false)
		return
	}
	// 仅 owner 或 admin
	if tr.Username != user && !isAdmin(role) {
		writeErr(w, http.StatusNotFound, CodeNotFound, "传输不存在", false)
		return
	}
	writeOK(w, map[string]any{
		"transferId":      tr.ID,
		"state":           tr.State,
		"expectedSize":    tr.ExpectedSize,
		"transferredSize": tr.TransferredSize,
		"nextOffset":      tr.TransferredSize,
		"remotePath":      path.Base(tr.RemotePath), // 不回完整路径给列表外用途；工作台续传需要 path，见 Resume
		"expiresAt":       tr.ExpiresAt,
	})
}

// ResumeUpload POST .../uploads/{tid}/resume — 绑定新 SSH session 继续上传
func (s *Service) ResumeUpload(w http.ResponseWriter, r *http.Request) {
	user, _, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "未登录", false)
		return
	}
	resourceID := r.PathValue("id")
	sessionID := r.PathValue("sid")
	tid := r.PathValue("tid")
	sess := s.sess.Get(sessionID, user, resourceID)
	if sess == nil {
		writeErr(w, http.StatusNotFound, CodeSessionClosed, "会话不存在或已结束", false)
		return
	}
	tr, found, err := getTransfer(s.db, tid)
	if err != nil || !found || tr.Username != user || tr.ResourceID != resourceID {
		writeErr(w, http.StatusNotFound, CodeNotFound, "传输不存在", false)
		return
	}
	if tr.State != TransferUploading {
		writeErr(w, http.StatusConflict, CodeResourceChanged, "传输不可恢复", false)
		return
	}
	sc, err := sess.SFTP()
	if err != nil {
		writeAPIError(w, err)
		return
	}
	st, err := sc.Stat(tr.RemoteTempPath)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, CodeRemotePartChanged, "远端临时文件不存在", false)
		return
	}
	if st.Size() != tr.TransferredSize {
		writeErr(w, http.StatusUnprocessableEntity, CodeRemotePartChanged, "远端临时文件与记录不一致", false)
		return
	}
	writeOK(w, map[string]any{
		"transferId":      tr.ID,
		"chunkSize":       ChunkSize,
		"nextOffset":      tr.TransferredSize,
		"expectedSize":    tr.ExpectedSize,
		"transferredSize": tr.TransferredSize,
		"expiresAt":       tr.ExpiresAt,
	})
}

// CompleteUpload POST .../uploads/{tid}/complete
func (s *Service) CompleteUpload(w http.ResponseWriter, r *http.Request) {
	user, _, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "未登录", false)
		return
	}
	resourceID := r.PathValue("id")
	sessionID := r.PathValue("sid")
	tid := r.PathValue("tid")
	sess := s.sess.Get(sessionID, user, resourceID)
	if sess == nil {
		writeErr(w, http.StatusNotFound, CodeSessionClosed, "会话不存在或已结束", false)
		return
	}

	lock := s.uploadLock(tid)
	lock.Lock()
	defer lock.Unlock()

	tr, found, err := getTransfer(s.db, tid)
	if err != nil || !found || tr.Username != user || tr.ResourceID != resourceID {
		writeErr(w, http.StatusNotFound, CodeNotFound, "传输不存在", false)
		return
	}
	if tr.State != TransferUploading {
		writeErr(w, http.StatusConflict, CodeResourceChanged, "传输状态不可完成", false)
		return
	}
	if tr.TransferredSize != tr.ExpectedSize {
		writeErr(w, http.StatusBadRequest, CodeValidation, "上传未完成", false)
		return
	}

	sc, err := sess.SFTP()
	if err != nil {
		writeAPIError(w, err)
		return
	}
	st, err := sc.Stat(tr.RemoteTempPath)
	if err != nil || st.Size() != tr.ExpectedSize {
		writeErr(w, http.StatusUnprocessableEntity, CodeRemotePartChanged, "远端临时文件大小不一致", false)
		return
	}

	now := time.Now().UnixMilli()
	_ = updateTransferState(s.db, tid, TransferCompleting, now)

	// 目标是否存在：存在则必须原子替换，不支持时明确失败且不删原文件
	_, targetErr := sc.Stat(tr.RemotePath)
	targetExists := targetErr == nil
	if targetExists {
		if err := sc.PosixRename(tr.RemoteTempPath, tr.RemotePath); err != nil {
			_ = updateTransferState(s.db, tid, TransferUploading, now)
			writeErr(w, http.StatusUnprocessableEntity, CodeAtomicReplaceUnsupp,
				"远端不支持安全覆盖，已保留原文件", false)
			return
		}
	} else {
		if err := sc.Rename(tr.RemoteTempPath, tr.RemotePath); err != nil {
			if err2 := sc.PosixRename(tr.RemoteTempPath, tr.RemotePath); err2 != nil {
				_ = updateTransferState(s.db, tid, TransferUploading, now)
				writeErr(w, http.StatusBadGateway, CodeSSHConnectionFailed, "重命名完成失败", true)
				return
			}
		}
	}

	_ = updateTransferState(s.db, tid, TransferCompleted, now)
	s.untrackUpload(tid)

	resName := resourceID
	if res, found, _ := getResource(s.db, resourceID); found {
		resName = res.Name
	}
	s.auditLog(user, "SFTP 上传", auditFileTarget(resName, "upload", tr.RemotePath, tr.ExpectedSize), "成功")

	final, _ := sc.Stat(tr.RemotePath)
	var size int64
	var mod int64
	if final != nil {
		size = final.Size()
		mod = final.ModTime().UnixMilli()
	}
	writeOK(w, map[string]any{
		"ok":   true,
		"path": tr.RemotePath,
		"size": size,
		"modifiedAt": mod,
	})
}

// CancelUpload DELETE .../uploads/{tid}
func (s *Service) CancelUpload(w http.ResponseWriter, r *http.Request) {
	user, _, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "未登录", false)
		return
	}
	resourceID := r.PathValue("id")
	sessionID := r.PathValue("sid")
	tid := r.PathValue("tid")
	sess := s.sess.Get(sessionID, user, resourceID)

	tr, found, err := getTransfer(s.db, tid)
	if err != nil || !found || tr.Username != user || tr.ResourceID != resourceID {
		writeErr(w, http.StatusNotFound, CodeNotFound, "传输不存在", false)
		return
	}
	now := time.Now().UnixMilli()
	cleaned := false
	if sess != nil && tr.RemoteTempPath != "" {
		if sc, err := sess.SFTP(); err == nil {
			if err := sc.Remove(tr.RemoteTempPath); err == nil {
				cleaned = true
			}
		}
	}
	state := TransferCancelled
	if !cleaned && tr.RemoteTempPath != "" {
		state = TransferCleanupPending
	}
	_ = updateTransferState(s.db, tid, state, now)
	s.untrackUpload(tid)

	resName := resourceID
	if res, found, _ := getResource(s.db, resourceID); found {
		resName = res.Name
	}
	s.auditLog(user, "SFTP 上传取消", auditFileTarget(resName, "upload", tr.RemotePath, tr.ExpectedSize), state)
	writeOK(w, map[string]any{"ok": true, "state": state})
}

func (s *Service) checkTransferLimits(user string) error {
	s.transferMu.Lock()
	defer s.transferMu.Unlock()
	if len(s.activeUploads) >= s.cfg.SFTPMaxTransfersTotal {
		return apiErr(CodeTransferLimit, "全局文件传输数已达上限", true)
	}
	// 用户维度：查 DB 中 uploading 数
	var n int
	_ = s.db.QueryRow(
		`SELECT COUNT(*) FROM server_file_transfers WHERE username=? AND state=?`,
		user, TransferUploading).Scan(&n)
	if n >= s.cfg.SFTPMaxTransfersPerUser {
		return apiErr(CodeTransferLimit, "您的文件传输数已达上限", true)
	}
	return nil
}

func (s *Service) trackUpload(tid string) {
	s.transferMu.Lock()
	s.activeUploads[tid] = struct{}{}
	s.transferMu.Unlock()
}

func (s *Service) untrackUpload(tid string) {
	s.transferMu.Lock()
	delete(s.activeUploads, tid)
	delete(s.uploadLocks, tid)
	s.transferMu.Unlock()
}

func (s *Service) uploadLock(tid string) *sync.Mutex {
	s.transferMu.Lock()
	defer s.transferMu.Unlock()
	if s.uploadLocks[tid] == nil {
		s.uploadLocks[tid] = &sync.Mutex{}
	}
	return s.uploadLocks[tid]
}
