package dataresource

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"math/big"
	"os"
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

func TestByteLimitWriterCountsActualBytes(t *testing.T) {
	var dst bytes.Buffer
	w := &byteLimitWriter{w: &dst, max: 5}
	if n, err := w.Write([]byte("12345")); err != nil || n != 5 {
		t.Fatalf("写入上限内数据失败: n=%d err=%v", n, err)
	}
	if n, err := w.Write([]byte("6")); !errors.Is(err, errCSVExportMaxBytes) || n != 0 {
		t.Fatalf("超过上限必须拒绝且不部分写入: n=%d err=%v", n, err)
	}
	if got := dst.String(); got != "12345" {
		t.Fatalf("超过上限后内容被污染: %q", got)
	}
}

func TestInvalidateImportsCancelsActiveAndRemovesIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	active := &ImportSession{
		ID: "active", ResourceID: "r1", Username: "alice",
		inUse: true, cancel: cancel,
	}
	idle := &ImportSession{ID: "idle", ResourceID: "r1", Username: "alice"}
	other := &ImportSession{ID: "other", ResourceID: "r2", Username: "alice"}
	svc := &Service{importSessions: map[string]*ImportSession{
		active.ID: active,
		idle.ID:   idle,
		other.ID:  other,
	}}

	svc.invalidateImports(func(session *ImportSession) bool {
		return session.ResourceID == "r1"
	})
	if !active.invalidated.Load() || !idle.invalidated.Load() {
		t.Fatal("匹配资源的导入会话应全部失效")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("活动导入未被取消")
	}
	if _, ok := svc.importSessions[idle.ID]; ok {
		t.Fatal("空闲导入会话应立即移除")
	}
	if _, ok := svc.importSessions[active.ID]; !ok {
		t.Fatal("活动会话应保留到执行 defer 清理")
	}
	if _, ok := svc.importSessions[other.ID]; !ok {
		t.Fatal("其它资源会话不应被影响")
	}
}

func TestImportCommitAndInvalidationAreOrdered(t *testing.T) {
	session := &ImportSession{}
	session.invalidate()
	called := false
	if err := session.commitIfValid(func() error {
		called = true
		return nil
	}); !errors.Is(err, errImportInvalidated) || called {
		t.Fatalf("已失效会话不得进入提交: called=%v err=%v", called, err)
	}

	session = &ImportSession{}
	commitStarted := make(chan struct{})
	releaseCommit := make(chan struct{})
	commitDone := make(chan error, 1)
	go func() {
		commitDone <- session.commitIfValid(func() error {
			close(commitStarted)
			<-releaseCommit
			return nil
		})
	}()
	<-commitStarted
	invalidateDone := make(chan struct{})
	go func() {
		session.invalidate()
		close(invalidateDone)
	}()
	select {
	case <-invalidateDone:
		t.Fatal("提交进行中时失效不得越过提交点")
	default:
	}
	close(releaseCommit)
	if err := <-commitDone; err != nil {
		t.Fatalf("先进入的提交应正常完成: %v", err)
	}
	<-invalidateDone
	if !session.invalidated.Load() {
		t.Fatal("提交完成后会话应被标记失效")
	}
}

func TestInvalidatedImportRollsBackBeforeCommit(t *testing.T) {
	db, base := openTestSQLite(t)
	defer db.Close()
	adapter := &editableTestAdapter{testSQLAdapter: base}
	path := t.TempDir() + "/import.csv"
	if err := os.WriteFile(path, []byte("id,name\n1,alice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := &ImportSession{
		Format: "csv", FilePath: path, Columns: []string{"id", "name"},
	}
	session.invalidate()
	result, err := executeImportGuarded(context.Background(), adapter, session, "items", "",
		[]string{"id", "name"}, excelize.Options{}, session.commitIfValid)
	if err != nil || result == nil || !strings.Contains(result.Error, errImportInvalidated.Error()) {
		t.Fatalf("失效导入应在提交点失败: result=%+v err=%v", result, err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("提交守卫拒绝后导入仍落库: %d", count)
	}
}
