package conn

import (
	"testing"
	"time"

	"mooncell/console/internal/etl/core"
)

func TestNormalizeWallClockTimestamps(t *testing.T) {
	sh, _ := time.LoadLocation("Asia/Shanghai")
	utcWall := time.Date(2026, 11, 1, 9, 59, 59, 0, time.UTC) // 驱动对无时区列的返回形态之一
	f := core.Field{Name: "ts", Kind: core.KindTimestamp, NativeType: "TIMESTAMP"}
	v, err := normalizeCommon(utcWall, f, sh)
	if err != nil {
		t.Fatal(err)
	}
	got := v.(time.Time)
	if got.Location() != sh || got.Hour() != 9 || got.Minute() != 59 {
		t.Fatalf("无时区列应按连接时区解释墙上时钟：%v", got)
	}
	// 达梦形态：驱动把无时区列标成会话偏移（+0800 固定时区），同样只认墙上时钟分量
	fixed := time.FixedZone("", 8*3600)
	dmWall := time.Date(2026, 11, 1, 9, 59, 59, 0, fixed)
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	v, err = normalizeCommon(dmWall, f, tokyo)
	if err != nil {
		t.Fatal(err)
	}
	got = v.(time.Time)
	if got.Location() != tokyo || got.Hour() != 9 || got.Minute() != 59 {
		t.Fatalf("固定偏移标注的无时区列应按连接时区保留墙上时钟：%v", got)
	}
	// 无时区列转字符串输出墙上时钟本身（不做瞬时换算）
	vs, err := normalizeCommon(dmWall, core.Field{Name: "s", Kind: core.KindString, NativeType: "TIMESTAMP"}, tokyo)
	if err != nil || vs != "2026-11-01 09:59:59" {
		t.Fatalf("无时区列转字符串应为墙上时钟：%v %v", vs, err)
	}
	// 带时区列保持瞬时不变
	ftz := core.Field{Name: "ts", Kind: core.KindTimestamp, NativeType: "TIMESTAMPTZ"}
	v, _ = normalizeCommon(utcWall, ftz, sh)
	if !v.(time.Time).Equal(utcWall) {
		t.Fatalf("带时区列不应改变瞬时：%v", v)
	}
	// 带时区列转字符串按连接时区取墙上时钟
	vs, _ = normalizeCommon(utcWall, core.Field{Name: "s", Kind: core.KindString, NativeType: "TIMESTAMPTZ"}, sh)
	if vs != "2026-11-01 17:59:59" {
		t.Fatalf("带时区列转字符串应换算到连接时区：%v", vs)
	}
	// 数值/文本/decimal 规整
	for _, c := range []struct {
		in   any
		kind core.Kind
		want string
	}{
		{[]byte("12.50"), core.KindDecimal, "12.5"},
		{[]byte("42"), core.KindInteger, "42"},
		{uint64(7), core.KindInteger, "7"},
		{"t", core.KindBoolean, "true"},
	} {
		v, err := normalizeCommon(c.in, core.Field{Kind: c.kind}, sh)
		if err != nil || core.FormatValue(v) != c.want {
			t.Errorf("normalize(%#v → %s) = %v %v，期望 %s", c.in, c.kind, v, err, c.want)
		}
	}
}

func TestKindMapping(t *testing.T) {
	cases := map[string]core.Kind{
		"character varying": core.KindString, "numeric": core.KindDecimal, "int8": core.KindInteger, "BIGINT": core.KindInteger, "int(10) unsigned": core.KindInteger, "bigint unsigned zerofill": core.KindInteger,
		"timestamp without time zone": core.KindTimestamp, "DATE": core.KindDate, "bytea": core.KindBinary, "DOUBLE": core.KindNumber,
		"boolean": core.KindBoolean, "DATETIME(6)": core.KindTimestamp, "decimal(10,2) unsigned": core.KindDecimal,
	}
	for typ, want := range cases {
		if got := kindForType(ColumnMeta{DBType: typ, Precision: -1}); got != want {
			t.Errorf("%s → %s，期望 %s", typ, got, want)
		}
	}
	if got := kindForType(ColumnMeta{DBType: "NUMBER", Precision: 10, Scale: 0}); got != core.KindInteger {
		t.Errorf("NUMBER(10,0) 应映射为整数，实际 %s", got)
	}
	if got := kindForType(ColumnMeta{DBType: "NUMBER", Precision: 20, Scale: 0}); got != core.KindDecimal {
		t.Errorf("NUMBER(20,0) 超出 int64，应为精确小数，实际 %s", got)
	}
}
