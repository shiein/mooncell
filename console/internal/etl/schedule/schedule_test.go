package schedule

import (
	"testing"
	"time"
)

func TestDescribe(t *testing.T) {
	for spec, want := range map[string]string{
		"0 2 * * *":    "每天 02:00",
		"*/15 * * * *": "每 15 分钟",
		"30 * * * *":   "每小时第 30 分",
		"0 */3 * * *":  "每 3 小时（第 0 分）",
		"0 9 * * 1-5":  "工作日（周一至周五）09:00",
		"0 9 * * 1,3":  "每周一、周三 09:00",
		"0 1 15 * *":   "每月 15 日 01:00",
		"@daily":       "每天 00:00",
		"0 0 2 * * *":  "每天 02:00",
		"0 0 1 1 *":    "自定义：0 0 1 1 *",
	} {
		if got := Describe(spec); got != want {
			t.Errorf("Describe(%q) = %q，期望 %q", spec, got, want)
		}
	}
}

func TestNextNHonorsTimezone(t *testing.T) {
	after := time.Date(2026, 11, 10, 17, 0, 0, 0, time.UTC) // 北京时间 11-11 01:00
	ts, err := NextN("0 2 * * *", "Asia/Shanghai", after, 2)
	if err != nil || len(ts) != 2 {
		t.Fatalf("%v %v", ts, err)
	}
	if got := ts[0].UTC().Format(time.RFC3339); got != "2026-11-10T18:00:00Z" {
		t.Fatalf("下一次应为北京时间 02:00（UTC 18:00），实际 %s", got)
	}
	if _, _, err := Parse("61 * * * *", ""); err == nil {
		t.Fatal("非法 cron 应报错")
	}
	if _, _, err := Parse("* * * * *", "Mars/Olympus"); err == nil {
		t.Fatal("非法时区应报错")
	}
}
