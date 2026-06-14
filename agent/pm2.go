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
// 进程类型(go-binary/java-jar/python)通过 ecosystem 的 interpreter 区分;env/args 写进 ecosystem 文件,
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
	case "python":
		if ip := strings.TrimSpace(cfg.Interpreter); ip != "" {
			app["interpreter"] = ip // venv 解释器
		} else {
			app["interpreter"] = "python3"
		}
	case "java-jar":
		app["interpreter"] = "java"
		app["interpreter_args"] = strings.TrimSpace("-jar " + cfg.JvmArgs)
	default: // go-binary:可执行文件直跑
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

	add("校验制品", true, "sha256 "+short(sha256File(artifact)), "目标 "+cfg.BinPath, "Runner pm2")

	bkDir, err := a.backupCurrent(cfg)
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

	os.MkdirAll(filepath.Dir(cfg.BinPath), 0755)
	if err := atomicReplace(artifact, cfg.BinPath); err != nil {
		add("替换制品", false, err.Error())
		res.Result = "failed"
		return res
	}
	add("替换制品", true, "tmp 落盘 → rename 原子替换 "+cfg.BinPath)

	eco, err := writePm2Eco(cfg)
	if err != nil {
		add("启动服务", false, "写 ecosystem 失败: "+err.Error())
		res.Result = "failed"
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
	rlog := []string{"读取 " + bkDir, "还原备份制品(原子替换)"}
	pm2("stop", unitName(cfg.ID))
	if err := atomicReplace(filepath.Join(bkDir, "app"), cfg.BinPath); err != nil {
		rlog = append(rlog, "还原失败: "+err.Error())
		add("回滚 · 还原备份", false, rlog...)
		res.Result = "failed"
		return res
	}
	ecoBak := filepath.Join(bkDir, "ecosystem.json")
	if fileExists(ecoBak) {
		copyFile(ecoBak, pm2EcoPath(cfg.BinPath), 0644)
		rlog = append(rlog, "还原 ecosystem 配置")
	}
	pm2("delete", unitName(cfg.ID))
	pm2("start", pm2EcoPath(cfg.BinPath), "--update-env")
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
