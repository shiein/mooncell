package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Type           string            `json:"type"`    // go-binary | java-jar | static-nginx;空默认 go-binary
	BinPath        string            `json:"binPath"` // go/java:制品落盘路径;static:对外 web root 软链路径
	Workdir        string            `json:"workdir"`
	Runner         string            `json:"runner"`      // systemd(默认)| pm2;决定进程托管方式
	Interpreter    string            `json:"interpreter"` // python:解释器路径(支持 venv,如 .../venv/bin/python);空则 python3
	Args           string            `json:"args"`        // 启动参数
	JvmArgs        string            `json:"jvmArgs"`     // java-jar:JVM 参数
	Env            map[string]string `json:"env"`
	User           string            `json:"user"`
	Health         string            `json:"health"` // HTTP 健康检查 URL,空则跳过
	Version        string            `json:"version"`
	ReleaseID      string            `json:"releaseId"`      // 幂等键:Agent 本地记录已成功的 releaseId,重复请求直接返回缓存结果
	ExpectedSha256 string            `json:"expectedSha256"` // 非空则部署前强校验制品 sha256,不匹配直接失败
	BackupKeep     int               `json:"backupKeep"`
	ReloadCmd      string            `json:"reloadCmd"` // static/tomcat:部署后 reload 钩子,白名单动作名(nginx-reload 等),非自由 shell
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

// execStart 按类型拼 systemd ExecStart。systemd 要求绝对路径,
// java-jar 需用 exec.LookPath 把 "java" 解析为绝对路径。
func execStart(cfg DeployConfig) (string, error) {
	switch cfg.Type {
	case "java-jar":
		java, err := exec.LookPath("java")
		if err != nil {
			return "", fmt.Errorf("未找到 java(请先安装 JRE): %w", err)
		}
		parts := []string{java}
		if j := strings.TrimSpace(cfg.JvmArgs); j != "" {
			parts = append(parts, j)
		}
		parts = append(parts, "-jar", cfg.BinPath)
		if a := strings.TrimSpace(cfg.Args); a != "" {
			parts = append(parts, a)
		}
		return strings.Join(parts, " "), nil
	case "python", "node":
		// 入口脚本经运行时执行(python3 / node)。Interpreter 指定时用之(venv / 自定义 node 路径)。
		rt, err := runtimeBin(cfg, map[string]string{"python": "python3", "node": "node"}[cfg.Type])
		if err != nil {
			return "", err
		}
		parts := []string{rt, cfg.BinPath}
		if a := strings.TrimSpace(cfg.Args); a != "" {
			parts = append(parts, a)
		}
		return strings.Join(parts, " "), nil
	default: // go-binary
		es := cfg.BinPath
		if a := strings.TrimSpace(cfg.Args); a != "" {
			es += " " + a
		}
		return es, nil
	}
}

// runtimeBin 解析运行时可执行(解释器):Interpreter 显式指定则用之(支持 venv / 自定义 node 路径),
// 否则在 PATH 查找 defaultName。python / node 共用。
func runtimeBin(cfg DeployConfig, defaultName string) (string, error) {
	if ip := strings.TrimSpace(cfg.Interpreter); ip != "" {
		return ip, nil
	}
	bin, err := exec.LookPath(defaultName)
	if err != nil {
		return "", fmt.Errorf("未找到 %s(或在配置里指定运行时路径): %w", defaultName, err)
	}
	return bin, nil
}

// validateUnitFields 拒绝会破坏 systemd unit 格式 / 注入额外指令的值(换行、回车、控制字符)。
// 配置虽已由 Console 据已存应用生成,但 unit 渲染仍做硬校验,纵深防御。
func validateUnitFields(cfg DeployConfig) error {
	bad := func(name, v string) error {
		if strings.ContainsAny(v, "\n\r\x00") {
			return fmt.Errorf("配置字段 %s 含非法字符(换行/控制字符),拒绝写入 unit", name)
		}
		return nil
	}
	for _, f := range []struct{ name, v string }{
		{"name", cfg.Name}, {"user", cfg.User}, {"workdir", cfg.Workdir},
		{"binPath", cfg.BinPath}, {"args", cfg.Args}, {"jvmArgs", cfg.JvmArgs}, {"interpreter", cfg.Interpreter},
	} {
		if err := bad(f.name, f.v); err != nil {
			return err
		}
	}
	for k, v := range cfg.Env {
		if err := bad("env key", k); err != nil {
			return err
		}
		if err := bad("env["+k+"]", v); err != nil {
			return err
		}
	}
	return nil
}

func writeUnit(cfg DeployConfig) error {
	if err := validateUnitFields(cfg); err != nil {
		return err
	}
	var env strings.Builder
	for k, v := range cfg.Env {
		fmt.Fprintf(&env, "Environment=%s=%s\n", k, v)
	}
	es, err := execStart(cfg)
	if err != nil {
		return err
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
`, cfg.Name, wd, es, env.String(), user)
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

// verSidecar 是记录"当前落盘制品版本"的旁车文件,紧邻制品。Agent 无状态,靠它在备份时
// 得知被备份的(旧)制品版本——而非正在部署的新版本,避免备份版本标签错位。
func verSidecar(binPath string) string { return binPath + ".ver" }

// currentVersion 读取制品旁车记录的版本;无旁车(早期部署/首次)返回空。
func currentVersion(binPath string) string {
	b, err := os.ReadFile(verSidecar(binPath))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// scriptArchived 判断本次 python/node 部署是否为多文件压缩包(决定解包到目录还是单文件替换)。
func scriptArchived(cfg DeployConfig, artifact string) bool {
	return (cfg.Type == "python" || cfg.Type == "node") && sniffArchive(artifact) != ""
}

// clearDir 清空目录内容但保留目录本身(多文件解包前清旧版本)。
func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(dir, 0755)
		}
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// tarDir 把目录打成 tar.gz(用系统 tar)。
func tarDir(src, dest string) error {
	out, err := exec.Command("tar", "-czf", dest, "-C", src, ".").CombinedOutput()
	if err != nil {
		return fmt.Errorf("打包失败: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// placeArtifact 落盘制品:python/node 压缩包 → 清空应用目录后智能解包(多文件);否则单文件原子替换。
func placeArtifact(cfg DeployConfig, artifact string) (string, error) {
	if scriptArchived(cfg, artifact) {
		appDir := filepath.Dir(cfg.BinPath)
		format := sniffArchive(artifact)
		if err := clearDir(appDir); err != nil {
			return "", err
		}
		if err := extractArchiveSmart(artifact, appDir, format); err != nil {
			return "", err
		}
		if _, err := os.Stat(cfg.BinPath); err != nil {
			return "", fmt.Errorf("解包后未找到入口脚本 %s(检查包内路径)", filepath.Base(cfg.BinPath))
		}
		return "多文件包(" + format + ")智能解包 → " + appDir, nil
	}
	os.MkdirAll(filepath.Dir(cfg.BinPath), 0755)
	if err := atomicReplace(artifact, cfg.BinPath); err != nil {
		return "", err
	}
	return "tmp 落盘 → rename 原子替换 " + cfg.BinPath, nil
}

// restoreArtifactFrom 从备份还原制品:含 app.tar.gz → 清目录后解包(多文件);否则单文件原子替换。
func restoreArtifactFrom(cfg DeployConfig, bkDir string) error {
	if tarPath := filepath.Join(bkDir, "app.tar.gz"); fileExists(tarPath) {
		appDir := filepath.Dir(cfg.BinPath)
		if err := clearDir(appDir); err != nil {
			return err
		}
		return extractArchiveSmart(tarPath, appDir, "gzip")
	}
	return atomicReplace(filepath.Join(bkDir, "app"), cfg.BinPath)
}

// backupCurrent 把当前制品复制到 backups/<id>/<ts>/,附 meta.json;首次部署(无当前制品)返回空。
// archived=true 时整应用目录打包为 app.tar.gz(多文件),否则单文件存为 app。
// meta.version 取旧制品旁车记录的版本(被备份的就是这个版本),不是正在部署的新版本。
func (a *agent) backupCurrent(cfg DeployConfig, archived bool) (string, error) {
	if _, err := os.Stat(cfg.BinPath); err != nil {
		return "", nil // 首次部署
	}
	// 纳秒精度:避免同秒内连续部署/还原撞同一目录名而互相覆盖,丢失备份。字典序仍 = 时间序。
	ts := time.Now().Format("20060102_150405.000000000")
	dir := filepath.Join(a.cfg.Paths.BackupDir, cfg.ID, ts)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	if archived {
		if err := tarDir(filepath.Dir(cfg.BinPath), filepath.Join(dir, "app.tar.gz")); err != nil {
			return "", err
		}
	} else if err := copyFile(cfg.BinPath, filepath.Join(dir, "app"), 0755); err != nil {
		return "", err
	}
	// 一并备份当前运行期配置(systemd unit 或 pm2 ecosystem):回滚要连配置一起还原,
	// 否则旧制品会跑在新部署改过的配置下(如 env 变更),回滚后行为仍是错的。
	if up := unitPath(cfg.ID); fileExists(up) {
		copyFile(up, filepath.Join(dir, "unit.service"), 0644)
	}
	if eco := pm2EcoPath(cfg.BinPath); fileExists(eco) {
		copyFile(eco, filepath.Join(dir, "ecosystem.json"), 0644)
	}
	meta := fmt.Sprintf(`{"version":%q,"sha256":%q,"time":%d,"operator":"console"}`,
		currentVersion(cfg.BinPath), sha256File(cfg.BinPath), time.Now().UnixMilli())
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

// processHealthy 进程类健康判定:配置了 HTTP 健康检查走 HTTP;否则退化为「进程是否真的在托管运行」。
// 杜绝「启动失败 + 未配健康检查」被误判为成功——alive 由各 Runner 传入(systemd is-active / pm2 online)。
func processHealthy(healthURL string, alive bool, logs *[]string) bool {
	if strings.TrimSpace(healthURL) != "" {
		return healthCheck(healthURL, logs)
	}
	if alive {
		*logs = append(*logs, "未配置 HTTP 健康检查 · 进程托管状态正常(active/online)")
		return true
	}
	*logs = append(*logs, "未配置 HTTP 健康检查 · 进程未处于运行态(启动失败)")
	return false
}

// reloadActions 是 static/tomcat 部署后可选 reload 钩子的白名单:动作名 → 固定 argv(不经 shell)。
// 杜绝把前端/Console 下发的字符串当 shell 执行(任意命令执行)。
var reloadActions = map[string][]string{
	"nginx-reload":   {"nginx", "-s", "reload"},
	"nginx-restart":  {"systemctl", "reload", "nginx"},
	"tomcat-restart": {"systemctl", "restart", "tomcat"},
}

// runReload 执行白名单内的 reload 动作。空动作跳过(ran=false);白名单外动作拒绝执行并报错。
func runReload(action string) (ran bool, log string, err error) {
	action = strings.TrimSpace(action)
	if action == "" {
		return false, "", nil
	}
	argv, ok := reloadActions[action]
	if !ok {
		return true, "拒绝执行白名单外的 reload 动作: " + action, fmt.Errorf("disallowed reload action %q", action)
	}
	out, e := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	return true, strings.Join(argv, " ") + " → " + strings.TrimSpace(string(out)), e
}

// copyToTemp 把 src 复制到独立临时文件,返回路径与清理函数。
// 还原时用它保护「被还原的备份制品」:流水线里 backupCurrent 会滚动清理备份,
// 若直接拿最老备份当源,可能在 atomicReplace 前被清掉——先拷出来就不受影响。
func copyToTemp(src string) (string, func(), error) {
	f, err := os.CreateTemp("", "mc-restore-*")
	if err != nil {
		return "", nil, err
	}
	f.Close()
	if err := copyFile(src, f.Name(), 0755); err != nil {
		os.Remove(f.Name())
		return "", nil, err
	}
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// ---------- 部署流水线 ----------

// releaseRecPath 是某 releaseId 的本地成功记录路径(filepath.Base 防穿越)。
func (a *agent) releaseRecPath(rid string) string {
	return filepath.Join(a.cfg.Paths.BackupDir, "_deploys", filepath.Base(rid))
}

// releaseDone 读取 releaseId 的已成功记录(仅 success 才算幂等命中)。
func (a *agent) releaseDone(rid string) (DeployResult, bool) {
	b, err := os.ReadFile(a.releaseRecPath(rid))
	if err != nil {
		return DeployResult{}, false
	}
	var res DeployResult
	if json.Unmarshal(b, &res) != nil || res.Result != "success" {
		return DeployResult{}, false
	}
	return res, true
}

func (a *agent) recordRelease(rid string, res DeployResult) {
	os.MkdirAll(filepath.Join(a.cfg.Paths.BackupDir, "_deploys"), 0755)
	b, _ := json.Marshal(res)
	os.WriteFile(a.releaseRecPath(rid), b, 0644)
}

// runDeployIdempotent 包裹 runDeploy 做 Agent 侧幂等:releaseId 已成功则直接返回缓存结果,
// 不重复执行(防 Console 重试 / Agent 直连导致重复部署);成功后记录。
func (a *agent) runDeployIdempotent(cfg DeployConfig, artifact string, emit func(Step)) DeployResult {
	if emit == nil {
		emit = func(Step) {}
	}
	if cfg.ReleaseID != "" {
		if cached, ok := a.releaseDone(cfg.ReleaseID); ok {
			emit(Step{Name: "幂等跳过", OK: true, Logs: []string{"releaseId 已成功部署,跳过重复执行"}})
			return cached
		}
	}
	res := a.runDeploy(cfg, artifact, emit)
	if cfg.ReleaseID != "" && res.Result == "success" {
		a.recordRelease(cfg.ReleaseID, res)
	}
	return res
}

// runDeploy 按应用类型分发:static-nginx 走软链切换,tomcat-war 走容器 WAR 替换,
// 其余(go-binary/java-jar/python)复用 systemd 进程流水线。
// emit 在每步完成时回调(用于 SSE 实时流);同步 JSON 端点传 nil 即可。
func (a *agent) runDeploy(cfg DeployConfig, artifact string, emit func(Step)) DeployResult {
	if emit == nil {
		emit = func(Step) {}
	}
	// 制品完整性:期望 sha256 不匹配则直接失败,不进任何部署路径(防传输损坏 / 制品被替换)。
	if exp := strings.TrimSpace(cfg.ExpectedSha256); exp != "" {
		if actual := sha256File(artifact); !strings.EqualFold(exp, actual) {
			s := Step{Name: "校验制品", OK: false, Logs: []string{"sha256 不匹配 · 期望 " + short(exp) + " 实得 " + short(actual)}}
			emit(s)
			return DeployResult{Result: "failed", Version: cfg.Version, Steps: []Step{s}}
		}
	}
	switch cfg.Type {
	case "static-nginx":
		return a.runDeployStatic(cfg, artifact, emit)
	case "tomcat-war":
		return a.runDeployTomcat(cfg, artifact, emit)
	default:
		if cfg.Runner == "pm2" {
			return a.runDeployPm2(cfg, artifact, emit)
		}
		return a.runDeployProcess(cfg, artifact, emit)
	}
}

// runDeployProcess 执行已验证的进程类部署闭环:备份 → 停 → 原子替换 → 生成 unit + 启动 → 健康检查;失败自动回滚。
// go-binary 与 java-jar 共用此流水线,差异只在 execStart 与制品落盘(jar 同样按文件原子替换)。
func (a *agent) runDeployProcess(cfg DeployConfig, artifact string, emit func(Step)) DeployResult {
	res := DeployResult{Version: cfg.Version}
	add := func(name string, ok bool, logs ...string) {
		s := Step{Name: name, OK: ok, Logs: logs}
		res.Steps = append(res.Steps, s)
		emit(s) // 每步完成即推送,供 SSE 实时呈现
	}

	archived := scriptArchived(cfg, artifact) // python/node 多文件压缩包 → 解包到目录

	// 1. 校验制品
	add("校验制品", true, "sha256 "+short(sha256File(artifact)), "目标 "+cfg.BinPath)

	// 2. 备份当前版本(多文件整目录打包)
	bkDir, err := a.backupCurrent(cfg, archived)
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

	// 4. 落盘制品(单文件原子替换 / 多文件智能解包)
	plog, err := placeArtifact(cfg, artifact)
	if err != nil {
		add("替换制品", false, err.Error())
		res.Result = "failed"
		return res
	}
	add("替换制品", true, plog)

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

	// 6. 健康检查:HTTP 探活;未配置 HTTP 时退化为查 systemd 进程状态,避免启动失败被判成功。
	var hlog []string
	if processHealthy(cfg.Health, isActive(cfg.ID), &hlog) {
		add("健康检查", true, hlog...)
		// 记录当前落盘版本到旁车,供下次部署/还原备份时正确标注被备份的版本。
		os.WriteFile(verSidecar(cfg.BinPath), []byte(cfg.Version), 0644)
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
	rlog := []string{"读取 " + bkDir, "还原备份制品"}
	sysctl("stop", unitName(cfg.ID))
	if err := restoreArtifactFrom(cfg, bkDir); err != nil {
		rlog = append(rlog, "还原失败: "+err.Error())
		add("回滚 · 还原备份", false, rlog...)
		res.Result = "failed"
		return res
	}
	// 连同 unit 一起还原(env/args 等),否则旧制品会跑在本次失败部署改过的配置下。
	if bu := filepath.Join(bkDir, "unit.service"); fileExists(bu) {
		if err := copyFile(bu, unitPath(cfg.ID), 0644); err == nil {
			sysctl("daemon-reload")
			rlog = append(rlog, "还原 unit + daemon-reload")
		}
	}
	sysctl("start", unitName(cfg.ID))
	time.Sleep(time.Second)
	var rh []string
	ok := processHealthy(cfg.Health, isActive(cfg.ID), &rh) // 回滚同样要确认进程真的起来,不能空健康检查直接判成功
	rlog = append(rlog, rh...)
	add("回滚 · 还原备份", ok, rlog...)
	if ok {
		res.Result = "rolledback"
	} else {
		res.Result = "failed"
	}
	return res
}

// ---------- 静态站点(软链切换)----------

// sniffArchive 按魔数(非扩展名)判断压缩格式:gzip(.tar.gz)/ zip / tar;非压缩单文件返回空。
func sniffArchive(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 264)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	switch {
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		return "gzip"
	case len(head) >= 4 && head[0] == 0x50 && head[1] == 0x4b && (head[2] == 3 || head[2] == 5 || head[2] == 7):
		return "zip"
	case len(head) >= 262 && string(head[257:262]) == "ustar":
		return "tar"
	default:
		return ""
	}
}

// extractArchive 按格式解包到 dest(已建)。用系统 tar/unzip。
func extractArchive(archive, dest, format string) error {
	if err := os.MkdirAll(dest, 0755); err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch format {
	case "gzip":
		cmd = exec.Command("tar", "-xzf", archive, "-C", dest)
	case "tar":
		cmd = exec.Command("tar", "-xf", archive, "-C", dest)
	case "zip":
		cmd = exec.Command("unzip", "-oq", archive, "-d", dest)
	default:
		return fmt.Errorf("不支持的压缩格式")
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("解包失败: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// nonHiddenEntries 过滤掉点文件与 __MACOSX(zip 元数据),用于判断压缩包真实顶层结构。
func nonHiddenEntries(dir string) []os.DirEntry {
	all, _ := os.ReadDir(dir)
	out := all[:0]
	for _, e := range all {
		if n := e.Name(); strings.HasPrefix(n, ".") || n == "__MACOSX" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// flattenSingleTopDir 智能去多余顶层目录:压缩包若只含一个顶层目录(如 myapp-v1/ 整包包裹),
// 把其内容上提一层、去掉它;若是散落文件(index.html、app.js…)则原样保留。
func flattenSingleTopDir(dir string) error {
	entries := nonHiddenEntries(dir)
	if len(entries) != 1 || !entries[0].IsDir() {
		return nil
	}
	top := filepath.Join(dir, entries[0].Name())
	children, err := os.ReadDir(top)
	if err != nil {
		return err
	}
	for _, c := range children {
		if err := os.Rename(filepath.Join(top, c.Name()), filepath.Join(dir, c.Name())); err != nil {
			return err
		}
	}
	return os.Remove(top)
}

// extractArchiveSmart 解包 + 智能去多余顶层目录。
func extractArchiveSmart(archive, dest, format string) error {
	if err := extractArchive(archive, dest, format); err != nil {
		return err
	}
	return flattenSingleTopDir(dest)
}

// switchSymlink 原子切换软链 link → target:先建临时软链再 rename 覆盖,避免出现 link 短暂消失的窗口。
func switchSymlink(target, link string) error {
	tmp := link + ".tmp"
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link)
}

// runDeployStatic 静态站点部署:解包到带时间戳的 release 目录,原子切换软链对外暴露,失败回滚到旧 release。
// cfg.BinPath 是对外 web root 软链路径(如 /srv/apps/site/current);releases 存于 <BinPath>-releases/<ts>/。
func (a *agent) runDeployStatic(cfg DeployConfig, artifact string, emit func(Step)) DeployResult {
	res := DeployResult{Version: cfg.Version}
	add := func(name string, ok bool, logs ...string) {
		s := Step{Name: name, OK: ok, Logs: logs}
		res.Steps = append(res.Steps, s)
		emit(s)
	}

	// 1. 校验制品
	add("校验制品", true, "sha256 "+short(sha256File(artifact)), "软链 "+cfg.BinPath)

	// 2. 记录当前软链指向(用于回滚);首次部署无旧目标
	prevTarget, _ := os.Readlink(cfg.BinPath)
	if prevTarget == "" {
		add("备份当前版本", true, "首次部署,无旧 release 需记录")
	} else {
		add("备份当前版本", true, "当前指向 "+prevTarget)
	}

	// 3. 解包到新 release 目录(纳秒精度,避免同秒连续部署撞目录)
	ts := time.Now().Format("20060102_150405.000000000")
	releasesDir := cfg.BinPath + "-releases"
	newRelease := filepath.Join(releasesDir, ts)
	if err := os.MkdirAll(newRelease, 0755); err != nil {
		add("解包制品", false, err.Error())
		res.Result = "failed"
		return res
	}
	format := sniffArchive(artifact)
	if format == "" {
		os.RemoveAll(newRelease)
		add("解包制品", false, "静态站点制品须为压缩包(tar.gz / zip),收到非压缩文件")
		res.Result = "failed"
		return res
	}
	if err := extractArchiveSmart(artifact, newRelease, format); err != nil {
		os.RemoveAll(newRelease)
		add("解包制品", false, err.Error())
		res.Result = "failed"
		return res
	}
	add("解包制品", true, format+" 解包 + 智能去顶层目录 → "+newRelease)

	// 4. 原子切换软链
	if err := switchSymlink(newRelease, cfg.BinPath); err != nil {
		add("切换软链", false, err.Error())
		res.Result = "failed"
		return res
	}
	add("切换软链", true, cfg.BinPath+" → "+newRelease)

	// 5. 可选 reload 钩子(白名单动作,如 nginx-reload)
	if ran, log, err := runReload(cfg.ReloadCmd); ran {
		add("reload", err == nil, log)
	}

	// 6. 健康检查
	var hlog []string
	if healthCheck(cfg.Health, &hlog) {
		add("健康检查", true, hlog...)
		// 软链 release 也按份数滚动清理
		a.rotateReleases(releasesDir, cfg.BackupKeep)
		res.Result = "success"
		return res
	}
	add("健康检查", false, hlog...)

	// 7. 失败 → 回滚软链
	if prevTarget == "" {
		add("回滚", false, "首次部署无旧 release 可回滚")
		res.Result = "failed"
		return res
	}
	rlog := []string{"切回 " + prevTarget}
	if err := switchSymlink(prevTarget, cfg.BinPath); err != nil {
		rlog = append(rlog, "回滚失败: "+err.Error())
		add("回滚 · 软链", false, rlog...)
		res.Result = "failed"
		return res
	}
	runReload(cfg.ReloadCmd)
	var rh []string
	ok := healthCheck(cfg.Health, &rh)
	rlog = append(rlog, rh...)
	add("回滚 · 软链", ok, rlog...)
	os.RemoveAll(newRelease) // 失效 release 清理
	if ok {
		res.Result = "rolledback"
	} else {
		res.Result = "failed"
	}
	return res
}

// rotateReleases 按份数滚动保留 release 目录;当前软链指向的目录永不删除。
func (a *agent) rotateReleases(releasesDir string, keep int) {
	if keep <= 0 {
		keep = 5
	}
	entries, err := os.ReadDir(releasesDir)
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
		os.RemoveAll(filepath.Join(releasesDir, dirs[0]))
		dirs = dirs[1:]
	}
}

// ---------- Tomcat WAR(容器托管)----------

// runDeployTomcat 部署 WAR 到 Tomcat webapps:容器由运维长驻,平台只负责原子替换 WAR、
// 清旧展开目录(令容器重新展开)、可选 reload 钩子、健康检查;失败回滚 WAR。
// cfg.BinPath 是 webapps 下的 WAR 路径(如 /opt/tomcat/webapps/app.war);展开目录为同名去 .war。
// 不停容器进程(同容器内可能跑着别的应用),与进程类的 systemctl stop 不同。
func (a *agent) runDeployTomcat(cfg DeployConfig, artifact string, emit func(Step)) DeployResult {
	res := DeployResult{Version: cfg.Version}
	add := func(name string, ok bool, logs ...string) {
		s := Step{Name: name, OK: ok, Logs: logs}
		res.Steps = append(res.Steps, s)
		emit(s)
	}
	exploded := strings.TrimSuffix(cfg.BinPath, ".war") // 容器展开目录

	// 1. 校验制品
	add("校验制品", true, "sha256 "+short(sha256File(artifact)), "WAR "+cfg.BinPath)

	// 2. 备份当前 WAR(单文件)
	bkDir, err := a.backupCurrent(cfg, false)
	if err != nil {
		add("备份当前版本", false, err.Error())
		res.Result = "failed"
		return res
	}
	if bkDir == "" {
		add("备份当前版本", true, "首次部署,无当前 WAR 需备份")
	} else {
		add("备份当前版本", true, "备份 → "+bkDir+" · 滚动保留 "+fmt.Sprint(cfg.BackupKeep)+" 份")
	}

	// 3. 原子替换 WAR + 清旧展开目录(令容器重新展开为新版本)
	os.MkdirAll(filepath.Dir(cfg.BinPath), 0755)
	if err := atomicReplace(artifact, cfg.BinPath); err != nil {
		add("替换 WAR", false, err.Error())
		res.Result = "failed"
		return res
	}
	if exploded != cfg.BinPath {
		os.RemoveAll(exploded)
	}
	add("替换 WAR", true, "原子替换 "+cfg.BinPath, "清理展开目录 "+exploded)

	// 4. 可选 reload 钩子(白名单动作,如 tomcat-restart / nginx-reload)
	if ran, log, err := runReload(cfg.ReloadCmd); ran {
		add("reload", err == nil, log)
	}

	// 5. 健康检查(容器重部署需时间,沿用重试)
	var hlog []string
	if healthCheck(cfg.Health, &hlog) {
		add("健康检查", true, hlog...)
		os.WriteFile(verSidecar(cfg.BinPath), []byte(cfg.Version), 0644)
		res.Result = "success"
		return res
	}
	add("健康检查", false, hlog...)

	// 6. 失败 → 回滚 WAR
	if bkDir == "" {
		add("回滚", false, "首次部署无备份可回滚")
		res.Result = "failed"
		return res
	}
	rlog := []string{"读取 " + bkDir, "还原备份 WAR(原子替换)"}
	if err := atomicReplace(filepath.Join(bkDir, "app"), cfg.BinPath); err != nil {
		rlog = append(rlog, "还原失败: "+err.Error())
		add("回滚 · 还原 WAR", false, rlog...)
		res.Result = "failed"
		return res
	}
	if exploded != cfg.BinPath {
		os.RemoveAll(exploded)
	}
	runReload(cfg.ReloadCmd)
	var rh []string
	ok := healthCheck(cfg.Health, &rh)
	rlog = append(rlog, rh...)
	add("回滚 · 还原 WAR", ok, rlog...)
	if ok {
		res.Result = "rolledback"
	} else {
		res.Result = "failed"
	}
	return res
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}
