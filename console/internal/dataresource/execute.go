// SQL 执行引擎：执行单条 SQL，返回结构化结果。
//
// 设计文档第三节「SQL 执行规则」：
//   - 一次请求只允许一条 SQL；拒绝多语句。
//   - 显式 BEGIN/COMMIT/ROLLBACK/SAVEPOINT 一律拒绝。
//   - SELECT 默认分页 100 行（服务端隐式 PageSQL，模板 SQL 不写 LIMIT）；
//     接口最大允许 10000 行（当前页展开全部上限）。
//   - 默认尝试 COUNT(*) FROM (query) 返回总数；统计失败仍返回数据，标记 totalStatus=unavailable。
//   - 自动提交查询和只读事务中的统计、分页在同一只读事务内执行。
//   - 大整数、DECIMAL 使用字符串传输；二进制字段以类型标识和截断预览返回。
package dataresource

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"
)

// 执行常量。
const (
	DefaultLimit = 100   // SELECT 默认每页行数（隐式，不写入编辑器 SQL）
	MaxLimit     = 10000 // SELECT 单次最大行数（展开全部上限）
)

// ExecutionResult 是 SQL 执行的返回结构。
type ExecutionResult struct {
	ExecutionID   string          `json:"executionId"`
	StatementType StatementType   `json:"statementType"`
	Columns       []string        `json:"columns,omitempty"`
	Rows          [][]any         `json:"rows,omitempty"`
	ReturnedRows  int             `json:"returnedRows"`
	Total         int             `json:"total,omitempty"`
	TotalStatus   string          `json:"totalStatus,omitempty"` // available/unavailable
	HasMore       bool            `json:"hasMore,omitempty"`
	Limit         int             `json:"limit,omitempty"`  // 本次分页 limit
	Offset        int             `json:"offset,omitempty"` // 本次分页 offset
	AffectedRows  int64           `json:"affectedRows,omitempty"`
	DurationMs    int64           `json:"durationMs"`
	Messages      []string        `json:"messages,omitempty"`
	TxState       string          `json:"txState"` // none/active/failed
	// Editable 非空表示该 SELECT 结果支持就地编辑（单表 + 有主键）；只读用户由 handler 省略。
	Editable *EditableInfo `json:"editable,omitempty"`
}

// columnTypeNames 从 *sql.Rows 取 DatabaseTypeName 列表（失败则返回等长空串）。
func columnTypeNames(rows *sql.Rows, n int) []string {
	names := make([]string, n)
	cts, err := rows.ColumnTypes()
	if err != nil || len(cts) != n {
		return names
	}
	for i, ct := range cts {
		if ct != nil {
			names[i] = ct.DatabaseTypeName()
		}
	}
	return names
}

// isBinaryDBType 判断驱动报告的列类型是否应作为二进制处理。
func isBinaryDBType(dbTypeName string) bool {
	t := strings.ToUpper(strings.TrimSpace(dbTypeName))
	if t == "" {
		return false
	}
	if strings.Contains(t, "BLOB") || strings.Contains(t, "BINARY") ||
		t == "BYTEA" || t == "RAW" || t == "LONG RAW" || t == "IMAGE" ||
		t == "VARBINARY" || t == "LONGBLOB" || t == "MEDIUMBLOB" || t == "TINYBLOB" {
		return true
	}
	return false
}

// isTextualDBType 判断 []byte 是否应按文本解码（MySQL TEXT/VARCHAR 常以 []byte 返回）。
func isTextualDBType(dbTypeName string) bool {
	t := strings.ToUpper(strings.TrimSpace(dbTypeName))
	if t == "" || isBinaryDBType(t) {
		return false
	}
	// CHAR/VARCHAR/TEXT/JSON/ENUM/SET/XML/UUID 及常见别名
	if strings.Contains(t, "CHAR") || strings.Contains(t, "TEXT") ||
		strings.Contains(t, "JSON") || strings.Contains(t, "XML") ||
		t == "ENUM" || t == "SET" || t == "UUID" || t == "NAME" ||
		t == "CITEXT" || strings.Contains(t, "CLOB") {
		return true
	}
	// DECIMAL/NUMERIC 等以 []byte 返回时也应当字符串
	if strings.Contains(t, "DECIMAL") || strings.Contains(t, "NUMERIC") ||
		t == "MONEY" || t == "NUMBER" {
		return true
	}
	return false
}

// scanRow 扫描一行数据，将特殊类型（大整数、DECIMAL、二进制）转换为 JSON 安全格式。
func scanRow(rows *sql.Rows, cols []string) ([]any, error) {
	values := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	typeNames := columnTypeNames(rows, len(cols))
	out := make([]any, len(cols))
	for i, v := range values {
		out[i] = normalizeValue(v, typeNames[i])
	}
	return out, nil
}

// normalizeValue 将数据库值转换为 JSON 安全格式。
// 大整数、DECIMAL 使用字符串传输，避免 JavaScript 精度丢失。
// 二进制字段以 base64 截断预览返回；文本列即使驱动以 []byte 返回也按字符串处理。
func normalizeValue(v any, dbTypeName string) any {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case string:
		return val
	case bool:
		return val
	case int8, int16, int32, int:
		return val
	case int64:
		// 超过 JavaScript 安全整数范围（2^53）用字符串
		if val > 9007199254740991 || val < -9007199254740991 {
			return fmt.Sprintf("%d", val)
		}
		return val
	case uint8, uint16, uint32:
		return val
	case uint64:
		if val > 9007199254740991 {
			return fmt.Sprintf("%d", val)
		}
		return val
	case float32, float64:
		return val
	case []byte:
		// MySQL 等驱动常以 []byte 返回 TEXT/VARCHAR/DECIMAL
		if isTextualDBType(dbTypeName) || (!isBinaryDBType(dbTypeName) && isLikelyUTF8Text(val)) {
			return string(val)
		}
		// 二进制：base64 截断预览（最多 256 字节）
		if len(val) > 256 {
			return map[string]any{
				"type":    "binary",
				"size":    len(val),
				"preview": base64.StdEncoding.EncodeToString(val[:256]),
			}
		}
		return map[string]any{
			"type":    "binary",
			"size":    len(val),
			"preview": base64.StdEncoding.EncodeToString(val),
		}
	case *big.Int:
		return val.String()
	case time.Time:
		return val.Format(time.RFC3339Nano)
	case sql.NullString:
		if val.Valid {
			return val.String
		}
		return nil
	case sql.NullInt64:
		if val.Valid {
			if val.Int64 > 9007199254740991 || val.Int64 < -9007199254740991 {
				return fmt.Sprintf("%d", val.Int64)
			}
			return val.Int64
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
			return val.Time.Format(time.RFC3339Nano)
		}
		return nil
	default:
		return fmt.Sprintf("%v", v)
	}
}

// isLikelyUTF8Text：类型未知时，若为合法 UTF-8 且无可打印控制字符则按文本处理。
func isLikelyUTF8Text(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	if !utf8.Valid(b) {
		return false
	}
	// 含过多 NUL 则更像二进制
	for _, c := range b {
		if c == 0 {
			return false
		}
	}
	return true
}

// Error 实现 error，保留稳定 Code 供 handler 透传（不再包一层丢失码）。
func (de DatabaseError) Error() string {
	if de.Message != "" {
		return de.Message
	}
	return de.Code
}

// toError 将 DatabaseError 转为 error；Code 为空时返回 nil。
func (de DatabaseError) toError() error {
	if de.Code == "" {
		return nil
	}
	e := de
	return e
}


