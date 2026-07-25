// SQL 语句分类：识别语句类型，只读模式下拒绝写操作。
//
// 设计文档第二节「只读模式」：
//   - 明确识别 INSERT/UPDATE/DELETE/MERGE/TRUNCATE/DDL/CALL/EXECUTE 时直接返回 403。
//   - 无法可靠分类的 SQL 在只读模式下一律拒绝。
//   - 检查器只用于单语句识别和用户提示，不作为最终安全边界。
//
// 设计文档第三节「SQL 执行规则」：
//   - 一次请求只允许一条 SQL；拒绝多语句。
//   - 显式 BEGIN/COMMIT/ROLLBACK/SAVEPOINT 一律拒绝。
package dataresource

import (
	"errors"
	"strings"
	"unicode"
)

// StatementType 语句类型。
type StatementType string

const (
	StmtSelect    StatementType = "SELECT"
	StmtInsert    StatementType = "INSERT"
	StmtUpdate    StatementType = "UPDATE"
	StmtDelete    StatementType = "DELETE"
	StmtMerge     StatementType = "MERGE"
	StmtTruncate  StatementType = "TRUNCATE"
	StmtDDL       StatementType = "DDL"   // CREATE/ALTER/DROP
	StmtDCL       StatementType = "DCL"   // GRANT/REVOKE
	StmtCall      StatementType = "CALL"  // CALL/EXECUTE
	StmtBegin     StatementType = "BEGIN" // 显式事务控制
	StmtCommit    StatementType = "COMMIT"
	StmtRollback  StatementType = "ROLLBACK"
	StmtSavepoint StatementType = "SAVEPOINT"
	StmtUnknown   StatementType = "UNKNOWN"
)

// IsReadOnly 返回语句是否为只读（SELECT）。
func (st StatementType) IsReadOnly() bool {
	return st == StmtSelect
}

// IsWrite 返回语句是否为写操作。
func (st StatementType) IsWrite() bool {
	switch st {
	case StmtInsert, StmtUpdate, StmtDelete, StmtMerge, StmtTruncate:
		return true
	}
	return false
}

// IsTransactionControl 返回是否为显式事务控制语句（含方言别名 END/ABORT）。
func (st StatementType) IsTransactionControl() bool {
	switch st {
	case StmtBegin, StmtCommit, StmtRollback, StmtSavepoint:
		return true
	}
	return false
}

// IsDDLorDCL 返回是否为 DDL/DCL。
func (st StatementType) IsDDLorDCL() bool {
	return st == StmtDDL || st == StmtDCL
}

// IsAutoCommitOnly 返回是否为只能自动提交模式执行的语句（DDL/DCL/TRUNCATE/过程调用）。
func (st StatementType) IsAutoCommitOnly() bool {
	return st == StmtDDL || st == StmtDCL || st == StmtTruncate || st == StmtCall
}

// IsDangerousWrite 返回是否为危险写操作（需要二次确认）。
// 设计文档：TRUNCATE、DROP、无 WHERE 的 DELETE/UPDATE 在读写模式也要求二次确认。
func (st StatementType) IsDangerousWrite(sqlText string) bool {
	switch st {
	case StmtTruncate:
		return true
	case StmtDDL:
		upper := strings.ToUpper(stripLeadingCommentsAndWhitespace(sqlText))
		return strings.HasPrefix(upper, "DROP")
	case StmtDelete, StmtUpdate:
		return !hasWhereClause(sqlText)
	}
	return false
}

// hasWhereClause 检查顶层 DELETE/UPDATE 是否带 WHERE（尽力而为，非完整解析）。
// 忽略括号内子查询中的 WHERE，避免 USING (SELECT … WHERE …) 绕过危险确认。
func hasWhereClause(sqlText string) bool {
	return hasTopLevelKeyword(sqlText, "WHERE")
}

// sqlHasTopLevelLimit 检查顶层是否已有 LIMIT（忽略子查询/括号内）。
// 用于 MySQL/达梦分页：无 LIMIT 时直接追加，避免子查询包装触发派生表重名列错误。
func sqlHasTopLevelLimit(sqlText string) bool {
	return hasTopLevelKeyword(sqlText, "LIMIT")
}

// hasTopLevelKeyword 在剥离字面量/注释后的 SQL 中查找顶层独立关键字。
func hasTopLevelKeyword(sqlText, keyword string) bool {
	upper := strings.ToUpper(stripStringLiteralsAndComments(sqlText))
	kw := strings.ToUpper(keyword)
	kwRunes := []rune(kw)
	kwLen := len(kwRunes)
	depth := 0
	runes := []rune(upper)
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			if depth != 0 {
				continue
			}
			if i+kwLen <= len(runes) && string(runes[i:i+kwLen]) == kw {
				beforeOK := i == 0 || !isIdentRune(runes[i-1])
				afterOK := i+kwLen >= len(runes) || !isIdentRune(runes[i+kwLen])
				if beforeOK && afterOK {
					return true
				}
			}
		}
	}
	return false
}

// stripStringLiteralsAndComments 移除 SQL 中的字符串字面量、注释与 dollar quote，避免误判关键字。
func stripStringLiteralsAndComments(sql string) string {
	var sb strings.Builder
	r := []rune(sql)
	i := 0
	for i < len(r) {
		c := r[i]
		switch {
		case c == '\'':
			i = skipSingleQuote(r, i)
		case c == '"':
			i = skipDoubleQuote(r, i)
		case c == '-' && i+1 < len(r) && r[i+1] == '-':
			i = skipLineComment(r, i)
		case c == '/' && i+1 < len(r) && r[i+1] == '*':
			i = skipBlockComment(r, i)
		case c == '$':
			if ni, ok := trySkipDollarQuote(r, i); ok {
				i = ni
				// dollar quote 内容整段丢弃
			} else {
				sb.WriteRune(c)
			}
		default:
			sb.WriteRune(c)
		}
		i++
	}
	return sb.String()
}

// containsSQLKeyword 在已清理文本中查找独立关键字（前后为非标识符字符）。
func containsSQLKeyword(upperCleaned, keyword string) bool {
	kw := strings.ToUpper(keyword)
	start := 0
	for {
		idx := strings.Index(upperCleaned[start:], kw)
		if idx < 0 {
			return false
		}
		abs := start + idx
		beforeOK := abs == 0 || !isIdentRune(rune(upperCleaned[abs-1]))
		after := abs + len(kw)
		afterOK := after >= len(upperCleaned) || !isIdentRune(rune(upperCleaned[after]))
		if beforeOK && afterOK {
			return true
		}
		start = abs + 1
	}
}

func isIdentRune(c rune) bool {
	return unicode.IsLetter(c) || unicode.IsDigit(c) || c == '_'
}

// classifyWithCTE 将 WITH 开头的语句分类。
// 数据修改 CTE（体内或最终语句含 INSERT/UPDATE/DELETE/MERGE/TRUNCATE）按写类型处理，
// 避免一律标 SELECT 导致危险确认/写审计旁路，或自动提交误走只读事务。
func classifyWithCTE(sql string) StatementType {
	cleaned := strings.ToUpper(stripStringLiteralsAndComments(sql))
	// 按出现顺序取第一个写关键字；均无则只读 CTE
	type hit struct {
		pos int
		t   StatementType
	}
	var best *hit
	for _, pair := range []struct {
		word string
		t    StatementType
	}{
		{"INSERT", StmtInsert},
		{"UPDATE", StmtUpdate},
		{"DELETE", StmtDelete},
		{"MERGE", StmtMerge},
		{"TRUNCATE", StmtTruncate},
	} {
		start := 0
		for {
			idx := strings.Index(cleaned[start:], pair.word)
			if idx < 0 {
				break
			}
			abs := start + idx
			beforeOK := abs == 0 || !isIdentRune(rune(cleaned[abs-1]))
			after := abs + len(pair.word)
			afterOK := after >= len(cleaned) || !isIdentRune(rune(cleaned[after]))
			if beforeOK && afterOK {
				if best == nil || abs < best.pos {
					best = &hit{pos: abs, t: pair.t}
				}
				break
			}
			start = abs + 1
		}
	}
	if best != nil {
		return best.t
	}
	return StmtSelect
}

// ClassifySQL 分析 SQL 文本，返回语句类型。
// 先去除前导注释和空白，再取首个关键字。
func ClassifySQL(sql string) StatementType {
	s := stripLeadingCommentsAndWhitespace(sql)
	if s == "" {
		return StmtUnknown
	}
	first := firstWord(s)
	upper := strings.ToUpper(first)

	switch upper {
	case "SELECT", "VALUES", "TABLE":
		return StmtSelect
	case "WITH":
		return classifyWithCTE(s)
	case "INSERT":
		return StmtInsert
	case "UPDATE":
		return StmtUpdate
	case "DELETE":
		return StmtDelete
	case "MERGE":
		return StmtMerge
	case "TRUNCATE":
		return StmtTruncate
	case "CREATE", "ALTER", "DROP", "RENAME", "COMMENT":
		return StmtDDL
	case "GRANT", "REVOKE":
		return StmtDCL
	case "CALL", "EXECUTE", "EXEC":
		return StmtCall
	case "BEGIN", "START":
		return StmtBegin
	case "COMMIT", "END":
		// PostgreSQL END ≡ COMMIT
		return StmtCommit
	case "ROLLBACK", "ABORT":
		// PostgreSQL ABORT ≡ ROLLBACK
		return StmtRollback
	case "SAVEPOINT":
		return StmtSavepoint
	}
	return StmtUnknown
}

// ValidateSingleStatement 校验 SQL 是否为单条语句（不含分号分隔的多语句）。
// 设计文档：一次请求只允许一条 SQL；服务端独立识别引号、注释和 PostgreSQL dollar quote。
// 另识别 MySQL 反引号标识符（`col` / “ `a;b` “），避免合法标识符被误判为多语句。
func ValidateSingleStatement(sql string) error {
	sql = strings.TrimSpace(sql)
	if sql == "" {
		return errors.New("SQL 不能为空")
	}
	// 扫描：跳过字符串字面量、注释、dollar quote、反引号，检测语句外的分号。
	rune := []rune(sql)
	i := 0
	for i < len(rune) {
		c := rune[i]
		switch {
		case c == '\'': // 单引号字符串
			i = skipSingleQuote(rune, i)
		case c == '"': // 双引号标识符
			i = skipDoubleQuote(rune, i)
		case c == '`': // MySQL 反引号标识符
			i = skipBacktick(rune, i)
		case c == '-' && i+1 < len(rune) && rune[i+1] == '-': // 行注释
			i = skipLineComment(rune, i)
		case c == '/' && i+1 < len(rune) && rune[i+1] == '*': // 块注释
			i = skipBlockComment(rune, i)
		case c == '$': // PG dollar quote：$tag$...$tag$（含 $$）
			if ni, ok := trySkipDollarQuote(rune, i); ok {
				i = ni
				continue
			}
			// 非 dollar quote（如 $1 参数）正常前进
		case c == ';': // 语句外分号
			// 分号后允许仅空白/注释（如 SELECT 1; -- note），有实质内容则视为多语句
			rest := stripLeadingCommentsAndWhitespace(string(rune[i+1:]))
			if rest != "" {
				return errors.New("一次请求只允许一条 SQL，检测到多语句")
			}
			return nil
		}
		i++
	}
	return nil
}

func stripLeadingCommentsAndWhitespace(sql string) string {
	r := []rune(sql)
	i := 0
	for i < len(r) {
		c := r[i]
		if unicode.IsSpace(c) {
			i++
			continue
		}
		if c == '-' && i+1 < len(r) && r[i+1] == '-' {
			// skipLineComment 停在 \n 或 len；EOF 行注释无换行时不得 i++ 越界
			i = skipLineComment(r, i)
			if i < len(r) {
				i++
			}
			continue
		}
		if c == '/' && i+1 < len(r) && r[i+1] == '*' {
			i = skipBlockComment(r, i)
			if i < len(r) {
				i++
			}
			continue
		}
		break
	}
	if i > len(r) {
		i = len(r)
	}
	return string(r[i:])
}

func firstWord(s string) string {
	var sb strings.Builder
	for _, c := range s {
		if unicode.IsLetter(c) || unicode.IsDigit(c) || c == '_' {
			sb.WriteRune(c)
		} else {
			break
		}
	}
	return sb.String()
}

func skipSingleQuote(r []rune, i int) int {
	i++ // 跳过开头 '
	for i < len(r) {
		if r[i] == '\'' {
			if i+1 < len(r) && r[i+1] == '\'' { // 转义的 ''
				i += 2
				continue
			}
			return i
		}
		i++
	}
	return i
}

func skipDoubleQuote(r []rune, i int) int {
	i++ // 跳过开头 "
	for i < len(r) {
		if r[i] == '"' {
			if i+1 < len(r) && r[i+1] == '"' {
				i += 2
				continue
			}
			return i
		}
		i++
	}
	return i
}

// skipBacktick 跳过 MySQL 反引号标识符：`name`；内部 “ 为转义反引号。
func skipBacktick(r []rune, i int) int {
	i++ // 跳过开头 `
	for i < len(r) {
		if r[i] == '`' {
			if i+1 < len(r) && r[i+1] == '`' {
				i += 2
				continue
			}
			return i
		}
		i++
	}
	return i
}

func skipLineComment(r []rune, i int) int {
	i += 2 // 跳过 --
	for i < len(r) {
		if r[i] == '\n' {
			return i
		}
		i++
	}
	return i
}

func skipBlockComment(r []rune, i int) int {
	i += 2 // 跳过 /*
	for i < len(r) {
		if r[i] == '*' && i+1 < len(r) && r[i+1] == '/' {
			return i + 1
		}
		i++
	}
	return i
}

// trySkipDollarQuote 识别 PostgreSQL dollar-quoted string：
//
//	$$...$$ 或 $tag$...$tag$
//
// tag 为可选标识符（字母/数字/下划线）。成功时返回闭合标签最后一个 rune 的下标。
// $1 等参数占位符返回 ok=false。
func trySkipDollarQuote(r []rune, i int) (int, bool) {
	if i >= len(r) || r[i] != '$' {
		return i, false
	}
	// 解析开标签：$[tag]$
	j := i + 1
	for j < len(r) && r[j] != '$' {
		c := r[j]
		if !(unicode.IsLetter(c) || unicode.IsDigit(c) || c == '_') {
			return i, false
		}
		j++
	}
	if j >= len(r) {
		// 无闭合的开标签（如 $1）
		return i, false
	}
	// r[j] 是开标签的结束 $
	tag := string(r[i : j+1])
	// 在正文中查找相同闭合标签
	k := j + 1
	tagRunes := []rune(tag)
	for k <= len(r)-len(tagRunes) {
		match := true
		for t := 0; t < len(tagRunes); t++ {
			if r[k+t] != tagRunes[t] {
				match = false
				break
			}
		}
		if match {
			return k + len(tagRunes) - 1, true
		}
		k++
	}
	// 未闭合：吞到末尾，避免后续误判
	return len(r) - 1, true
}
