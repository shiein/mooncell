package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// logStream 处理 GET /api/apps/{id}/logs/stream?tail=N&runner=<systemd|pm2>(SSE):
// 跟随该应用的运行时日志,先吐最近 tail 行再实时跟随。systemd 走 journald(-o json),
// pm2 走 `pm2 logs --raw`。每行推 `event: line`;客户端断开时 r.Context() 取消,杀掉子进程。
func (a *agent) logStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !requireValidID(w, id) {
		return
	}
	tail := r.URL.Query().Get("tail")
	if _, err := strconv.Atoi(tail); err != nil || tail == "" {
		tail = "200"
	}

	sse, ok := sseHeader(w)
	if !ok {
		return
	}

	// exec.Command 数组传参不经 shell,id 来自单段路径,无注入面。
	var cmd *exec.Cmd
	if r.URL.Query().Get("runner") == "nohup" {
		// nohup 无 journal/pm2 日志,直接 tail -F 重定向的日志文件;path 须在 log_roots 白名单内。
		path := r.URL.Query().Get("path")
		if !withinRoots(path, a.cfg.Paths.LogRoots) {
			sse("line", map[string]any{"ts": time.Now().UnixMilli(), "level": "error", "text": "日志路径不在 log_roots 白名单内: " + path})
			return
		}
		cmd = exec.CommandContext(r.Context(), "tail", "-n", tail, "-F", path)
	} else if r.URL.Query().Get("runner") == "pm2" {
		// pm2 应用日志走 pm2 logs --raw(裸消息);parseJournalLine 对非 JSON 行回退为纯文本。
		// 接管模式定位用户进程名(pm2NameReq),否则托管名 deploy-<id>。
		cmd = exec.CommandContext(r.Context(), "pm2", "logs", pm2NameReq(r, id), "--lines", tail, "--raw")
	} else {
		// journalctl -o json:逐行结构化,时间/优先级/消息精确可取,免去文本格式与时区的脆弱解析。
		cmd = exec.CommandContext(r.Context(), "journalctl",
			"-u", unitName(id), "-n", tail, "-f", "-o", "json", "--no-pager")
	}
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw // journalctl 的错误(无 unit / 权限)也透给前端,便于定位
	if err := cmd.Start(); err != nil {
		sse("line", map[string]any{"ts": time.Now().UnixMilli(), "level": "error", "text": "启动 journalctl 失败: " + err.Error()})
		return
	}
	go func() { cmd.Wait(); pw.Close() }() // 进程退出(含被 ctx 杀)后关闭管道,让 Scan 收尾

	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 放宽单行上限,防超长日志行截断报错
	for sc.Scan() {
		sse("line", parseJournalLine(sc.Text()))
	}
}

// parseJournalLine 把一行 journald JSON 解析为 {ts,level,text}:
// __REALTIME_TIMESTAMP(微秒)→ 毫秒;MESSAGE 为裸消息;level 以 syslog PRIORITY 为主、关键字兜底。
// 非 JSON 行(如 journalctl 报错文本)整体作消息、按当前时刻标记。
func parseJournalLine(raw string) map[string]any {
	var j struct {
		Message  string `json:"MESSAGE"`
		Realtime string `json:"__REALTIME_TIMESTAMP"`
		Priority string `json:"PRIORITY"`
	}
	ts := time.Now().UnixMilli()
	text := raw
	if err := json.Unmarshal([]byte(raw), &j); err == nil {
		if j.Message != "" {
			text = j.Message
		}
		if us, e := strconv.ParseInt(j.Realtime, 10, 64); e == nil && us > 0 {
			ts = us / 1000
		}
	}
	return map[string]any{"ts": ts, "level": lineLevel(text, priorityLevel(j.Priority)), "text": text}
}

// lineLevel 据消息关键字判定日志级别(应用经 stdout 的日志常被统一标为 info,关键字更贴近用户语义)。
// base 为已知的基础级别(如 syslog PRIORITY 推断),无则传 "info"。
func lineLevel(text, base string) string {
	if base == "" {
		base = "info"
	}
	switch up := strings.ToUpper(text); {
	case strings.Contains(up, "ERROR"), strings.Contains(up, "FATAL"), strings.Contains(up, "PANIC"):
		return "error"
	case strings.Contains(up, "WARN"):
		return "warn"
	}
	return base
}

// logDownload 处理 GET /api/apps/{id}/logs/download?since=&until=&runner= :
// 按时间范围导出该应用日志(systemd journal / pm2),gzip 打包为 attachment 下载。
func (a *agent) logDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !requireValidID(w, id) {
		return
	}
	q := r.URL.Query()
	fname := unitName(id) + "-logs-" + time.Now().Format("20060102_150405") + ".log.gz"
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "attachment; filename="+fname)
	gz := gzip.NewWriter(w)
	defer gz.Close()

	var cmd *exec.Cmd
	if q.Get("runner") == "nohup" {
		// nohup:导出重定向日志文件末尾若干行(纯文件无时间范围检索,since/until 不适用)。path 须在白名单内。
		path := q.Get("path")
		if !withinRoots(path, a.cfg.Paths.LogRoots) {
			return // 头已写,无法回 4xx;直接结束(导出空)
		}
		cmd = exec.CommandContext(r.Context(), "tail", "-n", "20000", path)
	} else if q.Get("runner") == "pm2" {
		cmd = exec.CommandContext(r.Context(), "pm2", "logs", pm2NameReq(r, id), "--lines", "20000", "--nostream", "--raw")
	} else {
		args := []string{"-u", unitName(id), "-o", "short-iso", "--no-pager"}
		// since/until 必须是纯数字 unix 秒,防注入。
		if s := q.Get("since"); isUnixSec(s) {
			args = append(args, "--since", "@"+s)
		}
		if u := q.Get("until"); isUnixSec(u) {
			args = append(args, "--until", "@"+u)
		}
		cmd = exec.CommandContext(r.Context(), "journalctl", args...)
	}
	cmd.Stdout = gz
	cmd.Run()
}

func isUnixSec(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.ParseInt(s, 10, 64)
	return err == nil
}

// logFileStream 处理 GET /api/apps/{id}/logs/file/stream?path=&tail= :
// tail -F 跟随声明的日志文件。path 必须在 log_roots 白名单内(防穿越/越权读任意文件)。
func (a *agent) logFileStream(w http.ResponseWriter, r *http.Request) {
	if !requireValidID(w, r.PathValue("id")) {
		return
	}
	path := r.URL.Query().Get("path")
	if !withinRoots(path, a.cfg.Paths.LogRoots) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "日志路径不在 log_roots 白名单内: " + path})
		return
	}
	tail := r.URL.Query().Get("tail")
	if _, err := strconv.Atoi(tail); err != nil || tail == "" {
		tail = "200"
	}
	sse, ok := sseHeader(w)
	if !ok {
		return
	}
	cmd := exec.CommandContext(r.Context(), "tail", "-n", tail, "-F", path)
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		sse("line", map[string]any{"ts": time.Now().UnixMilli(), "level": "error", "text": "tail 启动失败: " + err.Error()})
		return
	}
	go func() { cmd.Wait(); pw.Close() }()
	scn := bufio.NewScanner(pr)
	scn.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scn.Scan() {
		line := scn.Text()
		sse("line", map[string]any{"ts": time.Now().UnixMilli(), "level": lineLevel(line, "info"), "text": line})
	}
}

// priorityLevel 把 syslog 优先级(0~7)归并到三档:0-3 错误、4 告警、其余信息。
func priorityLevel(p string) string {
	switch p {
	case "0", "1", "2", "3":
		return "error"
	case "4":
		return "warn"
	default:
		return "info"
	}
}
