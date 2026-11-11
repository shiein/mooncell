//go:build etl_live

package steps_test

import (
	"context"
	"fmt"
	"testing"

	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/livetest"
	"mooncell/console/internal/etl/pipeline"
)

// TestLiveTableOutputDataErrorFallback：表输出接错误端口时，非空/超长等数据类坏行
// 必须转到错误输出、好行照常入库、步骤成功——四种方言同一断言。
// （达梦曾因错误码白名单写错，坏行会让整步失败、好行也写不进去。）
func TestLiveTableOutputDataErrorFallback(t *testing.T) {
	for _, dia := range []string{"postgres", "mysql", "dm", "kingbase"} {
		for _, kind := range []string{"notnull", "toolong"} {
			t.Run(dia+"/"+kind, func(t *testing.T) {
				ctx := context.Background()
				e := livetest.Env(t, dia)
				h := livetest.Open(t, dia)
				d := h.Dialect
				ref := conn.TableRef{Schema: d.FoldIdent(conn.EffectiveSchema(h.Config, "")), Name: d.FoldIdent("etl_fb")}
				q := d.Qualified(ref)
				_, _ = h.DB.ExecContext(ctx, "DROP TABLE "+q)
				if _, err := h.DB.ExecContext(ctx, `CREATE TABLE `+q+` (id INTEGER, v VARCHAR(3) NOT NULL)`); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _, _ = h.DB.ExecContext(context.Background(), "DROP TABLE "+q) })
				bad := "NULL"
				if kind == "toolong" {
					bad = "'abcdefgh'"
				}
				dual := ""
				if dia == "dm" {
					dual = " FROM DUAL"
				}
				src := fmt.Sprintf(`SELECT 1 AS %[3]s, 'a' AS %[4]s%[1]s UNION ALL SELECT 2, %[2]s%[1]s UNION ALL SELECT 3, 'c'%[1]s`,
					dual, bad, d.Quote("id"), d.Quote("v"))
				cid := livetest.ConnID(dia)
				def := &pipeline.Definition{
					Nodes: []pipeline.Node{
						node("ti", "table_input", map[string]any{"connection": cid, "sql": src}),
						node("out", "table_output", map[string]any{"connection": cid, "table": "etl_fb", "commitSize": 10,
							"fields": []map[string]any{{"stream": "id", "column": "id"}, {"stream": "v", "column": "v"}}}),
						node("bad", "dummy", map[string]any{}),
					},
					Hops: []pipeline.Hop{hop("ti", "main", "out"), {ID: "e1", From: "out", FromPort: "error", To: "bad", ToPort: "main"}},
				}
				plan := pipeline.Compile(ctx, def, pipeline.CompileOptions{Env: e})
				if plan.Problems.HasErrors() {
					t.Fatalf("%+v", plan.Problems)
				}
				res := pipeline.Run(ctx, plan, pipeline.RunOptions{Env: e})
				if res.Err != nil {
					t.Fatalf("数据类坏行不应让整步失败：%v", res.Err)
				}
				var n int
				if err := h.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n != 2 || res.Metrics["out"].Rejected != 1 {
					t.Fatalf("应入库 2 行、拒绝 1 行，实际入库 %d、拒绝 %d", n, res.Metrics["out"].Rejected)
				}
			})
		}
	}
}
