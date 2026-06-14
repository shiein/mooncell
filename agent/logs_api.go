package main

import (
	"bufio"
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
	if r.URL.Query().Get("runner") == "pm2" {
		// pm2 应用日志走 pm2 logs --raw(裸消息);parseJournalLine 对非 JSON 行回退为纯文本。
		cmd = exec.CommandContext(r.Context(), "pm2", "logs", unitName(id), "--lines", tail, "--raw")
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
	level := priorityLevel(j.Priority)
	// 应用经 stdout 的日志常被 journald 统一标为 info,关键字更贴近用户语义,优先级更高。
	switch up := strings.ToUpper(text); {
	case strings.Contains(up, "ERROR"), strings.Contains(up, "FATAL"), strings.Contains(up, "PANIC"):
		level = "error"
	case strings.Contains(up, "WARN"):
		level = "warn"
	}
	return map[string]any{"ts": ts, "level": level, "text": text}
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
