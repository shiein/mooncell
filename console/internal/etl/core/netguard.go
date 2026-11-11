package core

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// httpAllowLoopbackKey 是 Env 中「允许 HTTP 步骤访问回环地址」的键。
type httpAllowLoopbackKey struct{}

// HTTPAllowLoopbackEntry 返回注入 Env 的键值；由服务层按
// [etl] http_allow_loopback 配置注入，默认 false。
func HTTPAllowLoopbackEntry(allow bool) (any, any) { return httpAllowLoopbackKey{}, allow }

// HTTPAllowLoopbackFrom 从 Env 读取回环放行开关；未注入按 false。
func HTTPAllowLoopbackFrom(env Env) bool {
	if env == nil {
		return false
	}
	b, _ := env.Lookup(httpAllowLoopbackKey{}).(bool)
	return b
}

// blockedIP 判定 SSRF 目标是否禁用：回环、链路本地、未指定（0.0.0.0/::）。
// 私有网段放行——离线内网交付需要调内网接口。
func blockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// GuardedTransport 返回克隆自 http.DefaultTransport 的传输层：
// DialContext 先解析主机再逐个校验 IP，并复查最终连接的对端地址，
// 防 DNS 重绑定；每次重定向都是新请求→新拨号，跳转目标同样受检。
func GuardedTransport(env Env) *http.Transport {
	allow := HTTPAllowLoopbackFrom(env)
	tr, _ := http.DefaultTransport.(*http.Transport)
	clone := tr.Clone()
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	clone.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if !allow {
			ips, err := dialer.Resolver.LookupIP(ctx, "ip", host)
			if err != nil {
				return nil, fmt.Errorf("HTTP 目标主机解析失败（%s）：%w", host, err)
			}
			for _, ip := range ips {
				if blockedIP(ip) {
					return nil, fmt.Errorf("HTTP 目标 %s 解析到禁用地址 %s（回环/链路本地/未指定地址默认拒绝，可用 etl.http_allow_loopback 放开）", host, ip)
				}
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("HTTP 目标主机 %s 无可用地址", host)
			}
		}
		conn, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if !allow {
			if ip, ok := conn.RemoteAddr().(*net.TCPAddr); ok && blockedIP(ip.IP) {
				conn.Close()
				return nil, fmt.Errorf("HTTP 目标 %s 连接落到禁用地址 %s", host, ip.IP)
			}
		}
		return conn, nil
	}
	return clone
}

// GuardedHTTPClient 返回带 SSRF 防护的 HTTP 客户端。
func GuardedHTTPClient(env Env, timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: GuardedTransport(env)}
}
