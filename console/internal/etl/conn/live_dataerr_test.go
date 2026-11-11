//go:build etl_live

package conn_test

import (
	"context"
	"testing"

	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/livetest"
)

// TestLiveDataErrorClass 在真机上逐类触发数据错误，断言 conn.IsDataError 判为「数据类」：
// 表输出接错误端口时，只有数据类错误才退化逐行写入、把坏行转错误输出；
// 判错会让坏行导致整步失败（好行也写不进去）。错误码按真机实测维护，不凭文档猜测。
// 同时断言基础设施类错误（取消、对象不存在）不被判为数据类。
func TestLiveDataErrorClass(t *testing.T) {
	for _, dia := range []string{"postgres", "mysql", "dm", "kingbase"} {
		t.Run(dia, func(t *testing.T) {
			ctx := context.Background()
			h := livetest.Open(t, dia)
			d := h.Dialect
			ref := conn.TableRef{Schema: d.FoldIdent(conn.EffectiveSchema(h.Config, "")), Name: d.FoldIdent("etl_de")}
			q := d.Qualified(ref)
			pq := d.Qualified(conn.TableRef{Schema: ref.Schema, Name: d.FoldIdent("etl_de_p")})
			_, _ = h.DB.ExecContext(ctx, "DROP TABLE "+q)
			_, _ = h.DB.ExecContext(ctx, "DROP TABLE "+pq)
			for _, ddl := range []string{
				`CREATE TABLE ` + pq + ` (id INTEGER PRIMARY KEY)`,
				`CREATE TABLE ` + q + ` (id INTEGER PRIMARY KEY, v VARCHAR(3) NOT NULL, n NUMERIC(3,0), d DATE, p INTEGER, CHECK (n > 0), FOREIGN KEY (p) REFERENCES ` + pq + ` (id))`,
			} {
				if _, err := h.DB.ExecContext(ctx, ddl); err != nil {
					t.Fatalf("%s: %v", ddl, err)
				}
			}
			t.Cleanup(func() {
				_, _ = h.DB.ExecContext(context.Background(), "DROP TABLE "+q)
				_, _ = h.DB.ExecContext(context.Background(), "DROP TABLE "+pq)
			})
			cols := []string{d.FoldIdent("id"), d.FoldIdent("v"), d.FoldIdent("n")}
			ins := conn.InsertSQL(d, ref, cols, 1)
			if _, err := h.DB.ExecContext(ctx, ins, int64(1), "a", int64(1)); err != nil {
				t.Fatalf("基准行写入失败：%v", err)
			}
			cases := []struct {
				name string
				sql  string
				args []any
			}{
				{"唯一约束", ins, []any{int64(1), "b", int64(1)}},
				{"非空约束", ins, []any{int64(2), nil, int64(1)}},
				{"长度超限", ins, []any{int64(3), "abcdefgh", int64(1)}},
				{"数值溢出", ins, []any{int64(4), "a", int64(12345)}},
				{"检查约束", ins, []any{int64(5), "a", int64(-1)}},
				{"类型不符", ins, []any{"not-a-number", "a", int64(1)}},
				{"日期非法", conn.InsertSQL(d, ref, []string{d.FoldIdent("id"), d.FoldIdent("v"), d.FoldIdent("d")}, 1), []any{int64(6), "a", "2026-13-45"}},
				{"外键约束", conn.InsertSQL(d, ref, []string{d.FoldIdent("id"), d.FoldIdent("v"), d.FoldIdent("p")}, 1), []any{int64(7), "a", int64(999)}},
			}
			for _, c := range cases {
				_, err := h.DB.ExecContext(ctx, c.sql, c.args...)
				if err == nil {
					t.Errorf("%s：预期报错却成功", c.name)
					continue
				}
				if !conn.IsDataError(d, err) {
					t.Errorf("%s：应判为数据类错误，实际判为基础设施类：%v", c.name, err)
				} else {
					t.Logf("%s：数据类 ✓ %v", c.name, err)
				}
			}
			// 基础设施类：取消的上下文、对象不存在
			cctx, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := h.DB.ExecContext(cctx, ins, int64(9), "a", int64(1)); err == nil || conn.IsDataError(d, err) {
				t.Errorf("取消的上下文不应判为数据类：%v", err)
			}
			missing := d.Qualified(conn.TableRef{Schema: ref.Schema, Name: d.FoldIdent("etl_de_missing")})
			if _, err := h.DB.ExecContext(ctx, "INSERT INTO "+missing+" VALUES (1)"); err == nil || conn.IsDataError(d, err) {
				t.Errorf("表不存在不应判为数据类：%v", err)
			}
		})
	}
}
