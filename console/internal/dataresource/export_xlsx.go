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
// 在写入 HTTP 响应前在内存流式构建；超限时返回明确错误且不写出半截文件。
func ExportXLSX(ctx context.Context, adapter DataSourceAdapter, sqlText string, w http.ResponseWriter) error {
	if err := prepareExportSQL(sqlText); err != nil {
		return err
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

	rowCount := 0
	for rows.Next() {
		rowCount++
		if rowCount > ExportMaxRows {
			_ = sw.Flush()
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
		record := make([]interface{}, len(cols))
		rowBytes := 0
		for i, v := range values {
			cell := valueToXLSX(v)
			record[i] = cell
			switch t := cell.(type) {
			case string:
				rowBytes += len(t)
			default:
				rowBytes += 16
			}
		}
		if approxBytes+rowBytes > ExportMaxBytes {
			_ = sw.Flush()
			return &ErrExportLimit{Reason: fmt.Sprintf("已达到最大导出体积限制 (%d MB)", ExportMaxBytes>>20)}
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
