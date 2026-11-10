package secret

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoundTripAndFailClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k")
	b, created, err := LoadOrCreate(path)
	if err != nil || !created {
		t.Fatalf("首次应生成密钥：%v %v", created, err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("密钥文件权限应为 0600，实际 %v", st.Mode().Perm())
	}
	ct, err := b.Seal("p@ss:word", "conn-1")
	if err != nil || !strings.HasPrefix(ct, "v1:") || strings.Contains(ct, "p@ss") {
		t.Fatalf("密文格式异常：%q %v", ct, err)
	}
	if pt, err := b.Open(ct, "conn-1"); err != nil || pt != "p@ss:word" {
		t.Fatalf("解密失败：%q %v", pt, err)
	}
	// AAD 错位（密文被挪到别的连接）
	if _, err := b.Open(ct, "conn-2"); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("AAD 不同应解密失败：%v", err)
	}
	// 篡改
	bad := ct[:len(ct)-2] + "AA"
	if _, err := b.Open(bad, "conn-1"); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("篡改应解密失败：%v", err)
	}
	// 重新加载同一密钥可解
	b2, created2, err := LoadOrCreate(path)
	if err != nil || created2 {
		t.Fatalf("二次加载不应新建：%v %v", created2, err)
	}
	if pt, _ := b2.Open(ct, "conn-1"); pt != "p@ss:word" {
		t.Fatal("重载密钥后应可解密")
	}
	// 密钥丢失后新密钥解不开旧密文（fail-closed）
	os.Remove(path)
	b3, _, _ := LoadOrCreate(path)
	if _, err := b3.Open(ct, "conn-1"); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("换密钥后应解密失败：%v", err)
	}
	// 空值约定
	if s, _ := b.Seal("", "x"); s != "" {
		t.Fatal("空密码应得到空密文")
	}
	if s, err := b.Open("", "x"); s != "" || err != nil {
		t.Fatal("空密文应得到空密码")
	}
}
