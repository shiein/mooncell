package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// pm2 Runner:用 pm2 托管进程(node 生态常见)。与 systemd Runner 平行,不改已验证的 systemd 路径。
// 进程类型(native-binary/java-jar/python)通过 ecosystem 的 interpreter 区分;env/args 写进 ecosystem 文件,
// 与 systemd 一样在回滚时连配置一起还原(ecosystem 文件随制品一并备份)。

func pm2EcoPath(binPath string) string { return binPath + ".pm2.json" }

func pm2(args ...string) (string, error) {
	out, err := exec.Command("pm2", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// pm2Adopt 判断是否为「接管已有进程」模式:配置带 pm2Name 即接管(只 restart 用户已有进程,不写 ecosystem)。
func pm2Adopt(cfg DeployConfig) bool { return strings.TrimSpace(cfg.Pm2Name) != "" }

// pm2ProcName 解析要操作的 pm2 进程名:接管模式用用户指定的已有进程名,否则用 Mooncell 托管名 deploy-<id>。
func pm2ProcName(cfg DeployConfig) string {
	if n := strings.TrimSpace(cfg.Pm2Name); n != "" {
		return n
	}
	return unitName(cfg.ID)
}

// pm2DeployTarget 取接管模式下应被部署(备份+替换)的真实文件路径。用 jlist(JSON)而非 describe(文本)稳健解析。
// 关键:pm_exec_path 是「pm2 实际执行的那个文件」——node/python/native 下它就是脚本/二进制(即部署目标),
// 但 java-jar 下,若用户以 `pm2 start <java> -- -jar app.jar` 启动,pm_exec_path 是 **java 解释器**(不是 jar)!
// 直接拿它当目标会把新 jar 覆盖到 java 二进制上、毁掉 JDK。故对 java-jar 必须解析出真正的 .jar(见 parsePm2DeployTarget)。
func pm2DeployTarget(nameOrID, typ string) (string, error) {
	out, err := pm2("jlist")
	if err != nil {
		return "", fmt.Errorf("pm2 jlist 失败: %s", out)
	}
	return parsePm2DeployTarget(out, nameOrID, typ)
}

// pm2Proc 是 pm2 jlist 里单个进程的关心字段。args 在不同 pm2 版本可能是 []string 或空格分隔字符串,故 RawMessage 兜底。
type pm2Proc struct {
	Name   string `json:"name"`
	PmID   int    `json:"pm_id"`
	Pm2Env struct {
		PmExecPath string          `json:"pm_exec_path"`
		Args       json.RawMessage `json:"args"`
	} `json:"pm2_env"`
}

// pm2Args 把 pm2_env.args 归一为 []string(兼容数组或单字符串两种形态)。
func pm2Args(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		return arr
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.Fields(s)
	}
	return nil
}

// firstJar 返回参数里的 jar 路径:优先取紧跟 -jar 的那个,否则取第一个 .jar 结尾的 token。
func firstJar(args []string) string {
	for i, a := range args {
		if a == "-jar" && i+1 < len(args) && strings.HasSuffix(args[i+1], ".jar") {
			return args[i+1]
		}
	}
	for _, a := range args {
		if strings.HasSuffix(a, ".jar") {
			return a
		}
	}
	return ""
}

// parsePm2DeployTarget 从 pm2 jlist 按进程名/数字 id 解析接管模式的部署目标(纯函数,便于测试)。
//   - 非 java-jar:pm_exec_path 即被执行的脚本/二进制,直接作为目标;
//   - java-jar:pm_exec_path 是 .jar(--interpreter java 启动)→ 用之;否则它是 java 解释器
//     (`pm2 start <java> -- -jar app.jar` 启动)→ 必须从 args 里定位 .jar;找不到则拒绝(绝不覆盖 java)。
func parsePm2DeployTarget(jlist, nameOrID, typ string) (string, error) {
	var apps []pm2Proc
	if err := json.Unmarshal([]byte(jlist), &apps); err != nil {
		return "", fmt.Errorf("解析 pm2 jlist 失败: %v", err)
	}
	for _, p := range apps {
		if p.Name != nameOrID && fmt.Sprint(p.PmID) != nameOrID {
			continue
		}
		exec := strings.TrimSpace(p.Pm2Env.PmExecPath)
		if exec == "" {
			return "", fmt.Errorf("pm2 进程 %s 无 pm_exec_path", nameOrID)
		}
		if typ != "java-jar" || strings.HasSuffix(exec, ".jar") {
			return exec, nil
		}
		// java-jar 且 pm_exec_path 不是 jar(是 java 解释器):从启动参数里找真正的 jar。
		if jar := firstJar(pm2Args(p.Pm2Env.Args)); jar != "" {
			return jar, nil
		}
		return "", fmt.Errorf("接管的 pm2 进程 %s 的 pm_exec_path 是 java 解释器(%s)、启动参数中也未找到 .jar,"+
			"无法安全定位部署目标(拒绝覆盖 java 二进制)。请用绝对路径 jar 启动(pm2 start <绝对路径>.jar --interpreter java),"+
			"或确认启动参数含 -jar <jar 路径>", nameOrID, exec)
	}
	return "", fmt.Errorf("pm2 中找不到进程 %s", nameOrID)
}

// writePm2Eco 按类型生成 pm2 ecosystem 配置(含 interpreter / env / cwd)并落盘到制品旁。
func writePm2Eco(cfg DeployConfig) (string, error) {
	app := map[string]any{
		"name":   unitName(cfg.ID),
		"script": cfg.BinPath,
		"cwd":    cfg.Workdir,
	}
	if app["cwd"] == "" {
		app["cwd"] = filepath.Dir(cfg.BinPath)
	}
	switch cfg.Type {
	case "python", "node":
		// 运行时解释器:Interpreter 指定则用之(venv python / 自定义 node),否则默认 python3 / node。
		if ip := strings.TrimSpace(cfg.Interpreter); ip != "" {
			app["interpreter"] = ip
		} else {
			app["interpreter"] = map[string]string{"python": "python3", "node": "node"}[cfg.Type]
		}
	case "java-jar":
		// pm2 实际执行 `java <interpreter_args> <script>`,故 JVM 参数必须排在 -jar 之前——
		// 否则 `java -jar -Xmx512m app.jar` 会把 -Xmx512m 当 jar 名,进程起不来。
		app["interpreter"] = "java"
		if j := strings.TrimSpace(cfg.JvmArgs); j != "" {
			app["interpreter_args"] = j + " -jar"
		} else {
			app["interpreter_args"] = "-jar"
		}
	default: // native-binary:可执行文件直跑
		app["interpreter"] = "none"
	}
	if a := strings.TrimSpace(cfg.Args); a != "" {
		app["args"] = a
	}
	if len(cfg.Env) > 0 {
		app["env"] = cfg.Env
	}
	eco := map[string]any{"apps": []any{app}}
	b, err := json.MarshalIndent(eco, "", "  ")
	if err != nil {
		return "", err
	}
	path := pm2EcoPath(cfg.BinPath)
	return path, os.WriteFile(path, b, 0644)
}

// pm2Online 查进程是否在 pm2 中运行(pid 非空且非 0)。name 为已解析的 pm2 进程名(托管名或接管名)。
func pm2Online(name string) bool {
	out, err := pm2("pid", name)
	if err != nil {
		return false
	}
	p := strings.TrimSpace(out)
	return p != "" && p != "0"
}

// 进程态轮询预算:pm2 托管的慢启动进程(尤其 Java/JVM;接管模式 restart 时旧实例端口可能未及时释放,
// 新实例瞬时 BindException 崩溃后被 pm2 自动重启)在固定 1s 采样点常处于 launching/errored 窗口,
// `pm2 pid` 此刻返回 0,单次采样会把好进程误判成未起。给足预算轮询等其稳定 online,与 HTTP 探活同理。
const pm2OnlineRetries = 15
const pm2OnlineInterval = time.Second

// pm2OnlineWithin 在预算内轮询进程状态:一旦 online 立即返回 true;耗尽预算仍未 online 才返回 false。
// 立即先查一次(健康进程零额外延迟,与原 1s 单采样等速),不在线再按 interval 轮询容忍慢启动/重启窗口。
func pm2OnlineWithin(name string, retries int, interval time.Duration) bool {
	for i := 0; i < retries; i++ {
		if pm2Online(name) {
			return true
		}
		time.Sleep(interval)
	}
	return false
}

// pm2Start 以 ecosystem 文件启动(先 delete 再 start,保证用最新配置),返回 pid。
func pm2Start(cfg DeployConfig, eco string) (string, error) {
	name := unitName(cfg.ID)
	pm2("delete", name) // 忽略不存在的错误
	if out, err := pm2("start", eco, "--update-env"); err != nil {
		return "", fmt.Errorf("pm2 start 失败: %s", out)
	}
	pid, _ := pm2("pid", name)
	return strings.TrimSpace(pid), nil
}

// runDeployPm2 进程类部署的 pm2 实现:备份 → 停 → 原子替换 → 写 ecosystem + pm2 启动 → 健康检查;失败回滚。
func (a *agent) runDeployPm2(cfg DeployConfig, artifact string, emit func(Step)) DeployResult {
	res := DeployResult{Version: cfg.Version}
	add := func(name string, ok bool, logs ...string) {
		s := Step{Name: name, OK: ok, Logs: logs}
		res.Steps = append(res.Steps, s)
		emit(s)
	}

	archived := scriptArchived(cfg, artifact) // python/node 多文件压缩包 → 解包到目录(依赖 Type,不依赖 BinPath)

	// 接管模式:部署目标取该 pm2 进程真正应被替换的文件(覆盖 Console 下发的 BinPath)。
	// 对 java-jar 必须是 .jar 而非 java 解释器(见 pm2DeployTarget),否则会覆盖 java 二进制毁掉 JDK。
	// 用户只填进程名即可,不必手动对齐路径;取不到目标=进程不存在或无法安全定位,直接失败不替换。
	if pm2Adopt(cfg) {
		target, perr := pm2DeployTarget(pm2ProcName(cfg), cfg.Type)
		if perr != nil {
			add("校验目标", false, "接管模式无法定位部署目标:"+perr.Error()+"(请确认该 pm2 进程已存在)")
			res.Result = "failed"
			return res
		}
		// 复校 deploy_roots:prepareDeploy 校验的是 Console 下发的 BinPath,接管模式在此覆盖,
		// 覆盖后必须重新 withinRoots——否则 placeArtifact 会写白名单外的真实路径(纵深防御对称)。
		abs, aerr := filepath.Abs(target)
		if aerr != nil {
			add("校验目标", false, "接管目标路径绝对化失败: "+aerr.Error())
			res.Result = "failed"
			return res
		}
		if !withinRoots(abs, a.cfg.Paths.DeployRoots) {
			add("校验目标", false, "接管目标不在 deploy_roots 白名单内: "+abs)
			res.Result = "failed"
			return res
		}
		cfg.BinPath = abs
		add("接管目标", true, "pm2 进程 "+pm2ProcName(cfg)+" 部署目标 "+abs+";将备份并替换此文件后 restart")
	}

	add("校验制品", true, "sha256 "+short(sha256File(artifact)), "目标 "+cfg.BinPath, "Runner pm2")

	bkDir, err := a.backupCurrent(cfg, archived)
	if err != nil {
		add("备份当前版本", false, err.Error())
		res.Result = "failed"
		return res
	}
	if bkDir == "" {
		add("备份当前版本", true, "首次部署,无当前制品需备份")
	} else {
		add("备份当前版本", true, "备份(含 ecosystem) → "+bkDir+" · 滚动保留 "+fmt.Sprint(cfg.BackupKeep)+" 份")
	}

	name := pm2ProcName(cfg) // 接管模式=用户已有进程名(存在性已在上面取 pm_exec_path 时校验),否则 deploy-<id>
	pm2("stop", name)
	add("停止服务", true, "pm2 stop "+name)

	plog, err := a.placeArtifact(cfg, artifact)
	if err != nil {
		add("替换制品", false, err.Error())
		rlog, ok := a.rollbackPm2(cfg, bkDir)
		res.Result = rollbackResult(rlog, ok, add)
		return res
	}
	add("替换制品", true, plog)

	// python 多文件包:有 requirements.txt 则装依赖,失败回滚(与 systemd 路径一致)。
	if ran, ilog, ierr := installPyRequirements(cfg); ran {
		if ierr != nil {
			add("安装依赖", false, ilog)
			rlog, ok := a.rollbackPm2(cfg, bkDir)
			res.Result = rollbackResult(rlog, ok, add)
			return res
		}
		add("安装依赖", true, ilog)
	}

	// 启动:接管模式只 restart 用户已有进程(不写 ecosystem);托管模式写 ecosystem 后 delete+start。
	if pm2Adopt(cfg) {
		out, e := pm2("restart", name, "--update-env")
		if e != nil {
			add("启动服务", false, "pm2 restart 失败: "+out)
		} else {
			pid, _ := pm2("pid", name)
			add("启动服务", true, "pm2 restart(接管模式)· "+name+" · pid "+strings.TrimSpace(pid))
		}
	} else {
		eco, err := writePm2Eco(cfg)
		if err != nil {
			add("启动服务", false, "写 ecosystem 失败: "+err.Error())
			rlog, ok := a.rollbackPm2(cfg, bkDir)
			res.Result = rollbackResult(rlog, ok, add)
			return res
		}
		pid, err := pm2Start(cfg, eco)
		if err != nil {
			add("启动服务", false, err.Error())
		} else {
			add("启动服务", true, "pm2 托管 · "+name+" · pid "+pid)
		}
	}
	time.Sleep(time.Second)

	// 健康检查:先在预算内轮询确认 pm2 进程稳定 online(Java 等慢启动/接管重启窗口容错,见 pm2OnlineWithin),
	// 再做可选 HTTP 探活;未配置 HTTP 时即以进程态为准,避免启动失败被判成功。
	var hlog []string
	if processHealthy(cfg.Health, pm2OnlineWithin(name, pm2OnlineRetries, pm2OnlineInterval), &hlog) {
		add("健康检查", true, hlog...)
		os.WriteFile(verSidecar(cfg.BinPath), []byte(cfg.Version), 0644)
		res.Result = "success"
		return res
	}
	add("健康检查", false, hlog...)

	// 失败 → 回滚:还原制品 + ecosystem(回滚前配置),pm2 重启
	if bkDir == "" {
		pm2("stop", name)
		add("回滚", false, "首次部署无备份可回滚,已停止服务")
		res.Result = "failed"
		return res
	}
	rlog, ok := a.rollbackPm2(cfg, bkDir)
	res.Result = rollbackResult(rlog, ok, add)
	return res
}

// rollbackPm2 在 pm2 服务已停止或部署失败后恢复旧制品(托管模式还连 ecosystem 一起还原),并尝试拉起旧服务。
// 接管模式:Mooncell 不拥有 ecosystem,只还原旧二进制 + pm2 restart 用户进程(配置由用户自管,不碰)。
func (a *agent) rollbackPm2(cfg DeployConfig, bkDir string) ([]string, bool) {
	name := pm2ProcName(cfg)
	if bkDir == "" {
		pm2("stop", name)
		return []string{"首次部署无备份可回滚,已停止服务"}, false
	}
	rlog := []string{"读取 " + bkDir, "还原备份制品"}
	pm2("stop", name)
	if err := a.restoreArtifactFrom(cfg, bkDir); err != nil {
		return append(rlog, "还原失败: "+err.Error()), false
	}
	if pm2Adopt(cfg) {
		pm2("restart", name, "--update-env")
		rlog = append(rlog, "接管模式:restart 已有进程(不动用户 ecosystem)")
	} else {
		ecoBak := filepath.Join(bkDir, "ecosystem.json")
		if fileExists(ecoBak) {
			if err := copyFile(ecoBak, pm2EcoPath(cfg.BinPath), 0644); err != nil {
				return append(rlog, "还原 ecosystem 失败: "+err.Error()), false
			}
			rlog = append(rlog, "还原 ecosystem 配置")
		}
		pm2("delete", name)
		pm2("start", pm2EcoPath(cfg.BinPath), "--update-env")
	}
	time.Sleep(time.Second)
	var rh []string
	ok := processHealthy(cfg.Health, pm2OnlineWithin(name, pm2OnlineRetries, pm2OnlineInterval), &rh) // 回滚同样确认 pm2 进程真起来(慢启动容错)
	rlog = append(rlog, rh...)
	return rlog, ok
}
