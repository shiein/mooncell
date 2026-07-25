// 导出：CSV 和 XLSX 流式输出。
//
// 设计文档第四节「导出」：
//   - 支持 UTF-8 CSV 和 XLSX。
//   - 全量导出重新执行最近一次成功的查询；自动提交下数据可能与屏幕快照有时间差。
//   - 同步流式输出，不增加后台任务。
//   - 默认限制 20 万行或 200MB，先到者终止并返回明确错误。
//   - DDL 导出为 UTF-8 .sql。
package dataresource

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"
	"unicode/utf8"
)

// 导出常量。
const (
	ExportMaxRows  = 200000    // CSV 默认最大导出行数
	ExportMaxBytes = 200 << 20 // CSV 200MB
	// XLSX 在内存构建，上限更严，降低多用户并发 OOM 风险
	ExportXLSXMaxRows  = 100000
	ExportXLSXMaxBytes = 50 << 20 // 50MB
)

// ErrExportLimit 表示导出行数或字节上限触发。
type ErrExportLimit struct {
	Reason string
}

func (e *ErrExportLimit) Error() string { return e.Reason }

// ErrExportBodyStarted 表示 CSV 响应体已开始写入后发生错误。
// handler 不得再 writeErr JSON，否则会污染已发出的下载内容。
type ErrExportBodyStarted struct {
	Err error
}

func (e *ErrExportBodyStarted) Error() string {
	if e == nil || e.Err == nil {
		return "export response body already started"
	}
	return e.Err.Error()
}

func (e *ErrExportBodyStarted) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

var (
	exportCSVSem         = make(chan struct{}, 2)
	errCSVExportMaxBytes = errors.New("CSV export byte limit exceeded")
)

// byteLimitWriter 按实际写出的 CSV 字节计数（包含 BOM、引号转义和换行）。
type byteLimitWriter struct {
	w       io.Writer
	max     int64
	written int64
}

func (w *byteLimitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.max-w.written {
		return 0, errCSVExportMaxBytes
	}
	n, err := w.w.Write(p)
	w.written += int64(n)
	return n, err
}

func normalizeCSVWriteError(err error) error {
	if errors.Is(err, errCSVExportMaxBytes) {
		return &ErrExportLimit{Reason: fmt.Sprintf("已达到最大导出体积限制 (%d MB)", ExportMaxBytes>>20)}
	}
	return err
}

// prepareExportSQL 校验导出 SQL：单语句且必须为只读查询。
func prepareExportSQL(sqlText string) error {
	if err := ValidateSingleStatement(sqlText); err != nil {
		return err
	}
	if !ClassifySQL(sqlText).IsReadOnly() {
		return fmt.Errorf("仅支持导出 SELECT 查询结果")
	}
	return nil
}

// ExportCSV 先写入临时文件，确认未超限后再发送响应。
// 超限时返回 ErrExportLimit 且不写 HTTP 体，避免 200 + 截断文件被当成成功下载。
func ExportCSV(ctx context.Context, adapter DataSourceAdapter, sqlText string, w http.ResponseWriter) error {
	if err := prepareExportSQL(sqlText); err != nil {
		return err
	}
	select {
	case exportCSVSem <- struct{}{}:
		defer func() { <-exportCSVSem }()
	case <-ctx.Done():
		return ctx.Err()
	default:
		return fmt.Errorf("当前导出任务过多，请稍后再试")
	}

	tx, err := adapter.Begin(ctx, true)
	if err != nil {
		return fmt.Errorf("开启只读事务失败: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(ctx, sqlText)
	if err != nil {
		return adapter.NormalizeError(err).toError()
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	typeNames := columnTypeNames(rows, len(cols))

	tmp, err := os.CreateTemp("", "mc-export-*.csv")
	if err != nil {
		return fmt.Errorf("创建导出临时文件失败: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpPath)
	}()

	limited := &byteLimitWriter{w: tmp, max: ExportMaxBytes}
	// BOM for Excel UTF-8
	if _, err := limited.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		return normalizeCSVWriteError(err)
	}
	cw := csv.NewWriter(limited)
	if err := cw.Write(cols); err != nil {
		return normalizeCSVWriteError(err)
	}

	rowCount := 0
	for rows.Next() {
		if rowCount >= ExportMaxRows {
			return &ErrExportLimit{Reason: fmt.Sprintf("已达到最大导出行数限制 (%d 行)", ExportMaxRows)}
		}
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		record := make([]string, len(cols))
		for i, v := range values {
			record[i] = csvFormulaSafe(valueToCSV(v, typeNames[i]))
		}
		if err := cw.Write(record); err != nil {
			return normalizeCSVWriteError(err)
		}
		rowCount++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return normalizeCSVWriteError(err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}

	// 完整文件就绪后再写响应头与体
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=export-%s.csv", time.Now().Format("20060102-150405")))
	if _, err := io.Copy(w, tmp); err != nil {
		return &ErrExportBodyStarted{Err: err}
	}
	return nil
}

// csvFormulaSafe 防止 Excel 将单元格当公式执行。
// = @ 始终加前缀；+ - 仅当整串不能解析为数字时加前缀（避免金额/差值等负数被写成文本）。
// 同时覆盖 Tab/CR/LF 与常见全角公式起始符。
func csvFormulaSafe(s string) string {
	if s == "" {
		return s
	}
	first, _ := utf8.DecodeRuneInString(s)
	switch first {
	case '=', '@', '\t', '\r', '\n', '＝', '＋', '－', '＠':
		return "'" + s
	case '+', '-':
		// 纯数字（含小数、科学计数）保留原样，便于 Excel/pandas 按数值读
		if n, err := strconv.ParseFloat(s, 64); err == nil && !math.IsInf(n, 0) && !math.IsNaN(n) {
			return s
		}
		return "'" + s
	}
	return s
}

// valueToCSV 将数据库值转为 CSV 字符串。
func valueToCSV(v any, dbTypeName string) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case bool:
		if val {
			return "true"
		}
		return "false"
	case []byte:
		// 文本列（含 MySQL TEXT 以 []byte 返回）按字符串导出
		if isTextualDBType(dbTypeName) || (!isBinaryDBType(dbTypeName) && isLikelyUTF8Text(val)) {
			return string(val)
		}
		// 二进制：用 hex 表示前 64 字节
		if len(val) > 64 {
			return fmt.Sprintf("<binary %d bytes>", len(val))
		}
		return fmt.Sprintf("%x", val)
	case time.Time:
		return val.Format(time.RFC3339Nano)
	case sql.NullString:
		if val.Valid {
			return val.String
		}
		return ""
	case sql.NullInt64:
		if val.Valid {
			return strconv.FormatInt(val.Int64, 10)
		}
		return ""
	case sql.NullFloat64:
		if val.Valid {
			return strconv.FormatFloat(val.Float64, 'g', -1, 64)
		}
		return ""
	case sql.NullBool:
		if val.Valid {
			if val.Bool {
				return "true"
			}
			return "false"
		}
		return ""
	case sql.NullTime:
		if val.Valid {
			return val.Time.Format(time.RFC3339Nano)
		}
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}
