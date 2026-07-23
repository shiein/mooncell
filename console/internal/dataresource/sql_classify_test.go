package dataresource

import (
	"testing"
)

func TestClassifySQL(t *testing.T) {
	cases := []struct {
		sql  string
		want StatementType
	}{
		{"SELECT * FROM t", StmtSelect},
		{"select * from t", StmtSelect},
		{"WITH cte AS (SELECT 1) SELECT * FROM cte", StmtSelect},
		{"  -- comment\nSELECT 1", StmtSelect},
		{"/* block */ SELECT 1", StmtSelect},
		{"INSERT INTO t VALUES (1)", StmtInsert},
		{"UPDATE t SET a=1", StmtUpdate},
		{"DELETE FROM t", StmtDelete},
		{"TRUNCATE TABLE t", StmtTruncate},
		{"CREATE TABLE t (id int)", StmtDDL},
		{"ALTER TABLE t ADD COLUMN x int", StmtDDL},
		{"DROP TABLE t", StmtDDL},
		{"GRANT SELECT ON t TO u", StmtDCL},
		{"CALL proc()", StmtCall},
		{"BEGIN", StmtBegin},
		{"COMMIT", StmtCommit},
		{"ROLLBACK", StmtRollback},
		{"SAVEPOINT sp1", StmtSavepoint},
		{"", StmtUnknown},
		{"WEIRD STATEMENT", StmtUnknown},
	}
	for _, c := range cases {
		got := ClassifySQL(c.sql)
		if got != c.want {
			t.Errorf("ClassifySQL(%q) = %s, 期望 %s", c.sql, got, c.want)
		}
	}
}

func TestValidateSingleStatement(t *testing.T) {
	cases := []struct {
		sql  string
		ok   bool
	}{
		{"SELECT 1", true},
		{"SELECT 1;", true},
		{"SELECT 1; ", true},
		{"SELECT 1; SELECT 2", false},
		{"SELECT ';'; SELECT 2", false},
		{"SELECT 'hello;world'", true},
		{"SELECT \"col;name\"", true},
		{"-- comment\nSELECT 1", true},
		{"/* comment ; */ SELECT 1", true},
		{"SELECT $$hello; world$$", true},
		{"SELECT 1; -- comment\nSELECT 2", false},
		{"", false},
	}
	for _, c := range cases {
		err := ValidateSingleStatement(c.sql)
		if (err == nil) != c.ok {
			t.Errorf("ValidateSingleStatement(%q) 期望 ok=%v, 实际 err=%v", c.sql, c.ok, err)
		}
	}
}

func TestStatementTypeMethods(t *testing.T) {
	if !StmtSelect.IsReadOnly() {
		t.Error("SELECT 应为只读")
	}
	if !StmtInsert.IsWrite() {
		t.Error("INSERT 应为写操作")
	}
	if !StmtBegin.IsTransactionControl() {
		t.Error("BEGIN 应为事务控制")
	}
	if !StmtDDL.IsDDLorDCL() {
		t.Error("DDL 应为 DDL/DCL")
	}
	if !StmtTruncate.IsAutoCommitOnly() {
		t.Error("TRUNCATE 应为自动提交模式")
	}
	if StmtSelect.IsWrite() {
		t.Error("SELECT 不应为写操作")
	}
}

func TestReadOnlyRejection(t *testing.T) {
	// 只读模式下写操作应被拒绝
	writeSQLs := []string{
		"INSERT INTO t VALUES (1)",
		"UPDATE t SET a=1",
		"DELETE FROM t",
		"TRUNCATE TABLE t",
		"CREATE TABLE t (id int)",
		"DROP TABLE t",
		"CALL proc()",
	}
	for _, sql := range writeSQLs {
		stmtType := ClassifySQL(sql)
		if stmtType.IsReadOnly() {
			t.Errorf("写操作 %q 不应被识别为只读", sql)
		}
	}
}

func TestDollarQuote(t *testing.T) {
	// PostgreSQL dollar quote 内的分号不应被识别为多语句
	sql := `SELECT $$hello; world$$ AS msg`
	if err := ValidateSingleStatement(sql); err != nil {
		t.Errorf("dollar quote 内分号不应触发多语句错误: %v", err)
	}
	// dollar quote 后的分号应触发
	sql2 := `SELECT $$hello$$; SELECT 1`
	if err := ValidateSingleStatement(sql2); err == nil {
		t.Error("dollar quote 后的分号应触发多语句错误")
	}
	// 带标签的 $tag$...$tag$
	sql3 := `SELECT $body$hello; world$body$ AS msg`
	if err := ValidateSingleStatement(sql3); err != nil {
		t.Errorf("带标签 dollar quote 内分号不应触发多语句错误: %v", err)
	}
	sql4 := `SELECT $tag$; not end $tag$; SELECT 1`
	if err := ValidateSingleStatement(sql4); err == nil {
		t.Error("带标签 dollar quote 后的分号应触发多语句错误")
	}
	// $1 参数不应被当作 dollar quote 吞掉整句
	sql5 := `SELECT $1`
	if err := ValidateSingleStatement(sql5); err != nil {
		t.Errorf("$1 参数应作为单语句通过: %v", err)
	}
}

func TestDangerousWrite(t *testing.T) {
	cases := []struct {
		sql     string
		danger  bool
	}{
		{"TRUNCATE TABLE t", true},
		{"DROP TABLE t", true},
		{"DELETE FROM t", true},           // 无 WHERE
		{"DELETE FROM t WHERE id = 1", false},
		{"UPDATE t SET a = 1", true},      // 无 WHERE
		{"UPDATE t SET a = 1 WHERE id = 1", false},
		{"INSERT INTO t VALUES (1)", false},
		{"SELECT * FROM t", false},
		// WHERE 在字符串中不应误判
		{"DELETE FROM t WHERE name = 'hello WHERE world'", false},
	}
	for _, c := range cases {
		stmtType := ClassifySQL(c.sql)
		got := stmtType.IsDangerousWrite(c.sql)
		if got != c.danger {
			t.Errorf("IsDangerousWrite(%q) = %v, 期望 %v", c.sql, got, c.danger)
		}
	}
}
