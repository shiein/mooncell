package dataresource

import (
	"strings"
	"testing"
)

func TestAnalyzeSQLStatementKinds(t *testing.T) {
	cases := []struct {
		d           sqlDialect
		sql         string
		typ         StatementType
		passthrough bool
		noTx        bool
		reject      bool
	}{
		{dialectPG, "EXPLAIN SELECT 1", StmtSelect, true, false, false},
		{dialectPG, "EXPLAIN ANALYZE SELECT 1", StmtSelect, true, false, false},
		{dialectPG, "EXPLAIN (ANALYZE, BUFFERS) DELETE FROM t", StmtDelete, true, false, false},
		{dialectPG, "EXPLAIN (ANALYZE false) UPDATE t SET a = 1", StmtUpdate, true, false, false},
		{dialectPG, "EXPLAIN DELETE FROM t", StmtSelect, true, false, false},
		{dialectPG, "explain analyse verbose insert into t values (1)", StmtInsert, true, false, false},
		{dialectMySQL, "EXPLAIN FORMAT=JSON SELECT * FROM t", StmtSelect, true, false, false},
		{dialectMySQL, "DESC t", StmtSelect, true, false, false},
		{dialectMySQL, "DESCRIBE SELECT 1", StmtSelect, true, false, false},
		{dialectMySQL, "SHOW TABLES", StmtSelect, true, false, false},
		{dialectPG, "SHOW search_path", StmtSelect, true, false, false},
		{dialectPG, "(SELECT 1) UNION (SELECT 2)", StmtSelect, false, false, false},
		{dialectPG, "(DELETE FROM t)", StmtUnknown, false, false, false},
		{dialectMySQL, "REPLACE INTO t VALUES (1)", StmtInsert, false, false, false},
		{dialectPG, "DO $$BEGIN PERFORM 1; END$$", StmtCall, false, false, false},
		{dialectPG, "VACUUM t", StmtDDL, false, true, false},
		{dialectPG, "CREATE INDEX CONCURRENTLY i ON t(a)", StmtDDL, false, true, false},
		{dialectPG, "ALTER SYSTEM SET work_mem = '64MB'", StmtDDL, false, true, false},
		{dialectPG, "ANALYZE t", StmtDDL, false, false, false},
		{dialectMySQL, "OPTIMIZE TABLE t", StmtDDL, false, false, false},
		{dialectMySQL, "USE other", StmtUnknown, false, false, true},
		{dialectDM, "BEGIN INSERT INTO t VALUES (1); END;", StmtCall, false, false, false},
		{dialectDM, "DECLARE v INT; BEGIN v := 1; END;", StmtCall, false, false, false},
		{dialectDM, "BEGIN WORK", StmtBegin, false, false, false},
		{dialectMySQL, "BEGIN", StmtBegin, false, false, false},
		{dialectPG, "DECLARE c CURSOR FOR SELECT 1", StmtUnknown, false, false, false},
		{dialectPG, "RELEASE SAVEPOINT a", StmtSavepoint, false, false, false},
		{dialectMySQL, "SELECT 1 # comment", StmtSelect, false, false, false},
	}
	for _, c := range cases {
		an := analyzeSQL(c.sql, c.d)
		if an.Type != c.typ || an.Passthrough != c.passthrough || an.NoTx != c.noTx || (an.Reject != "") != c.reject {
			t.Errorf("analyzeSQL(%v, %q) = %+v, want type=%s passthrough=%v noTx=%v reject=%v", c.d, c.sql, an, c.typ, c.passthrough, c.noTx, c.reject)
		}
	}
}

// 回归：MySQL # 注释与反斜杠转义不得绕过无 WHERE 写操作的二次确认。
func TestDangerousWriteMySQLBypasses(t *testing.T) {
	cases := []struct {
		d      sqlDialect
		sql    string
		danger bool
	}{
		{dialectMySQL, "DELETE FROM t # WHERE id=1", true},
		{dialectMySQL, "UPDATE t SET a='x\\' WHERE id=1 -- '", true},
		{dialectMySQL, "UPDATE t SET a='x' WHERE id=1", false},
		{dialectMySQL, "DELETE FROM t WHERE a = 'it\\'s'", false},
		// PG：# 是运算符，WHERE 前出现也不影响
		{dialectPG, "DELETE FROM t WHERE a # 1 = 0", false},
		// PG 普通字符串不认反斜杠：'\\' 已闭合，后面没有真正的 WHERE
		{dialectPG, "UPDATE t SET a='\\', b=' WHERE id=1 '", true},
		{dialectPG, "EXPLAIN ANALYZE DELETE FROM t", true},
		{dialectPG, "-- c\nDROP TABLE t", true},
		{dialectDM, "DELETE FROM t WHERE id = 1", false},
	}
	for _, c := range cases {
		st := analyzeSQL(c.sql, c.d).Type
		if got := isDangerousWriteFor(st, c.sql, c.d); got != c.danger {
			t.Errorf("isDangerousWriteFor(%v, %q) = %v, want %v", c.d, c.sql, got, c.danger)
		}
	}
}

func TestPageSQLTrailingComment(t *testing.T) {
	pg := &pgAdapter{}
	got, _ := pg.PageSQL("SELECT 1 AS a -- 查一下", 101, 0)
	if !strings.Contains(got, "SELECT 1 AS a\n) AS _page") {
		t.Fatalf("PG 包装未剥离末尾注释: %q", got)
	}
	got, _ = pg.PageSQL("SELECT 1; -- note", 101, 0)
	if strings.Contains(got, ";") {
		t.Fatalf("PG 包装残留分号: %q", got)
	}
	my := &mysqlAdapter{}
	got, _ = my.PageSQL("SELECT n FROM s -- 第二页", 3, 2)
	if got != "SELECT n FROM s\nLIMIT 3 OFFSET 2" {
		t.Fatalf("MySQL 分页被注释吞掉: %q", got)
	}
	got, _ = my.PageSQL("SELECT n FROM s # x", 3, 2)
	if got != "SELECT n FROM s\nLIMIT 3 OFFSET 2" {
		t.Fatalf("MySQL # 注释未剥离: %q", got)
	}
	c, _ := my.CountSQL("SELECT 1 -- c")
	if c != "SELECT COUNT(*) FROM (\nSELECT 1\n) AS _count" {
		t.Fatalf("MySQL 计数包装: %q", c)
	}
}

func TestShiftErrorPosition(t *testing.T) {
	user := "SELECT 中文 FRM t"
	wrapped := "SELECT * FROM (\n" + user + "\n) AS _page LIMIT 101 OFFSET 0"
	// 「FRM」在用户 SQL 中是第 11 个字符；包装前缀 16 个字符
	err := shiftErrorPosition(DatabaseError{Code: "SYNTAX_ERROR", Message: "x", Position: 16 + 11}.toError(), wrapped, user)
	de, _ := err.(DatabaseError)
	if de.Position != 11 {
		t.Fatalf("位置换算错误: %d", de.Position)
	}
	err = shiftErrorPosition(DatabaseError{Code: "SYNTAX_ERROR", Message: "x", Position: 3}.toError(), wrapped, user)
	if de, _ := err.(DatabaseError); de.Position != 0 {
		t.Fatalf("包装前缀内的位置应清零: %d", de.Position)
	}
}
