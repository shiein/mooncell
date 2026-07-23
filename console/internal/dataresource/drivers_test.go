package dataresource

import (
	"database/sql"
	"sort"
	"testing"
)

// TestDriversRegistered 验证当前发布构建启用的四种驱动全部注册成功。
// Vastbase 官方本地 pq 未提供时不得用其它驱动伪装注册。
func TestDriversRegistered(t *testing.T) {
	all := sql.Drivers()
	sort.Strings(all)
	t.Logf("已注册驱动: %v", all)

	want := []string{DriverPostgreSQL, DriverMySQL, DriverDM, DriverKingbase}
	for _, name := range want {
		found := false
		for _, d := range all {
			if d == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("驱动 %q 未注册", name)
		}
	}
	for _, name := range []string{DriverVastbase, "openGauss"} {
		for _, d := range all {
			if d == name {
				t.Errorf("未认证的 Vastbase/openGauss 驱动 %q 不应注册", name)
			}
		}
	}
}

// TestSupportedDrivers 验证 SupportedDrivers 返回当前四种且排序稳定。
func TestSupportedDrivers(t *testing.T) {
	got := SupportedDrivers()
	want := []string{DriverDM, DriverKingbase, DriverMySQL, DriverPostgreSQL}
	if len(got) != len(want) {
		t.Fatalf("SupportedDrivers 返回 %d 项,期望 %d 项: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SupportedDrivers[%d] = %q,期望 %q", i, got[i], want[i])
		}
	}
}
