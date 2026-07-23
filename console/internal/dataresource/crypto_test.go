package dataresource

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCredentialKeyLoadOrCreate(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "test.key")

	// 首次：无资源 → 生成
	k1, err := LoadOrCreateCredentialKey(keyPath, false)
	if err != nil {
		t.Fatalf("首次生成密钥失败: %v", err)
	}
	if k1 == nil || len(k1.key) != keyLen {
		t.Fatalf("密钥长度不符: %d", len(k1.key))
	}

	// 再次：文件已存在 → 加载
	k2, err := LoadOrCreateCredentialKey(keyPath, true)
	if err != nil {
		t.Fatalf("加载已有密钥失败: %v", err)
	}
	if string(k1.key) != string(k2.key) {
		t.Fatal("两次加载的密钥不一致")
	}
}

func TestCredentialKeyMissingWithResources(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "missing.key")
	// 已有资源但密钥不存在 → 拒绝启动
	_, err := LoadOrCreateCredentialKey(keyPath, true)
	if err == nil {
		t.Fatal("已有资源但密钥丢失时应返回错误")
	}
}

func TestCredentialEncryptDecrypt(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "test.key")
	k, err := LoadOrCreateCredentialKey(keyPath, false)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}

	cases := []string{"", "password123", "P@ssw0rd!#$%", "中文密码🔐"}
	for _, plain := range cases {
		cipher, err := k.Encrypt(plain)
		if err != nil {
			t.Fatalf("加密失败: %v", err)
		}
		// 密文应与明文不同（空串加密后也不为空）
		if plain != "" && cipher == plain {
			t.Fatal("密文与明文相同")
		}
		got, err := k.Decrypt(cipher)
		if err != nil {
			t.Fatalf("解密失败: %v", err)
		}
		if got != plain {
			t.Errorf("解密结果不符: 期望 %q,实际 %q", plain, got)
		}
	}
}

func TestCredentialDecryptInvalidCipher(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "test.key")
	k, _ := LoadOrCreateCredentialKey(keyPath, false)

	// 篡改密文
	cipher, _ := k.Encrypt("test")
	tampered := cipher[:len(cipher)-4] + "AAAA"
	if _, err := k.Decrypt(tampered); err == nil {
		t.Fatal("篡改后的密文应解密失败")
	}

	// 错误版本号
	_, err := k.Decrypt("AQIDBAU=") // version=1 + 短数据
	if err == nil {
		t.Fatal("无效密文应解密失败")
	}

	// 错误密钥
	k2, _ := LoadOrCreateCredentialKey(filepath.Join(dir, "other.key"), false)
	if _, err := k2.Decrypt(cipher); err == nil {
		t.Fatal("用错误密钥应解密失败")
	}
}

func TestCredentialKeyWrongLength(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "short.key")
	// 写入短密钥
	if err := os.WriteFile(keyPath, []byte("short"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrCreateCredentialKey(keyPath, false)
	if err == nil {
		t.Fatal("短密钥应返回错误")
	}
}
