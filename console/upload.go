package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// 分块上传 + 断点续传:浏览器把大制品按块顺序 PUT 到 Console,Console 顺序追加到一个临时文件,
// 客户端断线/失败后可凭 uploadId 查进度、从 nextIndex 续传。完成后用 uploadId 触发部署
// (agentDeployStream 据 uploadId 直接读临时文件,免再整体上传)。
// 仅 Console↔浏览器分块;Console→Agent 仍单次 multipart(LAN,可靠)。

const uploadTTL = 2 * time.Hour // 未完成的上传会话保活时长,过期清理

type uploadSession struct {
	ID        string
	Path      string // 临时文件路径(块顺序追加)
	Filename  string
	Size      int64 // 客户端声明的总字节
	Received  int64 // 已落盘字节
	NextIndex int   // 下一个期望的块序号(0 起)
	Created   time.Time
	mu        sync.Mutex
}

func newUploadID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *api) uploadDirPath() string {
	d := filepath.Join(os.TempDir(), "mc-uploads")
	os.MkdirAll(d, 0700)
	return d
}

// uploadStart 处理 POST /api/upload/start {filename,size}:建会话与临时文件,返回 uploadId。
func (a *api) uploadStart(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req struct {
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	if req.Size <= 0 || req.Size > a.maxUpload {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "声明大小超出上限或非法"})
		return
	}
	id := newUploadID()
	path := filepath.Join(a.uploadDirPath(), id)
	f, err := os.Create(path)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "创建上传临时文件失败"})
		return
	}
	f.Close()
	sess := &uploadSession{ID: id, Path: path, Filename: req.Filename, Size: req.Size, Created: time.Now()}
	a.uploadsMu.Lock()
	a.uploads[id] = sess
	a.uploadsMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"uploadId": id, "received": 0, "nextIndex": 0})
}

func (a *api) getUpload(id string) (*uploadSession, bool) {
	a.uploadsMu.Lock()
	defer a.uploadsMu.Unlock()
	s, ok := a.uploads[id]
	return s, ok
}

// uploadChunk 处理 PUT /api/upload/{uploadId}?index=N:按序追加一块。
// index==NextIndex 追加;index<NextIndex 视为重复(幂等返回当前进度);index>NextIndex 报 409 + nextIndex 供续传。
func (a *api) uploadChunk(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	sess, ok := a.getUpload(r.PathValue("uploadId"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "上传会话不存在或已过期"})
		return
	}
	idx, err := strconv.Atoi(r.URL.Query().Get("index"))
	if err != nil || idx < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "index 非法"})
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if idx < sess.NextIndex {
		writeJSON(w, http.StatusOK, map[string]any{"received": sess.Received, "nextIndex": sess.NextIndex, "duplicate": true})
		return
	}
	if idx > sess.NextIndex {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "块乱序", "nextIndex": sess.NextIndex, "received": sess.Received})
		return
	}
	// 传输层硬上限:本块写入后不得超过声明大小与全局上限。
	remaining := sess.Size - sess.Received
	if remaining < 0 {
		remaining = 0
	}
	body := http.MaxBytesReader(w, r.Body, remaining+1) // +1 探测超限
	f, err := os.OpenFile(sess.Path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "打开上传文件失败"})
		return
	}
	// 记录已知 good 偏移:O_APPEND 下 io.Copy 在 MaxBytes 报错前可能已写入部分字节,
	// 任一失败路径须 Truncate 回 off,否则客户端重试同块会尾部多脏字节、后续写入错位、制品损坏。
	off := sess.Received
	n, cerr := io.Copy(f, body)
	if cerr != nil {
		f.Truncate(off) // 回到块写入前的长度,保证幂等重试干净(O_APPEND 下 Truncate 仅改长度)
		f.Close()
		if isMaxBytes(cerr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "上传超过声明大小/上限"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "写入分块失败"})
		return
	}
	f.Close()
	sess.Received += n
	sess.NextIndex++
	writeJSON(w, http.StatusOK, map[string]any{"received": sess.Received, "nextIndex": sess.NextIndex})
}

// uploadStatus 处理 GET /api/upload/{uploadId}:返回已收字节与下一块序号,供断点续传。
func (a *api) uploadStatus(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.getUpload(r.PathValue("uploadId"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "上传会话不存在或已过期"})
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"received": sess.Received, "nextIndex": sess.NextIndex, "size": sess.Size, "complete": sess.Received >= sess.Size})
}

// uploadAbort 处理 DELETE /api/upload/{uploadId}:放弃上传,删会话与临时文件。
func (a *api) uploadAbort(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("uploadId")
	a.finishUpload(id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// openUploadArtifact 部署时据 uploadId 打开已收齐的临时文件作为制品;未收齐/不存在返回 false。
func (a *api) openUploadArtifact(uploadID string) (*os.File, bool) {
	sess, ok := a.getUpload(uploadID)
	if !ok {
		return nil, false
	}
	sess.mu.Lock()
	complete := sess.Received >= sess.Size
	path := sess.Path
	sess.mu.Unlock()
	if !complete {
		return nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	return f, true
}

// finishUpload 删会话与临时文件(部署消费后或放弃时调用)。
func (a *api) finishUpload(id string) {
	a.uploadsMu.Lock()
	sess, ok := a.uploads[id]
	if ok {
		delete(a.uploads, id)
	}
	a.uploadsMu.Unlock()
	if ok {
		os.Remove(sess.Path)
	}
}

// cleanupStaleUploads 清理超过 TTL 仍未完成的上传会话(后台定时调用),防临时盘泄漏。
func (a *api) cleanupStaleUploads() int {
	now := time.Now()
	var stale []*uploadSession
	a.uploadsMu.Lock()
	for id, s := range a.uploads {
		if now.Sub(s.Created) > uploadTTL {
			stale = append(stale, s)
			delete(a.uploads, id)
		}
	}
	a.uploadsMu.Unlock()
	for _, s := range stale {
		os.Remove(s.Path)
	}
	return len(stale)
}
