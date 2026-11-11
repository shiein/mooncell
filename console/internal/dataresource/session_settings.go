// 会话设置：SET/RESET 白名单的解析、记录与重放。
//
// 工作台的每条语句都从连接池另取连接（自动提交各开一个事务），数据库会话状态天然不保留。
// 这里不把工作台钉死在一条物理连接上（导出/就地编辑/取消都会打破钉连接的假设），
// 而是把用户执行过的白名单 SET 记录下来，在该工作台每个事务开头重放：
//   - 查询、计数、写入、手工事务、全量导出、就地编辑都经 ws.Adapter.Begin，自动带上；
//   - PostgreSQL/Kingbase 以 SET LOCAL 重放，事务结束即失效，不污染池中连接；
//   - MySQL/达梦的会话变量不随事务回滚，重放值写到池连接上，但每次都会再次重放，结果一致。
//
// 只记录幂等的字面量赋值：重放会在每个事务开头重新求值，NOW()、子查询、@x+1 之类会漂移，一律拒绝。
// 临时表、预编译语句等其它会话状态仍不跨语句保留。
package dataresource

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"
)

// maxSessionSettings 单个工作台最多记录的设置条数。
const maxSessionSettings = 16

// sessionSetting 是一条已记录的设置。
type sessionSetting struct {
	Key     string // 去重键（变量名小写）
	Display string // 展示给用户的语句
	Replay  string // 事务内重放（PG 为 SET LOCAL）
	Session string // 事务外（NoTx 路径独占连接）重放
}

// sessionChange 是一条 SET/RESET 语句解析后的变更。
type sessionChange struct {
	Set      []sessionSetting
	ResetKey []string // RESET name / SET name TO DEFAULT
	ResetAll bool
}

type sessionState struct {
	mu    sync.Mutex
	items []sessionSetting
}

func (s *sessionState) snapshot() []sessionSetting {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sessionSetting(nil), s.items...)
}

func (s *sessionState) displays() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.items))
	for _, it := range s.items {
		out = append(out, it.Display)
	}
	return out
}

func (s *sessionState) clear() {
	s.mu.Lock()
	s.items = nil
	s.mu.Unlock()
}

// apply 合并变更：同名覆盖（后者生效），超过上限报错且不修改。
func (s *sessionState) apply(ch *sessionChange) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch.ResetAll {
		s.items = nil
		return nil
	}
	next := make([]sessionSetting, 0, len(s.items)+len(ch.Set))
	drop := map[string]bool{}
	for _, k := range ch.ResetKey {
		drop[k] = true
	}
	for _, it := range ch.Set {
		drop[it.Key] = true
	}
	for _, it := range s.items {
		if !drop[it.Key] {
			next = append(next, it)
		}
	}
	next = append(next, ch.Set...)
	if len(next) > maxSessionSettings {
		return fmt.Errorf("会话设置最多保留 %d 项，请先重置不再需要的设置", maxSessionSettings)
	}
	s.items = next
	return nil
}

// sessionAdapter 包装工作台适配器：每次 Begin 后按序重放已记录的设置。
type sessionAdapter struct {
	DataSourceAdapter
	state *sessionState
}

func newSessionAdapter(inner DataSourceAdapter) *sessionAdapter {
	return &sessionAdapter{DataSourceAdapter: inner, state: &sessionState{}}
}

func (a *sessionAdapter) Begin(ctx context.Context, readOnly bool) (Transaction, error) {
	tx, err := a.DataSourceAdapter.Begin(ctx, readOnly)
	if err != nil {
		return nil, err
	}
	for _, it := range a.state.snapshot() {
		if _, err := tx.Exec(ctx, it.Replay); err != nil {
			_ = tx.Rollback()
			return nil, &APIError{Code: "SESSION_SETTING_FAILED", Message: fmt.Sprintf("重放会话设置「%s」失败：%s；可在工具栏重置会话设置", it.Display, sanitizeSessionErr(a.DataSourceAdapter, err))}
		}
	}
	return tx, nil
}

func (a *sessionAdapter) sqlDialect() sqlDialect { return dialectOf(a.DataSourceAdapter) }

func (a *sessionAdapter) rawDB() *sql.DB {
	if r, ok := a.DataSourceAdapter.(rawDBAdapter); ok {
		return r.rawDB()
	}
	return nil
}

func sanitizeSessionErr(a DataSourceAdapter, err error) string {
	if de := a.NormalizeError(err); de.Message != "" {
		return de.Message
	}
	return err.Error()
}

// rawDBAdapter 由真实适配器实现：事务外执行（VACUUM 等）需要独占连接。
type rawDBAdapter interface {
	rawDB() *sql.DB
}

// execOutsideTx 在独占连接上（不开事务）重放会话设置并执行语句，用后丢弃连接，避免会话状态回流连接池。
func execOutsideTx(ctx context.Context, a DataSourceAdapter, settings []sessionSetting, sqlText string) (int64, error) {
	r, ok := a.(rawDBAdapter)
	if !ok || r.rawDB() == nil {
		return 0, &APIError{Code: "UNSUPPORTED", Message: "该数据源不支持事务外执行"}
	}
	conn, err := r.rawDB().Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer func() {
		// 返回 ErrBadConn 让 database/sql 关闭而不是归还该连接
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		_ = conn.Close()
	}()
	for _, it := range settings {
		if _, err := conn.ExecContext(ctx, it.Session); err != nil {
			return 0, fmt.Errorf("重放会话设置「%s」失败: %w", it.Display, err)
		}
	}
	res, err := conn.ExecContext(ctx, sqlText)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ---- 解析 ----

type setTok struct {
	kind sqlTokKind
	text string // 原文
	up   string // 大写（词）
}

func setTokens(sqlText string, d sqlDialect) []setTok {
	o := lexOptsFor(d)
	code := trimSQLForWrap(sqlText, d)
	var out []setTok
	i := 0
	for i < len(code) {
		t := nextSQLToken(code, i, o)
		i = t.end
		if t.kind == tokSpace || t.kind == tokComment {
			continue
		}
		txt := code[t.start:t.end]
		out = append(out, setTok{kind: t.kind, text: txt, up: strings.ToUpper(txt)})
	}
	return out
}

// isLiteralTok 判断是否为可安全重放的字面量单元（字符串、数字、标识符/关键字）。
func isLiteralTok(t setTok) bool {
	switch t.kind {
	case tokString, tokWord:
		return true
	}
	return false
}

// splitCommaGroups 按逗号切分 token 序列；出现非逗号标点时返回 false。
func splitCommaGroups(ts []setTok, allowSign bool) ([][]setTok, bool) {
	var groups [][]setTok
	var cur []setTok
	for i := 0; i < len(ts); i++ {
		t := ts[i]
		if t.kind == tokPunct {
			switch {
			case t.text == ",":
				if len(cur) == 0 {
					return nil, false
				}
				groups = append(groups, cur)
				cur = nil
				continue
			case allowSign && (t.text == "-" || t.text == "+") && len(cur) == 0 && i+1 < len(ts) && ts[i+1].kind == tokWord:
				cur = append(cur, t)
				continue
			case t.text == "." && len(cur) > 0 && i+1 < len(ts) && ts[i+1].kind == tokWord:
				cur = append(cur, t) // 小数 1.5 被词法拆成 1 . 5
				continue
			}
			return nil, false
		}
		if !isLiteralTok(t) {
			return nil, false
		}
		cur = append(cur, t)
	}
	if len(cur) == 0 {
		return nil, false
	}
	return append(groups, cur), true
}

func joinToks(ts []setTok) string {
	var sb strings.Builder
	for i, t := range ts {
		if i > 0 && t.text != "." && ts[i-1].text != "." && ts[i-1].text != "-" && ts[i-1].text != "+" {
			sb.WriteByte(' ')
		}
		sb.WriteString(t.text)
	}
	return sb.String()
}

func joinGroups(gs [][]setTok) string {
	parts := make([]string, len(gs))
	for i, g := range gs {
		parts[i] = joinToks(g)
	}
	return strings.Join(parts, ", ")
}

// parseSessionChange 解析 SET/RESET；返回非空 reason 表示拒绝。
func parseSessionChange(sqlText string, d sqlDialect) (*sessionChange, string) {
	ts := setTokens(sqlText, d)
	switch d {
	case dialectMySQL:
		return parseMySQLSet(ts)
	case dialectDM:
		return parseDMSet(ts)
	case dialectPG:
		return parsePGSet(ts)
	}
	return nil, "当前数据源不支持会话设置语句"
}

// PG/Kingbase 白名单：变量名 → 规范名
var pgSettable = map[string]string{
	"SEARCH_PATH":   "search_path",
	"TIMEZONE":      "timezone",
	"DATESTYLE":     "datestyle",
	"INTERVALSTYLE": "intervalstyle",
}

const pgSetHint = "仅支持 SET search_path / TIME ZONE / datestyle / intervalstyle（字面量取值）与对应的 RESET"

func parsePGSet(ts []setTok) (*sessionChange, string) {
	if len(ts) < 2 {
		return nil, pgSetHint
	}
	if ts[0].up == "RESET" {
		if len(ts) == 2 && ts[1].up == "ALL" {
			return &sessionChange{ResetAll: true}, ""
		}
		name := ""
		switch {
		case len(ts) == 3 && ts[1].up == "TIME" && ts[2].up == "ZONE":
			name = "timezone"
		case len(ts) == 2:
			name = pgSettable[ts[1].up]
		}
		if name == "" {
			return nil, pgSetHint
		}
		return &sessionChange{ResetKey: []string{name}}, ""
	}
	rest := ts[1:]
	if rest[0].up == "SESSION" {
		if len(rest) > 1 && (rest[1].up == "AUTHORIZATION" || rest[1].up == "CHARACTERISTICS") {
			return nil, "不允许切换会话身份或事务特性"
		}
		rest = rest[1:]
	}
	if len(rest) > 0 && rest[0].up == "LOCAL" {
		return nil, "SET LOCAL 只在当前事务内有效；工作台按会话记录设置，请去掉 LOCAL"
	}
	if len(rest) == 0 {
		return nil, pgSetHint
	}
	var name string
	var vals []setTok
	switch {
	case len(rest) >= 3 && rest[0].up == "TIME" && rest[1].up == "ZONE":
		name, vals = "timezone", rest[2:]
	case rest[0].up == "ROLE" || rest[0].up == "TRANSACTION" || rest[0].up == "CONSTRAINTS":
		return nil, "不允许 SET ROLE / TRANSACTION / CONSTRAINTS：事务与身份由工作台管理"
	default:
		name = pgSettable[rest[0].up]
		if name == "" || len(rest) < 3 || (rest[1].up != "TO" && rest[1].text != "=") {
			return nil, pgSetHint
		}
		vals = rest[2:]
	}
	groups, ok := splitCommaGroups(vals, true)
	if !ok {
		return nil, "设置值只能是字面量（字符串、数字或标识符），不能是表达式"
	}
	if len(groups) == 1 && len(groups[0]) == 1 && groups[0][0].up == "DEFAULT" {
		return &sessionChange{ResetKey: []string{name}}, ""
	}
	value := joinGroups(groups)
	target := name + " TO "
	if name == "timezone" {
		target = "TIME ZONE "
	}
	return &sessionChange{Set: []sessionSetting{{
		Key:     name,
		Display: "SET " + target + value,
		Replay:  "SET LOCAL " + target + value,
		Session: "SET " + target + value,
	}}}, ""
}

const mysqlSetHint = "仅支持 SET @变量 = 字面量 与 SET time_zone = '…'"

func parseMySQLSet(ts []setTok) (*sessionChange, string) {
	if len(ts) < 2 || ts[0].up != "SET" {
		return nil, mysqlSetHint
	}
	// 按顶层逗号切分赋值
	var assigns [][]setTok
	var cur []setTok
	for _, t := range ts[1:] {
		if t.kind == tokPunct && t.text == "," {
			assigns = append(assigns, cur)
			cur = nil
			continue
		}
		cur = append(cur, t)
	}
	assigns = append(assigns, cur)
	ch := &sessionChange{}
	for _, a := range assigns {
		eq := -1
		for i, t := range a {
			if t.kind == tokPunct && (t.text == "=" || t.text == ":") {
				eq = i
				break
			}
		}
		if eq <= 0 || eq+1 >= len(a) {
			return nil, mysqlSetHint
		}
		lhs := a[:eq]
		rhs := a[eq+1:]
		if a[eq].text == ":" { // :=
			if len(rhs) == 0 || rhs[0].text != "=" {
				return nil, mysqlSetHint
			}
			rhs = rhs[1:]
		}
		key, display, ok := mysqlSetTarget(lhs)
		if !ok {
			return nil, mysqlSetHint
		}
		lit, isDefault, ok := mysqlLiteral(rhs)
		if !ok {
			return nil, "设置值只能是字面量（字符串、数字、NULL、TRUE/FALSE），不能引用变量或函数"
		}
		if isDefault {
			if key != "time_zone" {
				return nil, mysqlSetHint
			}
			ch.ResetKey = append(ch.ResetKey, key)
			continue
		}
		stmt := "SET " + display + " = " + lit
		ch.Set = append(ch.Set, sessionSetting{Key: key, Display: stmt, Replay: stmt, Session: stmt})
	}
	return ch, ""
}

// mysqlSetTarget 识别 @var、time_zone、SESSION time_zone、@@time_zone、@@session.time_zone。
func mysqlSetTarget(lhs []setTok) (key, display string, ok bool) {
	if len(lhs) == 2 && lhs[0].text == "@" && (lhs[1].kind == tokWord || lhs[1].kind == tokString) {
		name := lhs[1].text
		return "@" + strings.ToLower(strings.Trim(name, "`'\"")), "@" + name, true
	}
	var parts []string
	for _, t := range lhs {
		parts = append(parts, t.up)
	}
	switch strings.Join(parts, " ") {
	case "TIME_ZONE", "SESSION TIME_ZONE", "LOCAL TIME_ZONE", "@ @ TIME_ZONE", "@ @ SESSION . TIME_ZONE", "@ @ LOCAL . TIME_ZONE":
		return "time_zone", "time_zone", true
	}
	return "", "", false
}

func mysqlLiteral(rhs []setTok) (lit string, isDefault, ok bool) {
	switch {
	case len(rhs) == 1 && rhs[0].kind == tokString && !strings.HasPrefix(rhs[0].text, "`"):
		return rhs[0].text, false, true
	case len(rhs) == 1 && rhs[0].kind == tokWord:
		switch rhs[0].up {
		case "NULL", "TRUE", "FALSE":
			return rhs[0].up, false, true
		case "DEFAULT":
			return "", true, true
		}
	}
	if num, ok := numericLiteral(rhs); ok {
		return num, false, true
	}
	return "", false, false
}

// numericLiteral 识别 [+-]digits[.digits]（词法会把它拆成多个单元）。
func numericLiteral(ts []setTok) (string, bool) {
	i := 0
	sign := ""
	if i < len(ts) && ts[i].kind == tokPunct && (ts[i].text == "-" || ts[i].text == "+") {
		sign = ts[i].text
		i++
	}
	if i >= len(ts) || !isNumericWord(ts[i].text) {
		return "", false
	}
	num := sign + ts[i].text
	i++
	if i+1 < len(ts) && ts[i].text == "." && isNumericWord(ts[i+1].text) {
		num += "." + ts[i+1].text
		i += 2
	}
	if i != len(ts) {
		return "", false
	}
	return num, true
}

func isNumericWord(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

const dmSetHint = "仅支持 SET SCHEMA 模式名 与 SET TIME ZONE '…'"

func parseDMSet(ts []setTok) (*sessionChange, string) {
	if len(ts) < 2 || ts[0].up != "SET" {
		return nil, dmSetHint
	}
	switch {
	case len(ts) == 3 && ts[1].up == "SCHEMA" && (ts[2].kind == tokWord || (ts[2].kind == tokString && strings.HasPrefix(ts[2].text, `"`))):
		stmt := "SET SCHEMA " + ts[2].text
		return &sessionChange{Set: []sessionSetting{{Key: "schema", Display: stmt, Replay: stmt, Session: stmt}}}, ""
	case len(ts) == 4 && ts[1].up == "TIME" && ts[2].up == "ZONE" && ts[3].kind == tokString && strings.HasPrefix(ts[3].text, "'"):
		stmt := "SET TIME ZONE " + ts[3].text
		return &sessionChange{Set: []sessionSetting{{Key: "timezone", Display: stmt, Replay: stmt, Session: stmt}}}, ""
	}
	return nil, dmSetHint
}
