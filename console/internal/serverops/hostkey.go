// SSH host key 探测、确认与比对。
// 普通用户不得对未知主机自行「忽略并继续」；仅 admin 可探测并确认。
package serverops

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// ProbeHostKey 仅做 TCP 连接 + SSH 密钥交换以捕获服务端 host key，无需密码。
// 使用固定拒绝回调拿到 key 后返回，连接随即关闭。
func ProbeHostKey(host string, port int, timeout time.Duration) (*HostKeyProbeResult, error) {
	if err := validateHost(host); err != nil {
		return nil, err
	}
	if port < 1 || port > 65535 {
		return nil, apiErr(CodeValidation, "端口必须在 1–65535", false)
	}
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	var captured ssh.PublicKey
	cfg := &ssh.ClientConfig{
		// 探测阶段用户名任意；部分服务器要求非空。
		User: "mooncell-probe",
		// 不提供认证方法：只关心 host key 交换。
		Auth: []ssh.AuthMethod{},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			captured = key
			// 返回错误以中止握手；key 已捕获。
			return fmt.Errorf("mooncell: host key captured")
		},
		Timeout:         timeout,
		HostKeyAlgorithms: nil, // 使用 x/crypto/ssh 安全默认
	}

	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, apiErr(CodeSSHConnectTimeout, "连接目标主机超时或不可达", true)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if c != nil {
		_ = c.Close()
	}
	if chans != nil {
		go ssh.DiscardRequests(reqs)
	}
	// 期望因 HostKeyCallback 错误而失败；若 captured 非空即成功。
	if captured == nil {
		if err != nil {
			// 可能是真正的连接/协议失败。
			return nil, apiErr(CodeSSHConnectionFailed, "SSH 握手失败，无法获取主机指纹", true)
		}
		return nil, apiErr(CodeSSHConnectionFailed, "未能获取主机指纹", true)
	}

	return &HostKeyProbeResult{
		Host:      host,
		Port:      port,
		Algorithm: captured.Type(),
		SHA256:    fingerprintSHA256(captured),
	}, nil
}

// fingerprintSHA256 返回 OpenSSH 风格 "SHA256:..." 指纹。
func fingerprintSHA256(key ssh.PublicKey) string {
	sum := sha256.Sum256(key.Marshal())
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// fixedHostKeyCallback 在连接时强制比对已确认指纹；不一致返回 HOST_KEY_MISMATCH 语义。
func fixedHostKeyCallback(expectedAlgo, expectedSHA256 string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if expectedSHA256 == "" {
			return fmt.Errorf("%s", CodeHostKeyUnconfirmed)
		}
		got := fingerprintSHA256(key)
		if got != expectedSHA256 {
			return fmt.Errorf("%s", CodeHostKeyMismatch)
		}
		// 算法不一致也拒绝（密钥轮换后应重新确认）。
		if expectedAlgo != "" && key.Type() != expectedAlgo {
			return fmt.Errorf("%s", CodeHostKeyMismatch)
		}
		return nil
	}
}
