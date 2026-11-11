package conn

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"mooncell/console/internal/etl/core"
)

// QuerySchema 用方言的 ProbeSQL 包装查询，只取列元数据推导字段（设计期，不读数据）。
func QuerySchema(ctx context.Context, h *Handle, query string, args []any) (*core.Schema, error) {
	rows, err := h.DB.QueryContext(ctx, h.Dialect.ProbeSQL(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return SchemaFromRows(h.Dialect, rows)
}

// SchemaFromRows 由结果集列类型构造 Schema；重名列按 Kettle 习惯追加 _1、_2。
func SchemaFromRows(d Dialect, rows *sql.Rows) (*core.Schema, error) {
	cts, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	seen := map[string]int{}
	fields := make([]core.Field, 0, len(cts))
	for _, ct := range cts {
		col := ColumnMeta{Name: ct.Name(), DBType: ct.DatabaseTypeName(), Length: -1, Precision: -1, Nullable: true}
		if l, ok := ct.Length(); ok && l < 1<<31 {
			col.Length = int(l)
		}
		if p, s, ok := ct.DecimalSize(); ok {
			col.Precision, col.Scale = int(p), int(s)
		}
		if n, ok := ct.Nullable(); ok {
			col.Nullable = n
		}
		f := d.FieldFor(col)
		if n := seen[f.Name]; n > 0 {
			f.Name = fmt.Sprintf("%s_%d", f.Name, n)
		}
		seen[col.Name]++
		fields = append(fields, f)
	}
	return core.NewSchema(fields...)
}

// RowReader 流式读取结果集并规整为逻辑行。
type RowReader struct {
	rows   *sql.Rows
	d      Dialect
	schema *core.Schema
	h      *Handle
	vals   []any
	ptrs   []any
}

// NewRowReader 包装结果集；schema 由 SchemaFromRows 产生。
func NewRowReader(h *Handle, rows *sql.Rows, schema *core.Schema) *RowReader {
	n := schema.Len()
	r := &RowReader{rows: rows, d: h.Dialect, schema: schema, h: h, vals: make([]any, n), ptrs: make([]any, n)}
	for i := range r.vals {
		r.ptrs[i] = &r.vals[i]
	}
	return r
}

// Next 读下一行；结束返回 ok=false。转换失败返回 *core.RowError（携带已读出的原始值便于定位）。
func (r *RowReader) Next() (core.Row, bool, error) {
	if !r.rows.Next() {
		return nil, false, r.rows.Err()
	}
	for i := range r.vals {
		r.vals[i] = nil
	}
	if err := r.rows.Scan(r.ptrs...); err != nil {
		return nil, false, err
	}
	row := make(core.Row, len(r.vals))
	loc := r.h.Location()
	for i, v := range r.vals {
		f := r.schema.Field(i)
		nv, err := r.d.Normalize(v, f, loc)
		if err != nil {
			return nil, false, core.NewRowError(f.Name, err)
		}
		row[i] = nv
	}
	return row, true, nil
}

// Close 关闭结果集。
func (r *RowReader) Close() error { return r.rows.Close() }

// RowsPerStatement 返回单条多值 INSERT 可容纳的行数（受方言参数上限与批大小约束）。
func RowsPerStatement(d Dialect, ncols, batch int) int {
	if ncols <= 0 {
		return 1
	}
	n := d.MaxParams() / ncols
	if batch > 0 && batch < n {
		n = batch
	}
	if n < 1 {
		n = 1
	}
	return n
}

// placeholders 生成从 start 开始的 n 个占位符。
func placeholders(d Dialect, start, n int) string {
	ps := make([]string, n)
	for i := range ps {
		ps[i] = d.Placeholder(start + i)
	}
	return strings.Join(ps, ", ")
}

func quoteAll(d Dialect, cols []string) string {
	qs := make([]string, len(cols))
	for i, c := range cols {
		qs[i] = d.Quote(c)
	}
	return strings.Join(qs, ", ")
}

// InsertSQL 生成 nrows 行的多值 INSERT。
func InsertSQL(d Dialect, t TableRef, cols []string, nrows int) string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(d.Qualified(t))
	b.WriteString(" (")
	b.WriteString(quoteAll(d, cols))
	b.WriteString(") VALUES ")
	for r := 0; r < nrows; r++ {
		if r > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		b.WriteString(placeholders(d, r*len(cols)+1, len(cols)))
		b.WriteByte(')')
	}
	return b.String()
}

// LookupSQL 生成按键批量查找已存在行的 SELECT：返回 keys + fetch 列。
// 单键用 IN (…)，复合键用 (k1=? AND k2=?) OR …。nkeys 是本批不同键的个数。
func LookupSQL(d Dialect, t TableRef, keys, fetch []string, nkeys int) string {
	var b strings.Builder
	b.WriteString("SELECT ")
	b.WriteString(quoteAll(d, append(append([]string(nil), keys...), fetch...)))
	b.WriteString(" FROM ")
	b.WriteString(d.Qualified(t))
	b.WriteString(" WHERE ")
	b.WriteString(keyWhere(d, keys, nkeys))
	return b.String()
}

// keyWhere 生成 nkeys 组键的等值条件：单键 IN (…)，复合键 (k1=? AND k2=?) OR …。
func keyWhere(d Dialect, keys []string, nkeys int) string {
	var b strings.Builder
	if len(keys) == 1 {
		b.WriteString(d.Quote(keys[0]))
		b.WriteString(" IN (")
		b.WriteString(placeholders(d, 1, nkeys))
		b.WriteByte(')')
		return b.String()
	}
	p := 1
	for i := 0; i < nkeys; i++ {
		if i > 0 {
			b.WriteString(" OR ")
		}
		b.WriteByte('(')
		for j, k := range keys {
			if j > 0 {
				b.WriteString(" AND ")
			}
			b.WriteString(d.Quote(k))
			b.WriteString(" = ")
			b.WriteString(d.Placeholder(p))
			p++
		}
		b.WriteByte(')')
	}
	return b.String()
}

// DeleteSQL 生成按键批量删除的 DELETE（单键 IN / 复合键 OR 合取，同 LookupSQL 的 WHERE）。
func DeleteSQL(d Dialect, t TableRef, keys []string, nkeys int) string {
	var b strings.Builder
	b.WriteString("DELETE FROM ")
	b.WriteString(d.Qualified(t))
	b.WriteString(" WHERE ")
	b.WriteString(keyWhere(d, keys, nkeys))
	return b.String()
}

// UpdateSQL 生成按键更新的 UPDATE（参数顺序：set 列…，key 列…）。
func UpdateSQL(d Dialect, t TableRef, set, keys []string) string {
	var b strings.Builder
	b.WriteString("UPDATE ")
	b.WriteString(d.Qualified(t))
	b.WriteString(" SET ")
	p := 1
	for i, c := range set {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(d.Quote(c))
		b.WriteString(" = ")
		b.WriteString(d.Placeholder(p))
		p++
	}
	b.WriteString(" WHERE ")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(" AND ")
		}
		b.WriteString(d.Quote(k))
		b.WriteString(" = ")
		b.WriteString(d.Placeholder(p))
		p++
	}
	return b.String()
}

// keyCond 生成「keys = 连续 n 个占位符」的合取条件，占位符从 p 开始，返回下一个序号。
func keyCond(d Dialect, b *strings.Builder, keys []string, p int) int {
	for i, k := range keys {
		if i > 0 {
			b.WriteString(" AND ")
		}
		b.WriteString(d.Quote(k))
		b.WriteString(" = ")
		b.WriteString(d.Placeholder(p))
		p++
	}
	return p
}

// DerivedCol 是派生行集中的一列：目标列名 + 首行参数可选的 CAST 目标类型。
type DerivedCol struct {
	Name string
	Cast string // 空 = 方言可自动推断参数类型（见 Dialect.ParamCast）
}

// UpdateFromSQL 生成「派生行集 + 连接更新」的批量 UPDATE：一条语句更新 nrows 行。
// 参数顺序为按行主序的 keys+set 值；首行参数按 DerivedCol.Cast 显式 CAST（派生列
// 类型由首行决定）。键列不可能是 NULL（上游已过滤），等值匹配安全。
//
//	UPDATE t SET c = m.c FROM (VALUES (…), …) AS m(k, c) WHERE t.k = m.k             (PG/KB/SQLite)
//	UPDATE t SET c = m.c FROM (SELECT ? AS k UNION ALL …) m WHERE t.k = m.k          (达梦)
//	UPDATE t JOIN (SELECT ? AS k UNION ALL …) m ON t.k = m.k SET t.c = m.c           (MySQL)
//
// VALUES 派生表是单个计划节点；N×SELECT UNION ALL 让优化器按 N 个子查询解析规划，
// 大批量下解析开销显著（R1-M2 实测 PG 通用路径 13k 行/s，改 VALUES 后见 §18）。
func UpdateFromSQL(d Dialect, t TableRef, keys, set []DerivedCol, nrows int) string {
	cols := append(append(make([]DerivedCol, 0, len(keys)+len(set)), keys...), set...)
	var b strings.Builder
	p := 1
	next := func() string {
		s := d.Placeholder(p)
		p++
		return s
	}
	mysql := d.ID() == "mysql"
	b.WriteString("UPDATE ")
	b.WriteString(d.Qualified(t))
	if mysql {
		b.WriteString(" t JOIN (")
	} else {
		// 表别名关键字：SQLite 要求 AS，Oracle 系（达梦）不允许 AS，PG 系两种皆可
		if d.ID() == "dm" {
			b.WriteString(" t SET ")
		} else {
			b.WriteString(" AS t SET ")
		}
		for i, c := range set {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(d.Quote(c.Name))
			b.WriteString(" = m.")
			b.WriteString(d.Quote(c.Name))
		}
		b.WriteString(" FROM (")
	}
	if id := d.ID(); id == "postgres" || id == "kingbase" {
		// PG 系：VALUES 派生表，单计划节点（SQLite/DM 不支持 AS m(列名) 形态，走 UNION ALL）
		b.WriteString("VALUES ")
		for r := 0; r < nrows; r++ {
			if r > 0 {
				b.WriteString(", ")
			}
			b.WriteByte('(')
			for i, c := range cols {
				if i > 0 {
					b.WriteString(", ")
				}
				if r == 0 && c.Cast != "" {
					b.WriteString("CAST(")
					b.WriteString(next())
					b.WriteString(" AS ")
					b.WriteString(c.Cast) // 服务器元数据给出的类型名，非用户输入
					b.WriteString(")")
				} else {
					b.WriteString(next())
				}
			}
			b.WriteByte(')')
		}
		b.WriteString(") AS m(")
		for i, c := range cols {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(d.Quote(c.Name))
		}
		b.WriteString(") ")
	} else {
		for r := 0; r < nrows; r++ {
			if r > 0 {
				b.WriteString(" UNION ALL ")
			}
			b.WriteString("SELECT ")
			for i, c := range cols {
				if i > 0 {
					b.WriteString(", ")
				}
				if r == 0 && c.Cast != "" {
					b.WriteString("CAST(")
					b.WriteString(next())
					b.WriteString(" AS ")
					b.WriteString(c.Cast) // 服务器元数据给出的类型名，非用户输入
					b.WriteString(")")
				} else {
					b.WriteString(next())
				}
				if r == 0 {
					b.WriteString(" AS ")
					b.WriteString(d.Quote(c.Name))
				}
			}
			if r == 0 && d.ID() == "dm" {
				b.WriteString(" FROM DUAL") // 达梦 SELECT 必须有 FROM
			}
		}
		b.WriteString(") m ")
	}
	if mysql {
		b.WriteString("ON ")
		for i, c := range keys {
			if i > 0 {
				b.WriteString(" AND ")
			}
			b.WriteString("t.")
			b.WriteString(d.Quote(c.Name))
			b.WriteString(" = m.")
			b.WriteString(d.Quote(c.Name))
		}
		b.WriteString(" SET ")
		for i, c := range set {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString("t.")
			b.WriteString(d.Quote(c.Name))
			b.WriteString(" = m.")
			b.WriteString(d.Quote(c.Name))
		}
		return b.String()
	}
	b.WriteString("WHERE ")
	for i, c := range keys {
		if i > 0 {
			b.WriteString(" AND ")
		}
		b.WriteString("t.")
		b.WriteString(d.Quote(c.Name))
		b.WriteString(" = m.")
		b.WriteString(d.Quote(c.Name))
	}
	return b.String()
}

// UpsertOnConflict 生成 PG 系（postgres/kingbase/sqlite）的 ON CONFLICT upsert。
func UpsertOnConflict(d Dialect, t TableRef, cols, keys, set []string, nrows int, doUpdate bool) string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(d.Qualified(t))
	b.WriteString(" (")
	b.WriteString(quoteAll(d, cols))
	b.WriteString(") VALUES ")
	for r := 0; r < nrows; r++ {
		if r > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		b.WriteString(placeholders(d, r*len(cols)+1, len(cols)))
		b.WriteByte(')')
	}
	b.WriteString(" ON CONFLICT (")
	b.WriteString(quoteAll(d, keys))
	b.WriteString(") DO ")
	if !doUpdate || len(set) == 0 {
		b.WriteString("NOTHING")
		return b.String()
	}
	b.WriteString("UPDATE SET ")
	for i, c := range set {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(d.Quote(c))
		b.WriteString(" = EXCLUDED.")
		b.WriteString(d.Quote(c))
	}
	return b.String()
}
