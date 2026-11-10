// Package sqltext 解析数据集成 SQL 中的变量引用：
//
//	${name}  绑定参数：替换为方言占位符，值类型化绑定（默认、安全）
//	#{name}  受控文本替换：值必须是（可带一级 schema 前缀的）标识符，用于表名/schema 名
//	$${      转义，输出字面量 ${（#{ 同理用 ##{）
//
// 解析器跳过字符串字面量、引用标识符、注释与 PG dollar-quote，不会在其中误识别变量。
package sqltext

import (
	"fmt"
	"regexp"
	"strings"
)

// Options 描述方言词法差异。
type Options struct {
	BackslashEscapes bool // MySQL：字符串内反斜杠转义下一个字符
	HashComments     bool // MySQL：# 开始行注释（#{ 除外）
	DollarQuotes     bool // PostgreSQL / Kingbase：$tag$ … $tag$
}

// Ref 是一处变量引用。
type Ref struct {
	Name   string
	Raw    bool // true = #{} 文本替换
	Offset int  // 在原 SQL 中的字节偏移（界面定位用）
}

var nameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// rawValueRe 限定 #{} 可替换的值：标识符或 schema.标识符。
var rawValueRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*(\.[A-Za-z_][A-Za-z0-9_$]*)?$`)

// Scan 列出 SQL 中的变量引用（不渲染），用于设计期校验「引用了未声明变量」。
func Scan(sql string, opts Options) ([]Ref, error) {
	var refs []Ref
	_, err := walk(sql, opts, func(r Ref) (string, error) {
		refs = append(refs, r)
		return "", nil
	})
	return refs, err
}

// Resolver 返回变量的绑定值（已是逻辑类型的规范表示）。
type Resolver func(name string) (any, bool)

// Render 生成可执行 SQL 与按序绑定参数。placeholder 接收 1-based 序号。
func Render(sql string, opts Options, placeholder func(i int) string, resolve Resolver) (string, []any, error) {
	var args []any
	out, err := walk(sql, opts, func(r Ref) (string, error) {
		v, ok := resolve(r.Name)
		if !ok {
			return "", fmt.Errorf("SQL 引用了未定义的变量 %q", r.Name)
		}
		if r.Raw {
			s, isStr := v.(string)
			if !isStr || !rawValueRe.MatchString(s) {
				return "", fmt.Errorf("变量 %q 用于 #{} 文本替换，其值必须是标识符（如 orders 或 ods.orders），实际为 %q", r.Name, fmt.Sprint(v))
			}
			return s, nil
		}
		args = append(args, v)
		return placeholder(len(args)), nil
	})
	if err != nil {
		return "", nil, err
	}
	return out, args, nil
}

// walk 是单遍词法扫描：普通文本原样输出，变量引用交给 onRef 生成替换文本。
func walk(sql string, opts Options, onRef func(Ref) (string, error)) (string, error) {
	var b strings.Builder
	b.Grow(len(sql))
	n := len(sql)
	i := 0
	for i < n {
		c := sql[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			j := skipQuoted(sql, i, c, opts.BackslashEscapes && c != '`')
			b.WriteString(sql[i:j])
			i = j
		case c == '-' && i+1 < n && sql[i+1] == '-':
			j := strings.IndexByte(sql[i:], '\n')
			if j < 0 {
				j = n - i
			}
			b.WriteString(sql[i : i+j])
			i += j
		case c == '/' && i+1 < n && sql[i+1] == '*':
			j := strings.Index(sql[i+2:], "*/")
			end := n
			if j >= 0 {
				end = i + 2 + j + 2
			}
			b.WriteString(sql[i:end])
			i = end
		case (c == '$' || c == '#') && i+2 < n && sql[i+1] == c && sql[i+2] == '{':
			// 转义：$${ → ${，##{ → #{
			b.WriteByte(c)
			b.WriteByte('{')
			i += 3
		case (c == '$' || c == '#') && i+1 < n && sql[i+1] == '{':
			end := strings.IndexByte(sql[i+2:], '}')
			if end < 0 {
				return "", fmt.Errorf("第 %d 个字符处的 %c{ 缺少右花括号", i+1, c)
			}
			name := strings.TrimSpace(sql[i+2 : i+2+end])
			if !nameRe.MatchString(name) {
				return "", fmt.Errorf("变量名 %q 不合法（仅限字母、数字、下划线，且不以数字开头）", name)
			}
			rep, err := onRef(Ref{Name: name, Raw: c == '#', Offset: i})
			if err != nil {
				return "", err
			}
			b.WriteString(rep)
			i += 2 + end + 1
		case c == '#' && opts.HashComments:
			j := strings.IndexByte(sql[i:], '\n')
			if j < 0 {
				j = n - i
			}
			b.WriteString(sql[i : i+j])
			i += j
		case c == '$' && opts.DollarQuotes:
			if j, ok := skipDollarQuote(sql, i); ok {
				b.WriteString(sql[i:j])
				i = j
				continue
			}
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), nil
}

// skipQuoted 返回引用段结束后的下标；同字符连写（”）视为转义。未闭合时吞到结尾。
func skipQuoted(s string, i int, q byte, backslash bool) int {
	n := len(s)
	j := i + 1
	for j < n {
		switch {
		case backslash && s[j] == '\\':
			j += 2
		case s[j] == q:
			if j+1 < n && s[j+1] == q {
				j += 2
				continue
			}
			return j + 1
		default:
			j++
		}
	}
	return n
}

// skipDollarQuote 识别 $tag$…$tag$（tag 可为空）。不是 dollar-quote 返回 ok=false。
func skipDollarQuote(s string, i int) (int, bool) {
	n := len(s)
	j := i + 1
	for j < n && (s[j] == '_' || isAlnum(s[j])) {
		j++
	}
	if j >= n || s[j] != '$' {
		return 0, false
	}
	// tag 不能以数字开头（$1 是位置参数）
	if j > i+1 && s[i+1] >= '0' && s[i+1] <= '9' {
		return 0, false
	}
	tag := s[i : j+1]
	k := strings.Index(s[j+1:], tag)
	if k < 0 {
		return n, true
	}
	return j + 1 + k + len(tag), true
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
