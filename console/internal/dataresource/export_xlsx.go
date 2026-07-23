// XLSX 导出：使用 Excelize 流式生成。
package dataresource

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/xuri/excelize/v2"
)

// ExportXLSX 流式导出查询结果为 XLSX。
func ExportXLSX(ctx context.Context, adapter DataSourceAdapter, sqlText string, w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=export-%s.xlsx", time.Now().Format("20060102-150405")))

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
	defer sw.Flush()

	// 写表头
	headerRow := make([]interface{}, len(cols))
	for i, col := range cols {
		headerRow[i] = col
	}
	if err := sw.SetRow("A1", headerRow); err != nil {
		return err
	}

	rowCount := 0
	for rows.Next() {
		rowCount++
		if rowCount > ExportMaxRows {
			break
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
		for i, v := range values {
			record[i] = valueToXLSX(v)
		}
		cell, _ := excelize.CoordinatesToCellName(1, rowCount+1)
		if err := sw.SetRow(cell, record); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	return f.Write(w)
}

// valueToXLSX 将数据库值转为 XLSX 单元格值。
func valueToXLSX(v any) interface{} {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case bool:
		return val
	case []byte:
		if len(val) > 256 {
			return fmt.Sprintf("<binary %d bytes>", len(val))
		}
		return string(val)
	case time.Time:
		return val.Format(time.RFC3339Nano)
	default:
		return fmt.Sprintf("%v", v)
	}
}
