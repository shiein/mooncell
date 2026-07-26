// SFTP 列目录与流式下载。
package serverops

import (
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
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

	if s.touch != nil {
		s.touch(user)
	}
	writeOK(w, FileListResponse{Path: absPath, Entries: out})
}

// DownloadFile GET .../sessions/{sid}/download?path=
// 流式转发，不落 Console 盘；支持单区间 Range。
func (s *Service) DownloadFile(w http.ResponseWriter, r *http.Request) {
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

	// 多 Range 第一版不支持
	rangeHdr := r.Header.Get("Range")
	if strings.Contains(rangeHdr, ",") {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, "multipart ranges not supported", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	start := int64(0)
	end := size - 1
	status := http.StatusOK
	if strings.HasPrefix(rangeHdr, "bytes=") {
		// If-Range：不匹配则完整下载
		if ir := r.Header.Get("If-Range"); ir != "" && ir != etag {
			// 忽略 Range，完整响应
		} else {
			spec := strings.TrimPrefix(rangeHdr, "bytes=")
			parts := strings.SplitN(spec, "-", 2)
			if len(parts) == 2 {
				if parts[0] != "" {
					if v, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
						start = v
					}
				}
				if parts[1] != "" {
					if v, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
						end = v
					}
				}
				if start < 0 || start >= size || end < start {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
					http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
					return
				}
				if end >= size {
					end = size - 1
				}
				status = http.StatusPartialContent
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
			}
		}
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

	buf := make([]byte, 256*1024)
	lr := io.LimitReader(f, length)
	_, copyErr := io.CopyBuffer(w, lr, buf)

	resName := resourceID
	if res, found, _ := getResource(s.db, resourceID); found {
		resName = res.Name
	}
	if copyErr != nil && r.Context().Err() == nil {
		s.auditLog(user, "SFTP 下载", auditFileTarget(resName, "download", cleaned, size), "失败")
		return
	}
	// 完整或客户端取消：仅完整成功记成功
	if copyErr == nil {
		s.auditLog(user, "SFTP 下载", auditFileTarget(resName, "download", cleaned, size), "成功")
	}
	if s.touch != nil {
		s.touch(user)
	}
}

func contentDispositionAttachment(filename string) string {
	// 简单 ASCII 回退 + filename* UTF-8
	safe := strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || r == '"' {
			return '_'
		}
		return r
	}, filename)
	return fmt.Sprintf(`attachment; filename="%s"`, safe)
}
