// 单表 SELECT 可编辑性检测：仅简单「单表 FROM」可就地改删（Navicat 同类约束）。
// 多表 JOIN/UNION/子查询 FROM 一律不可编辑；无主键时也不开放改删。
package dataresource

import (
	"strings"
	"unicode"
)

// EditableInfo 附在 SELECT 结果上，供前端开启就地编辑。
type EditableInfo struct {
	Schema      string   `json:"schema,omitempty"`
	Table       string   `json:"table"`
	PrimaryKeys []string `json:"primaryKeys"`
	// Reason 非空表示检测到单表但因故不可编辑（如无主键）。
	Reason string `json:"reason,omitempty"`
}

// EditableTarget 是从 SELECT 解析出的单表目标。
type EditableTarget struct {
	Schema string
	Table  string
}

// DetectEditableSelect 判断 SQL 是否为可安全就地编辑的单表 SELECT。
// 最小可靠策略（避免投影映射错误）：
//   - 必须是 SELECT * / table.* 从单表（允许 WHERE/ORDER/LIMIT）
//   - 禁止 JOIN/UNION/CTE/子查询 FROM/显式列清单/聚合
// 主键是否齐全在 attach 时结合结果列再校验。
func DetectEditableSelect(sqlText string) (EditableTarget, bool) {
	if ClassifySQL(sqlText) != StmtSelect {
		return EditableTarget{}, false
	}
	// 去掉末尾分号
	s := strings.TrimSpace(sqlText)
	if strings.HasSuffix(s, ";") {
		s = strings.TrimSpace(s[:len(s)-1])
	}
	upperScan := stripSQLLiteralsForKeywordScan(s)
	// 多表 / 集合操作 / CTE
	for _, kw := range []string{" JOIN ", " UNION ", " INTERSECT ", " EXCEPT ", " CROSS JOIN "} {
		if strings.Contains(upperScan, kw) {
			return EditableTarget{}, false
		}
	}
	if strings.HasPrefix(strings.TrimLeftFunc(upperScan, unicode.IsSpace), "WITH ") {
		return EditableTarget{}, false
	}
	// 仅允许 SELECT * 或 SELECT alias.*
	if !isSelectStarProjection(upperScan) {
		return EditableTarget{}, false
	}

	fromIdx := indexKeyword(upperScan, "FROM")
	if fromIdx < 0 {
		return EditableTarget{}, false
	}
	// FROM 之后到 WHERE/GROUP/ORDER/LIMIT/HAVING/WINDOW/OFFSET/FETCH 之前
	rest := s[fromIdx+4:]
	restUpper := upperScan[fromIdx+4:]
	endRel := len(restUpper)
	for _, kw := range []string{" WHERE ", " GROUP ", " HAVING ", " ORDER ", " LIMIT ", " OFFSET ", " FETCH ", " WINDOW ", " FOR "} {
		if i := strings.Index(restUpper, kw); i >= 0 && i < endRel {
			endRel = i
		}
	}
	fromClause := strings.TrimSpace(rest[:endRel])
	fromUpper := strings.TrimSpace(restUpper[:endRel])
	if strings.Contains(fromUpper, ",") {
		return EditableTarget{}, false
	}
	if strings.HasPrefix(strings.TrimSpace(fromClause), "(") {
		return EditableTarget{}, false
	}
	// 有 GROUP BY 的 * 仍是聚合场景，禁止
	if strings.Contains(upperScan, " GROUP ") {
		return EditableTarget{}, false
	}
	tablePart := firstTableRef(fromClause)
	if tablePart == "" {
		return EditableTarget{}, false
	}
	schema, table := splitQualifiedName(tablePart)
	if table == "" {
		return EditableTarget{}, false
	}
	return EditableTarget{Schema: schema, Table: table}, true
}

// isSelectStarProjection 要求 SELECT 与 FROM 之间仅为 * 或 ident.*（可有 DISTINCT）。
func isSelectStarProjection(upperScan string) bool {
	fromIdx := indexKeyword(upperScan, "FROM")
	if fromIdx < 0 {
		return false
	}
	head := strings.TrimSpace(upperScan[:fromIdx])
	if !strings.HasPrefix(head, "SELECT") {
		return false
	}
	proj := strings.TrimSpace(head[len("SELECT"):])
	if strings.HasPrefix(proj, "DISTINCT") {
		proj = strings.TrimSpace(proj[len("DISTINCT"):])
	} else if strings.HasPrefix(proj, "ALL") {
		proj = strings.TrimSpace(proj[len("ALL"):])
	}
	if proj == "*" {
		return true
	}
	// t.* / SCHEMA.T.*
	if strings.HasSuffix(proj, ".*") && !strings.Contains(proj, ",") {
		return true
	}
	return false
}

// stripSQLLiteralsForKeywordScan 将字符串/注释替换为空格，便于大写关键字扫描；保留长度对齐。
func stripSQLLiteralsForKeywordScan(sql string) string {
	r := []rune(sql)
	out := make([]rune, len(r))
	copy(out, r)
	i := 0
	for i < len(r) {
		switch {
		case r[i] == '\'':
			j := skipSingleQuote(r, i)
			for k := i; k <= j && k < len(out); k++ {
				out[k] = ' '
			}
			i = j + 1
		case r[i] == '"':
			j := skipDoubleQuote(r, i)
			for k := i; k <= j && k < len(out); k++ {
				out[k] = ' '
			}
			i = j + 1
		case r[i] == '`':
			j := skipBacktick(r, i)
			for k := i; k <= j && k < len(out); k++ {
				out[k] = ' '
			}
			i = j + 1
		case r[i] == '-' && i+1 < len(r) && r[i+1] == '-':
			j := skipLineComment(r, i)
			for k := i; k <= j && k < len(out); k++ {
				out[k] = ' '
			}
			i = j + 1
		case r[i] == '/' && i+1 < len(r) && r[i+1] == '*':
			j := skipBlockComment(r, i)
			for k := i; k <= j && k < len(out); k++ {
				out[k] = ' '
			}
			i = j + 1
		case r[i] == '$':
			if ni, ok := trySkipDollarQuote(r, i); ok {
				for k := i; k <= ni && k < len(out); k++ {
					out[k] = ' '
				}
				i = ni + 1
				continue
			}
			i++
		default:
			i++
		}
	}
	return strings.ToUpper(string(out))
}

func indexKeyword(upperSQL, kw string) int {
	// 找独立关键字
	kw = strings.ToUpper(kw)
	padded := " " + upperSQL + " "
	idx := strings.Index(padded, " "+kw+" ")
	if idx < 0 {
		return -1
	}
	return idx // padded 多一个前导空格，刚好对应 upperSQL 下标
}

// firstTableRef 取 FROM 子句第一个表引用，去掉 AS alias。
func firstTableRef(fromClause string) string {
	s := strings.TrimSpace(fromClause)
	if s == "" {
		return ""
	}
	// 读第一个标识符或 schema.table（可带引号）
	r := []rune(s)
	i := 0
	readIdent := func() (string, int) {
		if i >= len(r) {
			return "", i
		}
		switch r[i] {
		case '"':
			j := skipDoubleQuote(r, i)
			return string(r[i : j+1]), j + 1
		case '`':
			j := skipBacktick(r, i)
			return string(r[i : j+1]), j + 1
		case '[':
			j := i + 1
			for j < len(r) && r[j] != ']' {
				j++
			}
			if j < len(r) {
				j++
			}
			return string(r[i:j]), j
		default:
			j := i
			for j < len(r) && (unicode.IsLetter(r[j]) || unicode.IsDigit(r[j]) || r[j] == '_' || r[j] == '$') {
				j++
			}
			return string(r[i:j]), j
		}
	}
	part1, ni := readIdent()
	if part1 == "" {
		return ""
	}
	i = ni
	for i < len(r) && unicode.IsSpace(r[i]) {
		i++
	}
	if i < len(r) && r[i] == '.' {
		i++
		for i < len(r) && unicode.IsSpace(r[i]) {
			i++
		}
		part2, nj := func() (string, int) {
			if i >= len(r) {
				return "", i
			}
			switch r[i] {
			case '"':
				j := skipDoubleQuote(r, i)
				return string(r[i : j+1]), j + 1
			case '`':
				j := skipBacktick(r, i)
				return string(r[i : j+1]), j + 1
			default:
				j := i
				for j < len(r) && (unicode.IsLetter(r[j]) || unicode.IsDigit(r[j]) || r[j] == '_' || r[j] == '$') {
					j++
				}
				return string(r[i:j]), j
			}
		}()
		if part2 == "" {
			return unquoteIdent(part1)
		}
		i = nj
		return part1 + "." + part2
	}
	return part1
}

func splitQualifiedName(ref string) (schema, table string) {
	// 处理 "a"."b" / a.b / `a`.`b`
	parts := splitIdentParts(ref)
	if len(parts) == 1 {
		return "", unquoteIdent(parts[0])
	}
	if len(parts) >= 2 {
		return unquoteIdent(parts[0]), unquoteIdent(parts[1])
	}
	return "", ""
}

func splitIdentParts(ref string) []string {
	r := []rune(strings.TrimSpace(ref))
	var parts []string
	i := 0
	for i < len(r) {
		for i < len(r) && unicode.IsSpace(r[i]) {
			i++
		}
		if i >= len(r) {
			break
		}
		if r[i] == '.' {
			i++
			continue
		}
		switch r[i] {
		case '"':
			j := skipDoubleQuote(r, i)
			parts = append(parts, string(r[i:j+1]))
			i = j + 1
		case '`':
			j := skipBacktick(r, i)
			parts = append(parts, string(r[i:j+1]))
			i = j + 1
		default:
			j := i
			for j < len(r) && r[j] != '.' && !unicode.IsSpace(r[j]) {
				j++
			}
			parts = append(parts, string(r[i:j]))
			i = j
		}
	}
	return parts
}

func unquoteIdent(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '`' && s[len(s)-1] == '`') {
			inner := s[1 : len(s)-1]
			if s[0] == '"' {
				return strings.ReplaceAll(inner, `""`, `"`)
			}
			return strings.ReplaceAll(inner, "``", "`")
		}
	}
	return s
}

// primaryKeyColumns 从结构描述中提取主键列名（顺序保持约束定义）。
func primaryKeyColumns(structure ObjectStructure) []string {
	for _, c := range structure.Constraints {
		if c.Type == "primary" && len(c.Columns) > 0 {
			return append([]string(nil), c.Columns...)
		}
	}
	return nil
}
