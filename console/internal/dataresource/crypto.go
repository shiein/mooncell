// 凭据保护：AES-256-GCM 加解密外部数据库密码，密钥独立文件存储。
//
// 安全约束（见设计文档第二节「凭据保护」）：
//   - 密钥文件默认 mooncell-data.key，权限 0600，与 mooncell.db 分开备份。
//   - 首次启动且尚无数据资源时自动生成 32 字节随机密钥。
//   - 已存在资源但密钥丢失时拒绝启动，不生成新密钥伪装成功。
//   - 日志、错误响应中不得出现密码或完整 DSN。
package dataresource

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// cipherV1 是当前加密格式版本号。密文结构：base64(version(1) | nonce(12) | ciphertext)。
const cipherV1 byte = 1

// nonceLen 是 GCM 标准 nonce 长度。
const nonceLen = 12

// keyLen 是 AES-256 密钥长度（字节）。
const keyLen = 32

// CredentialKey 管理数据资源凭据加密密钥的加载与生成。
type CredentialKey struct {
	key []byte
}

// LoadOrCreateCredentialKey 按「首次无资源则生成、已有资源则必须存在」的规则处理密钥文件。
//
// hasExistingResources：当前是否已有数据资源记录（调用方查 data_resources 表）。
//   - false 且文件不存在：生成新密钥并写入 0600 文件。
//   - true  且文件不存在：返回错误，拒绝启动（不生成新密钥伪装成功）。
//   - 文件存在：加载并校验长度。
func LoadOrCreateCredentialKey(path string, hasExistingResources bool) (*CredentialKey, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) != keyLen {
			return nil, fmt.Errorf("密钥文件 %s 长度 %d 不符合 AES-256 要求(%d 字节)", path, len(data), keyLen)
		}
		return &CredentialKey{key: data}, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("读取密钥文件 %s 失败: %w", path, err)
	}
	// 文件不存在
	if hasExistingResources {
		return nil, fmt.Errorf("已存在数据资源但密钥文件 %s 丢失,拒绝启动(不生成新密钥以避免凭据不可解)", path)
	}
	// 首次使用：生成密钥
	key := make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("生成密钥失败: %w", err)
	}
	if err := os.WriteFile(path, key, 0600); err != nil {
		return nil, fmt.Errorf("写入密钥文件 %s 失败: %w", path, err)
	}
	return &CredentialKey{key: key}, nil
}

// Encrypt 加密明文密码，返回 base64 编码的密文（含版本号和 nonce）。
func (k *CredentialKey) Encrypt(plaintext string) (string, error) {
	block, err := aes.NewCipher(k.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	// 带版本号前缀，便于将来升级密文格式。
	out := make([]byte, 0, 1+nonceLen+len(plaintext)+gcm.Overhead())
	out = append(out, cipherV1)
	out = append(out, nonce...)
	out = gcm.Seal(out, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Decrypt 解密 Encrypt 产生的密文。
func (k *CredentialKey) Decrypt(cipherB64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(cipherB64)
	if err != nil {
		return "", fmt.Errorf("密文 base64 解码失败: %w", err)
	}
	if len(raw) < 1+nonceLen {
		return "", errors.New("密文过短")
	}
	if raw[0] != cipherV1 {
		return "", fmt.Errorf("不支持的密文版本: %d", raw[0])
	}
	nonce := raw[1 : 1+nonceLen]
	ciphertext := raw[1+nonceLen:]
	block, err := aes.NewCipher(k.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("密文解密失败: %w", err)
	}
	return string(plain), nil
}
