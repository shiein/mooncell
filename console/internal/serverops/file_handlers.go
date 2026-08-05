// SFTP 列目录与流式下载。
package serverops

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ListFiles GET .../sessions/{sid}/files?path=
func (s *Service) ListFiles(w http.ResponseWriter, r *http.Request) {
	user, _, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "未登录", false)
		return
	}
	resourceID := r.PathValue("id")
	sessionID := r.PathValue("sid")
	sess := s.sessionFromRequest(r, sessionID, user, resourceID)
	if sess == nil {
		writeErr(w, http.StatusNotFound, CodeSessionClosed, "会话不存在或已结束", false)
		return
	}
	sc, err := sess.SFTP()
	if err != nil {
		writeAPIError(w, err)
		return
	}

	reqPath := r.URL.Query().Get("path")
	if reqPath == "" {
		reqPath = "."
	}
	cleaned, err := cleanRemotePath(reqPath)
	if err != nil {
		writeAPIError(w, err)
		return
	}

	// 首次 "." 解析为远端 home
	absPath := cleaned
	if cleaned == "." {
		wd, err := sc.Getwd()
		if err != nil {
			writeErr(w, http.StatusBadGateway, CodeSSHConnectionFailed, "获取远端工作目录失败", true)
			return
		}
		absPath = wd
	}

	entries, err := sc.ReadDir(absPath)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeValidation, "无法读取目录", true)
		return
	}
	if len(entries) > maxDirEntries {
		writeErr(w, http.StatusBadRequest, CodeValidation,
			fmt.Sprintf("目录条目超过上限 %d，请缩小范围", maxDirEntries), false)
		return
	}

	out := make([]FileEntry, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		typ := "file"
		if e.IsDir() {
			typ = "directory"
		} else if e.Mode()&0o170000 == 0o120000 { // symlink
			typ = "symlink"
		} else if !e.Mode().IsRegular() {
			typ = "other"
		}
		mod := int64(0)
		if !e.ModTime().IsZero() {
			mod = e.ModTime().UnixMilli()
		}
		out = append(out, FileEntry{
			Name:       name,
			Path:       path.Join(absPath, name),
			Type:       typ,
			Size:       e.Size(),
			Mode:       padOctal(uint32(e.Mode().Perm()), 4),
			ModifiedAt: mod,
		})
	}
	// 目录优先，再按名称稳定排序
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Type == "directory" && out[j].Type != "directory" {
			return true
		}
		if out[i].Type != "directory" && out[j].Type == "directory" {
			return false
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})

	sess.Touch()
	s.touchLoginSessionFromRequest(r)
	writeOK(w, FileListResponse{Path: absPath, Entries: out})
}

// DownloadFile GET .../sessions/{sid}/download?path=
// 流式转发，不落 Console 盘；支持单区间 Range（含 suffix bytes=-N）。
func (s *Service) DownloadFile(w http.ResponseWriter, r *http.Request) {
	user, _, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "未登录", false)
		return
	}
	resourceID := r.PathValue("id")
	sessionID := r.PathValue("sid")
	sess := s.sessionFromRequest(r, sessionID, user, resourceID)
	if sess == nil {
		writeErr(w, http.StatusNotFound, CodeSessionClosed, "会话不存在或已结束", false)
		return
	}
	sess.Touch()
	// 下载 id 在检查时即原子占槽，避免并发请求同时越过全局/用户限制。
	dlID := newID("dl")
	if err := s.tryBeginDownload(dlID, user); err != nil {
		writeAPIError(w, err)
		return
	}
	defer s.releaseTransfer(dlID)

	sc, err := sess.SFTP()
	if err != nil {
		writeAPIError(w, err)
		return
	}

	reqPath := r.URL.Query().Get("path")
	cleaned, err := cleanRemotePath(reqPath)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if cleaned == "." || cleaned == "/" {
		writeErr(w, http.StatusBadRequest, CodeValidation, "请指定文件路径", false)
		return
	}

	f, err := sc.Open(cleaned)
	if err != nil {
		writeErr(w, http.StatusNotFound, CodeNotFound, "文件不存在或无法打开", false)
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusBadGateway, CodeSSHConnectionFailed, "读取文件信息失败", true)
		return
	}
	if st.IsDir() {
		writeErr(w, http.StatusBadRequest, CodeValidation, "不能下载目录", false)
		return
	}
	size := st.Size()
	if size > s.cfg.maxDownloadBytes() {
		writeErr(w, http.StatusRequestEntityTooLarge, CodeTransferTooLarge, "文件超过下载上限", false)
		return
	}

	base := path.Base(cleaned)
	etag := fmt.Sprintf(`W/"%d-%d"`, size, st.ModTime().Unix())
	w.Header().Set("ETag", etag)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", contentDispositionAttachment(base))

	start, end, status, okRange := parseSingleRange(r.Header.Get("Range"), r.Header.Get("If-Range"), etag, size)
	if !okRange {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if status == http.StatusPartialContent {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	}

	length := end - start + 1
	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			writeErr(w, http.StatusBadGateway, CodeSSHConnectionFailed, "定位文件失败", true)
			return
		}
	}
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(status)

	// 长下载期间周期性 Touch，避免 idle 回收在 Copy 中途掐断会话。
	buf := make([]byte, 256*1024)
	var copied int64
	var copyErr error
	lastTouch := time.Now()
	for copied < length {
		if err := r.Context().Err(); err != nil {
			copyErr = err
			break
		}
		toRead := int64(len(buf))
		if remain := length - copied; remain < toRead {
			toRead = remain
		}
		n, err := f.Read(buf[:toRead])
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				copyErr = werr
				break
			}
			copied += int64(n)
			if time.Since(lastTouch) >= 10*time.Second {
				sess.Touch()
				s.touchTransferSlot(dlID)
				s.touchLoginSessionFromRequest(r)
				lastTouch = time.Now()
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			copyErr = err
			break
		}
	}

	resName := resourceID
	if res, found, _ := getResource(s.db, resourceID); found {
		resName = res.Name
	}
	if copyErr != nil && r.Context().Err() == nil {
		s.auditLog(user, "SFTP 下载", auditFileTarget(resName, "download", cleaned, size), "失败")
		return
	}
	if copyErr == nil {
		s.auditLog(user, "SFTP 下载", auditFileTarget(resName, "download", cleaned, size), "成功")
	}
	sess.Touch()
	s.touchLoginSessionFromRequest(r)
}

// parseSingleRange 解析单区间 Range。返回 ok=false 表示应 416。
// 支持 bytes=start-end、bytes=start-、bytes=-suffix。非法数字 → 416，不返回错误 206。
func parseSingleRange(rangeHdr, ifRange, etag string, size int64) (start, end int64, status int, ok bool) {
	start, end, status = int64(0), size-1, http.StatusOK
	if size <= 0 {
		return 0, -1, http.StatusOK, true
	}
	if rangeHdr == "" {
		return start, end, status, true
	}
	if strings.Contains(rangeHdr, ",") {
		return 0, 0, 0, false
	}
	if !strings.HasPrefix(rangeHdr, "bytes=") {
		return 0, 0, 0, false
	}
	// If-Range 不匹配：忽略 Range，完整响应
	if ifRange != "" && ifRange != etag {
		return 0, size - 1, http.StatusOK, true
	}
	spec := strings.TrimPrefix(rangeHdr, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, 0, false
	}
	left, right := parts[0], parts[1]

	if left == "" && right == "" {
		return 0, 0, 0, false
	}
	// suffix: bytes=-N → 最后 N 字节
	if left == "" {
		n, err := strconv.ParseInt(right, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, http.StatusPartialContent, true
	}
	s, err := strconv.ParseInt(left, 10, 64)
	if err != nil || s < 0 {
		return 0, 0, 0, false
	}
	e := size - 1
	if right != "" {
		e, err = strconv.ParseInt(right, 10, 64)
		if err != nil || e < 0 {
			return 0, 0, 0, false
		}
	}
	if s >= size || e < s {
		return 0, 0, 0, false
	}
	if e >= size {
		e = size - 1
	}
	return s, e, http.StatusPartialContent, true
}

func contentDispositionAttachment(filename string) string {
	// ASCII 回退文件名 + RFC 5987 filename* UTF-8
	safe := strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || r == '"' || r == '\\' {
			return '_'
		}
		if r > 0x7e {
			return '_'
		}
		return r
	}, filename)
	if safe == "" {
		safe = "download"
	}
	encoded := url.PathEscape(filename)
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, safe, encoded)
}
