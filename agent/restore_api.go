package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
	dir := filepath.Join(a.cfg.Paths.BackupDir, id, backup)
	// 单文件备份为 app,多文件备份为 app.tar.gz;还原时按内容(魔数)自动判断解包与否。
	for _, name := range []string{"app", "app.tar.gz"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("备份制品不存在: %s", dir)
}

// prepareRestore 解析还原请求并做安全校验;失败已写好响应,ok=false。返回 cfg 与备份标识(进程类=备份目录名、static=release 时间戳)。
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
	if msg, ok := validIDAndRelease(cfg.ID, cfg.ReleaseID); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return zero, "", false
	}
	if !withinRoots(cfg.BinPath, a.cfg.Paths.DeployRoots) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "制品路径不在白名单内: " + cfg.BinPath})
		return zero, "", false
	}
	if b := req.Backup; b == "" || b != filepath.Base(b) || strings.Contains(b, "..") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "非法备份名"})
		return zero, "", false
	}
	return cfg, req.Backup, true
}

// runRestore 按类型还原:static-nginx 切换软链到历史 release;进程类用备份制品重跑流水线。
func (a *agent) runRestore(cfg DeployConfig, backup string, emit func(Step)) DeployResult {
	if emit == nil {
		emit = func(Step) {}
	}
	if cfg.Type == "static-nginx" {
		// static 还原也走 releaseId 幂等(防重复切软链/reload);runIdempotent 内部已加同应用串行锁。
		return a.runIdempotent("restore", cfg, emit, func(e func(Step)) DeployResult { return a.restoreStatic(cfg, backup, e) })
	}
	artifact, err := a.backupArtifact(cfg.ID, backup)
	if err != nil {
		s := Step{Name: "校验备份", OK: false, Logs: []string{err.Error()}}
		emit(s)
		return DeployResult{Result: "failed", Version: cfg.Version, Steps: []Step{s}}
	}
	// 拷到临时文件:流水线 backupCurrent 滚动清理可能删掉还原源,拷出来即免疫。
	tmp, cleanup, err := copyToTemp(artifact)
	if err != nil {
		s := Step{Name: "准备还原源", OK: false, Logs: []string{err.Error()}}
		emit(s)
		return DeployResult{Result: "failed", Version: cfg.Version, Steps: []Step{s}}
	}
	defer cleanup()
	return a.runDeployIdempotent("restore", cfg, tmp, emit)
}

// restoreStatic 把静态站点软链切回指定历史 release(<BinPath>-releases/<ts>/),失败回滚软链。
func (a *agent) restoreStatic(cfg DeployConfig, releaseTS string, emit func(Step)) DeployResult {
	res := DeployResult{Version: cfg.Version}
	add := func(name string, ok bool, logs ...string) {
		s := Step{Name: name, OK: ok, Logs: logs}
		res.Steps = append(res.Steps, s)
		emit(s)
	}
	releaseDir := filepath.Join(cfg.BinPath+"-releases", releaseTS)
	if !fileExists(releaseDir) {
		add("校验 release", false, "历史 release 不存在: "+releaseDir)
		res.Result = "failed"
		return res
	}
	add("校验 release", true, releaseDir)

	prevTarget, _ := os.Readlink(cfg.BinPath)
	add("记录当前指向", true, "当前 → "+prevTarget)

	if err := switchSymlink(releaseDir, cfg.BinPath); err != nil {
		add("切换软链", false, err.Error())
		res.Result = "failed"
		return res
	}
	add("切换软链", true, cfg.BinPath+" → "+releaseDir)
	// reload 失败即视为还原失败,触发回滚——不再丢弃错误、不再无条件标成功。
	reloadOK := true
	if ran, log, err := runReload(cfg.ReloadCmd); ran {
		reloadOK = err == nil
		add("reload", reloadOK, log)
	}

	var hlog []string
	if reloadOK && healthCheck(cfg.Health, &hlog) {
		add("健康检查", true, hlog...)
		res.Result = "success"
		return res
	}
	if !reloadOK {
		hlog = append(hlog, "reload 失败,跳过健康检查直接回滚")
	}
	add("健康检查", false, hlog...)
	// 回滚:切回原 release
	if prevTarget == "" {
		add("回滚", false, "无原 release 指向可回滚")
		res.Result = "failed"
		return res
	}
	switchSymlink(prevTarget, cfg.BinPath)
	_, rloadLog, rerr := runReload(cfg.ReloadCmd)
	var rh []string
	ok := rerr == nil && healthCheck(cfg.Health, &rh)
	rlogs := []string{"切回 " + prevTarget}
	if rloadLog != "" {
		rlogs = append(rlogs, rloadLog)
	}
	add("回滚 · 软链", ok, append(rlogs, rh...)...)
	if ok {
		res.Result = "rolledback"
	} else {
		res.Result = "failed"
	}
	return res
}

// restore 处理 POST /api/apps/{id}/restore(同步)。
func (a *agent) restore(w http.ResponseWriter, r *http.Request) {
	cfg, backup, ok := a.prepareRestore(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, a.runRestore(cfg, backup, nil))
}

// restoreStream 处理 POST /api/apps/{id}/restore/stream(SSE)。
func (a *agent) restoreStream(w http.ResponseWriter, r *http.Request) {
	cfg, backup, ok := a.prepareRestore(w, r)
	if !ok {
		return
	}
	runSSE(w, func(emit func(Step)) DeployResult { return a.runRestore(cfg, backup, emit) })
}

// listReleases 处理 GET /api/apps/{id}/releases?binPath= :列出静态站点历史 release(<binPath>-releases/,新→旧)。
// binPath 经白名单校验;复用 BackupInfo 形态,Dir 为 release 时间戳(还原时回传)。
func (a *agent) listReleases(w http.ResponseWriter, r *http.Request) {
	binPath := r.URL.Query().Get("binPath")
	if !withinRoots(binPath, a.cfg.Paths.DeployRoots) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "路径不在白名单内"})
		return
	}
	entries, err := os.ReadDir(binPath + "-releases")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"backups": []BackupInfo{}})
		return
	}
	cur, _ := os.Readlink(binPath) // 当前指向的 release(标注用)
	list := []BackupInfo{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		bi := BackupInfo{Dir: e.Name()}
		if t, perr := time.Parse("20060102_150405.000000000", e.Name()); perr == nil {
			bi.Time = t.UnixMilli()
		}
		if filepath.Base(cur) == e.Name() {
			bi.Version = "当前"
		}
		list = append(list, bi)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Dir > list[j].Dir })
	writeJSON(w, http.StatusOK, map[string]any{"backups": list})
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
		for _, name := range []string{"app", "app.tar.gz"} { // 单文件 / 多文件备份
			if fi, err := os.Stat(filepath.Join(dir, e.Name(), name)); err == nil {
				bi.Size = fi.Size()
				break
			}
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
