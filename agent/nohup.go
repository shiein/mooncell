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

// nohupCommand 拼要 nohup 起的命令。executable 与每个用户参数都经 shell 引用后再拼入:
// 不同于 systemd(ExecStart 由 systemd 自己分词、不过 shell),nohup 走 `sh -c`,若原样拼接
// jvmArgs/args,串里的 ;|`$() 等会被 shell 解释 → 命令注入。故先用 splitArgs 按 shell 词法切成
// 独立 token,再逐个 shQuote——既保留「按空白/引号分词」的预期,又让元字符成为字面参数(对齐
// systemd 的「分词但不执行」语义,消除注入面)。
func nohupCommand(cfg DeployConfig) (string, error) {
	switch cfg.Type {
	case "java-jar":
		java, err := exec.LookPath("java")
		if err != nil {
			return "", fmt.Errorf("未找到 java(请先安装 JRE): %w", err)
		}
		parts := append([]string{shQuote(java)}, quoteArgs(cfg.JvmArgs)...)
		parts = append(parts, "-jar", shQuote(cfg.BinPath))
		parts = append(parts, quoteArgs(cfg.Args)...)
		return strings.Join(parts, " "), nil
	case "python", "node":
		rt, err := runtimeBin(cfg, map[string]string{"python": "python3", "node": "node"}[cfg.Type])
		if err != nil {
			return "", err
		}
		parts := append([]string{shQuote(rt), shQuote(cfg.BinPath)}, quoteArgs(cfg.Args)...)
		return strings.Join(parts, " "), nil
	default: // native-binary
		parts := append([]string{shQuote(cfg.BinPath)}, quoteArgs(cfg.Args)...)
		return strings.Join(parts, " "), nil
	}
}

// shQuote 用单引号包裹并转义内嵌单引号,供安全拼入 sh -c(仅用于 agent 控制的可执行/路径)。
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// quoteArgs 把用户自由参数串按 shell 词法切成独立 token 后逐个 shQuote,返回可直接拼入 sh -c 的切片。
func quoteArgs(raw string) []string {
	toks := splitArgs(raw)
	out := make([]string, len(toks))
	for i, t := range toks {
		out[i] = shQuote(t)
	}
	return out
}

// splitArgs 按 shell 词法把参数串切成 argv:空白分隔,单/双引号分组(引号本身剥离),
// 反斜杠转义下一字符(单引号内除外)。仅做分词,不做 $ 展开/命令替换——切出的 token 由
// quoteArgs 单引号包裹后才进 shell,故 token 内的元字符一律字面。空串返回 nil。
func splitArgs(s string) []string {
	var args []string
	var cur strings.Builder
	inTok := false
	var quote byte // 0=无, '\'' 或 '"'
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case quote == '"':
			if c == '"' {
				quote = 0
			} else if c == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\') {
				i++
				cur.WriteByte(s[i])
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote = c
			inTok = true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inTok = true
		case c == ' ' || c == '\t' || c == '\n':
			if inTok {
				args = append(args, cur.String())
				cur.Reset()
				inTok = false
			}
		default:
			cur.WriteByte(c)
			inTok = true
		}
	}
	if inTok {
		args = append(args, cur.String())
	}
	return args
}

// nohupStart 据 cfg 构造启动规格、原子落 sidecar,再启动;供部署流水线调用。返回 PID。
// sidecar 写失败必须报错(不能吞):否则部署"成功"但后续启停/回滚无规格可重建命令。
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
	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("序列化启动规格失败: %w", err)
	}
	if err := atomicWrite(nohupSpecPath(cfg.BinPath), b, 0644); err != nil {
		return "", fmt.Errorf("写启动规格失败: %w", err)
	}
	return nohupLaunch(spec)
}

// atomicWrite 同目录临时文件 + rename 原子写,避免半截文件被读到或写失败留下损坏文件。
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// nohupResolvedLogPath 据 binPath 推导实际日志文件:优先 spec.LogPath(部署时已确定,含 fallback),
// 读不到 spec 则退回 <binPath>.nohup.log。供日志流/导出定位真实日志,与启动时写入目标始终一致。
func nohupResolvedLogPath(binPath string) string {
	if b, err := os.ReadFile(nohupSpecPath(binPath)); err == nil {
		var spec nohupSpec
		if json.Unmarshal(b, &spec) == nil && strings.TrimSpace(spec.LogPath) != "" {
			return spec.LogPath
		}
	}
	return binPath + ".nohup.log"
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

// nohupLaunch 后台启动并把运行时状态(pid + starttime)落 pidfile,返回 PID。
// $! 先落临时文件,Go 读出后补 starttime 原子写 pidfile——纯 PID 不足以判活(PID 会被复用)。
// nohup 进程脱离 SIGHUP,Agent 退出/自更新重启都不影响它。
func nohupLaunch(spec nohupSpec) (string, error) {
	if err := os.MkdirAll(filepath.Dir(spec.LogPath), 0755); err != nil {
		return "", fmt.Errorf("创建日志目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(spec.PidFile), ".nohup-pid-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpName)
	line := fmt.Sprintf("nohup %s >> %s 2>&1 & echo $! > %s", spec.Cmd, shQuote(spec.LogPath), shQuote(tmpName))
	c := exec.Command("sh", "-c", line)
	c.Dir = spec.Workdir
	c.Env = os.Environ()
	for k, v := range spec.Env {
		c.Env = append(c.Env, k+"="+v)
	}
	if out, err := c.CombinedOutput(); err != nil {
		return "", fmt.Errorf("nohup 启动失败: %s", strings.TrimSpace(string(out)))
	}
	time.Sleep(300 * time.Millisecond) // 给进程一点时间:若立即崩溃,starttime 读不到、后续判活如实反映
	pb, _ := os.ReadFile(tmpName)
	pidStr := strings.TrimSpace(string(pb))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return "", fmt.Errorf("无法获取启动 PID: %q", pidStr)
	}
	if err := writeNohupState(spec.PidFile, pid); err != nil {
		// pidfile 没落成 → nohupStop 将找不到这个进程而留孤儿(磁盘满/权限变/rename 失败时会发生)。
		// 用刚拿到的 pid 主动收尾(带身份校验,几乎无复用窗口)。
		st := nohupState{Pid: pid, StartTime: procStartTime(strconv.Itoa(pid))}
		terminate(pid, func() bool { return stateAlive(st) })
		return "", fmt.Errorf("写 pidfile 失败,已终止刚启动的进程: %w", err)
	}
	return pidStr, nil
}

// nohupState 是落 pidfile 的运行时状态:PID + 进程身份指纹(starttime),用于识别 PID 复用——
// 进程退出后 PID 可能被系统分给无关进程,光有 PID 会误判存活、停止/下线时误杀无关进程。
type nohupState struct {
	Pid       int    `json:"pid"`
	StartTime string `json:"startTime"` // /proc/<pid>/stat starttime;非 linux 为空(退化为仅存活探测)
}

func writeNohupState(pidFile string, pid int) error {
	st := nohupState{Pid: pid, StartTime: procStartTime(strconv.Itoa(pid))}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return atomicWrite(pidFile, b, 0644)
}

func readNohupState(cfg DeployConfig) (nohupState, bool) {
	b, err := os.ReadFile(nohupPidFile(cfg))
	if err != nil {
		return nohupState{}, false
	}
	var st nohupState
	if json.Unmarshal(b, &st) != nil || st.Pid <= 0 {
		return nohupState{}, false
	}
	return st, true
}

// procStartTime 取 /proc/<pid>/stat 的 starttime(自系统启动以来的时钟滴答),作进程身份指纹。
// 非 linux / 读不到返回 ""(此时身份校验退化为仅存活探测)。
func procStartTime(pid string) string {
	if f := procStatFields(pid); len(f) >= 20 {
		return f[19]
	}
	return ""
}

// pidRunning 用 signal 0 探测 PID 是否存活(不实际投递信号)。
func pidRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// stateAlive 判定记录的进程是否"还是它":存活 + 身份匹配。记录了 starttime 且当前可读时才比对
// (防 PID 复用);非 linux / 读不到 starttime 则退化为仅存活探测。
func stateAlive(st nohupState) bool {
	if !pidRunning(st.Pid) {
		return false
	}
	if st.StartTime != "" {
		if cur := procStartTime(strconv.Itoa(st.Pid)); cur != "" && cur != st.StartTime {
			return false // 同 PID 但 starttime 不同 = PID 已被复用为无关进程
		}
	}
	return true
}

// nohupAlive 据 pidfile 判断本应用进程是否在运行(含 PID 复用身份校验)。
func nohupAlive(cfg DeployConfig) bool {
	st, ok := readNohupState(cfg)
	return ok && stateAlive(st)
}

// terminate 优雅终止 pid:SIGTERM → 最多等 5s → 仍在则 SIGKILL。每步都用 alive() 判定,
// alive 必须带身份校验(stateAlive)——否则等待期间原进程退出、PID 被复用,会误杀新占用者。
func terminate(pid int, alive func() bool) {
	if !alive() {
		return
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	p.Signal(syscall.SIGTERM)
	for i := 0; i < 25; i++ {
		if !alive() {
			return // 原进程已退出(或 PID 已被复用为别的进程):停止等待,绝不强杀新占用者
		}
		time.Sleep(200 * time.Millisecond)
	}
	if alive() {
		p.Signal(syscall.SIGKILL)
	}
}

// nohupStop 停止进程:身份校验通过才发信号;无记录 / 已退出 / PID 复用(stale)一律只清 pidfile,
// 绝不对不属于自己的 PID 发信号。
func nohupStop(cfg DeployConfig) {
	defer os.Remove(nohupPidFile(cfg))
	if st, ok := readNohupState(cfg); ok {
		terminate(st.Pid, func() bool { return stateAlive(st) })
	}
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
		rlog, ok := a.rollbackNohup(cfg, bkDir)
		res.Result = rollbackResult(rlog, ok, add)
		return res
	}
	add("启动服务", true, "nohup 托管 · pid "+pid+" · 日志 "+a.nohupLogPath(cfg))
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
	} else {
		// 备份里没有启动规格(旧版用别的 runner 部署、或规格被手工删除):无从还原旧配置,
		// 只能用本次(失败)部署的 cfg 拉起旧制品——env/args 可能与旧版不一致,据实告警让运维核对。
		rlog = append(rlog, "⚠ 备份缺启动规格,用当前部署配置拉起旧制品(env/args 可能与旧版不符,请人工核对)")
		if _, err := a.nohupStart(cfg); err != nil {
			return append(rlog, "重启旧版失败: "+err.Error()), false
		}
	}
	time.Sleep(time.Second)
	var rh []string
	ok := processHealthy(cfg.Health, nohupAlive(cfg), &rh)
	rlog = append(rlog, rh...)
	return rlog, ok
}
