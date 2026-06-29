package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 制品仓库:版本化制品库。上传一次 → 落盘 + sha256 + 元数据留存 → 可部署到任意应用/Agent,
// 或一键重部署任意历史制品。复用文件柜的二进制落盘地基,但无 TTL(手动管理,版本化留存)。
//
// 与文件柜的区别:文件柜是临时中转(有过期、提取码、可匿名);制品仓库是部署制品的版本化留存
// (无过期、需登录、面向重部署)。部署时可在 deploy stream 里用 artifactId 引用已留存制品,
// 免重复上传——多 Agent 部署同一制品 / 回滚到已验证构建时尤其有用。

// artifactPath 是某制品条目的落盘路径(以 id 命名,避免文件名穿越/冲突)。
func (a *api) artifactPath(id string) string {
	return filepath.Join(a.artifactDir, filepath.Base(id))
}

// openArtifactFile 打开已留存制品供部署消费(只读、可 seek);不存在返回 false。
func (a *api) openArtifactFile(id string) (*os.File, bool) {
	if _, ok := a.store.getArtifact(id); !ok {
		return nil, false
	}
	f, err := os.Open(a.artifactPath(id))
	if err != nil {
		return nil, false
	}
	return f, true
}

// uploadArtifact 处理 POST /api/artifacts(write):上传制品到版本化制品库。
// 表单:file(制品)+ version(版本标签,可选)。服务端权威计算 sha256,落盘 + 写元数据。
// 同 sha256 已存在则提示已留存(幂等:不重复落盘,返回既有条目)。
func (a *api) uploadArtifact(w http.ResponseWriter, r *http.Request) {
	uploader, _, _ := a.currentUser(r)
	// 早拦:ContentLength 超限直接 413(与文件柜一致,避免中途截断后客户端只看到网络错误)。
	limitMB := a.maxUpload >> 20
	if r.ContentLength > a.maxUpload {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("制品超过 %d MB 上限", limitMB)})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, a.maxUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		if isMaxBytes(err) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("制品超过 %d MB 上限", limitMB)})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "表单解析失败"})
		return
	}
	defer cleanupMultipart(r)
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 file 字段"})
		return
	}
	defer file.Close()
	version := strings.TrimSpace(r.FormValue("version"))

	if err := os.MkdirAll(a.artifactDir, 0755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "创建制品目录失败"})
		return
	}

	// 流式落临时文件 + 计算 sha256(不整文件入内存)。
	id := fmt.Sprintf("art%d", time.Now().UnixNano())
	tmpPath := a.artifactPath(id) + ".part"
	dst, err := os.Create(tmpPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "落盘失败"})
		return
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), file)
	dst.Close()
	if err != nil {
		os.Remove(tmpPath)
		if isMaxBytes(err) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("制品超过 %d MB 上限", limitMB)})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "写入失败"})
		return
	}
	sha := hex.EncodeToString(h.Sum(nil))

	// 幂等:同 sha256 已留存则不重复落盘,返回既有条目(避免多机重传同一制品占盘)。
	if existing, ok := a.store.artifactBySha(sha); ok {
		os.Remove(tmpPath)
		writeJSON(w, http.StatusOK, map[string]any{"artifact": existing, "deduped": true})
		return
	}

	if err := os.Rename(tmpPath, a.artifactPath(id)); err != nil {
		os.Remove(tmpPath)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "制品落盘失败"})
		return
	}
	row := ArtifactRow{
		ID: id, Name: hdr.Filename, Version: version, Sha256: sha,
		Size: n, Uploader: uploader, CreatedAt: time.Now().UnixMilli(),
	}
	if err := a.store.addArtifact(row); err != nil {
		os.Remove(a.artifactPath(id))
		// sha256 UNIQUE 兜底:并发同 sha 上传都越过前面的 dedup 检查时,后者 INSERT 冲突。
		// 此时既有条目已落库,优雅返回它(deduped),而非报 500。
		if existing, ok := a.store.artifactBySha(sha); ok {
			writeJSON(w, http.StatusOK, map[string]any{"artifact": existing, "deduped": true})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "写元数据失败"})
		return
	}
	a.store.appendAudit(uploader, "上传制品", fmt.Sprintf("制品库 · %s(%s)", hdr.Filename, version), "成功")
	writeJSON(w, http.StatusOK, map[string]any{"artifact": row})
}

// listArtifactsHandler 处理 GET /api/artifacts:列出制品仓库(新→旧)。
func (a *api) listArtifactsHandler(w http.ResponseWriter, r *http.Request) {
	rows, err := a.store.listArtifacts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取制品失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": rows})
}

// deleteArtifactHandler 处理 DELETE /api/artifacts/{id}(write):删制品 + 落盘字节。
func (a *api) deleteArtifactHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	row, ok := a.store.getArtifact(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "制品不存在"})
		return
	}
	if err := a.store.deleteArtifact(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "删除失败"})
		return
	}
	// 元数据先删(避免悬空条目:有记录无文件、下载 404);落盘字节删失败只会留孤儿文件(占盘),记日志便于运维清理。
	if err := os.Remove(a.artifactPath(id)); err != nil && !os.IsNotExist(err) {
		log.Printf("[artifact] 删除落盘文件失败(元数据已删,留孤儿文件): id=%s path=%s err=%v", id, a.artifactPath(id), err)
	}
	a.store.appendAudit(a.sessionUser(r), "删除制品", fmt.Sprintf("制品库 · %s(%s)", row.Name, row.Version), "成功")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// downloadArtifact 处理 GET /api/artifacts/{id}/download(登录,任意角色):下载制品原文件。
func (a *api) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	row, ok := a.store.getArtifact(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "制品不存在"})
		return
	}
	f, err := os.Open(a.artifactPath(id))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "制品文件不存在或已清理"})
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", urlEscape(row.Name)))
	io.Copy(w, f)
}

// pinArtifactHandler 处理 POST /api/artifacts/{id}/pin(write):⭐ 标记/取消重要。
// 被标记的制品豁免每应用滚动淘汰、永久保留;body: {pinned: bool}。
func (a *api) pinArtifactHandler(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	id := r.PathValue("id")
	row, ok := a.store.getArtifact(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "制品不存在"})
		return
	}
	var body struct {
		Pinned bool `json:"pinned"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	if err := a.store.setArtifactPinned(id, body.Pinned); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "更新失败"})
		return
	}
	act := "取消标记重要"
	if body.Pinned {
		act = "标记重要"
	}
	a.store.appendAudit(a.sessionUser(r), "制品"+act, fmt.Sprintf("制品库 · %s(%s)", row.Name, row.Version), "成功")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pinned": body.Pinned})
}

// archiveDeployedArtifact 在真机部署成功后,把本次部署的制品自动沉淀进制品库(source=auto,记来源应用)。
// src 须可 seek(部署链路的制品文件);sha 由调用方权威算好(与发给 Agent 的一致)。同 sha 已在库则跳过(去重)。
// 入库后按每应用保留上限滚动淘汰旧的自动条目(⭐/手动豁免)。artifactKeep<=0 表示关闭自动归档,直接返回。
// 失败仅记日志、不影响部署结果(归档是部署成功后的旁路增强,不能反过来拖垮部署)。
func (a *api) archiveDeployedArtifact(appID, name, version, sha, uploader string, src io.ReadSeeker) {
	if a.artifactKeep <= 0 || strings.TrimSpace(sha) == "" {
		return
	}
	if _, ok := a.store.artifactBySha(sha); ok {
		return // 同内容已在库(多机/重复部署同一制品),不重复落盘
	}
	if err := os.MkdirAll(a.artifactDir, 0755); err != nil {
		log.Printf("[artifact] 自动归档建目录失败: %v", err)
		return
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		log.Printf("[artifact] 自动归档 seek 失败: %v", err)
		return
	}
	id := fmt.Sprintf("art%d", time.Now().UnixNano())
	tmp := a.artifactPath(id) + ".part"
	dst, err := os.Create(tmp)
	if err != nil {
		log.Printf("[artifact] 自动归档落盘失败: %v", err)
		return
	}
	n, cerr := io.Copy(dst, src)
	dst.Close()
	if cerr != nil {
		os.Remove(tmp)
		log.Printf("[artifact] 自动归档拷贝失败: %v", cerr)
		return
	}
	if err := os.Rename(tmp, a.artifactPath(id)); err != nil {
		os.Remove(tmp)
		log.Printf("[artifact] 自动归档 rename 失败: %v", err)
		return
	}
	row := ArtifactRow{
		ID: id, Name: name, Version: version, Sha256: sha, Size: n,
		Uploader: uploader, CreatedAt: time.Now().UnixMilli(), AppID: appID, Source: "auto",
	}
	if err := a.store.addArtifact(row); err != nil {
		os.Remove(a.artifactPath(id)) // 入库失败(如并发同 sha 越过去重):删落盘字节,不留孤儿
		return
	}
	a.evictAutoArtifacts(appID)
}

// evictAutoArtifacts 滚动淘汰某应用超出保留上限的自动归档制品(⭐/手动豁免)。删元数据 + 落盘字节。
func (a *api) evictAutoArtifacts(appID string) {
	ev, err := a.store.evictableAutoArtifacts(appID, a.artifactKeep)
	if err != nil {
		log.Printf("[artifact] 查淘汰候选失败: app=%s err=%v", appID, err)
		return
	}
	for _, r := range ev {
		if err := a.store.deleteArtifact(r.ID); err != nil {
			continue // 元数据删失败:跳过,下次再淘汰(不删落盘以免悬空)
		}
		if err := os.Remove(a.artifactPath(r.ID)); err != nil && !os.IsNotExist(err) {
			log.Printf("[artifact] 自动淘汰删落盘失败(元数据已删,留孤儿): id=%s err=%v", r.ID, err)
		}
	}
}
