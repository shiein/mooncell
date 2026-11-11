package dataresource

// 真库联调（默认跳过）：设置以下任一环境变量后运行
//
//	DR_LIVE_PG_DSN='host=127.0.0.1 port=15433 user=postgres password=… dbname=etl sslmode=disable'
//	DR_LIVE_MYSQL_DSN='root:…@tcp(127.0.0.1:13306)/etl?parseTime=false'
//	DR_LIVE_DM_DSN='dm://SYSDBA:…@127.0.0.1:15236?schema=SYSDBA'
//	DR_LIVE_KB_DSN='host=… port=… user=… password=… dbname=… sslmode=disable'
//
// 覆盖：末尾注释的分页、limit+1 翻页、EXPLAIN/SHOW 直通、SET 记录与重放（只读事务内）、
// 按需计数、事务外 VACUUM、过程体/匿名块单语句执行。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

type liveDB struct {
	env, driver, dbType, schema string
}

var liveDBs = []liveDB{
	{"DR_LIVE_PG_DSN", "pgx", DriverPostgreSQL, ""},
	{"DR_LIVE_KB_DSN", "kingbase", DriverKingbase, ""},
	{"DR_LIVE_MYSQL_DSN", "mysql", DriverMySQL, ""},
	{"DR_LIVE_DM_DSN", "dm", DriverDM, ""},
}

func openLiveWorkspace(t *testing.T, l liveDB) (*WorkspaceManager, *Workspace, *sql.DB) {
	dsn := os.Getenv(l.env)
	if dsn == "" {
		t.Skipf("未设置 %s", l.env)
	}
	db, err := sql.Open(l.driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(5)
	if err := db.Ping(); err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	adapter, err := NewAdapter(db, l.dbType, l.schema)
	if err != nil {
		t.Fatal(err)
	}
	wm := NewWorkspaceManager(NewPoolManager())
	ws := wm.CreateWorkspace("live-"+l.dbType, "tester", adapter, false)
	t.Cleanup(func() { wm.DeleteWorkspace(ws.ID) })
	return wm, ws, db
}

func mustExec(t *testing.T, wm *WorkspaceManager, ws *Workspace, sqlText string, limit, offset int) *ExecutionResult {
	t.Helper()
	res, err := wm.ExecuteInWorkspace(context.Background(), ws, sqlText, limit, offset)
	if err != nil {
		t.Fatalf("执行 %q 失败: %v", sqlText, err)
	}
	return res
}

func firstCell(res *ExecutionResult) string {
	if len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		return "<empty>"
	}
	return strings.TrimSpace(strings.ToLower(toString(res.Rows[0][0])))
}

func toString(v any) string {
	if v == nil {
		return "<nil>"
	}
	return fmt.Sprint(v)
}

func asDatabaseError(err error, de *DatabaseError) bool { return errors.As(err, de) }

func TestLiveWorkspaceBehaviours(t *testing.T) {
	for _, l := range liveDBs {
		l := l
		t.Run(l.dbType, func(t *testing.T) {
			wm, ws, _ := openLiveWorkspace(t, l)
			d := dialectOf(ws.Adapter)
			// 用真实表：达梦对 UNION ALL…FROM DUAL 派生表 + ORDER BY 会忽略 LIMIT（disql 直接执行亦然）
			_, _ = wm.ExecuteInWorkspace(context.Background(), ws, "DROP TABLE dr_live_n", 0, 0)
			mustExec(t, wm, ws, "CREATE TABLE dr_live_n (n INT)", 0, 0)
			t.Cleanup(func() { _, _ = wm.ExecuteInWorkspace(context.Background(), ws, "DROP TABLE dr_live_n", 0, 0) })
			mustExec(t, wm, ws, "INSERT INTO dr_live_n VALUES (1), (2), (3), (4), (5)", 0, 0)
			ordered := "SELECT n FROM dr_live_n ORDER BY n"

			// 1. 末尾行注释：分页不被吞、翻页正确、hasMore 精确
			res := mustExec(t, wm, ws, ordered+" -- 第二页", 2, 2)
			if res.ReturnedRows != 2 || firstCell(res) != "3" || !res.HasMore {
				t.Fatalf("末尾注释翻页错误: rows=%v hasMore=%v", res.Rows, res.HasMore)
			}
			res = mustExec(t, wm, ws, ordered+";  -- note", 2, 4)
			if res.ReturnedRows != 1 || res.HasMore {
				t.Fatalf("最后一页: rows=%v hasMore=%v", res.Rows, res.HasMore)
			}
			if d == dialectMySQL {
				res = mustExec(t, wm, ws, ordered+" # mysql 注释", 2, 0)
				if res.ReturnedRows != 2 || !res.HasMore {
					t.Fatalf("MySQL # 注释: %v", res.Rows)
				}
			}

			// 2. 按需计数（含末尾注释）
			total, _, err := wm.CountInWorkspace(context.Background(), ws, ordered+" -- c")
			if err != nil || total != 5 {
				t.Fatalf("计数: total=%d err=%v", total, err)
			}

			// 3. EXPLAIN 直通
			if d != dialectDM {
				res = mustExec(t, wm, ws, "EXPLAIN "+ordered, 100, 0)
				if !res.Passthrough || len(res.Columns) == 0 {
					t.Fatalf("EXPLAIN 未直通: %+v", res)
				}
			} else if _, err := wm.ExecuteInWorkspace(context.Background(), ws, "EXPLAIN "+ordered, 100, 0); err == nil || !strings.Contains(err.Error(), "达梦") {
				t.Fatalf("达梦 EXPLAIN 应给出明确提示: %v", err)
			}

			// 4. SET 记录与重放（只读事务内）
			switch d {
			case dialectPG:
				mustExec(t, wm, ws, "SET TIME ZONE 'Asia/Shanghai'", 0, 0)
				res = mustExec(t, wm, ws, "SHOW timezone", 0, 0)
				if firstCell(res) != "asia/shanghai" {
					t.Fatalf("PG 时区未重放: %v", res.Rows)
				}
				if len(res.SessionSettings) != 1 {
					t.Fatalf("会话设置未回传: %v", res.SessionSettings)
				}
				// SET LOCAL 重放不污染连接池：直接在池上看不到
				var tz string
				if err := ws.Adapter.(*sessionAdapter).DataSourceAdapter.(*pgAdapter).db.QueryRow("SHOW timezone").Scan(&tz); err == nil && strings.EqualFold(tz, "Asia/Shanghai") {
					t.Fatalf("SET LOCAL 泄漏到连接池: %s", tz)
				}
				// 5. 事务外 VACUUM
				mustExec(t, wm, ws, "CREATE TABLE IF NOT EXISTS dr_live_vac (a int)", 0, 0)
				mustExec(t, wm, ws, "VACUUM dr_live_vac", 0, 0)
				mustExec(t, wm, ws, "DROP TABLE dr_live_vac", 0, 0)
				// 错误位置换算回用户 SQL
				_, err := wm.ExecuteInWorkspace(context.Background(), ws, "SELECT 1 FRM x", 100, 0)
				var de DatabaseError
				if !asDatabaseError(err, &de) || de.Position <= 0 || de.Position > len("SELECT 1 FRM x") {
					t.Fatalf("错误位置未换算: %v %+v", err, de)
				}
			case dialectMySQL:
				mustExec(t, wm, ws, "SET @dr_live = 42", 0, 0)
				mustExec(t, wm, ws, "SET time_zone = '+03:00'", 0, 0)
				res = mustExec(t, wm, ws, "SELECT @dr_live, @@time_zone", 0, 0)
				if firstCell(res) != "42" || toString(res.Rows[0][1]) != "+03:00" {
					t.Fatalf("MySQL 变量未重放: %v", res.Rows)
				}
				// 过程体（含体内分号）作为单语句执行
				mustExec(t, wm, ws, "DROP PROCEDURE IF EXISTS dr_live_p", 0, 0)
				mustExec(t, wm, ws, "CREATE PROCEDURE dr_live_p()\nBEGIN\n  SELECT 1;\n  SELECT 2;\nEND", 0, 0)
				mustExec(t, wm, ws, "DROP PROCEDURE dr_live_p", 0, 0)
				res = mustExec(t, wm, ws, "SHOW TABLES", 0, 0)
				if !res.Passthrough {
					t.Fatal("SHOW 未直通")
				}
			case dialectDM:
				if _, err := wm.ExecuteInWorkspace(context.Background(), ws, "SET TIME ZONE '+03:00'", 0, 0); err == nil {
					t.Fatal("达梦 SET TIME ZONE 应被拒绝（驱动缓存时区，无法撤销）")
				}
				mustExec(t, wm, ws, "SET SCHEMA SYSDBA", 0, 0)
				res = mustExec(t, wm, ws, "SELECT SYS_CONTEXT('USERENV','CURRENT_SCHEMA') FROM DUAL", 0, 0)
				if firstCell(res) != "sysdba" {
					t.Fatalf("DM schema 未重放: %v", res.Rows)
				}
				// 匿名块作为单语句执行
				mustExec(t, wm, ws, "BEGIN\n  NULL;\n  NULL;\nEND;", 0, 0)
			}
		})
	}
}

// 回归：MySQL/达梦会话变量不随事务回滚。重置记录、关闭工作台后，池中连接不得残留旧值。
func TestLiveSessionResetNoLeak(t *testing.T) {
	for _, l := range liveDBs {
		l := l
		if l.dbType != DriverMySQL && l.dbType != DriverDM {
			continue
		}
		t.Run(l.dbType, func(t *testing.T) {
			wm, ws, db := openLiveWorkspace(t, l)
			tzSQL := "SELECT @@time_zone"
			setSQL := "SET time_zone = '+03:00'"
			marker := "+03:00"
			if l.dbType == DriverDM {
				_, _ = wm.ExecuteInWorkspace(context.Background(), ws, "CREATE SCHEMA DR_LEAK_S", 0, 0)
				t.Cleanup(func() { _, _ = db.Exec("DROP SCHEMA DR_LEAK_S CASCADE") })
				tzSQL = "SELECT SYS_CONTEXT('USERENV','CURRENT_SCHEMA') FROM DUAL"
				setSQL = "SET SCHEMA DR_LEAK_S"
				marker = "dr_leak_s"
			}
			mustExec(t, wm, ws, setSQL, 0, 0)
			for i := 0; i < 5; i++ {
				if got := firstCell(mustExec(t, wm, ws, tzSQL, 0, 0)); !strings.Contains(got, marker) {
					t.Fatalf("设置未生效: %s", got)
				}
			}
			if l.dbType == DriverMySQL {
				mustExec(t, wm, ws, "SET @dr_leak = 7", 0, 0)
			}
			wm.ResetSessionSettings(ws)
			for i := 0; i < 5; i++ {
				if got := firstCell(mustExec(t, wm, ws, tzSQL, 0, 0)); strings.Contains(got, marker) {
					t.Fatalf("重置后第 %d 次仍是旧时区: %s", i, got)
				}
			}
			if l.dbType == DriverMySQL {
				if got := firstCell(mustExec(t, wm, ws, "SELECT @dr_leak", 0, 0)); got != "<nil>" {
					t.Fatalf("重置后用户变量残留: %s", got)
				}
			}
			// 关闭工作台后：同一连接池上直接查询也不得残留
			mustExec(t, wm, ws, setSQL, 0, 0)
			mustExec(t, wm, ws, tzSQL, 0, 0)
			wm.DeleteWorkspace(ws.ID)
			for i := 0; i < 5; i++ {
				var tz string
				if err := db.QueryRow(tzSQL).Scan(&tz); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(strings.ToLower(tz), marker) {
					t.Fatalf("工作台关闭后池连接残留时区: %s", tz)
				}
			}
		})
	}
}
