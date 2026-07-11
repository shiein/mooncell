package main

import "testing"

func TestClampLogTail(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "200"},
		{"abc", "200"},
		{"0", "200"},
		{"-5", "200"},
		{"100", "100"},
		{"5000", "5000"},
		{"5001", "5000"},
		{"999999999", "5000"},
	}
	for _, c := range cases {
		if got := clampLogTail(c.in); got != c.want {
			t.Errorf("clampLogTail(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

// TestAcquireLogStreamQuota:占满 maxLogStreams 后再申请被拒;释放一个后又可申请。
func TestAcquireLogStreamQuota(t *testing.T) {
	a := &agent{}
	var releases []func()
	for i := 0; i < maxLogStreams; i++ {
		rel, ok := a.acquireLogStream()
		if !ok {
			t.Fatalf("第 %d 次申请应成功(未达上限)", i+1)
		}
		releases = append(releases, rel)
	}
	// 已满,再申请必拒。
	if _, ok := a.acquireLogStream(); ok {
		t.Fatalf("超过 maxLogStreams 应被拒")
	}
	// 计数未被失败的申请污染(应恰为上限,而非上限+1)。
	if n := a.logStreams.Load(); n != maxLogStreams {
		t.Fatalf("失败申请应回滚计数,期望 %d,got %d", maxLogStreams, n)
	}
	// 释放一个后可再申请。
	releases[0]()
	rel, ok := a.acquireLogStream()
	if !ok {
		t.Fatalf("释放一个额度后应可再申请")
	}
	rel()
	for _, r := range releases[1:] {
		r()
	}
	if n := a.logStreams.Load(); n != 0 {
		t.Fatalf("全部释放后计数应归零,got %d", n)
	}
}
