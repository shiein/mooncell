package dataresource

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

type splitCase struct {
	Name    string   `json:"name"`
	Dialect string   `json:"dialect"`
	SQL     string   `json:"sql"`
	Expect  []string `json:"expect"`
}

func parseDialect(t *testing.T, s string) sqlDialect {
	switch s {
	case "pg":
		return dialectPG
	case "mysql":
		return dialectMySQL
	case "dm":
		return dialectDM
	case "generic":
		return dialectGeneric
	}
	t.Fatalf("未知方言 %q", s)
	return dialectGeneric
}

// TestSplitSQLSharedCases 与前端 sql-split.test.js 共用同一份用例，保证前后端拆分一致。
func TestSplitSQLSharedCases(t *testing.T) {
	raw, err := os.ReadFile("testdata/sql_split_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []splitCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, c := range doc.Cases {
		d := parseDialect(t, c.Dialect)
		stmts := splitSQLStatements(c.SQL, d)
		got := make([]string, 0, len(stmts))
		for _, s := range stmts {
			got = append(got, s.SQL)
			// From/To 必须能还原原文（去尾空白）
			if c.SQL[s.From:s.To] != s.SQL {
				t.Errorf("[%s] 偏移不一致: %q vs %q", c.Name, c.SQL[s.From:s.To], s.SQL)
			}
		}
		if !reflect.DeepEqual(got, c.Expect) {
			t.Errorf("[%s] 拆分结果\n got: %q\nwant: %q", c.Name, got, c.Expect)
		}
		// 每条拆出的语句单独提交时必须通过后端单语句校验
		for _, s := range c.Expect {
			if err := validateSingleStatementFor(s, d); err != nil {
				t.Errorf("[%s] 单条 %q 应通过单语句校验: %v", c.Name, s, err)
			}
		}
	}
}

func TestTrimSQLForWrap(t *testing.T) {
	cases := []struct {
		d    sqlDialect
		in   string
		want string
	}{
		{dialectPG, "SELECT 1 -- note", "SELECT 1"},
		{dialectPG, "SELECT 1; -- note\n /* x */ ;", "SELECT 1"},
		{dialectPG, "SELECT '--x' AS a", "SELECT '--x' AS a"},
		{dialectPG, "SELECT 5 # 3", "SELECT 5 # 3"},
		{dialectMySQL, "SELECT 1 # note", "SELECT 1"},
		{dialectMySQL, "SELECT 'a\\';' ;", "SELECT 'a\\';'"},
		{dialectDM, "  SELECT 1 FROM DUAL;  ", "  SELECT 1 FROM DUAL"},
	}
	for _, c := range cases {
		if got := trimSQLForWrap(c.in, c.d); got != c.want {
			t.Errorf("trimSQLForWrap(%v, %q) = %q, want %q", c.d, c.in, got, c.want)
		}
	}
}

func TestValidateSingleStatementForDialects(t *testing.T) {
	cases := []struct {
		d  sqlDialect
		in string
		ok bool
	}{
		// PG：# 是运算符，后面的语句必须被识别为第二条
		{dialectPG, "SELECT 1 # 2; DROP TABLE x", false},
		// MySQL：# 注释内的分号不算
		{dialectMySQL, "SELECT 1 # a;b", true},
		{dialectMySQL, "SELECT 'it\\'s; ok'", true},
		// PG 普通字符串不认反斜杠转义：'a\' 已闭合，后面是第二条
		{dialectPG, "SELECT 'a\\'; SELECT 2", false},
		{dialectMySQL, "CREATE PROCEDURE p() BEGIN SELECT 1; SELECT 2; END", true},
		{dialectMySQL, "CREATE PROCEDURE p() BEGIN SELECT 1; END; DROP TABLE t", false},
		{dialectDM, "BEGIN INSERT INTO t VALUES (1); END;", true},
		{dialectDM, "BEGIN INSERT INTO t VALUES (1); END; DELETE FROM t", false},
		{dialectGeneric, "SELECT `a;b` FROM t", true},
	}
	for _, c := range cases {
		err := validateSingleStatementFor(c.in, c.d)
		if (err == nil) != c.ok {
			t.Errorf("validateSingleStatementFor(%v, %q) ok=%v, err=%v", c.d, c.in, c.ok, err)
		}
	}
}
