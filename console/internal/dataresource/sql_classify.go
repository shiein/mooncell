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
	StmtDDL       StatementType = "DDL"       // CREATE/ALTER/DROP
	StmtDCL       StatementType = "DCL"       // GRANT/REVOKE
	StmtCall      StatementType = "CALL"      // CALL/EXECUTE
	StmtBegin     StatementType = "BEGIN"     // 显式事务控制
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

// IsTransactionControl 返回是否为显式事务控制语句。
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
	case "SELECT", "WITH", "VALUES", "TABLE":
		return StmtSelect
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
	case "COMMIT":
		return StmtCommit
	case "ROLLBACK":
		return StmtRollback
	case "SAVEPOINT":
		return StmtSavepoint
	}
	return StmtUnknown
}

// ValidateSingleStatement 校验 SQL 是否为单条语句（不含分号分隔的多语句）。
// 设计文档：一次请求只允许一条 SQL；服务端独立识别引号、注释和 PostgreSQL dollar quote。
func ValidateSingleStatement(sql string) error {
	sql = strings.TrimSpace(sql)
	if sql == "" {
		return errors.New("SQL 不能为空")
	}
	// 扫描：跳过字符串字面量、注释、dollar quote，检测语句外的分号。
	rune := []rune(sql)
	i := 0
	for i < len(rune) {
		c := rune[i]
		switch {
		case c == '\'': // 单引号字符串
			i = skipSingleQuote(rune, i)
		case c == '"': // 双引号标识符
			i = skipDoubleQuote(rune, i)
		case c == '-' && i+1 < len(rune) && rune[i+1] == '-': // 行注释
			i = skipLineComment(rune, i)
		case c == '/' && i+1 < len(rune) && rune[i+1] == '*': // 块注释
			i = skipBlockComment(rune, i)
		case c == '$' && i+1 < len(rune) && rune[i+1] == '$': // PG dollar quote
			i = skipDollarQuote(rune, i)
		case c == ';': // 语句外分号
			// 检查分号后是否还有非空白内容
			rest := strings.TrimSpace(string(rune[i+1:]))
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
			i = skipLineComment(r, i)
			i++
			continue
		}
		if c == '/' && i+1 < len(r) && r[i+1] == '*' {
			i = skipBlockComment(r, i)
			i++
			continue
		}
		break
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

func skipDollarQuote(r []rune, i int) int {
	i += 2 // 跳过 $$
	for i < len(r) {
		if r[i] == '$' && i+1 < len(r) && r[i+1] == '$' {
			return i + 1
		}
		i++
	}
	return i
}
