package dataresource

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
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

	result, err := executeImport(context.Background(), nil, session, "target", "public", []string{"only_one"})
	if err != nil || !strings.Contains(result.Error, "数量") {
		t.Fatalf("应拒绝数量不一致的映射: result=%+v err=%v", result, err)
	}

	result, err = executeImport(context.Background(), nil, session, "target", "public", []string{"ID", "id"})
	if err != nil || !strings.Contains(result.Error, "重复") {
		t.Fatalf("应拒绝重复目标列: result=%+v err=%v", result, err)
	}
}

func TestExportSnapshotCSVUsesProvidedRows(t *testing.T) {
	recorder := httptest.NewRecorder()
	err := ExportSnapshotCSV(
		[]string{"id", "name"},
		[][]any{{float64(1), "screen value"}},
		recorder,
	)
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.TrimPrefix(recorder.Body.Bytes(), []byte{0xEF, 0xBB, 0xBF})
	if got := string(body); got != "id,name\n1,screen value\n" {
		t.Fatalf("快照 CSV 内容不符: %q", got)
	}
}
