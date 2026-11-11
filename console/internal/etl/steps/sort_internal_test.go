package steps

import (
	"bufio"
	"bytes"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"mooncell/console/internal/etl/core"
)

// 溢写编码/解码往返：全类型、NULL、时区偏移都须原样还原。
func TestRowCodecRoundtrip(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	s := core.MustSchema(
		core.NewField("i", core.KindInteger),
		core.NewField("s", core.KindString),
		core.NewField("d", core.KindDecimal),
		core.NewField("f", core.KindNumber),
		core.NewField("b", core.KindBoolean),
		core.NewField("t", core.KindTimestamp),
		core.NewField("dt", core.KindDate),
		core.NewField("bin", core.KindBinary),
		core.NewField("nul", core.KindString),
	)
	rows := []core.Row{
		{int64(-42), "héllo", decimal.RequireFromString("123.456"), 2.5, true, time.Date(2026, 11, 5, 13, 14, 15, 123456789, loc), time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC), []byte{0, 1, 255}, nil},
		{int64(0), "", decimal.Zero, -0.0, false, time.Time{}, time.Time{}, []byte{}, "x"},
	}
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	for _, r := range rows {
		if err := encodeRow(w, s, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	rd := bufio.NewReader(&buf)
	for i, want := range rows {
		got, err := decodeRow(rd, s)
		if err != nil {
			t.Fatalf("行 %d 解码失败：%v", i, err)
		}
		for j := range want {
			wv, gv := want[j], got[j]
			switch w := wv.(type) {
			case time.Time:
				g, ok := gv.(time.Time)
				if !ok || !w.Equal(g) || w.Format("15:04:05 -07") != g.Format("15:04:05 -07") {
					t.Errorf("行 %d 列 %d 时间不一致：%v vs %v", i, j, w, gv)
				}
			case decimal.Decimal:
				if w.Cmp(gv.(decimal.Decimal)) != 0 {
					t.Errorf("行 %d 列 %d decimal 不一致：%v vs %v", i, j, w, gv)
				}
			case []byte:
				if !bytes.Equal(w, gv.([]byte)) {
					t.Errorf("行 %d 列 %d bytes 不一致", i, j)
				}
			default:
				if wv != gv {
					t.Errorf("行 %d 列 %d：%v(%T) vs %v(%T)", i, j, wv, wv, gv, gv)
				}
			}
		}
	}
}

// 比较器：NULL 最小、数值跨类型比较、desc 反向。
func TestCompareValues(t *testing.T) {
	cases := []struct {
		a, b any
		want int
	}{
		{nil, int64(1), -1},
		{nil, nil, 0},
		{int64(2), int64(1), 1},
		{int64(3), float64(3.5), -1},
		{int64(5), decimal.RequireFromString("4.9"), 1},
		{decimal.RequireFromString("1.10"), decimal.RequireFromString("1.1"), 0},
		{"a", "b", -1},
		{true, false, 1},
		{[]byte{1}, []byte{0}, 1},
		{"x", int64(1), 1}, // 跨类型按固定类型序（数值 < 字符串）
	}
	for i, c := range cases {
		if got := compareValues(c.a, c.b); got != c.want {
			t.Errorf("用例 %d：compare(%v,%v)=%d 期望 %d", i, c.a, c.b, got, c.want)
		}
	}
	less := rowLess([]int{0}, []bool{true})
	if less(core.Row{int64(1)}, core.Row{int64(2)}) <= 0 {
		t.Error("desc 排序应反向")
	}
}
