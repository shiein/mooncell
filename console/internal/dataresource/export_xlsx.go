// XLSX 导出：使用 Excelize 流式生成。
package dataresource

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

// exportXLSXSem 全局并发上限，XLSX 在内存构建，防止多用户同时导出 OOM。
var exportXLSXSem = make(chan struct{}, 2)

// ExportXLSX 在内存构建工作簿后写出；超限时返回明确错误且不写出半截文件。
func ExportXLSX(ctx context.Context, adapter DataSourceAdapter, sqlText string, w http.ResponseWriter) error {
	if err := prepareExportSQL(sqlText); err != nil {
		return err
	}
	select {
	case exportXLSXSem <- struct{}{}:
		defer func() { <-exportXLSXSem }()
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

	f := excelize.NewFile()
	defer f.Close()
	sheet := "Sheet1"
	sw, err := f.NewStreamWriter(sheet)
	if err != nil {
		return err
	}

	// 写表头
	headerRow := make([]interface{}, len(cols))
	approxBytes := 0
	for i, col := range cols {
		headerRow[i] = col
		approxBytes += len(col)
	}
	if err := sw.SetRow("A1", headerRow); err != nil {
		return err
	}
	typeNames := columnTypeNames(rows, len(cols))

	rowCount := 0
	for rows.Next() {
		rowCount++
		if rowCount > ExportXLSXMaxRows {
			_ = sw.Flush()
			return &ErrExportLimit{Reason: fmt.Sprintf("已达到 XLSX 最大导出行数限制 (%d 行)，请改用 CSV", ExportXLSXMaxRows)}
		}
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		record := make([]interface{}, len(cols))
		rowBytes := 0
		for i, v := range values {
			cell := valueToXLSX(v, typeNames[i])
			record[i] = cell
			switch t := cell.(type) {
			case string:
				rowBytes += len(t)
			case time.Time:
				rowBytes += 24
			default:
				rowBytes += 16
			}
		}
		if approxBytes+rowBytes > ExportXLSXMaxBytes {
			_ = sw.Flush()
			return &ErrExportLimit{Reason: fmt.Sprintf("已达到 XLSX 最大导出体积限制 (%d MB)，请改用 CSV", ExportXLSXMaxBytes>>20)}
		}
		cell, _ := excelize.CoordinatesToCellName(1, rowCount+1)
		if err := sw.SetRow(cell, record); err != nil {
			return err
		}
		approxBytes += rowBytes
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := sw.Flush(); err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=export-%s.xlsx", time.Now().Format("20060102-150405")))
	return f.Write(w)
}

// valueToXLSX 将数据库值转为 excelize 可识别的原生单元格类型（数值/日期非文本）。
func valueToXLSX(v any, dbTypeName string) interface{} {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case string:
		return val
	case bool:
		return val
	case int:
		return excelSafeInt64(int64(val))
	case int8:
		return excelSafeInt64(int64(val))
	case int16:
		return excelSafeInt64(int64(val))
	case int32:
		return excelSafeInt64(int64(val))
	case int64:
		return excelSafeInt64(val)
	case uint:
		return excelSafeUint64(uint64(val))
	case uint8:
		return excelSafeUint64(uint64(val))
	case uint16:
		return excelSafeUint64(uint64(val))
	case uint32:
		return excelSafeUint64(uint64(val))
	case uint64:
		return excelSafeUint64(val)
	case float32:
		return float64(val)
	case float64:
		return val
	case []byte:
		if isTextualDBType(dbTypeName) || (!isBinaryDBType(dbTypeName) && isLikelyUTF8Text(val)) {
			// DECIMAL/NUMERIC 常以 []byte 返回；仅在 Excel 15 位有效数字范围内写为数字。
			if isNumericDBType(dbTypeName) {
				return excelNumericText(string(val))
			}
			return string(val)
		}
		if len(val) > 256 {
			return fmt.Sprintf("<binary %d bytes>", len(val))
		}
		return fmt.Sprintf("%x", val)
	case time.Time:
		// excelize 接受 time.Time 为日期单元格
		return val
	case *big.Int:
		if val == nil {
			return nil
		}
		if val.IsInt64() {
			return excelSafeInt64(val.Int64())
		}
		return val.String()
	case *big.Float:
		if val == nil {
			return nil
		}
		return excelNumericText(val.Text('g', -1))
	case sql.NullString:
		if val.Valid {
			return val.String
		}
		return nil
	case sql.NullInt64:
		if val.Valid {
			return excelSafeInt64(val.Int64)
		}
		return nil
	case sql.NullFloat64:
		if val.Valid {
			return val.Float64
		}
		return nil
	case sql.NullBool:
		if val.Valid {
			return val.Bool
		}
		return nil
	case sql.NullTime:
		if val.Valid {
			return val.Time
		}
		return nil
	default:
		return fmt.Sprintf("%v", v)
	}
}

// Excel 对数值只保留 15 位有效数字。超过该边界的整数按文本写出，
// 避免主键、流水号等在打开工作簿时被静默改写。
const excelMaxExactInteger uint64 = 999_999_999_999_999

func excelSafeInt64(v int64) interface{} {
	if v > int64(excelMaxExactInteger) || v < -int64(excelMaxExactInteger) {
		return strconv.FormatInt(v, 10)
	}
	return v
}

func excelSafeUint64(v uint64) interface{} {
	if v > excelMaxExactInteger {
		return strconv.FormatUint(v, 10)
	}
	return int64(v)
}

// excelNumericText 将精度在 Excel 安全范围内的 DECIMAL/NUMERIC 写为原生数值；
// 超过 15 位有效数字或无法可靠解析时保留原始文本。
func excelNumericText(s string) interface{} {
	if decimalSignificantDigits(s) > 15 {
		return s
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return s
	}
	return f
}

func decimalSignificantDigits(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if s[0] == '+' || s[0] == '-' {
		s = s[1:]
	}
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		s = s[:i]
	}
	s = strings.ReplaceAll(s, ".", "")
	s = strings.TrimLeft(s, "0")
	if s == "" {
		return 1
	}
	return len(s)
}

func isNumericDBType(dbTypeName string) bool {
	t := strings.ToUpper(strings.TrimSpace(dbTypeName))
	return strings.Contains(t, "DECIMAL") || strings.Contains(t, "NUMERIC") ||
		strings.Contains(t, "NUMBER") || t == "MONEY" ||
		strings.Contains(t, "INT") || strings.Contains(t, "FLOAT") ||
		strings.Contains(t, "DOUBLE") || strings.Contains(t, "REAL") ||
		strings.Contains(t, "SERIAL")
}
