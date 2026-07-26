// SSH 握手、密码认证与超时。
// 密码仅用于本次认证，不持久化、不进入日志。
package serverops

import (
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// dialSSH 使用密码认证连接目标，并强制 host key 指纹校验。
// password 在返回后由调用方负责不再引用；本函数不保存副本。
func dialSSH(host string, port int, username, password string, hostKeyAlgo, hostKeySHA256 string, timeout time.Duration) (*ssh.Client, error) {
	if hostKeySHA256 == "" {
		return nil, apiErr(CodeHostKeyUnconfirmed, "主机指纹未确认，无法连接", false)
	}
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	cfg := &ssh.ClientConfig{
		User: username,
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
		},
		HostKeyCallback:   fixedHostKeyCallback(hostKeyAlgo, hostKeySHA256),
		Timeout:           timeout,
		HostKeyAlgorithms: nil, // 安全默认，不静默启用弱算法
	}

	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, classifySSHDialError(err)
	}
	return client, nil
}

func classifySSHDialError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	// host key
	if containsCode(msg, CodeHostKeyMismatch) {
		return apiErr(CodeHostKeyMismatch, "远端主机指纹与已确认值不一致", false)
	}
	if containsCode(msg, CodeHostKeyUnconfirmed) {
		return apiErr(CodeHostKeyUnconfirmed, "主机指纹未确认，无法连接", false)
	}
	// 认证失败：x/crypto 通常含 "unable to authenticate"
	if containsFold(msg, "unable to authenticate") || containsFold(msg, "permission denied") {
		// 必须 422，不能 401，避免触发 Mooncell 全局退出登录。
		return apiErr(CodeSSHAuthFailed, "SSH 用户名或密码错误", true)
	}
	if containsFold(msg, "timeout") || containsFold(msg, "i/o timeout") || containsFold(msg, "deadline exceeded") {
		return apiErr(CodeSSHConnectTimeout, "连接或握手超时", true)
	}
	return apiErr(CodeSSHConnectionFailed, "SSH 连接失败", true)
}

func containsCode(s, code string) bool {
	return len(s) >= len(code) && (s == code || containsFold(s, code))
}

func containsFold(s, sub string) bool {
	// 小写简单包含，避免引入 strings 额外依赖风格；用标准库。
	return len(sub) == 0 || (len(s) >= len(sub) && indexFold(s, sub) >= 0)
}

func indexFold(s, sub string) int {
	// 简易 ASCII 不区分大小写搜索，足够匹配 SSH 错误串。
	ls, lsub := len(s), len(sub)
	if lsub == 0 {
		return 0
	}
	for i := 0; i+lsub <= ls; i++ {
		ok := true
		for j := 0; j < lsub; j++ {
			a, b := s[i+j], sub[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}
