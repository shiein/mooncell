package dataresource

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

func TestIsReadOnlyState(t *testing.T) {
	for _, value := range []any{true, int64(1), uint64(1), []byte("ON"), " yes "} {
		if !isReadOnlyState(value) {
			t.Errorf("应识别为只读: %#v", value)
		}
	}
	for _, value := range []any{false, int64(0), uint64(2), []byte("off"), "read write", nil} {
		if isReadOnlyState(value) {
			t.Errorf("不应识别为只读: %#v", value)
		}
	}
}

func TestExecuteImportRejectsInvalidMappingsBeforeDatabaseAccess(t *testing.T) {
	session := &ImportSession{Columns: []string{"source_a", "source_b"}}

	result, err := executeImport(context.Background(), nil, session, "target", "public", []string{"only_one"}, excelize.Options{})
	if err != nil || !strings.Contains(result.Error, "数量") {
		t.Fatalf("应拒绝数量不一致的映射: result=%+v err=%v", result, err)
	}

	result, err = executeImport(context.Background(), nil, session, "target", "public", []string{"ID", "id"}, excelize.Options{})
	if err != nil || !strings.Contains(result.Error, "重复") {
		t.Fatalf("应拒绝重复目标列: result=%+v err=%v", result, err)
	}
}

func TestCSVFormulaSafePreservesNumbers(t *testing.T) {
	// 负数/正数/小数：不得加 '
	for _, s := range []string{"-5", "-3.14", "+2", "1e-3", "0"} {
		if got := csvFormulaSafe(s); got != s {
			t.Fatalf("csvFormulaSafe(%q)=%q, want unchanged", s, got)
		}
	}
	// 公式与危险前缀
	if got := csvFormulaSafe("=1+2"); got != "'=1+2" {
		t.Fatalf("= 应加前缀: %q", got)
	}
	if got := csvFormulaSafe("@cmd"); got != "'@cmd" {
		t.Fatalf("@ 应加前缀: %q", got)
	}
	if got := csvFormulaSafe("- not a number"); got != "'- not a number" {
		t.Fatalf("非数字 - 应加前缀: %q", got)
	}
	if got := csvFormulaSafe("+profit"); got != "'+profit" {
		t.Fatalf("非数字 + 应加前缀: %q", got)
	}
	for _, s := range []string{"+Inf", "-Inf", "\t=1+2", "\r=1+2", "\n=1+2", "＝1+2", "＋cmd", "－cmd", "＠cmd"} {
		if got := csvFormulaSafe(s); got != "'"+s {
			t.Fatalf("危险前缀 %q 应加前缀, got %q", s, got)
		}
	}
}

func TestValueToXLSXPreservesHighPrecisionNumbers(t *testing.T) {
	const preciseDecimal = "12345678901234567890.12"
	if got := valueToXLSX([]byte(preciseDecimal), "DECIMAL(22,2)"); got != preciseDecimal {
		t.Fatalf("高精度 DECIMAL 必须按文本保真, got %#v", got)
	}
	if got := valueToXLSX([]byte("12345.67"), "NUMERIC"); got != float64(12345.67) {
		t.Fatalf("安全范围 NUMERIC 应保留原生数值, got %#v", got)
	}

	const largeID int64 = 9_007_199_254_740_991
	if got := valueToXLSX(largeID, "BIGINT"); got != "9007199254740991" {
		t.Fatalf("大整数 ID 必须按文本保真, got %#v", got)
	}
	if got := valueToXLSX(sql.NullInt64{Int64: largeID, Valid: true}, "BIGINT"); got != "9007199254740991" {
		t.Fatalf("NullInt64 大整数必须按文本保真, got %#v", got)
	}
	bigID, ok := new(big.Int).SetString("123456789012345678901234567890", 10)
	if !ok {
		t.Fatal("构造 big.Int 失败")
	}
	if got := valueToXLSX(bigID, "NUMERIC"); got != bigID.String() {
		t.Fatalf("big.Int 必须按文本保真, got %#v", got)
	}
}

func TestImportCellValueEmptyToNull(t *testing.T) {
	if v := importCellValue("", true); v != nil {
		t.Fatalf("可空列空串应绑定 NULL, got %#v", v)
	}
	if v := importCellValue("", false); v != "" {
		t.Fatalf("不可空列空串应保留空串, got %#v", v)
	}
	if v := importCellValue("x", true); v != "x" {
		t.Fatalf("非空串应原样绑定, got %#v", v)
	}
}

func TestNewImportCSVReaderSharedConfig(t *testing.T) {
	// 含不规范引号：预览与执行必须都能解析且切分一致
	raw := "a,b\n1,\"he said \"\"hi\"\" ok\"\n"
	r1 := newImportCSVReader(strings.NewReader(raw))
	r2 := newImportCSVReader(strings.NewReader(raw))
	var rows1, rows2 [][]string
	for {
		row, err := r1.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		rows1 = append(rows1, append([]string(nil), row...))
	}
	for {
		row, err := r2.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		rows2 = append(rows2, append([]string(nil), row...))
	}
	if len(rows1) != len(rows2) {
		t.Fatalf("行数不一致 %d vs %d", len(rows1), len(rows2))
	}
	for i := range rows1 {
		if strings.Join(rows1[i], "|") != strings.Join(rows2[i], "|") {
			t.Fatalf("行 %d 切分不一致: %v vs %v", i, rows1[i], rows2[i])
		}
	}
}

func TestWrapIfBodyStarted(t *testing.T) {
	base := fmt.Errorf("scan failed")
	if err := wrapIfBodyStarted(false, base); err != base {
		t.Fatalf("body 未开始应返回原错误")
	}
	err := wrapIfBodyStarted(true, base)
	var partial *ErrExportBodyStarted
	if !errors.As(err, &partial) {
		t.Fatalf("body 已开始应包装为 ErrExportBodyStarted, got %T", err)
	}
	if !errors.Is(err, base) {
		t.Fatalf("应 Unwrap 到原错误")
	}
}
