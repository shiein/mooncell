// Package steps 是内置步骤。每个步骤一个文件，在 init 中 core.Steps.Register(core.Define(...))。
//
// 新增步骤的完整清单：
//  1. 本包新建 <id>.go：Config 结构（带 json tag 与默认值）+ Infer + Open；
//  2. 前端 src/etl/forms/ 新建表单并 registerForm('<id>', Component)；
//  3. 本包补单元测试（至少：推导、正常路径、错误行、取消）。
//
// 引擎与编译器无需任何改动。
package steps

import (
	"context"
	"fmt"
	"strings"

	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/expr"
	"mooncell/console/internal/etl/sqltext"
)

// varsResolver 把运行期变量适配为 sqltext.Resolver。
func varsResolver(vars *core.Vars) sqltext.Resolver {
	return func(name string) (any, bool) {
		v, ok := vars.Get(name)
		return v.V, ok
	}
}

// samplesResolver 把设计期样例值适配为 sqltext.Resolver。
func samplesResolver(ic *core.InferContext) sqltext.Resolver {
	return func(name string) (any, bool) {
		if _, declared := ic.VarKinds[name]; !declared {
			return nil, false
		}
		v, ok := ic.Sample(name)
		return v, ok
	}
}

// render 按方言渲染 SQL，并把逻辑值绑定为驱动参数。
func render(d conn.Dialect, sql string, resolve sqltext.Resolver) (string, []any, error) {
	text, args, err := sqltext.Render(sql, d.Lexer(), d.Placeholder, resolve)
	if err != nil {
		return "", nil, err
	}
	for i, a := range args {
		b, err := d.Bind(a, core.Field{Kind: core.KindOf(a)})
		if err != nil {
			return "", nil, err
		}
		args[i] = b
	}
	return text, args, nil
}

// bindExtra 把下推优化写入配置的追加参数接到 ${} 参数之后
//（其占位符编号已在改写时按序生成，故此序不可颠倒）。
func bindExtra(d conn.Dialect, args, extra []any) ([]any, error) {
	for _, a := range extra {
		b, err := d.Bind(a, core.Field{Kind: core.KindOf(a)})
		if err != nil {
			return nil, err
		}
		args = append(args, b)
	}
	return args, nil
}

// handleFor 在 Infer 中取连接（失败即报告问题并返回 nil）。
func handleFor(ic *core.InferContext, connID, path string) (*conn.Pool, *conn.Handle) {
	if strings.TrimSpace(connID) == "" {
		ic.Errorf(path, "connection_required", "请选择数据库连接")
		return nil, nil
	}
	pool, err := conn.PoolFrom(ic.Env)
	if err != nil {
		ic.Errorf(path, "env", "%s", err.Error())
		return nil, nil
	}
	h, err := pool.Get(ic.Ctx, connID)
	if err != nil {
		ic.Errorf(path, "connection", "无法连接数据库：%s", err.Error())
		return nil, nil
	}
	return pool, h
}

// runHandle 在运行期取连接。
func runHandle(ctx context.Context, rc *core.RunContext, connID string) (*conn.Handle, error) {
	pool, err := conn.PoolFrom(rc.Env)
	if err != nil {
		return nil, err
	}
	h, err := pool.Get(ctx, connID)
	if err != nil {
		return nil, fmt.Errorf("无法连接数据库：%w", err)
	}
	return h, nil
}

// FieldMap 是「流字段 → 表列」映射。
type FieldMap struct {
	Stream string `json:"stream"`
	Column string `json:"column"`
}

// targetColumns 解析目标表列并按映射校验；mapping 为空时按同名映射全部流字段。
// 返回：按映射顺序的（流字段下标、目标列字段）。
type boundColumn struct {
	streamIdx int
	stream    core.Field
	column    conn.ColumnMeta
	target    core.Field // 目标列的逻辑字段（写入前按它转换）
}

func bindColumns(ic *core.InferContext, h *conn.Handle, table conn.TableRef, in *core.Schema, mapping []FieldMap, path string) ([]boundColumn, []conn.ColumnMeta, bool) {
	cols, err := h.Dialect.Columns(ic.Ctx, h.DB, table)
	if err != nil {
		ic.Errorf(path, "columns", "读取表 %s 的字段失败：%s", table, err.Error())
		return nil, nil, false
	}
	if len(cols) == 0 {
		ic.Errorf(path, "table_missing", "目标表 %s 不存在或没有可见字段", table)
		return nil, nil, false
	}
	byName := map[string]conn.ColumnMeta{}
	byFold := map[string]conn.ColumnMeta{}
	for _, c := range cols {
		byName[c.Name] = c
		byFold[strings.ToLower(c.Name)] = c
	}
	if len(mapping) == 0 {
		for _, f := range in.Fields() {
			mapping = append(mapping, FieldMap{Stream: f.Name, Column: f.Name})
		}
	}
	var out []boundColumn
	ok := true
	seenCol := map[string]bool{}
	for i, m := range mapping {
		idx := in.Index(m.Stream)
		if idx < 0 {
			ic.Errorf(fmt.Sprintf("%s[%d].stream", path, i), "field_missing", "输入中没有字段 %q", m.Stream)
			ok = false
			continue
		}
		col, found := byName[m.Column]
		if !found {
			col, found = byFold[strings.ToLower(m.Column)]
		}
		if !found {
			ic.Errorf(fmt.Sprintf("%s[%d].column", path, i), "column_missing", "目标表 %s 没有字段 %q", table, m.Column)
			ok = false
			continue
		}
		if seenCol[col.Name] {
			ic.Errorf(fmt.Sprintf("%s[%d].column", path, i), "column_dup", "目标字段 %q 被映射了多次", col.Name)
			ok = false
			continue
		}
		seenCol[col.Name] = true
		sf := in.Field(idx)
		tf := h.Dialect.FieldFor(col)
		if !convertible(sf.Kind, tf.Kind) {
			ic.Warnf(fmt.Sprintf("%s[%d]", path, i), "kind_mismatch", "字段 %s（%s）写入 %s（%s）需要类型转换，失败的行将进入错误输出", sf.Name, sf.Kind.Label(), col.Name, col.DBType)
		}
		out = append(out, boundColumn{streamIdx: idx, stream: sf, column: col, target: tf})
	}
	return out, cols, ok
}

// convertible 判断是否无需警告的「自然」转换。
func convertible(from, to core.Kind) bool {
	if from == to || to == core.KindString {
		return true
	}
	if from.IsNumeric() && to.IsNumeric() {
		return true
	}
	return from.IsTemporal() && to.IsTemporal()
}

// bindRow 把一行按映射转换为驱动参数；转换失败返回 *core.RowError。
func bindRow(d conn.Dialect, cols []boundColumn, row core.Row, dst []any) ([]any, error) {
	for _, c := range cols {
		v, err := core.Convert(row[c.streamIdx], c.target.Kind, core.ConvertOptions{})
		if err != nil {
			return dst, core.NewRowError(c.stream.Name, err)
		}
		b, err := d.Bind(v, c.target)
		if err != nil {
			return dst, core.NewRowError(c.stream.Name, err)
		}
		dst = append(dst, b)
	}
	return dst, nil
}

func columnNames(cols []boundColumn) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.column.Name
	}
	return out
}

func tableRef(h *conn.Handle, schema, table string) conn.TableRef {
	ref := conn.TableRef{Schema: conn.EffectiveSchema(h.Config, schema), Name: strings.TrimSpace(table)}
	// 用户输入按方言折叠为服务器规范形式（达梦 → 大写），否则限定引用找不到对象。
	ref.Schema, ref.Name = h.Dialect.FoldIdent(ref.Schema), h.Dialect.FoldIdent(ref.Name)
	return ref
}

// withOrigin 返回把全部字段 Origin 设为 nodeID 的新 Schema。
func withOrigin(s *core.Schema, nodeID string) *core.Schema {
	fields := s.Fields()
	for i := range fields {
		fields[i].Origin = nodeID
	}
	out, err := core.NewSchema(fields...)
	if err != nil {
		return s
	}
	return out
}

// goLayout 把 yyyy-MM-dd 风格格式转为 Go layout；空串保持空（自动识别）。
func goLayout(f string) string {
	if strings.TrimSpace(f) == "" {
		return ""
	}
	return expr.GoLayout(f)
}
