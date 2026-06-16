package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// nohup Runner(托管模式):不依赖 systemd / pm2,直接 `nohup <cmd> >> <log> 2>&1 & echo $! > <pidfile>`
// 启动并把 PID 落 pidfile,后续启停/状态/重启都据 pidfile。适配大量历史上以 `nohup ... &` 起的应用。
// 与 nohup 本身一致:无监管进程,崩了不会自动拉起(靠备份 + 失败回滚兜底,要自愈请改用 systemd)。
//
// 启动规格落 sidecar <binPath>.nohup.json:nohup 无 unit/ecosystem,启停/状态这类无状态请求(只带 binPath)
// 据此重建启动命令——与 systemd unit / pm2 ecosystem 对称,回滚时也连规格一起还原。

func nohupPidFile(cfg DeployConfig) string { return cfg.BinPath + ".pid" }
func nohupSpecPath(binPath string) string  { return binPath + ".nohup.json" }

// nohupSpec 是落盘的启动规格:启停/状态据此重建,无需每次重传全量配置。
type nohupSpec struct {
	Cmd     string            `json:"cmd"`     // 已解析好的待 nohup 命令(executable 绝对路径 + 用户参数)
	Workdir string            `json:"workdir"` // 工作目录
	LogPath string            `json:"logPath"` // stdout/stderr 重定向目标
	PidFile string            `json:"pidFile"`
	Env     map[string]string `json:"env,omitempty"`
}

// nohupLogPath 解析 stdout/stderr 重定向目标:Console 下发的 LogPath 在 log_roots 白名单内则用之,
// 否则退回 <binPath>.nohup.log(始终在 deploy_roots 内,因 binPath 已校验过)。
func (a *agent) nohupLogPath(cfg DeployConfig) string {
	if p := strings.TrimSpace(cfg.LogPath); p != "" && withinRoots(p, a.cfg.Paths.LogRoots) {
		return p
	}
	return cfg.BinPath + ".nohup.log"
}

// nohupCommand 拼要 nohup 起的命令(executable 经 LookPath 解析为绝对路径并 shell 引用;
// jvmArgs/args 为用户自由参数,与 systemd ExecStart 同样直接拼入,信任面不变)。
func nohupCommand(cfg DeployConfig) (string, error) {
	switch cfg.Type {
	case "java-jar":
		java, err := exec.LookPath("java")
		if err != nil {
			return "", fmt.Errorf("未找到 java(请先安装 JRE): %w", err)
		}
		parts := []string{shQuote(java)}
		if j := strings.TrimSpace(cfg.JvmArgs); j != "" {
			parts = append(parts, j)
		}
		parts = append(parts, "-jar", shQuote(cfg.BinPath))
		if a := strings.TrimSpace(cfg.Args); a != "" {
			parts = append(parts, a)
		}
		return strings.Join(parts, " "), nil
	case "python", "node":
		rt, err := runtimeBin(cfg, map[string]string{"python": "python3", "node": "node"}[cfg.Type])
		if err != nil {
			return "", err
		}
		parts := []string{shQuote(rt), shQuote(cfg.BinPath)}
		if a := strings.TrimSpace(cfg.Args); a != "" {
			parts = append(parts, a)
		}
		return strings.Join(parts, " "), nil
	default: // native-binary
		cmd := shQuote(cfg.BinPath)
		if a := strings.TrimSpace(cfg.Args); a != "" {
			cmd += " " + a
		}
		return cmd, nil
	}
}

// shQuote 用单引号包裹并转义内嵌单引号,供安全拼入 sh -c(仅用于 agent 控制的可执行/路径)。
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// nohupStart 据 cfg 构造启动规格、落 sidecar,再启动;供部署流水线调用。返回 PID。
func (a *agent) nohupStart(cfg DeployConfig) (string, error) {
	cmdStr, err := nohupCommand(cfg)
	if err != nil {
		return "", err
	}
	wd := cfg.Workdir
	if wd == "" {
		wd = filepath.Dir(cfg.BinPath)
	}
	spec := nohupSpec{Cmd: cmdStr, Workdir: wd, LogPath: a.nohupLogPath(cfg), PidFile: nohupPidFile(cfg), Env: cfg.Env}
	if b, e := json.MarshalIndent(spec, "", "  "); e == nil {
		os.WriteFile(nohupSpecPath(cfg.BinPath), b, 0644) // 启停/状态据此重建
	}
	return nohupLaunch(spec)
}

// nohupStartFromSpec 据已落盘的启动规格拉起(供 lifecycle「启动」等只带 binPath 的无状态请求)。
func nohupStartFromSpec(binPath string) (string, error) {
	b, err := os.ReadFile(nohupSpecPath(binPath))
	if err != nil {
		return "", fmt.Errorf("未找到启动规格 %s(请先部署一次): %w", nohupSpecPath(binPath), err)
	}
	var spec nohupSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		return "", fmt.Errorf("启动规格损坏: %w", err)
	}
	return nohupLaunch(spec)
}

// nohupLaunch 后台启动并把 PID 写入 pidfile,返回 PID。sh 把 nohup 进程放后台、echo $! 落 pid 后即退出,
// 故 Run 返回时 pidfile 已就绪;nohup 进程脱离 SIGHUP,Agent 退出/自更新重启都不影响它。
func nohupLaunch(spec nohupSpec) (string, error) {
	if err := os.MkdirAll(filepath.Dir(spec.LogPath), 0755); err != nil {
		return "", fmt.Errorf("创建日志目录失败: %w", err)
	}
	line := fmt.Sprintf("nohup %s >> %s 2>&1 & echo $! > %s", spec.Cmd, shQuote(spec.LogPath), shQuote(spec.PidFile))
	c := exec.Command("sh", "-c", line)
	c.Dir = spec.Workdir
	c.Env = os.Environ()
	for k, v := range spec.Env {
		c.Env = append(c.Env, k+"="+v)
	}
	if out, err := c.CombinedOutput(); err != nil {
		return "", fmt.Errorf("nohup 启动失败: %s", strings.TrimSpace(string(out)))
	}
	time.Sleep(300 * time.Millisecond) // 给进程一点时间:若立即崩溃,后续 alive 检查能如实反映
	b, _ := os.ReadFile(spec.PidFile)
	return strings.TrimSpace(string(b)), nil
}

// nohupReadPid 读 pidfile 返回 PID(trim);读不到/为空返回 ""。
func nohupReadPid(cfg DeployConfig) string {
	b, err := os.ReadFile(nohupPidFile(cfg))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// nohupAlive 判断 pidfile 记录的进程是否存活(signal 0 探测,不实际投递信号)。
func nohupAlive(cfg DeployConfig) bool { return pidAlive(nohupReadPid(cfg)) }

func pidAlive(pid string) bool {
	pid = strings.TrimSpace(pid)
	if pid == "" || pid == "0" {
		return false
	}
	n, err := strconv.Atoi(pid)
	if err != nil {
		return false
	}
	p, err := os.FindProcess(n)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// nohupStop 停止进程:SIGTERM → 最多等 5s 优雅退出 → 仍在则 SIGKILL;清理 pidfile。
func nohupStop(cfg DeployConfig) {
	pid := strings.TrimSpace(nohupReadPid(cfg))
	defer os.Remove(nohupPidFile(cfg))
	if pid == "" || pid == "0" {
		return
	}
	n, err := strconv.Atoi(pid)
	if err != nil {
		return
	}
	p, err := os.FindProcess(n)
	if err != nil {
		return
	}
	p.Signal(syscall.SIGTERM)
	for i := 0; i < 25; i++ {
		if !pidAlive(pid) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	p.Signal(syscall.SIGKILL)
}

// runDeployNohup nohup runner 的进程类部署闭环:备份 → 停 → 原子替换 → nohup 启动 → 健康检查;失败回滚。
func (a *agent) runDeployNohup(cfg DeployConfig, artifact string, emit func(Step)) DeployResult {
	res := DeployResult{Version: cfg.Version}
	add := func(name string, ok bool, logs ...string) {
		s := Step{Name: name, OK: ok, Logs: logs}
		res.Steps = append(res.Steps, s)
		emit(s)
	}

	archived := scriptArchived(cfg, artifact)

	add("校验制品", true, "sha256 "+short(sha256File(artifact)), "目标 "+cfg.BinPath, "Runner nohup")

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

	nohupStop(cfg)
	add("停止服务", true, "停止旧进程(nohup pidfile)")

	plog, err := a.placeArtifact(cfg, artifact)
	if err != nil {
		add("替换制品", false, err.Error())
		rlog, ok := a.rollbackNohup(cfg, bkDir)
		res.Result = rollbackResult(rlog, ok, add)
		return res
	}
	add("替换制品", true, plog)

	if ran, ilog, ierr := installPyRequirements(cfg); ran {
		if ierr != nil {
			add("安装依赖", false, ilog)
			rlog, ok := a.rollbackNohup(cfg, bkDir)
			res.Result = rollbackResult(rlog, ok, add)
			return res
		}
		add("安装依赖", true, ilog)
	}

	pid, err := a.nohupStart(cfg)
	if err != nil {
		add("启动服务", false, err.Error())
	} else {
		add("启动服务", true, "nohup 托管 · pid "+pid+" · 日志 "+a.nohupLogPath(cfg))
	}
	time.Sleep(time.Second)

	var hlog []string
	if processHealthy(cfg.Health, nohupAlive(cfg), &hlog) {
		add("健康检查", true, hlog...)
		os.WriteFile(verSidecar(cfg.BinPath), []byte(cfg.Version), 0644)
		res.Result = "success"
		return res
	}
	add("健康检查", false, hlog...)

	if bkDir == "" {
		nohupStop(cfg)
		add("回滚", false, "首次部署无备份可回滚,已停止服务")
		res.Result = "failed"
		return res
	}
	rlog, ok := a.rollbackNohup(cfg, bkDir)
	res.Result = rollbackResult(rlog, ok, add)
	return res
}

// rollbackNohup 部署失败后还原旧制品并用 nohup 重新拉起旧版。
func (a *agent) rollbackNohup(cfg DeployConfig, bkDir string) ([]string, bool) {
	if bkDir == "" {
		nohupStop(cfg)
		return []string{"首次部署无备份可回滚,已停止服务"}, false
	}
	rlog := []string{"读取 " + bkDir, "还原备份制品"}
	nohupStop(cfg)
	if err := a.restoreArtifactFrom(cfg, bkDir); err != nil {
		return append(rlog, "还原失败: "+err.Error()), false
	}
	// 连启动规格一起还原(env/args 等),否则旧制品会跑在本次失败部署改过的配置下;有备份规格则据它拉起。
	if sp := filepath.Join(bkDir, "nohup.json"); fileExists(sp) {
		if err := copyFile(sp, nohupSpecPath(cfg.BinPath), 0644); err != nil {
			return append(rlog, "还原启动规格失败: "+err.Error()), false
		}
		rlog = append(rlog, "还原启动规格")
		if _, err := nohupStartFromSpec(cfg.BinPath); err != nil {
			return append(rlog, "重启旧版失败: "+err.Error()), false
		}
	} else if _, err := a.nohupStart(cfg); err != nil {
		return append(rlog, "重启旧版失败: "+err.Error()), false
	}
	time.Sleep(time.Second)
	var rh []string
	ok := processHealthy(cfg.Health, nohupAlive(cfg), &rh)
	rlog = append(rlog, rh...)
	return rlog, ok
}
