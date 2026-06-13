package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DeployConfig 是 Console 随每次部署下发的应用配置。
// Agent 无状态:Console 持有期望状态,每次部署带全量配置,Agent 只负责执行。
type DeployConfig struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	BinPath    string            `json:"binPath"` // 制品落盘绝对路径,须在 deploy_roots 白名单内
	Workdir    string            `json:"workdir"`
	Args       string            `json:"args"` // 启动参数(空格分隔)
	Env        map[string]string `json:"env"`
	User       string            `json:"user"`
	Health     string            `json:"health"` // HTTP 健康检查 URL,空则跳过
	Version    string            `json:"version"`
	BackupKeep int               `json:"backupKeep"`
}

// Step 是流水线一步的执行记录;Result 为整体结果。
type Step struct {
	Name string   `json:"name"`
	OK   bool     `json:"ok"`
	Logs []string `json:"logs"`
}

type DeployResult struct {
	Result  string `json:"result"` // success | rolledback | failed
	Version string `json:"version"`
	Steps   []Step `json:"steps"`
}

const healthRetries = 5
const healthInterval = 2 * time.Second

// ---------- systemd Runner ----------

func unitName(id string) string { return "deploy-" + id }
func unitPath(id string) string { return "/etc/systemd/system/" + unitName(id) + ".service" }

func sysctl(args ...string) (string, error) {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func writeUnit(cfg DeployConfig) error {
	var env strings.Builder
	for k, v := range cfg.Env {
		fmt.Fprintf(&env, "Environment=%s=%s\n", k, v)
	}
	execStart := cfg.BinPath
	if a := strings.TrimSpace(cfg.Args); a != "" {
		execStart += " " + a
	}
	user := cfg.User
	if user == "" {
		user = "root"
	}
	wd := cfg.Workdir
	if wd == "" {
		wd = filepath.Dir(cfg.BinPath)
	}
	unit := fmt.Sprintf(`[Unit]
Description=Mooncell deploy %s
After=network.target

[Service]
Type=simple
WorkingDirectory=%s
ExecStart=%s
%sRestart=on-failure
RestartSec=2
User=%s

[Install]
WantedBy=multi-user.target
`, cfg.Name, wd, execStart, env.String(), user)
	return os.WriteFile(unitPath(cfg.ID), []byte(unit), 0644)
}

func mainPID(id string) string {
	out, _ := sysctl("show", "-p", "MainPID", "--value", unitName(id))
	return out
}

func isActive(id string) bool {
	out, _ := sysctl("is-active", unitName(id))
	return out == "active"
}

// ---------- 文件与校验 ----------

func sha256File(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// atomicReplace 先落到 tmp、校验后 rename,避免半成品状态。
func atomicReplace(src, dst string) error {
	tmp := dst + ".tmp"
	if err := copyFile(src, tmp, 0755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// withinRoots 是安全边界:目标路径规范化后必须落在某个白名单根目录内(防穿越)。
func withinRoots(p string, roots []string) bool {
	ap, err := filepath.Abs(filepath.Clean(p))
	if err != nil {
		return false
	}
	for _, r := range roots {
		ar, err := filepath.Abs(filepath.Clean(r))
		if err != nil {
			continue
		}
		if ap == ar || strings.HasPrefix(ap, ar+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// ---------- 备份 ----------

// backupCurrent 把当前制品复制到 backups/<id>/<ts>/,附 meta.json;首次部署(无当前制品)返回空。
func (a *agent) backupCurrent(cfg DeployConfig) (string, error) {
	if _, err := os.Stat(cfg.BinPath); err != nil {
		return "", nil // 首次部署
	}
	ts := time.Now().Format("20060102_150405")
	dir := filepath.Join(a.cfg.Paths.BackupDir, cfg.ID, ts)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	if err := copyFile(cfg.BinPath, filepath.Join(dir, "app"), 0755); err != nil {
		return "", err
	}
	meta := fmt.Sprintf(`{"version":%q,"sha256":%q,"time":%d,"operator":"console"}`,
		cfg.Version, sha256File(cfg.BinPath), time.Now().UnixMilli())
	os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0644)
	a.rotateBackups(cfg.ID, cfg.BackupKeep)
	return dir, nil
}

// rotateBackups 按份数滚动保留(时间戳命名,字典序即时间序)。
func (a *agent) rotateBackups(id string, keep int) {
	if keep <= 0 {
		keep = 5
	}
	dir := filepath.Join(a.cfg.Paths.BackupDir, id)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	for len(dirs) > keep {
		os.RemoveAll(filepath.Join(dir, dirs[0]))
		dirs = dirs[1:]
	}
}

// ---------- 健康检查 ----------

func httpHealthy(url string) bool {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func healthCheck(url string, logs *[]string) bool {
	if strings.TrimSpace(url) == "" {
		*logs = append(*logs, "未配置健康检查,跳过")
		return true
	}
	for i := 1; i <= healthRetries; i++ {
		if httpHealthy(url) {
			*logs = append(*logs, fmt.Sprintf("%s → 200 OK(第 %d 次)", url, i))
			return true
		}
		*logs = append(*logs, fmt.Sprintf("%s → 未通过,%s 后重试(%d/%d)", url, healthInterval, i, healthRetries))
		if i < healthRetries {
			time.Sleep(healthInterval)
		}
	}
	return false
}

// ---------- 部署流水线 ----------

// runDeploy 执行已验证的部署闭环:备份 → 停 → 原子替换 → 生成 unit + 启动 → 健康检查;失败自动回滚。
func (a *agent) runDeploy(cfg DeployConfig, artifact string) DeployResult {
	res := DeployResult{Version: cfg.Version}
	add := func(name string, ok bool, logs ...string) {
		res.Steps = append(res.Steps, Step{Name: name, OK: ok, Logs: logs})
	}

	// 1. 校验制品
	add("校验制品", true, "sha256 "+short(sha256File(artifact)), "目标 "+cfg.BinPath)

	// 2. 备份当前版本
	bkDir, err := a.backupCurrent(cfg)
	if err != nil {
		add("备份当前版本", false, err.Error())
		res.Result = "failed"
		return res
	}
	if bkDir == "" {
		add("备份当前版本", true, "首次部署,无当前制品需备份")
	} else {
		add("备份当前版本", true, "备份 → "+bkDir+" · 滚动保留 "+fmt.Sprint(cfg.BackupKeep)+" 份")
	}

	// 3. 停止服务
	sysctl("stop", unitName(cfg.ID))
	add("停止服务", true, "systemctl stop "+unitName(cfg.ID))

	// 4. 原子替换制品
	os.MkdirAll(filepath.Dir(cfg.BinPath), 0755)
	if err := atomicReplace(artifact, cfg.BinPath); err != nil {
		add("替换制品", false, err.Error())
		res.Result = "failed"
		return res
	}
	add("替换制品", true, "tmp 落盘 → rename 原子替换 "+cfg.BinPath)

	// 5. 生成 unit + 启动
	if err := writeUnit(cfg); err != nil {
		add("启动服务", false, "写 unit 失败: "+err.Error())
		res.Result = "failed"
		return res
	}
	sysctl("daemon-reload")
	sysctl("enable", unitName(cfg.ID))
	if out, err := sysctl("start", unitName(cfg.ID)); err != nil {
		add("启动服务", false, "systemctl start 失败: "+out)
	} else {
		add("启动服务", true, "systemd 托管 · "+unitName(cfg.ID)+" · pid "+mainPID(cfg.ID))
	}
	time.Sleep(time.Second)

	// 6. 健康检查
	var hlog []string
	if healthCheck(cfg.Health, &hlog) {
		add("健康检查", true, hlog...)
		res.Result = "success"
		return res
	}
	add("健康检查", false, hlog...)

	// 7. 失败 → 自动回滚
	if bkDir == "" {
		sysctl("stop", unitName(cfg.ID))
		add("回滚", false, "首次部署无备份可回滚,已停止服务")
		res.Result = "failed"
		return res
	}
	rlog := []string{"读取 " + bkDir, "还原备份制品(原子替换)"}
	sysctl("stop", unitName(cfg.ID))
	if err := atomicReplace(filepath.Join(bkDir, "app"), cfg.BinPath); err != nil {
		rlog = append(rlog, "还原失败: "+err.Error())
		add("回滚 · 还原备份", false, rlog...)
		res.Result = "failed"
		return res
	}
	sysctl("start", unitName(cfg.ID))
	time.Sleep(time.Second)
	var rh []string
	ok := healthCheck(cfg.Health, &rh)
	rlog = append(rlog, rh...)
	add("回滚 · 还原备份", ok, rlog...)
	if ok {
		res.Result = "rolledback"
	} else {
		res.Result = "failed"
	}
	return res
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}
