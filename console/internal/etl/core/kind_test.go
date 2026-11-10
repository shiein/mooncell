package core

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestFormatFieldValueDecimalScale(t *testing.T) {
	f := Field{Kind: KindDecimal, Scale: 2}
	if got := FormatFieldValue(decimal.NewFromFloat(12.5), f); got != "12.50" {
		t.Fatalf("scale=2 应补尾零：%s", got)
	}
	// 未声明标度时按原值输出
	f0 := Field{Kind: KindDecimal, Scale: -1}
	if got := FormatFieldValue(decimal.NewFromFloat(12.5), f0); got != "12.5" {
		t.Fatalf("未声明标度不应补零：%s", got)
	}
	// 非 decimal 值类型不受影响（容错路径）
	if got := FormatFieldValue("12.5", f); got != "12.5" {
		t.Fatalf("字符串值不应按标度补零：%s", got)
	}
	// 其他类型字段不受影响
	if got := FormatFieldValue(decimal.NewFromFloat(12.5), Field{Kind: KindNumber, Scale: 2}); got != "12.5" {
		t.Fatalf("非 decimal 字段不应补零：%s", got)
	}
	// nil
	if got := FormatFieldValue(nil, f); got != "<null>" {
		t.Fatalf("nil 应为 <null>：%s", got)
	}
}
