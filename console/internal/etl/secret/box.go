// Package secret 用 AES-256-GCM 加密数据集成连接密码。
//
//   - 密钥来自独立文件（32 字节随机，0600），首次启动自动生成；
//   - 密文格式 "v1:" + base64(nonce ‖ ciphertext)，AAD 绑定记录 ID，防止密文被挪用到别的连接；
//   - 密钥丢失 → 解密失败（fail-closed），绝不回退明文。
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const prefix = "v1:"

// ErrUndecryptable 表示密文无法用当前密钥解开（密钥丢失/更换或密文被篡改）。
var ErrUndecryptable = errors.New("凭据无法解密：密钥文件已更换或丢失，请重新录入密码")

// Box 是加解密器。并发安全。
type Box struct {
	aead cipher.AEAD
}

// New 用 32 字节密钥构造。
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("密钥长度必须为 32 字节，实际 %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// LoadOrCreate 读取密钥文件；不存在时生成（0600）。created 报告本次是否新建。
func LoadOrCreate(path string) (box *Box, created bool, err error) {
	key, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, false, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, false, err
		}
		// O_EXCL：并发启动时不覆盖对方刚生成的密钥
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				return LoadOrCreate(path)
			}
			return nil, false, err
		}
		if _, err := f.Write(key); err != nil {
			f.Close()
			return nil, false, err
		}
		if err := f.Close(); err != nil {
			return nil, false, err
		}
		created = true
	} else if err != nil {
		return nil, false, err
	}
	box, err = New(key)
	return box, created, err
}

// Seal 加密明文；aad 通常是记录 ID。空明文返回空串（表示未设置密码）。
func (b *Box) Seal(plaintext, aad string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := b.aead.Seal(nil, nonce, []byte(plaintext), []byte(aad))
	return prefix + base64.StdEncoding.EncodeToString(append(nonce, ct...)), nil
}

// Open 解密；空串返回空串。
func (b *Box) Open(sealed, aad string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	if !strings.HasPrefix(sealed, prefix) {
		return "", ErrUndecryptable
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, prefix))
	if err != nil || len(raw) < b.aead.NonceSize() {
		return "", ErrUndecryptable
	}
	n := b.aead.NonceSize()
	pt, err := b.aead.Open(nil, raw[:n], raw[n:], []byte(aad))
	if err != nil {
		return "", ErrUndecryptable
	}
	return string(pt), nil
}
