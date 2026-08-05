package serverops

import "testing"

func TestCredentialLeaseIsolationAndCleanup(t *testing.T) {
	m := newCredentialLeaseManager()
	original := []byte("secret-a")
	m.Store("alice", "login-a", "srv-1", original)
	original[0] = 'X'

	got, ok := m.Get("login-a", "srv-1")
	if !ok || string(got) != "secret-a" {
		t.Fatalf("租约应保存独立副本，got=%q ok=%v", got, ok)
	}
	got[0] = 'Y'
	again, _ := m.Get("login-a", "srv-1")
	if string(again) != "secret-a" {
		t.Fatal("读取方不得修改租约中的密码")
	}
	if _, ok := m.Get("login-b", "srv-1"); ok {
		t.Fatal("同账号其它登录会话不得读取凭据租约")
	}
	oldVersion := m.Store("alice", "login-a", "srv-1", []byte("old"))
	m.Store("alice", "login-a", "srv-1", []byte("new"))
	m.DeleteIfVersion("login-a", "srv-1", oldVersion)
	latest, ok := m.Get("login-a", "srv-1")
	if !ok || string(latest) != "new" {
		t.Fatal("陈旧握手失败不得误删并发写入的新凭据租约")
	}

	m.DeleteLoginSession("login-a")
	if _, ok := m.Get("login-a", "srv-1"); ok {
		t.Fatal("退出或过期后必须清除凭据租约")
	}
}
