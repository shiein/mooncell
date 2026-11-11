package dataresource

import "testing"

func TestParseSessionChange(t *testing.T) {
	ok := []struct {
		d      sqlDialect
		sql    string
		key    string
		replay string
	}{
		{dialectPG, "SET search_path TO app, public", "search_path", "SET LOCAL search_path TO app, public"},
		{dialectPG, "set search_path = \"$user\", public;", "search_path", "SET LOCAL search_path TO \"$user\", public"},
		{dialectPG, "SET TIME ZONE 'Asia/Shanghai'", "timezone", "SET LOCAL TIME ZONE 'Asia/Shanghai'"},
		{dialectPG, "SET timezone = -8", "timezone", "SET LOCAL TIME ZONE -8"},
		{dialectPG, "SET SESSION datestyle TO ISO, YMD", "datestyle", "SET LOCAL datestyle TO ISO, YMD"},
		{dialectMySQL, "SET @id = 5", "@id", "SET @id = 5"},
		{dialectMySQL, "SET @name := 'x'", "@name", "SET @name = 'x'"},
		{dialectMySQL, "SET time_zone = '+08:00'", "time_zone", "SET time_zone = '+08:00'"},
		{dialectMySQL, "SET @@session.time_zone = '+00:00'", "time_zone", "SET time_zone = '+00:00'"},
		{dialectMySQL, "SET @p = -1.5", "@p", "SET @p = -1.5"},
		{dialectDM, "SET SCHEMA APP", "schema", "SET SCHEMA APP"},
		{dialectDM, "SET TIME ZONE '+08:00'", "timezone", "SET TIME ZONE '+08:00'"},
	}
	for _, c := range ok {
		ch, reason := parseSessionChange(c.sql, c.d)
		if reason != "" || ch == nil || len(ch.Set) != 1 {
			t.Errorf("%q 应被接受: reason=%q ch=%+v", c.sql, reason, ch)
			continue
		}
		if ch.Set[0].Key != c.key || ch.Set[0].Replay != c.replay {
			t.Errorf("%q => key=%q replay=%q, want %q %q", c.sql, ch.Set[0].Key, ch.Set[0].Replay, c.key, c.replay)
		}
	}
	reject := []struct {
		d   sqlDialect
		sql string
	}{
		{dialectPG, "SET ROLE admin"},
		{dialectPG, "SET SESSION AUTHORIZATION postgres"},
		{dialectPG, "SET TRANSACTION READ WRITE"},
		{dialectPG, "SET default_transaction_read_only = off"},
		{dialectPG, "SET LOCAL search_path TO x"},
		{dialectPG, "SET search_path TO (SELECT 1)"},
		{dialectMySQL, "SET @x = @x + 1"},
		{dialectMySQL, "SET @x = NOW()"},
		{dialectMySQL, "SET @x = (SELECT 1)"},
		{dialectMySQL, "SET GLOBAL time_zone = '+00:00'"},
		{dialectMySQL, "SET NAMES latin1"},
		{dialectMySQL, "SET autocommit = 0"},
		{dialectMySQL, "SET @x = @@version"},
		{dialectMySQL, "SET SESSION TRANSACTION READ WRITE"},
		{dialectDM, "SET SCHEMA a b"},
		{dialectDM, "SET TRANSACTION READ WRITE"},
	}
	for _, c := range reject {
		if _, reason := parseSessionChange(c.sql, c.d); reason == "" {
			t.Errorf("%v %q 应被拒绝", c.d, c.sql)
		}
	}
	// RESET / DEFAULT 清除
	ch, reason := parseSessionChange("RESET search_path", dialectPG)
	if reason != "" || len(ch.ResetKey) != 1 || ch.ResetKey[0] != "search_path" {
		t.Fatalf("RESET: %+v %q", ch, reason)
	}
	ch, _ = parseSessionChange("RESET ALL", dialectPG)
	if !ch.ResetAll {
		t.Fatal("RESET ALL")
	}
	ch, _ = parseSessionChange("SET search_path TO DEFAULT", dialectPG)
	if len(ch.ResetKey) != 1 {
		t.Fatal("TO DEFAULT 应清除")
	}
}

func TestSessionStateApply(t *testing.T) {
	st := &sessionState{}
	set := func(sql string) {
		ch, reason := parseSessionChange(sql, dialectMySQL)
		if reason != "" {
			t.Fatal(reason)
		}
		if err := st.apply(ch); err != nil {
			t.Fatal(err)
		}
	}
	set("SET @a = 1")
	set("SET @b = 2")
	set("SET @A = 3") // 用户变量大小写不敏感：覆盖 @a
	got := st.displays()
	if len(got) != 2 || got[0] != "SET @b = 2" || got[1] != "SET @A = 3" {
		t.Fatalf("去重/顺序错误: %v", got)
	}
	for i := 0; i < maxSessionSettings; i++ {
		ch, _ := parseSessionChange("SET @v"+string(rune('a'+i))+" = 1", dialectMySQL)
		if err := st.apply(ch); err != nil {
			if len(st.displays()) > maxSessionSettings {
				t.Fatal("超出上限")
			}
			return
		}
	}
	t.Fatal("应触发上限")
}
