package dataresource

import "testing"

func TestDetectEditableSelect(t *testing.T) {
	cases := []struct {
		sql    string
		ok     bool
		schema string
		table  string
	}{
		{`SELECT * FROM users`, true, "", "users"},
		{`SELECT * FROM public.users WHERE id=1`, true, "public", "users"},
		{`SELECT * FROM "MySchema"."MyTable" ORDER BY id`, true, "MySchema", "MyTable"},
		{`SELECT * FROM users u`, true, "", "users"},
		{`SELECT * FROM users AS u`, true, "", "users"},
		{`SELECT * FROM users;`, true, "", "users"},
		// 显式列 / 聚合 / 表达式：不可编辑
		{`SELECT id, name FROM public.users WHERE id=1`, false, "", ""},
		{`SELECT count(*) FROM users`, false, "", ""},
		{`SELECT id, price * 1.2 AS price FROM products`, false, "", ""},
		{`SELECT name FROM users`, false, "", ""},
		{`SELECT * FROM a JOIN b ON a.id=b.id`, false, "", ""},
		{`SELECT * FROM a, b`, false, "", ""},
		{`SELECT * FROM (SELECT 1) t`, false, "", ""},
		{`SELECT * FROM a UNION SELECT * FROM b`, false, "", ""},
		{`WITH t AS (SELECT 1) SELECT * FROM t`, false, "", ""},
		{`INSERT INTO users VALUES (1)`, false, "", ""},
		{`SELECT * FROM ` + "`db`.`tbl`", true, "db", "tbl"},
	}
	for _, c := range cases {
		got, ok := DetectEditableSelect(c.sql)
		if ok != c.ok {
			t.Errorf("DetectEditableSelect(%q) ok=%v want %v", c.sql, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.Schema != c.schema || got.Table != c.table {
			t.Errorf("DetectEditableSelect(%q) = %+v want schema=%q table=%q", c.sql, got, c.schema, c.table)
		}
	}
}
