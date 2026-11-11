// 方言感知的 SQL 词法扫描与语句拆分。
//
// 与前端 src/lib/sql-split.js 是同一份规则的两种实现，共享用例 testdata/sql_split_cases.json：
//   - 前端用它把脚本拆成多条语句逐条提交（后端仍坚持「一次请求一条语句」）；
//   - 后端用它校验单语句、剥离末尾注释/分号后再做分页/计数包装。
//
// 词法规则（只认 ASCII 分隔符，按字节扫描对 UTF-8 安全）：
//   - 行注释 --；MySQL 额外认 #；块注释 /* */
//   - 单引号字符串 ” 转义；MySQL 额外认反斜杠转义；PG 仅 E'…' 认反斜杠转义
//   - 双引号 "" 转义（MySQL 双引号为字符串，同样认反斜杠）
//   - 反引号标识符：MySQL（generic 兼容旧行为也认）
//   - dollar quote $tag$…$tag$：PG/Kingbase（generic 也认）
//
// 语句边界：
//   - 顶层分号；MySQL 支持客户端指令 DELIMITER xx（指令行本身不属于任何语句）；
//   - 达梦支持单独一行的 / 结束当前语句（PL/SQL 惯例）；
//   - 过程体（块模式）：MySQL 的 CREATE PROCEDURE/FUNCTION/TRIGGER/EVENT，
//     达梦的 CREATE PROCEDURE/FUNCTION/TRIGGER/PACKAGE/TYPE 及 BEGIN…END / DECLARE 匿名块，
//     体内分号不切分，按 BEGIN/CASE…END 深度判断结束。
package dataresource

import (
	"errors"
	"strings"
)

// sqlDialect 决定词法细节与块模式规则。
type sqlDialect int

const (
	// dialectGeneric 兼容旧的方言无关扫描（反引号 + dollar quote，无 # 注释、无反斜杠转义）。
	dialectGeneric sqlDialect = iota
	dialectPG
	dialectMySQL
	dialectDM
)

func (d sqlDialect) String() string {
	switch d {
	case dialectPG:
		return "pg"
	case dialectMySQL:
		return "mysql"
	case dialectDM:
		return "dm"
	}
	return "generic"
}

// sqlDialectAware 由真实适配器实现，告知执行层使用哪种方言规则。
type sqlDialectAware interface {
	sqlDialect() sqlDialect
}

// dialectOf 取适配器方言；未实现（测试替身）时退回 generic。
func dialectOf(a DataSourceAdapter) sqlDialect {
	if da, ok := a.(sqlDialectAware); ok {
		return da.sqlDialect()
	}
	return dialectGeneric
}

type sqlLexOpts struct {
	hashComment    bool // MySQL # 行注释
	backslashQuote bool // 普通 '…' / "…" 内反斜杠转义（MySQL）
	eString        bool // PG E'…' 内反斜杠转义
	backtick       bool
	dollarQuote    bool
}

func lexOptsFor(d sqlDialect) sqlLexOpts {
	switch d {
	case dialectMySQL:
		return sqlLexOpts{hashComment: true, backslashQuote: true, backtick: true}
	case dialectPG:
		return sqlLexOpts{eString: true, dollarQuote: true}
	case dialectDM:
		return sqlLexOpts{}
	}
	return sqlLexOpts{backtick: true, dollarQuote: true}
}

type sqlTokKind int

const (
	tokSpace sqlTokKind = iota
	tokComment
	tokString // 字符串、引号标识符、dollar quote
	tokWord   // 标识符/关键字/数字
	tokPunct  // 其它单字符
)

type sqlTok struct {
	kind       sqlTokKind
	start, end int // [start, end) 字节偏移
}

func isWordByte(c byte) bool {
	return c == '_' || c == '$' || c == '#' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isWordStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c >= 0x80
}

// skipQuoted 从 s[i]（开引号）扫描到闭引号之后；doubled 允许 ” 形式转义，backslash 允许 \x。
func skipQuoted(s string, i int, q byte, backslash bool) int {
	i++
	for i < len(s) {
		c := s[i]
		if backslash && c == '\\' {
			i += 2
			continue
		}
		if c == q {
			if i+1 < len(s) && s[i+1] == q {
				i += 2
				continue
			}
			return i + 1
		}
		i++
	}
	return len(s)
}

// dollarTagAt 识别 $tag$ 开标签（tag 为空或标识符，不以数字开头），返回标签文本。
func dollarTagAt(s string, i int) (string, bool) {
	if i > 0 && isWordByte(s[i-1]) {
		return "", false // a$b$ 之类是标识符的一部分
	}
	j := i + 1
	for j < len(s) && s[j] != '$' {
		c := s[j]
		if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80 || (j > i+1 && c >= '0' && c <= '9')) {
			return "", false
		}
		j++
	}
	if j >= len(s) {
		return "", false
	}
	return s[i : j+1], true
}

// nextSQLToken 返回从 i 开始的一个词法单元。
func nextSQLToken(s string, i int, o sqlLexOpts) sqlTok {
	c := s[i]
	switch {
	case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
		j := i + 1
		for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r' || s[j] == '\f' || s[j] == '\v') {
			j++
		}
		return sqlTok{tokSpace, i, j}
	case c == '-' && i+1 < len(s) && s[i+1] == '-', o.hashComment && c == '#':
		j := strings.IndexByte(s[i:], '\n')
		if j < 0 {
			return sqlTok{tokComment, i, len(s)}
		}
		return sqlTok{tokComment, i, i + j}
	case c == '/' && i+1 < len(s) && s[i+1] == '*':
		j := strings.Index(s[i+2:], "*/")
		if j < 0 {
			return sqlTok{tokComment, i, len(s)}
		}
		return sqlTok{tokComment, i, i + 2 + j + 2}
	case c == '\'':
		bs := o.backslashQuote
		if !bs && o.eString && i > 0 && (s[i-1] == 'E' || s[i-1] == 'e') && (i < 2 || !isWordByte(s[i-2])) {
			bs = true
		}
		return sqlTok{tokString, i, skipQuoted(s, i, '\'', bs)}
	case c == '"':
		return sqlTok{tokString, i, skipQuoted(s, i, '"', o.backslashQuote)}
	case c == '`' && o.backtick:
		return sqlTok{tokString, i, skipQuoted(s, i, '`', false)}
	case c == '$' && o.dollarQuote:
		if tag, ok := dollarTagAt(s, i); ok {
			body := i + len(tag)
			k := strings.Index(s[body:], tag)
			if k < 0 {
				return sqlTok{tokString, i, len(s)}
			}
			return sqlTok{tokString, i, body + k + len(tag)}
		}
		return sqlTok{tokPunct, i, i + 1}
	case isWordStart(c):
		j := i + 1
		for j < len(s) && isWordByte(s[j]) {
			j++
		}
		return sqlTok{tokWord, i, j}
	}
	return sqlTok{tokPunct, i, i + 1}
}

// sqlStatement 是拆分出的一条语句；SQL 为去掉首尾空白、不含 / 或自定义分隔符的原文（保留结尾分号）。
type sqlStatement struct {
	From, To int
	SQL      string
}

// blockKind 块模式类型。
type blockKind int

const (
	blockNone      blockKind = iota
	blockBody                // 按 BEGIN/CASE…END 深度结束
	blockSlashOnly           // 仅 / 或文本结尾结束（达梦 PACKAGE BODY / TYPE BODY）
)

// detectBlockMode 根据语句开头的关键字序列判断是否进入块模式。
func detectBlockMode(words []string, d sqlDialect) (blockKind, bool) {
	if len(words) == 0 {
		return blockNone, false
	}
	// decided=false 表示还需要更多关键字才能判定
	i := 0
	switch d {
	case dialectMySQL:
		if words[0] != "CREATE" {
			return blockNone, true
		}
		i = 1
		for i < len(words) {
			w := words[i]
			switch w {
			case "OR", "REPLACE", "AGGREGATE", "DEFINER":
				i++
				continue
			case "PROCEDURE", "FUNCTION", "TRIGGER", "EVENT":
				return blockBody, true
			}
			// DEFINER = user@host 中的值：跳过非关键字词
			if i >= 2 && words[i-1] == "DEFINER" {
				i++
				continue
			}
			if containsWord([]string{"TABLE", "VIEW", "INDEX", "UNIQUE", "DATABASE", "SCHEMA", "USER", "ROLE", "TEMPORARY", "ALGORITHM", "SQL", "SPATIAL", "FULLTEXT", "TABLESPACE", "SERVER", "LOGFILE", "RESOURCE"}, w) {
				return blockNone, true
			}
			i++
			if i > 8 {
				return blockNone, true
			}
		}
		return blockNone, false
	case dialectDM:
		switch words[0] {
		case "DECLARE":
			return blockBody, true
		case "BEGIN":
			if len(words) < 2 {
				return blockNone, false
			}
			if words[1] == "WORK" || words[1] == "TRANSACTION" || words[1] == "TRAN" {
				return blockNone, true
			}
			return blockBody, true
		case "CREATE":
			i = 1
			for i < len(words) {
				switch words[i] {
				case "OR", "REPLACE", "EDITIONABLE", "NONEDITIONABLE":
					i++
					continue
				case "PROCEDURE", "FUNCTION", "TRIGGER":
					return blockBody, true
				case "PACKAGE", "TYPE", "CLASS":
					if i+1 < len(words) {
						if words[i+1] == "BODY" {
							return blockSlashOnly, true
						}
						return blockBody, true
					}
					return blockNone, false
				}
				return blockNone, true
			}
			return blockNone, false
		}
		return blockNone, true
	}
	return blockNone, true
}

func containsWord(list []string, w string) bool {
	for _, x := range list {
		if x == w {
			return true
		}
	}
	return false
}

// splitSQLStatements 按方言把脚本拆成语句。只含空白/注释/分号的片段被丢弃。
func splitSQLStatements(s string, d sqlDialect) []sqlStatement {
	o := lexOptsFor(d)
	var out []sqlStatement
	delim := ";"
	stmtStart := -1 // 当前语句首个代码字节；-1 表示尚未开始
	lastCodeEnd := 0
	var words []string // 语句开头的关键字（判定块模式用）
	mode := blockNone
	modeDecided := false
	depth := 0
	sawEnd := false
	// 刚处理过 END：下一个词若是 IF/LOOP/WHILE/REPEAT/FOR 说明它闭合的结构未计层，需撤销
	pendingEnd := false
	endDecremented := false
	prevSawEnd := false

	reset := func() {
		stmtStart = -1
		words = words[:0]
		mode = blockNone
		modeDecided = false
		depth = 0
		sawEnd = false
		pendingEnd = false
	}
	emit := func(end int) {
		if stmtStart >= 0 && end > stmtStart {
			raw := s[stmtStart:end]
			text := strings.TrimSpace(raw)
			if text != "" && text != ";" {
				out = append(out, sqlStatement{From: stmtStart, To: stmtStart + len(strings.TrimRight(raw, " \t\r\n\f\v")), SQL: text})
			}
		}
		reset()
	}
	atLineStart := func(i int) bool {
		j := i - 1
		for j >= 0 && (s[j] == ' ' || s[j] == '\t') {
			j--
		}
		return j < 0 || s[j] == '\n' || s[j] == '\r'
	}
	lineEnd := func(i int) int {
		j := strings.IndexByte(s[i:], '\n')
		if j < 0 {
			return len(s)
		}
		return i + j
	}
	applyBlockWord := func(w string) {
		if pendingEnd {
			pendingEnd = false
			switch w {
			case "IF", "LOOP", "WHILE", "REPEAT", "FOR":
				if endDecremented {
					depth++
				}
				sawEnd = prevSawEnd
				return
			case "CASE":
				return // END CASE：-1 正确，CASE 不再开层
			}
		}
		switch w {
		case "BEGIN", "CASE":
			depth++
		case "END":
			prevSawEnd = sawEnd
			endDecremented = depth > 0
			if depth > 0 {
				depth--
			}
			sawEnd = depth == 0
			pendingEnd = true
		}
	}

	i := 0
	for i < len(s) {
		// MySQL DELIMITER 指令：只在语句开头、行首识别
		if d == dialectMySQL && stmtStart < 0 && atLineStart(i) && len(s)-i >= 10 && strings.EqualFold(s[i:i+9], "DELIMITER") && (s[i+9] == ' ' || s[i+9] == '\t') {
			le := lineEnd(i)
			if nd := strings.TrimSpace(s[i+9 : le]); nd != "" {
				delim = nd
			}
			i = le
			continue
		}
		// 达梦：单独一行的 / 结束当前语句
		if d == dialectDM && s[i] == '/' && atLineStart(i) {
			le := lineEnd(i)
			if strings.TrimSpace(s[i+1:le]) == "" {
				emit(lastCodeEnd)
				i = le
				continue
			}
		}
		// 自定义分隔符（非 ;）
		if delim != ";" && strings.HasPrefix(s[i:], delim) {
			emit(lastCodeEnd)
			i += len(delim)
			continue
		}
		t := nextSQLToken(s, i, o)
		if delim != ";" && (t.kind == tokWord || t.kind == tokPunct) {
			// END$$ 之类：自定义分隔符可能紧贴在词尾
			if k := strings.Index(s[t.start:t.end], delim); k > 0 {
				t.end = t.start + k
			}
		}
		i = t.end
		if t.kind == tokSpace || t.kind == tokComment {
			continue
		}
		if t.kind == tokPunct && s[t.start] == ';' && delim == ";" {
			if stmtStart < 0 {
				continue // 孤立分号
			}
			if mode == blockNone || (mode == blockBody && depth == 0 && (d == dialectMySQL || sawEnd)) {
				lastCodeEnd = t.end
				emit(t.end)
				continue
			}
		}
		if stmtStart < 0 {
			stmtStart = t.start
		}
		lastCodeEnd = t.end
		if t.kind != tokWord {
			pendingEnd = false
			continue
		}
		w := strings.ToUpper(s[t.start:t.end])
		if !modeDecided {
			words = append(words, w)
			mode, modeDecided = detectBlockMode(words, d)
			if mode == blockBody {
				// 判定前已读过的开头词（如达梦 BEGIN 匿名块的 BEGIN）补计深度
				for _, pw := range words[:len(words)-1] {
					applyBlockWord(pw)
				}
			}
		}
		if mode == blockBody {
			applyBlockWord(w)
		}
	}
	if stmtStart >= 0 {
		emit(lastCodeEnd)
	}
	return out
}

// errMultiStatement 是多语句拒绝的统一错误。
var errMultiStatement = errors.New("一次请求只允许一条 SQL，检测到多语句；执行脚本请用「执行全部」")

// validateSingleStatementFor 按方言校验单语句（过程体内的分号不算多语句）。
func validateSingleStatementFor(sqlText string, d sqlDialect) error {
	if strings.TrimSpace(sqlText) == "" {
		return errors.New("SQL 不能为空")
	}
	stmts := splitSQLStatements(sqlText, d)
	if len(stmts) == 0 {
		return errors.New("SQL 不能为空")
	}
	if len(stmts) > 1 {
		return errMultiStatement
	}
	return nil
}

// trimSQLForWrap 去掉末尾的空白、注释与分号（可重复出现），返回可安全放进子查询或追加 LIMIT 的正文。
// 例如 "SELECT 1; -- note" → "SELECT 1"。
func trimSQLForWrap(sqlText string, d sqlDialect) string {
	o := lexOptsFor(d)
	lastEnd := 0
	i := 0
	for i < len(sqlText) {
		t := nextSQLToken(sqlText, i, o)
		if t.kind != tokSpace && t.kind != tokComment && !(t.kind == tokPunct && sqlText[t.start] == ';') {
			lastEnd = t.end
		}
		i = t.end
	}
	return sqlText[:lastEnd]
}

// leadingSQLWords 返回去掉注释后的前 n 个关键字（大写）；遇到非词的代码单元即停止。
// 前导左括号会被跳过（(SELECT …) UNION …）。
func leadingSQLWords(sqlText string, d sqlDialect, n int) []string {
	o := lexOptsFor(d)
	var out []string
	i := 0
	for i < len(sqlText) && len(out) < n {
		t := nextSQLToken(sqlText, i, o)
		i = t.end
		switch t.kind {
		case tokSpace, tokComment:
			continue
		case tokWord:
			out = append(out, strings.ToUpper(sqlText[t.start:t.end]))
			continue
		case tokPunct:
			if sqlText[t.start] == '(' && len(out) == 0 {
				continue
			}
		}
		break
	}
	return out
}

// sqlCodeOnly 用空格替换字符串与注释，保留代码（大写），供关键字扫描。
func sqlCodeOnly(sqlText string, o sqlLexOpts) string {
	var sb strings.Builder
	i := 0
	for i < len(sqlText) {
		t := nextSQLToken(sqlText, i, o)
		switch t.kind {
		case tokString, tokComment:
			sb.WriteByte(' ')
		default:
			sb.WriteString(sqlText[t.start:t.end])
		}
		i = t.end
	}
	return strings.ToUpper(sb.String())
}
