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

	tid := newID("tx")
	newReservation, err := s.reserveTransfer(tid, user)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	keepReservation := false
	defer func() {
		if newReservation && !keepReservation {
			s.releaseTransfer(tid)
		}
	}()

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
	keepReservation = true

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

	offsetStr := r.URL.Query().Get("offset")
	offset, err := strconv.ParseInt(offsetStr, 10, 64)
	if err != nil || offset < 0 {
		writeErr(w, http.StatusBadRequest, CodeValidation, "offset 无效", false)
		return
	}

	lock := s.uploadLock(tid)
	lock.Lock()
	defer lock.Unlock()

	tr, found, err := getTransfer(s.db, tid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeDBError, "读取传输失败", true)
		return
	}
	if !found || tr.Username != user || tr.ResourceID != resourceID {
		writeErr(w, http.StatusNotFound, CodeNotFound, "传输不存在", false)
		return
	}
	if tr.State != TransferUploading {
		writeErr(w, http.StatusConflict, CodeResourceChanged, "传输状态不可写入", false)
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
	if _, err := s.reserveTransfer(tid, user); err != nil {
		writeAPIError(w, err)
		return
	}

	// 允许实际读取第 ChunkSize+1 字节，明确判断超限；不能只读满 8 MiB 后假定 EOF。
	buf, readErr := readUploadChunk(w, r)
	if readErr != nil {
		if errors.Is(readErr, errChunkTooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, CodeTransferTooLarge, "分块超过 8 MiB 上限", false)
			return
		}
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "读取分块失败", true)
		return
	}
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
	if !validSHA256Hex(want) {
		writeErr(w, http.StatusBadRequest, CodeValidation, "缺少或无效的 X-Chunk-SHA256", false)
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
	written, err := f.Write(buf)
	if err != nil || written != len(buf) {
		_ = f.Truncate(offset)
		writeErr(w, http.StatusBadGateway, CodeSSHConnectionFailed, "写入远端失败", true)
		return
	}

	now := time.Now().UnixMilli()
	newSize := offset + int64(len(buf))
	if err := recordTransferChunk(s.db, tid, offset, int64(len(buf)), got, newSize, now); err != nil {
		_ = f.Truncate(offset)
		writeErr(w, http.StatusInternalServerError, CodeDBError, "更新进度失败", true)
		return
	}
	s.touchTransferSlot(tid)

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
			"chunkSize":       ChunkSize,
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
		"chunkSize":       ChunkSize,
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
	lock := s.uploadLock(tid)
	lock.Lock()
	defer lock.Unlock()

	tr, found, err := getTransfer(s.db, tid)
	if err != nil || !found || tr.Username != user || tr.ResourceID != resourceID {
		writeErr(w, http.StatusNotFound, CodeNotFound, "传输不存在", false)
		return
	}
	if tr.State != TransferUploading {
		writeErr(w, http.StatusConflict, CodeResourceChanged, "传输不可恢复", false)
		return
	}
	if tr.ExpiresAt > 0 && tr.ExpiresAt <= time.Now().UnixMilli() {
		writeErr(w, http.StatusConflict, CodeResourceChanged, "传输已过期，请丢弃后重新上传", false)
		return
	}
	// 客户端须重新计算已上传前缀每块摘要，证明选择的是同一份本地文件。
	var body struct {
		LocalSize    int64              `json:"localSize"`
		PrefixChunks []UploadChunkProof `json:"prefixChunks"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "请求格式错误", false)
		return
	}
	if body.LocalSize != tr.ExpectedSize {
		writeErr(w, http.StatusUnprocessableEntity, CodeRemotePartChanged, "本地文件大小与上传记录不一致", false)
		return
	}
	storedChunks, err := listTransferChunks(s.db, tid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeDBError, "读取分块身份失败", true)
		return
	}
	if err := validateResumeProof(tr.TransferredSize, storedChunks, body.PrefixChunks); err != nil {
		writeAPIError(w, err)
		return
	}

	newReservation, err := s.reserveTransfer(tid, user)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	keepReservation := false
	defer func() {
		if newReservation && !keepReservation {
			s.releaseTransfer(tid)
		}
	}()

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
	remoteSize := st.Size()
	switch {
	case remoteSize == tr.TransferredSize:
		// 与权威进度一致，可续传。
	case remoteSize > tr.TransferredSize:
		// 写块中途断连时 part 可能多出未确认字节；前缀已由 chunk SHA256 证明，截断回 DB offset 安全。
		f, openErr := sc.OpenFile(tr.RemoteTempPath, os.O_WRONLY)
		if openErr != nil {
			writeErr(w, http.StatusUnprocessableEntity, CodeRemotePartChanged, "远端临时文件无法截断", false)
			return
		}
		truncErr := f.Truncate(tr.TransferredSize)
		_ = f.Close()
		if truncErr != nil {
			writeErr(w, http.StatusUnprocessableEntity, CodeRemotePartChanged, "远端临时文件截断失败", false)
			return
		}
	default:
		// part 比记录短：无法安全猜测缺失数据
		writeErr(w, http.StatusUnprocessableEntity, CodeRemotePartChanged, "远端临时文件与记录不一致", false)
		return
	}
	keepReservation = true
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
		if err := updateTransferState(s.db, tid, TransferUploading, now); err != nil {
			writeErr(w, http.StatusInternalServerError, CodeDBError, "恢复传输状态失败", true)
			return
		}
		writeErr(w, http.StatusConflict, CodeRemoteTargetExists, "目标文件已存在且未允许覆盖", false)
		return
	}
	if targetExists {
		if err := sc.PosixRename(tr.RemoteTempPath, tr.RemotePath); err != nil {
			if stateErr := updateTransferState(s.db, tid, TransferUploading, now); stateErr != nil {
				writeErr(w, http.StatusInternalServerError, CodeDBError, "恢复传输状态失败", true)
				return
			}
			writeErr(w, http.StatusUnprocessableEntity, CodeAtomicReplaceUnsupp,
				"远端不支持安全覆盖，已保留原文件", false)
			return
		}
	} else {
		// 非覆盖：仅用非原子 Rename。失败后必须重新 Stat，禁止降级 PosixRename
		//（POSIX rename 有覆盖语义，会踩中 Stat→rename 竞态新建的同名文件）。
		if err := sc.Rename(tr.RemoteTempPath, tr.RemotePath); err != nil {
			if _, againErr := sc.Stat(tr.RemotePath); againErr == nil {
				if stateErr := updateTransferState(s.db, tid, TransferUploading, now); stateErr != nil {
					writeErr(w, http.StatusInternalServerError, CodeDBError, "恢复传输状态失败", true)
					return
				}
				writeErr(w, http.StatusConflict, CodeRemoteTargetExists,
					"目标文件已存在且未允许覆盖", false)
				return
			}
			if stateErr := updateTransferState(s.db, tid, TransferUploading, now); stateErr != nil {
				writeErr(w, http.StatusInternalServerError, CodeDBError, "恢复传输状态失败", true)
				return
			}
			writeErr(w, http.StatusBadGateway, CodeSSHConnectionFailed, "重命名完成失败", true)
			return
		}
	}

	if err := updateTransferState(s.db, tid, TransferCompleted, now); err != nil {
		// 远端已 rename 成功但元数据失败：仍返回错误，避免伪报成功；part 已不存在。
		s.releaseTransfer(tid)
		writeErr(w, http.StatusInternalServerError, CodeDBError, "远端文件已就绪，但更新传输记录失败", false)
		return
	}
	s.releaseTransfer(tid)

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

	lock := s.uploadLock(tid)
	lock.Lock()
	defer lock.Unlock()

	tr, found, err := getTransfer(s.db, tid)
	if err != nil || !found || tr.Username != user || tr.ResourceID != resourceID {
		writeErr(w, http.StatusNotFound, CodeNotFound, "传输不存在", false)
		return
	}
	switch tr.State {
	case TransferCancelled:
		s.releaseTransfer(tid)
		writeOK(w, map[string]any{"ok": true, "state": TransferCancelled})
		return
	case TransferUploading, TransferCleanupPending:
		// 活动上传可取消；cleanup_pending 可在新会话中重试精确清理。
	case TransferCompleting:
		writeErr(w, http.StatusConflict, CodeResourceChanged,
			"上传完成结果正在核对，请重新连接后刷新状态", true)
		return
	default:
		writeErr(w, http.StatusConflict, CodeResourceChanged, "当前传输状态不可取消", false)
		return
	}
	now := time.Now().UnixMilli()
	cleaned := tr.RemoteTempPath == ""
	if sess != nil && tr.RemoteTempPath != "" {
		if sc, err := sess.SFTP(); err == nil {
			if err := sc.Remove(tr.RemoteTempPath); err == nil || os.IsNotExist(err) {
				cleaned = true
			}
		}
	}
	state := TransferCancelled
	if !cleaned && tr.RemoteTempPath != "" {
		state = TransferCleanupPending
	}
	updated, err := updateTransferStateFrom(s.db, tid, tr.State, state, now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeDBError, "更新取消状态失败", true)
		return
	}
	if !updated {
		writeErr(w, http.StatusConflict, CodeResourceChanged, "传输状态已变化，请刷新后重试", true)
		return
	}
	s.releaseTransfer(tid)

	resName := resourceID
	if res, found, _ := getResource(s.db, resourceID); found {
		resName = res.Name
	}
	s.auditLog(user, "SFTP 上传取消", auditFileTarget(resName, "upload", tr.RemotePath, tr.ExpectedSize), state)
	writeOK(w, map[string]any{"ok": true, "state": state})
}

// reserveTransfer 原子校验并占用文件传输槽。相同 id/owner 的恢复视为已占用成功。
func (s *Service) reserveTransfer(id, user string) (bool, error) {
	s.transferMu.Lock()
	defer s.transferMu.Unlock()
	// 顺带回收本用户长期无活动的槽，避免「关浏览器后 24h 无法上传」。
	cut := time.Now().Add(-transferSlotIdle)
	for tid, t := range s.transferLastAct {
		if t.Before(cut) {
			delete(s.activeTransfers, tid)
			delete(s.transferLastAct, tid)
		}
	}
	if owner, ok := s.activeTransfers[id]; ok {
		if owner == user {
			s.transferLastAct[id] = time.Now()
			return false, nil
		}
		return false, apiErr(CodeTransferLimit, "文件传输已被其他用户占用", true)
	}
	if len(s.activeTransfers) >= s.cfg.SFTPMaxTransfersTotal {
		return false, apiErr(CodeTransferLimit, "全局文件传输数已达上限，请稍后重试或丢弃未完成任务", true)
	}
	n := 0
	for _, owner := range s.activeTransfers {
		if owner == user {
			n++
		}
	}
	if n >= s.cfg.SFTPMaxTransfersPerUser {
		return false, apiErr(CodeTransferLimit, "您的文件传输数已达上限，可在文件面板丢弃未完成上传后重试", true)
	}
	s.activeTransfers[id] = user
	s.transferLastAct[id] = time.Now()
	return true, nil
}

// tryBeginDownload 占用一个下载并发槽；失败返回错误。
func (s *Service) tryBeginDownload(id, user string) error {
	_, err := s.reserveTransfer(id, user)
	return err
}

func (s *Service) touchTransferSlot(tid string) {
	s.transferMu.Lock()
	if _, ok := s.activeTransfers[tid]; ok {
		s.transferLastAct[tid] = time.Now()
	}
	s.transferMu.Unlock()
}

func (s *Service) releaseTransfer(tid string) {
	s.transferMu.Lock()
	delete(s.activeTransfers, tid)
	delete(s.transferLastAct, tid)
	s.transferMu.Unlock()
}

func (s *Service) uploadLock(tid string) *sync.Mutex {
	s.transferMu.Lock()
	defer s.transferMu.Unlock()
	// mutex 一旦发布便在 Service 生命周期内保持同一对象；删除后重建会让
	// 仍持有旧指针的请求与新请求失去互斥。条目体积很小，随服务重启回收。
	if s.uploadLocks[tid] == nil {
		s.uploadLocks[tid] = &sync.Mutex{}
	}
	return s.uploadLocks[tid]
}

func validSHA256Hex(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

var errChunkTooLarge = errors.New("upload chunk too large")

func readUploadChunk(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.ContentLength > int64(ChunkSize) {
		return nil, errChunkTooLarge
	}
	r.Body = http.MaxBytesReader(w, r.Body, int64(ChunkSize)+1)
	buf, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, errChunkTooLarge
		}
		return nil, err
	}
	if len(buf) > ChunkSize {
		return nil, errChunkTooLarge
	}
	return buf, nil
}

func validateResumeProof(transferred int64, stored, provided []UploadChunkProof) error {
	if len(stored) != len(provided) {
		return apiErr(CodeRemotePartChanged, "本地文件身份与上传记录不一致", false)
	}
	var covered int64
	for i := range stored {
		want := stored[i]
		got := provided[i]
		if want.Offset != covered || want.Size <= 0 || want.Size > ChunkSize ||
			want.Offset+want.Size > transferred || !validSHA256Hex(want.SHA256) {
			return apiErr(CodeRemotePartChanged, "服务端分块身份记录不完整，请丢弃后重新上传", false)
		}
		if got.Offset != want.Offset || got.Size != want.Size ||
			!strings.EqualFold(got.SHA256, want.SHA256) {
			return apiErr(CodeRemotePartChanged, "所选本地文件不是原上传文件", false)
		}
		covered += want.Size
	}
	if covered != transferred {
		return apiErr(CodeRemotePartChanged, "服务端分块身份记录不完整，请丢弃后重新上传", false)
	}
	return nil
}
