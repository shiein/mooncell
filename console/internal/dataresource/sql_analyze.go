// 方言感知的语句分析：在 ClassifySQL 的类型之上，给出执行路径需要的附加信息。
//
//   - Passthrough：返回结果集但不能被包装成分页/计数子查询的语句（SHOW、DESC、EXPLAIN），
//     直接执行并按行数上限读取；
//   - NoTx：PostgreSQL 系不能在事务块内执行的维护语句（VACUUM、CONCURRENTLY、ALTER SYSTEM、
//     CREATE/DROP DATABASE/TABLESPACE），走事务外路径；
//   - Session：SET/RESET 白名单（见 session_settings.go）；
//   - Reject：可识别但明确不支持的语句，给出可操作提示（如 USE 切库）。
//
// EXPLAIN 按内层语句分类：内层只读或未带 ANALYZE 时只生成计划，按只读处理；
// 带 ANALYZE 且内层为写操作时会真实执行，按内层写类型鉴权/危险确认/审计。
package dataresource

import "strings"

// SQLAnalysis 是 analyzeSQL 的结果。
type SQLAnalysis struct {
	Type        StatementType
	Passthrough bool
	NoTx        bool
	Session     *sessionChange
	Reject      string
}

// explainStarters 是 EXPLAIN/DESCRIBE 之后内层语句可能的开头关键字。
var explainStarters = map[string]bool{
	"SELECT": true, "WITH": true, "INSERT": true, "UPDATE": true, "DELETE": true, "REPLACE": true,
	"MERGE": true, "VALUES": true, "TABLE": true, "CREATE": true, "EXECUTE": true, "DECLARE": true,
}

// startsWithParen 判断去注释后的首个代码字符是否为左括号。
func startsWithParen(sqlText string, d sqlDialect) bool {
	o := lexOptsFor(d)
	i := 0
	for i < len(sqlText) {
		t := nextSQLToken(sqlText, i, o)
		i = t.end
		if t.kind == tokSpace || t.kind == tokComment {
			continue
		}
		return t.kind == tokPunct && sqlText[t.start] == '('
	}
	return false
}

// analyzeSQL 按方言分析单条语句。
func analyzeSQL(sqlText string, d sqlDialect) SQLAnalysis {
	words := leadingSQLWords(sqlText, d, 6)
	if len(words) == 0 {
		return SQLAnalysis{Type: StmtUnknown}
	}
	first := words[0]
	second := ""
	if len(words) > 1 {
		second = words[1]
	}
	if startsWithParen(sqlText, d) {
		// (SELECT …) UNION (SELECT …)：只接受查询类开头
		switch first {
		case "SELECT", "WITH", "VALUES", "TABLE":
		default:
			return SQLAnalysis{Type: StmtUnknown}
		}
	}

	switch first {
	case "SELECT", "VALUES", "TABLE":
		return SQLAnalysis{Type: StmtSelect}
	case "WITH":
		return SQLAnalysis{Type: classifyWithCTE(stripLeadingCommentsAndWhitespace(sqlText))}
	case "INSERT", "REPLACE":
		return SQLAnalysis{Type: StmtInsert}
	case "UPDATE":
		return SQLAnalysis{Type: StmtUpdate}
	case "DELETE":
		return SQLAnalysis{Type: StmtDelete}
	case "MERGE":
		return SQLAnalysis{Type: StmtMerge}
	case "TRUNCATE":
		return SQLAnalysis{Type: StmtTruncate}
	case "CREATE", "ALTER", "DROP", "RENAME", "COMMENT":
		return SQLAnalysis{Type: StmtDDL, NoTx: pgNeedsNoTx(sqlText, words, d)}
	case "VACUUM":
		return SQLAnalysis{Type: StmtDDL, NoTx: d == dialectPG || d == dialectGeneric}
	case "ANALYZE", "ANALYSE", "REINDEX", "CLUSTER", "REFRESH", "OPTIMIZE":
		return SQLAnalysis{Type: StmtDDL, NoTx: pgNeedsNoTx(sqlText, words, d)}
	case "GRANT", "REVOKE":
		return SQLAnalysis{Type: StmtDCL}
	case "CALL", "EXECUTE", "EXEC":
		return SQLAnalysis{Type: StmtCall}
	case "DO":
		if d == dialectPG || d == dialectGeneric {
			return SQLAnalysis{Type: StmtCall} // PG 匿名块
		}
		return SQLAnalysis{Type: StmtUnknown}
	case "DECLARE":
		if d == dialectDM {
			return SQLAnalysis{Type: StmtCall} // 达梦 DECLARE…BEGIN…END 匿名块
		}
		return SQLAnalysis{Type: StmtUnknown}
	case "BEGIN":
		if d == dialectDM && second != "" && second != "WORK" && second != "TRANSACTION" && second != "TRAN" {
			return SQLAnalysis{Type: StmtCall} // 达梦 BEGIN…END 匿名块
		}
		return SQLAnalysis{Type: StmtBegin}
	case "START":
		return SQLAnalysis{Type: StmtBegin}
	case "COMMIT", "END":
		return SQLAnalysis{Type: StmtCommit} // PostgreSQL END ≡ COMMIT
	case "ROLLBACK", "ABORT":
		return SQLAnalysis{Type: StmtRollback} // PostgreSQL ABORT ≡ ROLLBACK
	case "SAVEPOINT", "RELEASE":
		return SQLAnalysis{Type: StmtSavepoint}
	case "SHOW":
		return SQLAnalysis{Type: StmtSelect, Passthrough: true}
	case "EXPLAIN":
		if d == dialectDM {
			// 实测：达梦驱动对 EXPLAIN 返回「非查询SQL语句」，计划不以结果集返回
			return SQLAnalysis{Type: StmtUnknown, Reject: "达梦不以结果集返回执行计划，工作台无法展示 EXPLAIN；请在达梦管理工具/disql 中查看"}
		}
		return analyzeExplain(sqlText, d)
	case "DESC", "DESCRIBE":
		if d == dialectDM {
			return SQLAnalysis{Type: StmtUnknown, Reject: "DESC 是 disql 客户端命令，达梦服务端不支持；请在左侧对象树右键「查看表结构」"}
		}
		if explainStarters[second] || second == "FORMAT" || second == "ANALYZE" || second == "EXTENDED" || second == "PARTITIONS" {
			return analyzeExplain(sqlText, d) // MySQL：DESCRIBE SELECT … ≡ EXPLAIN
		}
		return SQLAnalysis{Type: StmtSelect, Passthrough: true}
	case "USE":
		return SQLAnalysis{Type: StmtUnknown, Reject: "数据资源绑定单一数据库，不支持 USE 切换；跨库访问请写 库名.表名，或由管理员另建资源"}
	case "SET", "RESET":
		ch, reason := parseSessionChange(sqlText, d)
		if reason != "" {
			return SQLAnalysis{Type: StmtSession, Reject: reason}
		}
		return SQLAnalysis{Type: StmtSession, Session: ch}
	}
	return SQLAnalysis{Type: StmtUnknown}
}

// analyzeExplain 找到 EXPLAIN 的内层语句并按其类型分类。
func analyzeExplain(sqlText string, d sqlDialect) SQLAnalysis {
	o := lexOptsFor(d)
	i := 0
	seenHead := false
	depth := 0
	analyze := false
	for i < len(sqlText) {
		t := nextSQLToken(sqlText, i, o)
		i = t.end
		switch t.kind {
		case tokSpace, tokComment, tokString:
			continue
		case tokPunct:
			switch sqlText[t.start] {
			case '(':
				depth++
			case ')':
				if depth > 0 {
					depth--
				}
			}
			continue
		}
		w := strings.ToUpper(sqlText[t.start:t.end])
		if !seenHead {
			seenHead = true // EXPLAIN / DESC / DESCRIBE
			continue
		}
		if w == "ANALYZE" || w == "ANALYSE" {
			analyze = true // 含 PG (ANALYZE false)：保守按 ANALYZE 处理
			continue
		}
		if depth == 0 && explainStarters[w] {
			inner := analyzeSQL(sqlText[t.start:], d)
			switch {
			case inner.Type == StmtSelect:
				return SQLAnalysis{Type: StmtSelect, Passthrough: true}
			case inner.Type == StmtUnknown || inner.Reject != "":
				return SQLAnalysis{Type: StmtUnknown, Reject: inner.Reject}
			case analyze:
				// EXPLAIN ANALYZE 会真实执行内层写操作
				return SQLAnalysis{Type: inner.Type, Passthrough: true}
			default:
				return SQLAnalysis{Type: StmtSelect, Passthrough: true}
			}
		}
	}
	// EXPLAIN tbl（MySQL ≡ DESCRIBE）、EXPLAIN FOR CONNECTION n 等
	return SQLAnalysis{Type: StmtSelect, Passthrough: true}
}

// pgNeedsNoTx 识别 PostgreSQL 系不能在事务块中执行的语句。
func pgNeedsNoTx(sqlText string, words []string, d sqlDialect) bool {
	if d != dialectPG && d != dialectGeneric {
		return false
	}
	if len(words) >= 2 {
		switch words[0] + " " + words[1] {
		case "ALTER SYSTEM", "CREATE DATABASE", "DROP DATABASE", "CREATE TABLESPACE", "DROP TABLESPACE":
			return true
		}
	}
	code := sqlCodeOnly(sqlText, lexOptsFor(d))
	return containsSQLKeyword(code, "CONCURRENTLY")
}

// isDangerousWriteFor 是方言感知的危险写判定。
// MySQL 同时按「认反斜杠转义」与「不认」（NO_BACKSLASH_ESCAPES）两种词法检查 WHERE，
// 任一词法下看不到顶层 WHERE 即要求确认：宁可多确认，不可漏确认。
func isDangerousWriteFor(st StatementType, sqlText string, d sqlDialect) bool {
	switch st {
	case StmtTruncate:
		return true
	case StmtDDL:
		words := leadingSQLWords(sqlText, d, 1)
		return len(words) == 1 && words[0] == "DROP"
	case StmtDelete, StmtUpdate:
		lexes := []sqlLexOpts{lexOptsFor(d)}
		if d == dialectMySQL {
			alt := lexOptsFor(d)
			alt.backslashQuote = false
			lexes = append(lexes, alt)
		}
		for _, o := range lexes {
			if !topLevelWord(sqlCodeOnly(sqlText, o), "WHERE") {
				return true
			}
		}
		return false
	}
	return false
}

// topLevelWord 在已去除字符串/注释的大写代码中查找括号深度为 0 的独立关键字。
func topLevelWord(code, kw string) bool {
	depth := 0
	for i := 0; i < len(code); i++ {
		switch code[i] {
		case '(':
			depth++
			continue
		case ')':
			if depth > 0 {
				depth--
			}
			continue
		}
		if depth != 0 || !strings.HasPrefix(code[i:], kw) {
			continue
		}
		before := i == 0 || !isWordByte(code[i-1])
		after := i+len(kw) >= len(code) || !isWordByte(code[i+len(kw)])
		if before && after {
			return true
		}
	}
	return false
}
