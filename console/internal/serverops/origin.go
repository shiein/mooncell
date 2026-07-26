// WebSocket Origin 同源校验：精确比较 host+port，禁止后缀/子串绕过。
package serverops

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// originOK 校验 Origin 与请求 Host 精确同源。
// 空 Origin（非浏览器/脚本）放行；有 Origin 时必须 scheme 合理且 host:port 完全一致。
// 不信任 X-Forwarded-*：代理终止 TLS 后应改写 Host，或由外层网关保证。
func originOK(r *http.Request) bool {
	o := strings.TrimSpace(r.Header.Get("Origin"))
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	oHost, oPort := splitHostPortDefault(u.Host, scheme)
	rHost, rPort := splitHostPortDefault(r.Host, scheme)
	if oHost == "" || rHost == "" {
		return false
	}
	// 主机名大小写不敏感；端口必须一致。
	return strings.EqualFold(oHost, rHost) && oPort == rPort
}

// originPatterns 供 coder/websocket 二次校验：仅放行已通过 originOK 的精确 Origin。
func originPatterns(r *http.Request) []string {
	o := strings.TrimSpace(r.Header.Get("Origin"))
	if o == "" {
		// 无 Origin 时 Accept 侧用通配；业务侧 originOK 已放行非浏览器。
		return []string{"*"}
	}
	return []string{o}
}

func splitHostPortDefault(hostport, scheme string) (host, port string) {
	hostport = strings.TrimSpace(hostport)
	if hostport == "" {
		return "", ""
	}
	// IPv6 字面量可能已带 []。
	h, p, err := net.SplitHostPort(hostport)
	if err == nil {
		return h, p
	}
	// 无端口：按 scheme 补默认端口，避免 "example.com" 与 "example.com:443" 比较失败，
	// 也避免用 HasSuffix 把 evil.example.com 误判为同源。
	host = hostport
	// 去掉误加的方括号（非 JoinHostPort 场景）
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	switch strings.ToLower(scheme) {
	case "https":
		port = "443"
	default:
		port = "80"
	}
	return host, port
}
