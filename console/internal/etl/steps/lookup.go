package steps

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/core"
)

// LookupField 是查询返回的一个字段：取参照列 Column，输出为 Name，未命中填 Default。
type LookupField struct {
	Column  string `json:"column"`
	Name    string `json:"name"`    // 空 = 同 Column
	Default string `json:"default"` // 未命中默认值（文本字面量，按列类型转换；空 = NULL）
}

// 解析返回字段映射：参照 schema 中的列 → 输出字段 / 列下标 / 已按类型转换的默认值。
func planReturns(ref *core.Schema, returns []LookupField, nodeID string) ([]core.Field, []int, []any, []core.Problem) {
	var probs []core.Problem
	var fields []core.Field
	var idx []int
	var defaults []any
	for i, rf := range returns {
		path := fmt.Sprintf("returns[%d]", i)
		ci := ref.Index(rf.Column)
		if ci < 0 {
			probs = append(probs, core.Problem{Severity: core.SeverityError, Path: path, Code: "field_missing", Message: fmt.Sprintf("参照中没有字段 %q", rf.Column)})
			continue
		}
		out := ref.Field(ci)
		if rf.Name != "" {
			out.Name = rf.Name
		}
		out.Origin = nodeID
		var def any
		if rf.Default != "" {
			cv, err := core.Convert(rf.Default, out.Kind, core.ConvertOptions{})
			if err != nil {
				probs = append(probs, core.Problem{Severity: core.SeverityError, Path: path + ".default", Code: "convert", Message: fmt.Sprintf("默认值 %q 无法转为%s：%s", rf.Default, out.Kind.Label(), err.Error())})
				continue
			}
			def = cv
		}
		fields = append(fields, out)
		idx = append(idx, ci)
		defaults = append(defaults, def)
	}
	return fields, idx, defaults, probs
}

// schemaFromColumns 用方言列元数据构造参照 schema。
func schemaFromColumns(h *conn.Handle, cols []conn.ColumnMeta) (*core.Schema, error) {
	fields := make([]core.Field, len(cols))
	for i, c := range cols {
		fields[i] = h.Dialect.FieldFor(c)
	}
	return core.NewSchema(fields...)
}

// ---------------------------------------------------------------- 流查询

// StreamLookupConfig 是「流查询」（Kettle Stream lookup）：
// 参照流（info 端口）整体装入哈希表，主流行按键查表并追加返回字段。
type StreamLookupConfig struct {
	Keys    []FieldMap    `json:"keys"`    // stream=主流字段 → column=参照流字段
	Returns []LookupField `json:"returns"`
}

func init() {
	core.Steps.Register(core.Define(core.Definition[StreamLookupConfig]{
		Meta: core.Meta{
			ID: "stream_lookup", Order: 40, Label: "流查询", Category: core.CategoryLookup, Icon: "list-search",
			Description: "把参照流按键装入内存哈希表，主流行查表并追加返回字段；重复键保留首行，未命中填默认值（默认 NULL）。参照数据量大请改用「数据库查询」。",
			Inputs: []core.PortDecl{
				{Name: core.PortMain, Label: "输入", Required: true},
				{Name: PortInfo, Label: "参照", Required: true},
			},
			Outputs:  []core.PortDecl{{Name: core.PortMain, Label: "输出"}},
			Keywords: []string{"Stream lookup", "流查询", "内存查询", "关联"},
		},
		Defaults: func() StreamLookupConfig { return StreamLookupConfig{} },
		Infer: func(ic *core.InferContext, c *StreamLookupConfig) error {
			main, ref := ic.Main(), ic.Input(PortInfo)
			if ref == nil {
				ic.Errorf("", "info_required", "请连接参照输入")
				ic.SetOutput(core.PortMain, main)
				return nil
			}
			if len(c.Keys) == 0 {
				ic.Errorf("keys", "keys_required", "至少需要一个查询键")
			}
			for i, k := range c.Keys {
				if main.Index(k.Stream) < 0 {
					ic.Errorf(fmt.Sprintf("keys[%d]", i), "field_missing", "主流中没有字段 %q", k.Stream)
				}
				if ref.Index(k.Column) < 0 {
					ic.Errorf(fmt.Sprintf("keys[%d]", i), "field_missing", "参照流中没有字段 %q", k.Column)
				}
			}
			fields, _, _, probs := planReturns(ref, c.Returns, ic.NodeID)
			for _, p := range probs {
				ic.Errorf(p.Path, p.Code, "%s", p.Message)
			}
			if len(fields) == 0 {
				ic.Warnf("returns", "no_returns", "没有返回字段")
			}
			s, err := main.Append(fields...)
			if err != nil {
				ic.Errorf("returns", "schema", "%s", err.Error())
				return nil
			}
			ic.SetOutput(core.PortMain, s)
			return nil
		},
		Open: func(rc *core.RunContext, c *StreamLookupConfig) (core.Runner, error) {
			main, ref := rc.In(core.PortMain).Schema, rc.In(PortInfo).Schema
			if len(c.Keys) == 0 {
				return nil, fmt.Errorf("至少需要一个查询键")
			}
			mk := make([]int, len(c.Keys))
			rk := make([]int, len(c.Keys))
			for i, k := range c.Keys {
				if mk[i] = main.Index(k.Stream); mk[i] < 0 {
					return nil, fmt.Errorf("主流中没有字段 %q", k.Stream)
				}
				if rk[i] = ref.Index(k.Column); rk[i] < 0 {
					return nil, fmt.Errorf("参照流中没有字段 %q", k.Column)
				}
			}
			_, rIdx, defaults, probs := planReturns(ref, c.Returns, rc.NodeID)
			if len(probs) > 0 {
				return nil, probs[0]
			}
			var maxEntries int64 = -1
			if rc.MemoryBudget > 0 {
				// 条目估算：哈希 16 + 每返回值 ~40 字节。
				maxEntries = rc.MemoryBudget / int64(16+len(rIdx)*40)
			}
			return core.RunnerFunc(func(ctx context.Context) error {
				// 1) 参照流全量入哈希表（重复键保留首行）
				table := map[[16]byte][]any{}
				var nEntries int64
				buf := make([]byte, 0, 128)
				err := rc.In(PortInfo).Each(ctx, func(b core.Batch) error {
					for _, r := range b.Rows {
						buf = rowKey(buf[:0], r, rk)
						sum := sha256.Sum256(buf)
						var h [16]byte
						copy(h[:], sum[:16])
						if _, ok := table[h]; ok {
							continue
						}
						if maxEntries >= 0 && nEntries >= maxEntries {
							return fmt.Errorf("参照流已超过内存预算（%d 条），请调大转换内存预算或改用「数据库查询」", maxEntries)
						}
						vals := make([]any, len(rIdx))
						for j, idx := range rIdx {
							vals[j] = r[idx]
						}
						table[h] = vals
						nEntries++
					}
					return nil
				})
				if err != nil {
					return err
				}
				// 2) 主流逐行查表（键含 NULL 也照常哈希比对，与参照流 NULL 匹配）
				return core.MapRows(rc, func(r core.Row) (core.Row, error) {
					out := make(core.Row, len(r)+len(rIdx))
					copy(out, r)
					buf = rowKey(buf[:0], r, mk)
					sum := sha256.Sum256(buf)
					var h [16]byte
					copy(h[:], sum[:16])
					vals, ok := table[h]
					if !ok {
						vals = defaults
					}
					for j := range vals {
						out[len(r)+j] = vals[j]
					}
					return out, nil
				}).Run(ctx)
			}), nil
		},
	}))
}

// ---------------------------------------------------------------- 数据库查询

// DBLookupConfig 是「数据库查询」（Kettle Database lookup）：
// 逐行以流字段为参数等值查目标表，追加返回列；结果按查询键缓存。
type DBLookupConfig struct {
	Connection  string        `json:"connection"`
	Schema      string        `json:"schema"`
	Table       string        `json:"table"`
	Keys        []FieldMap    `json:"keys"`    // 等值查询键
	Returns     []LookupField `json:"returns"`
	OrderBy     string        `json:"orderBy"`     // 多行命中时的排序列（可选）
	FailOnMulti bool          `json:"failOnMulti"` // 多行命中 → 错误行（默认取第一行）
	NoCache     bool          `json:"noCache"`     // 关闭结果缓存（默认按键缓存）
}

func init() {
	core.Steps.Register(core.Define(core.Definition[DBLookupConfig]{
		Meta: core.Meta{
			ID: "db_lookup", Order: 50, Label: "数据库查询", Category: core.CategoryLookup, Icon: "database-search",
			Description: "逐行按键等值查询目标表并追加返回列；相同键的结果在运行内缓存（可用「关闭缓存」禁用），多行命中默认取第一行。",
			Inputs:      []core.PortDecl{{Name: core.PortMain, Label: "输入", Required: true}},
			Outputs:     []core.PortDecl{{Name: core.PortMain, Label: "输出"}},
			ErrorPort:   true,
			Keywords:    []string{"Database lookup", "数据库查询", "维度查询", "lookup"},
		},
		Defaults: func() DBLookupConfig { return DBLookupConfig{} },
		Infer: func(ic *core.InferContext, c *DBLookupConfig) error {
			ic.SetOutput(core.PortMain, ic.Main())
			if strings.TrimSpace(c.Table) == "" {
				ic.Errorf("table", "table_required", "请选择目标表")
				return nil
			}
			if len(c.Keys) == 0 {
				ic.Errorf("keys", "keys_required", "至少需要一个查询键")
				return nil
			}
			_, h := handleFor(ic, c.Connection, "connection")
			if h == nil {
				return nil
			}
			ref := tableRef(h, c.Schema, c.Table)
			kb, cols, ok := bindColumns(ic, h, ref, ic.Main(), c.Keys, "keys")
			if !ok {
				return nil
			}
			refSchema, err := schemaFromColumns(h, cols)
			if err != nil {
				ic.Errorf("table", "schema", "%s", err.Error())
				return nil
			}
			fields, _, _, probs := planReturns(refSchema, c.Returns, ic.NodeID)
			for _, p := range probs {
				ic.Errorf(p.Path, p.Code, "%s", p.Message)
			}
			if len(fields) == 0 {
				ic.Warnf("returns", "no_returns", "没有返回字段：步骤只做存在性判断")
			}
			s, err := ic.Main().Append(fields...)
			if err != nil {
				ic.Errorf("returns", "schema", "%s", err.Error())
				return nil
			}
			ic.SetOutput(core.PortMain, s)
			if idx, err := h.Dialect.Indexes(ic.Ctx, h.DB, ref); err == nil && !coveredByIndex(idx, columnNames(kb)) {
				ic.Warnf("keys", "no_index", "目标表在关键字段（%s）上没有可用索引，逐行查询会很慢", strings.Join(columnNames(kb), ", "))
			}
			return nil
		},
		Open: func(rc *core.RunContext, c *DBLookupConfig) (core.Runner, error) {
			return core.RunnerFunc(func(ctx context.Context) error {
				h, err := runHandle(ctx, rc, c.Connection)
				if err != nil {
					return err
				}
				ref := tableRef(h, c.Schema, c.Table)
				in := rc.In(core.PortMain).Schema
				ic := core.NewInferContext(ctx, rc.NodeID, rc.Env, nil, map[string]*core.Schema{core.PortMain: in})
				keys, cols, ok := bindColumns(ic, h, ref, in, c.Keys, "keys")
				if !ok {
					return ic.Problems().First()
				}
				refSchema, err := schemaFromColumns(h, cols)
				if err != nil {
					return err
				}
				_, rIdx, defaults, probs := planReturns(refSchema, c.Returns, rc.NodeID)
				if len(probs) > 0 {
					return probs[0]
				}
				l := &dbLookup{h: h, keys: keys, defaults: defaults, failMulti: c.FailOnMulti, loc: h.Location()}
				for _, k := range keys {
					l.keyIdx = append(l.keyIdx, k.streamIdx)
				}
				// SELECT "c1",… FROM t WHERE "k1" = ? AND … [ORDER BY "o"]
				d := h.Dialect
				var retCols []string
				for _, ci := range rIdx {
					f := refSchema.Field(ci)
					l.retFields = append(l.retFields, f)
					retCols = append(retCols, d.Quote(f.Name))
				}
				var where []string
				for i, c := range keys {
					where = append(where, fmt.Sprintf("%s = %s", d.Quote(c.column.Name), d.Placeholder(i+1)))
				}
				l.sql = fmt.Sprintf("SELECT %s FROM %s WHERE %s", strings.Join(retCols, ", "), ref, strings.Join(where, " AND "))
				if c.OrderBy != "" {
					l.sql += " ORDER BY " + d.Quote(c.OrderBy)
				}
				if !c.NoCache {
					l.cache = map[[16]byte][]any{}
					l.maxEntries = -1
					if rc.MemoryBudget > 0 {
						l.maxEntries = rc.MemoryBudget / int64(16+len(rIdx)*40)
					}
				}
				l.buf = make([]byte, 0, 128)
				out := rc.Out(core.PortMain)
				return rc.In(core.PortMain).Each(ctx, func(b core.Batch) error {
					var rows []core.Row
					for _, r := range b.Rows {
						or, err := l.one(ctx, r)
						if err != nil {
							if err := rc.Reject(ctx, r, err, ""); err != nil {
								return err
							}
							continue
						}
						rows = append(rows, or)
					}
					return out.Send(ctx, core.Batch{Schema: out.Schema, Rows: rows})
				})
			}), nil
		},
	}))
}

type dbLookup struct {
	h          *conn.Handle
	sql        string
	keys       []boundColumn
	keyIdx     []int
	retFields  []core.Field
	defaults   []any
	failMulti  bool
	cache      map[[16]byte][]any
	maxEntries int64
	loc        *time.Location
	buf        []byte
}

// one 查一行：先查缓存，未命中走库；NULL 键直接判未命中（= NULL 恒不成立）。
// vals=nil 表示「查过了、没行」，与「还没查」区分（缓存也存 nil）。
func (l *dbLookup) one(ctx context.Context, r core.Row) (core.Row, error) {
	out := make(core.Row, len(r)+len(l.defaults))
	copy(out, r)
	keyVals, err := convertCols(l.keys, r)
	if err != nil {
		return nil, err
	}
	var vals []any
	resolved := false
	missKey := false
	for _, v := range keyVals {
		if v == nil {
			missKey = true
			break
		}
	}
	if !missKey {
		l.buf = rowKey(l.buf[:0], r, l.keyIdx)
		sum := sha256.Sum256(l.buf)
		var h [16]byte
		copy(h[:], sum[:16])
		if l.cache != nil {
			if cv, ok := l.cache[h]; ok {
				vals, resolved = cv, true
			}
		}
		if !resolved {
			args := make([]any, len(l.keys))
			for i, v := range keyVals {
				b, err := l.h.Dialect.Bind(v, l.keys[i].target)
				if err != nil {
					return nil, err
				}
				args[i] = b
			}
			rows, err := l.h.DB.QueryContext(ctx, l.sql, args...)
			if err != nil {
				return nil, fmt.Errorf("查询 %s 失败：%w", l.sql, err)
			}
			n := 0
			for rows.Next() {
				if n == 0 {
					vals = make([]any, len(l.defaults))
					scan := make([]any, len(l.defaults))
					for i := range scan {
						scan[i] = &vals[i]
					}
					if err := rows.Scan(scan...); err != nil {
						rows.Close()
						return nil, err
					}
					for i := range vals {
						v, err := l.h.Dialect.Normalize(vals[i], l.retFields[i], l.loc)
						if err != nil {
							rows.Close()
							return nil, err
						}
						vals[i] = v
					}
				}
				n++
				if n >= 2 {
					break
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return nil, err
			}
			rows.Close()
			if l.failMulti && n > 1 {
				return nil, fmt.Errorf("查询命中多行（「多行命中报错」已开启）")
			}
			if n == 0 {
				vals = nil
			}
			resolved = true
			if l.cache != nil && (l.maxEntries < 0 || int64(len(l.cache)) < l.maxEntries) {
				l.cache[h] = vals
			}
		}
	}
	if vals == nil {
		copy(out[len(r):], l.defaults)
	} else {
		copy(out[len(r):], vals)
	}
	return out, nil
}
