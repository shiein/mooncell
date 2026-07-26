// PTY 与 WebSocket 桥接。
// binary frame = PTY 原始字节（含 ZMODEM）；text frame = 控制 JSON。
package serverops

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

// TerminalWS GET /api/server-resources/{id}/sessions/{sid}/terminal
func (s *Service) TerminalWS(w http.ResponseWriter, r *http.Request) {
	user, _, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "未登录", false)
		return
	}
	resourceID := r.PathValue("id")
	sessionID := r.PathValue("sid")

	if !originOK(r) {
		writeErr(w, http.StatusForbidden, CodeForbidden, "Origin 不允许", false)
		return
	}

	sess := s.sess.Get(sessionID, user, resourceID)
	if sess == nil {
		writeErr(w, http.StatusNotFound, CodeSessionClosed, "会话不存在或已结束", false)
		return
	}
	sess.Touch()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
		// 仅放行 originOK 已校验的精确 Origin，禁止后缀匹配。
		OriginPatterns: originPatterns(r),
	})
	if err != nil {
		return
	}
	// WebSocket 已 hijack，不继续依赖 request context；用专用 context 统一中断读写。
	ctx, cancelWS := context.WithCancel(context.Background())
	defer cancelWS()
	// WS 结束后必须释放 PTY；若客户端已走，关闭整会话避免占限额（SFTP 同会话一并结束）。
	// 浏览器正常卸载会再调 DELETE session；此处保证异常断线也能回收。
	defer func() {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()

	client := sess.Client()
	if client == nil {
		_ = writeWSJSON(r.Context(), conn, map[string]any{"type": "error", "code": CodeSessionClosed, "message": "会话已结束"})
		return
	}

	sshSess, err := client.NewSession()
	if err != nil {
		_ = writeWSJSON(r.Context(), conn, map[string]any{"type": "error", "code": CodeSSHConnectionFailed, "message": "创建 SSH 会话失败"})
		return
	}
	sess.mu.Lock()
	// 同一 session 只允许一个 PTY；替换旧的。
	if sess.sshSession != nil {
		_ = sess.sshSession.Close()
	}
	sess.sshSession = sshSess
	sess.mu.Unlock()

	// 远端 shell 退出或 WS 结束时的统一清理。
	ptyDone := make(chan struct{})
	var closeOnce sync.Once
	finish := func(closeFullSession bool) {
		closeOnce.Do(func() {
			sess.mu.Lock()
			if sess.sshSession == sshSess {
				sess.sshSession = nil
			}
			sess.mu.Unlock()
			_ = sshSess.Close()
			close(ptyDone)
			if closeFullSession && !sess.closed.Load() {
				// 终端是工作台主通道：WS 异常结束后回收整个 SSH，防止僵尸占限额。
				// 用户若仍需 SFTP，须保持终端 WS 或重新创建 session。
				sess.Close()
			}
			// 远端 exit、撤权、idle/绝对超时都必须立即打断 conn.Read。
			cancelWS()
		})
	}
	defer finish(true)

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	cols, rows := sess.InitialCols, sess.InitialRows
	if cols <= 0 {
		cols = 120
	}
	if rows <= 0 {
		rows = 36
	}
	if err := sshSess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		_ = writeWSJSON(r.Context(), conn, map[string]any{"type": "error", "code": CodeSSHConnectionFailed, "message": "申请 PTY 失败"})
		return
	}

	stdout, err := sshSess.StdoutPipe()
	if err != nil {
		_ = writeWSJSON(r.Context(), conn, map[string]any{"type": "error", "code": CodeSSHConnectionFailed, "message": "打开终端输出失败"})
		return
	}
	stderr, err := sshSess.StderrPipe()
	if err != nil {
		_ = writeWSJSON(r.Context(), conn, map[string]any{"type": "error", "code": CodeSSHConnectionFailed, "message": "打开终端错误流失败"})
		return
	}
	stdin, err := sshSess.StdinPipe()
	if err != nil {
		_ = writeWSJSON(r.Context(), conn, map[string]any{"type": "error", "code": CodeSSHConnectionFailed, "message": "打开终端输入失败"})
		return
	}
	if err := sshSess.Shell(); err != nil {
		_ = writeWSJSON(r.Context(), conn, map[string]any{"type": "error", "code": CodeSSHConnectionFailed, "message": "启动 Shell 失败"})
		return
	}

	// 监听远端 shell 退出，及时通知浏览器并回收。
	go func() {
		_ = sshSess.Wait()
		finish(true)
	}()

	_ = writeWSJSON(ctx, conn, map[string]any{"type": "ready"})

	// 有界输出队列：慢客户端丢弃新输出并提示，不直接杀 SSH（cat 大文件在慢链路上很常见）。
	// 注意：只有写协程消费 outCh，pump 不得从 channel 偷读。
	type chunk struct{ b []byte }
	outCh := make(chan chunk, 64)
	var (
		outMu       sync.Mutex
		outBytes    int
		needDropNote bool
	)
	tryEnqueue := func(data []byte) {
		outMu.Lock()
		defer outMu.Unlock()
		if outBytes+len(data) > maxPTYOutputQueue || len(outCh) >= cap(outCh) {
			needDropNote = true
			return
		}
		cp := make([]byte, len(data))
		copy(cp, data)
		outBytes += len(cp)
		select {
		case outCh <- chunk{cp}:
		default:
			outBytes -= len(cp)
			needDropNote = true
		}
	}

	var pumps sync.WaitGroup
	pump := func(rd io.Reader) {
		defer pumps.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := rd.Read(buf)
			if n > 0 {
				tryEnqueue(buf[:n])
				outMu.Lock()
				note := needDropNote
				if note {
					needDropNote = false
				}
				outMu.Unlock()
				if note {
					tryEnqueue([]byte("\r\n\x1b[33m[Mooncell] 输出过快，已丢弃部分终端输出\x1b[0m\r\n"))
				}
			}
			if err != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-sess.ctx.Done():
				return
			case <-ptyDone:
				return
			default:
			}
		}
	}
	pumps.Add(2)
	go pump(stdout)
	go pump(stderr)
	go func() {
		pumps.Wait()
		close(outCh)
	}()

	// Mooncell 会话校验 + SSH keepalive
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		missed := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-sess.ctx.Done():
				return
			case <-ptyDone:
				return
			case <-t.C:
				if s.valid != nil && !s.valid(user) {
					_ = writeWSJSON(ctx, conn, map[string]any{"type": "error", "code": CodeMooncellSessionExp, "message": "登录已过期"})
					finish(true)
					return
				}
				if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
					missed++
					if missed >= defaultKeepaliveMissed {
						finish(true)
						return
					}
				} else {
					missed = 0
				}
			}
		}
	}()

	// 写协程：所有 WS 写串行
	var writeMu sync.Mutex
	writeBin := func(data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return conn.Write(wctx, websocket.MessageBinary, data)
	}
	writeTxt := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return writeWSJSONLocked(ctx, conn, v)
	}

	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		for {
			select {
			case <-ctx.Done():
				return
			case <-sess.ctx.Done():
				return
			case <-ptyDone:
				return
			case c, ok := <-outCh:
				if !ok {
					return
				}
				outMu.Lock()
				outBytes -= len(c.b)
				if outBytes < 0 {
					outBytes = 0
				}
				outMu.Unlock()
				if err := writeBin(c.b); err != nil {
					return
				}
			}
		}
	}()

	// 读循环：用户输入 / 控制消息
readLoop:
	for {
		msgType, data, err := conn.Read(ctx)
		if err != nil {
			break
		}
		select {
		case <-ptyDone:
			break readLoop
		default:
		}
		if msgType == websocket.MessageBinary {
			if len(data) > maxWSBinaryFrame {
				_ = writeTxt(map[string]any{"type": "error", "code": CodeBadRequest, "message": "单帧过大"})
				break
			}
			if _, err := stdin.Write(data); err != nil {
				break
			}
			sess.Touch()
			if s.touch != nil {
				s.touch(user)
			}
			continue
		}
		var ctrl struct {
			Type string `json:"type"`
			Cols int    `json:"cols"`
			Rows int    `json:"rows"`
		}
		if err := json.Unmarshal(data, &ctrl); err != nil {
			continue
		}
		switch ctrl.Type {
		case "resize":
			if ctrl.Cols > 0 && ctrl.Rows > 0 {
				_ = sshSess.WindowChange(ctrl.Rows, ctrl.Cols)
				sess.Touch()
				if s.touch != nil {
					s.touch(user)
				}
			}
		case "ping":
			_ = writeTxt(map[string]any{"type": "pong"})
		case "close":
			_ = writeTxt(map[string]any{"type": "exit", "code": 0})
			finish(true)
			return
		}
	}

	_ = stdin.Close()
	select {
	case <-writeDone:
	case <-time.After(2 * time.Second):
	}
	_ = writeTxt(map[string]any{"type": "exit", "code": 0})
	log.Printf("[serverops] terminal closed user=%s resource=%s", user, resourceID)
	// defer finish(true) 回收 SSH
}

func writeWSJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}

func writeWSJSONLocked(ctx context.Context, conn *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}
