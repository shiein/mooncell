// Package core 是数据集成的领域内核：逻辑类型、行元数据、批次、步骤契约与注册表。
// 本包不依赖任何兄弟包，所有上层（pipeline/workflow/steps/conn）都只通过这里的类型协作。
package core

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Kind 是转换内部的逻辑类型。数据库原生类型只在输入/输出步骤与 Kind 互转（见 conn.Dialect）。
type Kind string

const (
	KindString    Kind = "string"
	KindInteger   Kind = "integer"
	KindNumber    Kind = "number"  // float64，近似值，不用于金额
	KindDecimal   Kind = "decimal" // decimal.Decimal，精确十进制
	KindBoolean   Kind = "boolean"
	KindDate      Kind = "date"      // time.Time，00:00:00 UTC
	KindTimestamp Kind = "timestamp" // time.Time
	KindBinary    Kind = "binary"    // []byte
)

// AllKinds 按界面展示顺序返回全部逻辑类型。
func AllKinds() []Kind {
	return []Kind{KindString, KindInteger, KindNumber, KindDecimal, KindBoolean, KindDate, KindTimestamp, KindBinary}
}

// Valid 报告 k 是否为已知类型。
func (k Kind) Valid() bool {
	switch k {
	case KindString, KindInteger, KindNumber, KindDecimal, KindBoolean, KindDate, KindTimestamp, KindBinary:
		return true
	}
	return false
}

// Label 返回中文展示名。
func (k Kind) Label() string {
	switch k {
	case KindString:
		return "字符串"
	case KindInteger:
		return "整数"
	case KindNumber:
		return "浮点数"
	case KindDecimal:
		return "精确小数"
	case KindBoolean:
		return "布尔"
	case KindDate:
		return "日期"
	case KindTimestamp:
		return "日期时间"
	case KindBinary:
		return "二进制"
	}
	return string(k)
}

// IsNumeric 报告是否数值类。
func (k Kind) IsNumeric() bool {
	return k == KindInteger || k == KindNumber || k == KindDecimal
}

// IsTemporal 报告是否时间类。
func (k Kind) IsTemporal() bool { return k == KindDate || k == KindTimestamp }

// ConvertError 是类型转换失败。引擎据此决定进错误输出还是使步骤失败。
type ConvertError struct {
	From  Kind
	To    Kind
	Value string // 截断后的原值文本，便于日志定位；不含二进制原文
	Err   error
}

func (e *ConvertError) Error() string {
	msg := fmt.Sprintf("无法将 %s 值 %q 转换为 %s", e.From.Label(), e.Value, e.To.Label())
	if e.Err != nil {
		msg += "：" + e.Err.Error()
	}
	return msg
}

func (e *ConvertError) Unwrap() error { return e.Err }

// ConvertOptions 控制字符串与时间互转。零值即合理默认。
type ConvertOptions struct {
	// Layout 是 Go 时间格式；为空时按常见格式依次尝试解析，格式化用 RFC3339 风格。
	Layout string
	// Location 解释无时区的时间文本；nil = time.Local。
	Location *time.Location
}

// KindOf 推断 Go 值对应的逻辑类型；nil 返回 ""。
func KindOf(v any) Kind {
	switch v.(type) {
	case nil:
		return ""
	case string:
		return KindString
	case int64, int, int32, int16, int8, uint32, uint16, uint8:
		return KindInteger
	case float64, float32:
		return KindNumber
	case decimal.Decimal:
		return KindDecimal
	case bool:
		return KindBoolean
	case time.Time:
		return KindTimestamp
	case []byte:
		return KindBinary
	}
	return ""
}

// Normalize 把宽松的 Go 值规整为 Kind 的规范表示（int→int64、float32→float64 等）。
// 已是规范表示时原样返回，不分配。
func Normalize(v any) any {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case int16:
		return int64(x)
	case int8:
		return int64(x)
	case uint32:
		return int64(x)
	case uint16:
		return int64(x)
	case uint8:
		return int64(x)
	case float32:
		return float64(x)
	}
	return v
}

// Convert 把 v 转为目标逻辑类型，是全模块唯一的类型转换入口。nil 恒为 nil。
func Convert(v any, to Kind, opts ConvertOptions) (any, error) {
	if v == nil {
		return nil, nil
	}
	v = Normalize(v)
	from := KindOf(v)
	if from == "" {
		return nil, &ConvertError{To: to, Value: fmt.Sprintf("%T", v), Err: errors.New("不支持的值类型")}
	}
	if from == to {
		if to == KindDate {
			return truncateDate(v.(time.Time)), nil
		}
		return v, nil
	}
	fail := func(err error) (any, error) {
		return nil, &ConvertError{From: from, To: to, Value: preview(v), Err: err}
	}
	switch to {
	case KindString:
		return formatString(v, opts), nil
	case KindInteger:
		switch x := v.(type) {
		case string:
			s := strings.TrimSpace(x)
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return n, nil
			}
			d, err := decimal.NewFromString(s)
			if err != nil {
				return fail(err)
			}
			return decimalToInt(d, fail)
		case float64:
			if math.IsNaN(x) || math.IsInf(x, 0) || x != math.Trunc(x) || x > math.MaxInt64 || x < math.MinInt64 {
				return fail(errors.New("不是整数或超出范围"))
			}
			return int64(x), nil
		case decimal.Decimal:
			return decimalToInt(x, fail)
		case bool:
			if x {
				return int64(1), nil
			}
			return int64(0), nil
		}
	case KindNumber:
		switch x := v.(type) {
		case string:
			f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
			if err != nil {
				return fail(err)
			}
			return f, nil
		case int64:
			return float64(x), nil
		case decimal.Decimal:
			f, _ := x.Float64()
			return f, nil
		case bool:
			if x {
				return 1.0, nil
			}
			return 0.0, nil
		}
	case KindDecimal:
		switch x := v.(type) {
		case string:
			d, err := decimal.NewFromString(strings.TrimSpace(x))
			if err != nil {
				return fail(err)
			}
			return d, nil
		case int64:
			return decimal.NewFromInt(x), nil
		case float64:
			if math.IsNaN(x) || math.IsInf(x, 0) {
				return fail(errors.New("NaN/Inf 不能转为精确小数"))
			}
			return decimal.NewFromFloat(x), nil
		case bool:
			if x {
				return decimal.NewFromInt(1), nil
			}
			return decimal.Zero, nil
		}
	case KindBoolean:
		switch x := v.(type) {
		case string:
			switch strings.ToLower(strings.TrimSpace(x)) {
			case "true", "t", "yes", "y", "1", "是":
				return true, nil
			case "false", "f", "no", "n", "0", "否":
				return false, nil
			}
			return fail(errors.New("无法识别的布尔文本"))
		case int64:
			return x != 0, nil
		case float64:
			return x != 0, nil
		case decimal.Decimal:
			return !x.IsZero(), nil
		}
	case KindDate, KindTimestamp:
		var t time.Time
		switch x := v.(type) {
		case string:
			parsed, err := ParseTime(x, opts)
			if err != nil {
				return fail(err)
			}
			t = parsed
		case time.Time:
			t = x
		case int64: // Unix 毫秒
			t = time.UnixMilli(x)
		default:
			return fail(errors.New("不支持的来源类型"))
		}
		if to == KindDate {
			return truncateDate(t), nil
		}
		return t, nil
	case KindBinary:
		switch x := v.(type) {
		case string:
			return []byte(x), nil
		}
	}
	return fail(errors.New("不支持的类型组合"))
}

func decimalToInt(d decimal.Decimal, fail func(error) (any, error)) (any, error) {
	if !d.Equal(d.Truncate(0)) {
		return fail(errors.New("含小数部分"))
	}
	if !d.BigInt().IsInt64() {
		return fail(errors.New("超出 64 位整数范围"))
	}
	return d.IntPart(), nil
}

func truncateDate(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// 常见时间文本格式，按从严到宽排列。
var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	"2006/01/02 15:04:05",
	"2006/01/02",
	"20060102150405",
	"20060102",
}

// ParseTime 按 opts.Layout（或常见格式）解析时间文本。
func ParseTime(s string, opts ConvertOptions) (time.Time, error) {
	s = strings.TrimSpace(s)
	loc := opts.Location
	if loc == nil {
		loc = time.Local
	}
	if opts.Layout != "" {
		return time.ParseInLocation(opts.Layout, s, loc)
	}
	for _, l := range timeLayouts {
		if t, err := time.ParseInLocation(l, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("无法识别的时间格式")
}

func formatString(v any, opts ConvertOptions) string {
	switch x := v.(type) {
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case decimal.Decimal:
		return x.String()
	case bool:
		if x {
			return "true"
		}
		return "false"
	case time.Time:
		if opts.Layout != "" {
			return x.Format(opts.Layout)
		}
		if x.Hour() == 0 && x.Minute() == 0 && x.Second() == 0 && x.Nanosecond() == 0 {
			return x.Format("2006-01-02")
		}
		return x.Format("2006-01-02 15:04:05")
	case []byte:
		return base64.StdEncoding.EncodeToString(x)
	}
	return fmt.Sprint(v)
}

// FormatValue 把逻辑值转为界面/日志展示文本（NULL → "<null>"）。
func FormatValue(v any) string {
	if v == nil {
		return "<null>"
	}
	return formatString(Normalize(v), ConvertOptions{})
}

// FormatFieldValue 按字段元数据格式化展示文本：decimal 字段声明了 Scale 时
// 按该标度补齐尾零（12.50 不显示为 12.5）；值本身不做任何舍入改写。
func FormatFieldValue(v any, f Field) string {
	if v == nil {
		return "<null>"
	}
	if f.Kind == KindDecimal && f.Scale > 0 {
		if d, ok := Normalize(v).(decimal.Decimal); ok {
			return d.StringFixed(int32(f.Scale))
		}
	}
	return FormatValue(v)
}

func preview(v any) string {
	if b, ok := v.([]byte); ok {
		return fmt.Sprintf("<%d 字节>", len(b))
	}
	s := FormatValue(v)
	if r := []rune(s); len(r) > 64 {
		return string(r[:64]) + "…"
	}
	return s
}
