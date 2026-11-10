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
