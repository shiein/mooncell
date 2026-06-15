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

// pm2Online 查进程是否在 pm2 中运行(pid 非空且非 0)。
func pm2Online(id string) bool {
	out, err := pm2("pid", unitName(id))
	if err != nil {
		return false
	}
	p := strings.TrimSpace(out)
	return p != "" && p != "0"
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

	archived := scriptArchived(cfg, artifact) // python/node 多文件压缩包 → 解包到目录
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

	pm2("stop", unitName(cfg.ID))
	add("停止服务", true, "pm2 stop "+unitName(cfg.ID))

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
		add("启动服务", true, "pm2 托管 · "+unitName(cfg.ID)+" · pid "+pid)
	}
	time.Sleep(time.Second)

	// 健康检查:HTTP 探活;未配置 HTTP 时退化为查 pm2 进程状态,避免启动失败被判成功。
	var hlog []string
	if processHealthy(cfg.Health, pm2Online(cfg.ID), &hlog) {
		add("健康检查", true, hlog...)
		os.WriteFile(verSidecar(cfg.BinPath), []byte(cfg.Version), 0644)
		res.Result = "success"
		return res
	}
	add("健康检查", false, hlog...)

	// 失败 → 回滚:还原制品 + ecosystem(回滚前配置),pm2 重启
	if bkDir == "" {
		pm2("stop", unitName(cfg.ID))
		add("回滚", false, "首次部署无备份可回滚,已停止服务")
		res.Result = "failed"
		return res
	}
	rlog, ok := a.rollbackPm2(cfg, bkDir)
	res.Result = rollbackResult(rlog, ok, add)
	return res
}

// rollbackPm2 在 pm2 服务已停止或部署失败后恢复旧制品与旧 ecosystem,并尝试拉起旧服务。
func (a *agent) rollbackPm2(cfg DeployConfig, bkDir string) ([]string, bool) {
	if bkDir == "" {
		pm2("stop", unitName(cfg.ID))
		return []string{"首次部署无备份可回滚,已停止服务"}, false
	}
	rlog := []string{"读取 " + bkDir, "还原备份制品"}
	pm2("stop", unitName(cfg.ID))
	if err := a.restoreArtifactFrom(cfg, bkDir); err != nil {
		return append(rlog, "还原失败: "+err.Error()), false
	}
	ecoBak := filepath.Join(bkDir, "ecosystem.json")
	if fileExists(ecoBak) {
		if err := copyFile(ecoBak, pm2EcoPath(cfg.BinPath), 0644); err != nil {
			return append(rlog, "还原 ecosystem 失败: "+err.Error()), false
		}
		rlog = append(rlog, "还原 ecosystem 配置")
	}
	pm2("delete", unitName(cfg.ID))
	pm2("start", pm2EcoPath(cfg.BinPath), "--update-env")
	time.Sleep(time.Second)
	var rh []string
	ok := processHealthy(cfg.Health, pm2Online(cfg.ID), &rh) // 回滚同样确认 pm2 进程真起来
	rlog = append(rlog, rh...)
	return rlog, ok
}
