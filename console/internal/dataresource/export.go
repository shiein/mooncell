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
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// 导出常量。
const (
	ExportMaxRows  = 200000    // 默认最大导出行数
	ExportMaxBytes = 200 << 20 // 200MB
)

// ErrExportLimit 表示导出行数或字节上限触发。
type ErrExportLimit struct {
	Reason string
}

func (e *ErrExportLimit) Error() string { return e.Reason }

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

// ExportCSV 流式导出查询结果为 CSV。
// sqlText 是要导出的查询语句，header 写入表头。
func ExportCSV(ctx context.Context, adapter DataSourceAdapter, sqlText string, w http.ResponseWriter) error {
	if err := prepareExportSQL(sqlText); err != nil {
		return err
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=export-%s.csv", time.Now().Format("20060102-150405")))

	// BOM for Excel UTF-8
	n, _ := w.Write([]byte{0xEF, 0xBB, 0xBF})
	approxBytes := n

	cw := csv.NewWriter(w)
	defer cw.Flush()

	// 在只读事务中执行全量查询
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

	// 写表头
	if err := cw.Write(cols); err != nil {
		return err
	}
	for _, c := range cols {
		approxBytes += len(c) + 1
	}
	typeNames := columnTypeNames(rows, len(cols))

	rowCount := 0
	for rows.Next() {
		if rowCount >= ExportMaxRows {
			cw.Flush()
			msg := "\n... 已达到最大导出行数限制 (" + strconv.Itoa(ExportMaxRows) + " 行)\n"
			w.Write([]byte(msg))
			return &ErrExportLimit{Reason: "已达到最大导出行数限制"}
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
		rowBytes := 0
		for i, v := range values {
			record[i] = valueToCSV(v, typeNames[i])
			rowBytes += len(record[i]) + 1
		}
		if approxBytes+rowBytes > ExportMaxBytes {
			cw.Flush()
			msg := fmt.Sprintf("\n... 已达到最大导出体积限制 (%d MB)\n", ExportMaxBytes>>20)
			w.Write([]byte(msg))
			return &ErrExportLimit{Reason: "已达到最大导出体积限制"}
		}
		if err := cw.Write(record); err != nil {
			return err
		}
		approxBytes += rowBytes
		rowCount++
	}
	return rows.Err()
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


