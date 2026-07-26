// SSH 握手、密码认证与超时。
// 密码仅用于本次认证，不持久化、不进入日志。
package serverops

import (
	"fmt"
	"net"
	"strings"
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
	if strings.Contains(msg, CodeHostKeyMismatch) {
		return apiErr(CodeHostKeyMismatch, "远端主机指纹与已确认值不一致", false)
	}
	if strings.Contains(msg, CodeHostKeyUnconfirmed) {
		return apiErr(CodeHostKeyUnconfirmed, "主机指纹未确认，无法连接", false)
	}
	low := strings.ToLower(msg)
	if strings.Contains(low, "unable to authenticate") || strings.Contains(low, "permission denied") {
		return apiErr(CodeSSHAuthFailed, "SSH 用户名或密码错误", true)
	}
	if strings.Contains(low, "timeout") || strings.Contains(low, "i/o timeout") || strings.Contains(low, "deadline exceeded") {
		return apiErr(CodeSSHConnectTimeout, "连接或握手超时", true)
	}
	return apiErr(CodeSSHConnectionFailed, "SSH 连接失败", true)
}
