package consoleapp

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// failLimiter 是按 key 计失败次数的滑动窗口限流器（进程内；Console 单实例运行）。
// 零值可用。窗口内失败次数达到 max 即锁定 lock 时长；成功可 reset。
// 表项数超过 maxKeys 时先清理过期项，仍超限则丢弃最早的窗口，内存有界。
type failLimiter struct {
	mu      sync.Mutex
	m       map[string]*failEntry
	max     int
	window  time.Duration
	lock    time.Duration
	maxKeys int
}

type failEntry struct {
	count       int
	first       time.Time
	lockedUntil time.Time
}

func newFailLimiter(max int, window, lock time.Duration) *failLimiter {
	return &failLimiter{max: max, window: window, lock: lock, maxKeys: 10000}
}

// blocked 返回该 key 是否处于锁定期及剩余时长。
func (l *failLimiter) blocked(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[key]
	if e == nil || !now.Before(e.lockedUntil) {
		return false, 0
	}
	return true, e.lockedUntil.Sub(now)
}

// fail 记一次失败；返回本次是否触发锁定。
func (l *failLimiter) fail(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]*failEntry{}
	}
	e := l.m[key]
	if e == nil || now.Sub(e.first) > l.window {
		if len(l.m) >= l.maxKeys {
			l.pruneLocked(now)
		}
		e = &failEntry{first: now}
		l.m[key] = e
	}
	e.count++
	if e.count >= l.max && !now.Before(e.lockedUntil) {
		e.lockedUntil = now.Add(l.lock)
		e.count = 0
		e.first = now
		return true
	}
	return false
}

func (l *failLimiter) reset(key string) {
	l.mu.Lock()
	delete(l.m, key)
	l.mu.Unlock()
}

func (l *failLimiter) pruneLocked(now time.Time) {
	for k, e := range l.m {
		if now.Sub(e.first) > l.window && !now.Before(e.lockedUntil) {
			delete(l.m, k)
		}
	}
	for len(l.m) >= l.maxKeys { // 仍超限：丢弃最早的窗口
		var oldestK string
		var oldest time.Time
		for k, e := range l.m {
			if oldestK == "" || e.first.Before(oldest) {
				oldestK, oldest = k, e.first
			}
		}
		delete(l.m, oldestK)
	}
}

// remoteIP 只取 TCP 对端地址（不可伪造）；clientIP 会信任 X-Forwarded-For，仅作标识。
func remoteIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// writeTooMany 回 429 + Retry-After（秒，向上取整）。
func writeTooMany(w http.ResponseWriter, wait time.Duration, msg string) {
	sec := int(wait.Seconds())
	if time.Duration(sec)*time.Second < wait {
		sec++
	}
	if sec < 1 {
		sec = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(sec))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": msg, "code": "TOO_MANY_ATTEMPTS", "retryAfter": sec})
}

// 登录：同一「客户端 IP + 用户名」15 分钟内失败 5 次锁 15 分钟；
// 同一 TCP 来源 15 分钟内失败 100 次锁 15 分钟（防轮换伪造 X-Forwarded-For；经反代时为反代地址，故阈值放宽）。
// 不做「按用户名全局锁定」，否则任何人都能把 admin 锁死。
var (
	loginPairLimiter   = newFailLimiter(5, 15*time.Minute, 15*time.Minute)
	loginRemoteLimiter = newFailLimiter(100, 15*time.Minute, 15*time.Minute)
	// 免登录提取码：同一 TCP 来源 10 分钟内 20 次未命中锁 10 分钟。
	pubCodeLimiter = newFailLimiter(20, 10*time.Minute, 10*time.Minute)
)
