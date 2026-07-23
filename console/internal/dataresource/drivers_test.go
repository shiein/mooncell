package dataresource

import (
	"database/sql"
	"sort"
	"testing"
)

// TestDriversRegistered 验证五种驱动在同一二进制中全部注册成功。
// 这是设计文档第一节要求的「最小验证程序」：pgx 与 Vastbase(openGauss) 必须共存。
func TestDriversRegistered(t *testing.T) {
	all := sql.Drivers()
	sort.Strings(all)
	t.Logf("已注册驱动: %v", all)

	want := []string{DriverPostgreSQL, DriverMySQL, DriverDM, DriverKingbase, DriverVastbase}
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
}

// TestSupportedDrivers 验证 SupportedDrivers 返回全部五种且排序稳定。
func TestSupportedDrivers(t *testing.T) {
	got := SupportedDrivers()
	want := []string{DriverDM, DriverKingbase, DriverMySQL, DriverVastbase, DriverPostgreSQL} // 排序后: dm, kingbase, mysql, opengauss, pgx
	if len(got) != len(want) {
		t.Fatalf("SupportedDrivers 返回 %d 项,期望 %d 项: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SupportedDrivers[%d] = %q,期望 %q", i, got[i], want[i])
		}
	}
}
