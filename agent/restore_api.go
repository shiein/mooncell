package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RestoreRequest 是还原请求体。Agent 无状态:Console 下发全量应用配置 + 要还原到的备份目录名。
// 还原即「用某个历史备份制品重跑部署流水线」——还原前自动备份当前版本,失败自动回滚,无需另写一套逻辑。
type RestoreRequest struct {
	Config DeployConfig `json:"config"`
	Backup string       `json:"backup"` // 备份时间戳目录名(BackupDir/<id>/<backup>/)
}

// BackupInfo 是一条备份的元信息,供 Console 列出可还原版本与展示。
type BackupInfo struct {
	Dir     string `json:"dir"`     // 时间戳目录名(即还原时回传的 backup 标识)
	Version string `json:"version"` // 备份时记录的版本
	Sha256  string `json:"sha256"`
	Time    int64  `json:"time"` // UnixMilli
	Size    int64  `json:"size"` // 备份制品字节数
}

// backupArtifact 定位并校验备份制品路径:backup 必须是单层目录名(防路径穿越),
// 且 BackupDir/<id>/<backup>/app 须真实存在。返回制品文件绝对路径。
func (a *agent) backupArtifact(id, backup string) (string, error) {
	if backup == "" || backup != filepath.Base(backup) || strings.Contains(backup, "..") {
		return "", fmt.Errorf("非法备份名: %q", backup)
	}
	artifact := filepath.Join(a.cfg.Paths.BackupDir, id, backup, "app")
	if _, err := os.Stat(artifact); err != nil {
		return "", fmt.Errorf("备份制品不存在: %s", artifact)
	}
	return artifact, nil
}

// prepareRestore 解析还原请求、定位备份制品并做安全校验;失败已写好响应,ok=false。
func (a *agent) prepareRestore(w http.ResponseWriter, r *http.Request) (DeployConfig, string, bool) {
	var zero DeployConfig
	id := r.PathValue("id")
	var req RestoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体解析失败"})
		return zero, "", false
	}
	cfg := req.Config
	cfg.ID = id // 以路径为准

	// static-nginx 的历史版本是 <BinPath>-releases/<ts>/ 软链,不走 BackupDir,还原机制不同。
	if cfg.Type == "static-nginx" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "static-nginx 暂不支持备份还原(历史版本以 release 软链保留)"})
		return zero, "", false
	}
	// 安全边界:落盘路径必须在白名单根目录内(与部署同一道防穿越)。
	if !withinRoots(cfg.BinPath, a.cfg.Paths.DeployRoots) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "制品路径不在白名单内: " + cfg.BinPath})
		return zero, "", false
	}
	artifact, err := a.backupArtifact(id, req.Backup)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return zero, "", false
	}
	return cfg, artifact, true
}

// restore 处理 POST /api/apps/{id}/restore(同步):用指定备份制品重跑部署流水线,一次性返回结果。
func (a *agent) restore(w http.ResponseWriter, r *http.Request) {
	cfg, artifact, ok := a.prepareRestore(w, r)
	if !ok {
		return
	}
	// 先把备份制品拷到临时文件:流水线里 backupCurrent 会滚动清理备份,
	// 还原最老备份且达保留上限时,源可能在替换前被清掉。拷出来即免疫。
	tmp, cleanup, err := copyToTemp(artifact)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "准备还原源失败"})
		return
	}
	defer cleanup()
	res := a.runDeploy(cfg, tmp, nil)
	writeJSON(w, http.StatusOK, res)
}

// restoreStream 处理 POST /api/apps/{id}/restore/stream(SSE):还原流水线逐步推送,前端实时呈现。
func (a *agent) restoreStream(w http.ResponseWriter, r *http.Request) {
	cfg, artifact, ok := a.prepareRestore(w, r)
	if !ok {
		return
	}
	tmp, cleanup, err := copyToTemp(artifact) // 同步端注释:保护被还原的备份源不被滚动清理删掉
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "准备还原源失败"})
		return
	}
	defer cleanup()
	runSSE(w, func(emit func(Step)) DeployResult { return a.runDeploy(cfg, tmp, emit) })
}

// listBackups 处理 GET /api/apps/{id}/backups:列出该应用 BackupDir 下的真实备份(新→旧)。
// 备份目录不存在(尚未部署过)返回空列表,而非错误。
func (a *agent) listBackups(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	dir := filepath.Join(a.cfg.Paths.BackupDir, id)
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"backups": []BackupInfo{}})
		return
	}
	list := []BackupInfo{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		bi := BackupInfo{Dir: e.Name()}
		if fi, err := os.Stat(filepath.Join(dir, e.Name(), "app")); err == nil {
			bi.Size = fi.Size()
		}
		if mb, err := os.ReadFile(filepath.Join(dir, e.Name(), "meta.json")); err == nil {
			var m struct {
				Version string `json:"version"`
				Sha256  string `json:"sha256"`
				Time    int64  `json:"time"`
			}
			json.Unmarshal(mb, &m)
			bi.Version, bi.Sha256, bi.Time = m.Version, m.Sha256, m.Time
		}
		list = append(list, bi)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Dir > list[j].Dir }) // 时间戳命名,字典序倒排即新→旧
	writeJSON(w, http.StatusOK, map[string]any{"backups": list})
}
