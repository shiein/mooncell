// SFTP 分块上传：初始化、分块、完成、取消、恢复。
// Console 不缓存完整文件；远端同目录临时 part，完成后 rename。
package serverops

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	sess.Touch()
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

	if err := s.checkTransferLimits(user); err != nil {
		writeAPIError(w, err)
		return
	}

	remotePath := joinRemote(dir, body.Filename)
	if st, err := sc.Stat(remotePath); err == nil && st != nil && !body.Overwrite {
		writeErr(w, http.StatusConflict, CodeRemoteTargetExists, "目标文件已存在", false)
		return
	}

	randB := make([]byte, 8)
	_, _ = rand.Read(randB)
	tempName := uploadTempName(body.Filename, hex.EncodeToString(randB))
	tempPath := path.Join(dir, tempName)

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
		Overwrite:       body.Overwrite,
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
	sess.Touch()
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

	tr, _, err = getTransfer(s.db, tid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeDBError, "读取传输失败", true)
		return
	}
	if offset != tr.TransferredSize {
		if offset < tr.TransferredSize {
			writeOK(w, map[string]any{"nextOffset": tr.TransferredSize, "transferredSize": tr.TransferredSize})
			return
		}
		writeErrOffset(w, http.StatusConflict, CodeChunkOffsetMismatch, "分块 offset 不一致", tr.TransferredSize)
		return
	}

	// 单块硬限制：MaxBytesReader 限制为 ChunkSize；再读 1 字节确认没有更多数据。
	r.Body = http.MaxBytesReader(w, r.Body, int64(ChunkSize))
	buf := make([]byte, ChunkSize)
	n, readErr := io.ReadFull(r.Body, buf)
	if readErr == io.ErrUnexpectedEOF || readErr == io.EOF {
		buf = buf[:n]
		readErr = nil
	}
	if readErr != nil {
		msg := readErr.Error()
		if errors.Is(readErr, http.ErrBodyReadAfterClose) ||
			strings.Contains(msg, "request body too large") ||
			strings.Contains(msg, "http: request body too large") {
			writeErr(w, http.StatusRequestEntityTooLarge, CodeTransferTooLarge, "分块超过 8 MiB 上限", false)
			return
		}
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "读取分块失败", true)
		return
	}
	// 确认 body 已耗尽：再读应得到 EOF；若仍有字节说明被截断前已满（MaxBytesReader 会在超过时错误）。
	// 上面 MaxBytesReader(ChunkSize) 在第 ChunkSize+1 字节会返回错误，不会静默成功。
	if len(buf) == 0 {
		writeErr(w, http.StatusBadRequest, CodeValidation, "空分块", false)
		return
	}
	if tr.TransferredSize+int64(len(buf)) > tr.ExpectedSize {
		writeErr(w, http.StatusBadRequest, CodeValidation, "分块超出声明大小", false)
		return
	}

	// 块校验：要求 X-Chunk-SHA256，防止静默损坏推进 offset。
	want := strings.TrimSpace(r.Header.Get("X-Chunk-SHA256"))
	if want == "" {
		writeErr(w, http.StatusBadRequest, CodeValidation, "缺少 X-Chunk-SHA256", false)
		return
	}
	sum := sha256.Sum256(buf)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, want) {
		writeErr(w, http.StatusBadRequest, CodeValidation, "分块校验失败", true)
		return
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

// ListActiveUploads GET /api/server-resources/{id}/uploads?active=1
// 返回当前用户可续传的 uploading 列表（断点续传入口）。
func (s *Service) ListActiveUploads(w http.ResponseWriter, r *http.Request) {
	user, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "未登录", false)
		return
	}
	resourceID := r.PathValue("id")
	if _, err := RequireAccess(s.db, user, role, resourceID); err != nil {
		writeAPIError(w, err)
		return
	}
	list, err := listActiveTransfers(s.db, user, resourceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeDBError, "读取传输列表失败", true)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		out = append(out, map[string]any{
			"transferId":      t.ID,
			"filename":        path.Base(t.RemotePath),
			"directory":       path.Dir(t.RemotePath),
			"expectedSize":    t.ExpectedSize,
			"transferredSize": t.TransferredSize,
			"nextOffset":      t.TransferredSize,
			"overwrite":       t.Overwrite,
			"state":           t.State,
			"expiresAt":       t.ExpiresAt,
			"updatedAt":       t.UpdatedAt,
		})
	}
	writeOK(w, map[string]any{"transfers": out})
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
		"filename":        path.Base(tr.RemotePath),
		"directory":       path.Dir(tr.RemotePath),
		"overwrite":       tr.Overwrite,
		"expiresAt":       tr.ExpiresAt,
	})
}

// ResumeUpload POST .../uploads/{tid}/resume
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
	sess.Touch()
	tr, found, err := getTransfer(s.db, tid)
	if err != nil || !found || tr.Username != user || tr.ResourceID != resourceID {
		writeErr(w, http.StatusNotFound, CodeNotFound, "传输不存在", false)
		return
	}
	if tr.State != TransferUploading {
		writeErr(w, http.StatusConflict, CodeResourceChanged, "传输不可恢复", false)
		return
	}
	// 可选：客户端声明本地文件大小，必须与 expected 一致。
	var body struct {
		LocalSize int64 `json:"localSize"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.LocalSize > 0 && body.LocalSize != tr.ExpectedSize {
		writeErr(w, http.StatusUnprocessableEntity, CodeRemotePartChanged, "本地文件大小与上传记录不一致", false)
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
	s.trackUpload(tid)
	writeOK(w, map[string]any{
		"transferId":      tr.ID,
		"chunkSize":       ChunkSize,
		"nextOffset":      tr.TransferredSize,
		"expectedSize":    tr.ExpectedSize,
		"transferredSize": tr.TransferredSize,
		"overwrite":       tr.Overwrite,
		"filename":        path.Base(tr.RemotePath),
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
	sess.Touch()

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
	if err := updateTransferState(s.db, tid, TransferCompleting, now); err != nil {
		writeErr(w, http.StatusInternalServerError, CodeDBError, "更新传输状态失败", true)
		return
	}

	// 完成阶段重新检查目标是否存在，严格遵守初始化时持久化的 overwrite。
	_, targetErr := sc.Stat(tr.RemotePath)
	targetExists := targetErr == nil
	if targetExists && !tr.Overwrite {
		_ = updateTransferState(s.db, tid, TransferUploading, now)
		writeErr(w, http.StatusConflict, CodeRemoteTargetExists, "目标文件已存在且未允许覆盖", false)
		return
	}
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

	if err := updateTransferState(s.db, tid, TransferCompleted, now); err != nil {
		// 远端已 rename 成功但元数据失败：仍返回错误，避免伪报成功；part 已不存在。
		writeErr(w, http.StatusInternalServerError, CodeDBError, "远端文件已就绪，但更新传输记录失败", false)
		return
	}
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
		"ok":         true,
		"path":       tr.RemotePath,
		"size":       size,
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
	if sess != nil {
		sess.Touch()
	}

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
	if err := updateTransferState(s.db, tid, state, now); err != nil {
		writeErr(w, http.StatusInternalServerError, CodeDBError, "更新取消状态失败", true)
		return
	}
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
	var n int
	_ = s.db.QueryRow(
		`SELECT COUNT(*) FROM server_file_transfers WHERE username=? AND state=?`,
		user, TransferUploading).Scan(&n)
	if n >= s.cfg.SFTPMaxTransfersPerUser {
		return apiErr(CodeTransferLimit, "您的文件传输数已达上限", true)
	}
	return nil
}

// tryBeginDownload 占用一个下载并发槽；失败返回错误。
func (s *Service) tryBeginDownload(user string) error {
	return s.checkTransferLimits(user) // 与上传共用传输限额语义
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
