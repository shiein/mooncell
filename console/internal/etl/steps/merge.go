package steps

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/core"
)

// PortInfo 是第二输入端口（参照/对比流等辅助输入）。
const PortInfo = "info"

// 合并标志值（Kettle flagfield 常量）。
const (
	FlagIdentical = "identical"
	FlagChanged   = "changed"
	FlagNew       = "new"
	FlagDeleted   = "deleted"
)

// MergeRowsConfig 是「合并记录」（Kettle Merge rows (diff)）：
// 两路按键有序的流做归并比较，给每行追加标志字段。
//   - main（参照）：现有/旧数据；info（对比）：新数据
//   - identical 两边全等 / changed 键同值不同（发对比行）/ new 只在对比 / deleted 只在参照
type MergeRowsConfig struct {
	Keys      []string `json:"keys"`
	Values    []string `json:"values"`    // 参与比较的字段；空 = 全部非键字段
	FlagField string   `json:"flagField"` // 标志字段名，默认 flag
}

func mergeKeyIdx(in *core.Schema, keys []string) ([]int, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("至少需要一个合并键字段")
	}
	idx := make([]int, len(keys))
	for i, k := range keys {
		idx[i] = in.Index(k)
		if idx[i] < 0 {
			return nil, fmt.Errorf("合并键字段 %q 不在输入中", k)
		}
	}
	return idx, nil
}

func mergeValueIdx(in *core.Schema, keys []string, values []string) ([]int, error) {
	isKey := map[int]bool{}
	for _, k := range keys {
		isKey[in.Index(k)] = true
	}
	if len(values) == 0 {
		var idx []int
		for i := range in.Fields() {
			if !isKey[i] {
				idx = append(idx, i)
			}
		}
		return idx, nil
	}
	idx := make([]int, len(values))
	for i, v := range values {
		idx[i] = in.Index(v)
		if idx[i] < 0 {
			return nil, fmt.Errorf("比较字段 %q 不在输入中", v)
		}
	}
	return idx, nil
}

func mergeOutSchema(in *core.Schema, flagField, nodeID string) (*core.Schema, error) {
	if flagField == "" {
		flagField = "flag"
	}
	if in.Index(flagField) >= 0 {
		return nil, fmt.Errorf("标志字段 %q 与输入字段同名", flagField)
	}
	f := core.NewField(flagField, core.KindString)
	f.Origin = nodeID
	return in.Append(f)
}

func init() {
	core.Steps.Register(core.Define(core.Definition[MergeRowsConfig]{
		Meta: core.Meta{
			ID: "merge_rows", Order: 60, Label: "合并记录", Category: core.CategoryJoin, Icon: "merge",
			Stateful:     true,
			OrderedInput: true,
			Description:  "把按键有序的两条流（参照 main / 对比 info）归并为一条，追加标志字段：identical/changed/new/deleted。常与「数据同步」配合做全量比对同步。",
			Inputs: []core.PortDecl{
				{Name: core.PortMain, Label: "参照", Required: true},
				{Name: PortInfo, Label: "对比", Required: true},
			},
			Outputs:  []core.PortDecl{{Name: core.PortMain, Label: "输出"}},
			Keywords: []string{"Merge rows", "合并记录", "diff", "比对"},
		},
		Defaults: func() MergeRowsConfig { return MergeRowsConfig{FlagField: "flag"} },
		Infer: func(ic *core.InferContext, c *MergeRowsConfig) error {
			ref, cmp := ic.Input(core.PortMain), ic.Input(PortInfo)
			if ref == nil || cmp == nil {
				if ref == nil {
					ic.Errorf("", "input_required", "请连接参照输入")
				}
				if cmp == nil {
					ic.Errorf("", "info_required", "请连接对比输入")
				}
				return nil
			}
			if err := ref.Compatible(cmp); err != nil {
				ic.Errorf("keys", "schema_mismatch", "两路输入字段布局不一致：%s", err.Error())
				return nil
			}
			if _, err := mergeKeyIdx(cmp, c.Keys); err != nil {
				ic.Errorf("keys", "field_missing", "%s", err.Error())
			}
			if _, err := mergeValueIdx(cmp, c.Keys, c.Values); err != nil {
				ic.Errorf("values", "field_missing", "%s", err.Error())
			}
			s, err := mergeOutSchema(cmp, c.FlagField, ic.NodeID)
			if err != nil {
				ic.Errorf("flagField", "field_dup", "%s", err.Error())
			} else {
				ic.SetOutput(core.PortMain, s)
			}
			return nil
		},
		Open: func(rc *core.RunContext, c *MergeRowsConfig) (core.Runner, error) {
			refIn, cmpIn := rc.In(core.PortMain), rc.In(PortInfo)
			if err := refIn.Schema.Compatible(cmpIn.Schema); err != nil {
				return nil, fmt.Errorf("两路输入字段布局不一致：%w", err)
			}
			keys, err := mergeKeyIdx(cmpIn.Schema, c.Keys)
			if err != nil {
				return nil, err
			}
			values, err := mergeValueIdx(cmpIn.Schema, c.Keys, c.Values)
			if err != nil {
				return nil, err
			}
			return core.RunnerFunc(func(ctx context.Context) error {
				out := rc.Out(core.PortMain)
				ref, cmp := &rowCursor{p: refIn}, &rowCursor{p: cmpIn}
				batch := make([]core.Row, 0, rc.BatchSize)
				emit := func(r core.Row, flag string) error {
					batch = append(batch, append(r.Clone(), flag))
					if len(batch) == rc.BatchSize {
						err := out.Send(ctx, core.Batch{Schema: out.Schema, Rows: batch})
						batch = batch[:0]
						return err
					}
					return nil
				}
				for {
					rr, rOk, err := ref.peek(ctx)
					if err != nil {
						return err
					}
					cr, cOk, err := cmp.peek(ctx)
					if err != nil {
						return err
					}
					if !rOk && !cOk {
						break
					}
					keyCmp := 1
					if rOk && cOk {
						keyCmp = 0
						for _, k := range keys {
							if keyCmp = compareValues(rr[k], cr[k]); keyCmp != 0 {
								break
							}
						}
					} else if rOk {
						keyCmp = -1
					}
					switch {
					case keyCmp < 0: // 只在参照 → deleted
						if err := emit(rr, FlagDeleted); err != nil {
							return err
						}
						ref.advance()
					case keyCmp > 0: // 只在对比 → new
						if err := emit(cr, FlagNew); err != nil {
							return err
						}
						cmp.advance()
					default: // 键相同 → 值比较
						same := true
						for _, v := range values {
							if compareValues(rr[v], cr[v]) != 0 {
								same = false
								break
							}
						}
						if same {
							err = emit(cr, FlagIdentical)
						} else {
							err = emit(cr, FlagChanged)
						}
						if err != nil {
							return err
						}
						ref.advance()
						cmp.advance()
					}
				}
				if len(batch) > 0 {
					return out.Send(ctx, core.Batch{Schema: out.Schema, Rows: batch})
				}
				return nil
			}), nil
		},
	}))
}

// rowCursor 是单输入的逐行游标（peek/advance 语义）。
type rowCursor struct {
	p    *core.InPort
	buf  []core.Row
	i    int
	done bool
	cur  core.Row
	has  bool
}

func (c *rowCursor) peek(ctx context.Context) (core.Row, bool, error) {
	if c.has {
		return c.cur, true, nil
	}
	r, ok, err := c.next(ctx)
	if err != nil || !ok {
		return nil, ok, err
	}
	c.cur, c.has = r, true
	return r, true, nil
}

func (c *rowCursor) advance() { c.has, c.cur = false, nil }

func (c *rowCursor) next(ctx context.Context) (core.Row, bool, error) {
	for c.i >= len(c.buf) {
		if c.done {
			return nil, false, nil
		}
		b, ok, err := c.p.Next(ctx)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			c.done = true
			return nil, false, nil
		}
		c.buf, c.i = b.Rows, 0
	}
	r := c.buf[c.i]
	c.i++
	return r, true, nil
}

// ---------------------------------------------------------------- 数据同步

// SyncAfterMergeConfig 是「数据同步」（Kettle Synchronize after merge）：
// 上游行带标志字段（通常来自「合并记录」），按标志写目标表：
// new → 插入、changed → 更新、deleted → 删除、identical → 不动。
type SyncAfterMergeConfig struct {
	Connection string     `json:"connection"`
	Schema     string     `json:"schema"`
	Table      string     `json:"table"`
	FlagField  string     `json:"flagField"` // 默认 flag
	Keys       []FieldMap `json:"keys"`      // 查找键（行字段 → 表列）
	Fields     []FieldMap `json:"fields"`    // 插入/更新字段映射
	CommitSize int        `json:"commitSize"`
}

func init() {
	core.Steps.Register(core.Define(core.Definition[SyncAfterMergeConfig]{
		Meta: core.Meta{
			ID: "sync_after_merge", Order: 50, Label: "数据同步", Category: core.CategoryOutput, Icon: "sync",
			Stateful:    true,
			Description: "按标志字段分流写入：new 插入、changed 更新、deleted 删除、identical 不动。用于「合并记录」之后把差异同步到目标表（全量同步，含删除）。",
			Inputs:      []core.PortDecl{{Name: core.PortMain, Label: "输入", Required: true}},
			Outputs:     []core.PortDecl{{Name: core.PortMain, Label: "已处理"}},
			ErrorPort:   true,
			Keywords:    []string{"Synchronize after merge", "数据同步", "全量同步", "sync"},
		},
		Defaults: func() SyncAfterMergeConfig { return SyncAfterMergeConfig{CommitSize: 1000, FlagField: "flag"} },
		Infer: func(ic *core.InferContext, c *SyncAfterMergeConfig) error {
			ic.SetOutput(core.PortMain, ic.Main())
			if strings.TrimSpace(c.Table) == "" {
				ic.Errorf("table", "table_required", "请选择目标表")
				return nil
			}
			flag := c.FlagField
			if flag == "" {
				flag = "flag"
			}
			if ic.Main().Index(flag) < 0 {
				ic.Errorf("flagField", "field_missing", "输入中没有标志字段 %q（通常来自「合并记录」）", flag)
			}
			if len(c.Keys) == 0 {
				ic.Errorf("keys", "keys_required", "至少需要一个查询关键字")
				return nil
			}
			_, h := handleFor(ic, c.Connection, "connection")
			if h == nil {
				return nil
			}
			ref := tableRef(h, c.Schema, c.Table)
			kb, _, ok1 := bindColumns(ic, h, ref, ic.Main(), c.Keys, "keys")
			var fm []FieldMap
			for _, f := range c.Fields {
				fm = append(fm, f)
			}
			if len(fm) == 0 {
				// 缺省映射：除键与标志字段外的全部输入字段
				keySet := map[string]bool{}
				for _, k := range c.Keys {
					keySet[k.Stream] = true
				}
				for _, f := range ic.Main().Fields() {
					if f.Name != flag && !keySet[f.Name] {
						fm = append(fm, FieldMap{Stream: f.Name, Column: f.Name})
					}
				}
			}
			ins, _, ok2 := bindColumns(ic, h, ref, ic.Main(), fm, "fields")
			if !ok1 || !ok2 {
				return nil
			}
			if len(ins) == 0 {
				ic.Warnf("fields", "no_fields", "没有可写入的字段映射：插入/更新将只写关键字列")
			}
			if idx, err := h.Dialect.Indexes(ic.Ctx, h.DB, ref); err == nil && !coveredByIndex(idx, columnNames(kb)) {
				ic.Warnf("keys", "no_index", "目标表在关键字段（%s）上没有可用索引，大表上查找会很慢", strings.Join(columnNames(kb), ", "))
			}
			return nil
		},
		Open: func(rc *core.RunContext, c *SyncAfterMergeConfig) (core.Runner, error) {
			return core.RunnerFunc(func(ctx context.Context) error {
				h, err := runHandle(ctx, rc, c.Connection)
				if err != nil {
					return err
				}
				ref := tableRef(h, c.Schema, c.Table)
				in := rc.In(core.PortMain).Schema
				ic := core.NewInferContext(ctx, rc.NodeID, rc.Env, nil, map[string]*core.Schema{core.PortMain: in})
				flag := c.FlagField
				if flag == "" {
					flag = "flag"
				}
				flagIdx := in.Index(flag)
				if flagIdx < 0 {
					return fmt.Errorf("输入中没有标志字段 %q", flag)
				}
				keys, _, ok1 := bindColumns(ic, h, ref, in, c.Keys, "keys")
				var fm []FieldMap
				for _, f := range c.Fields {
					fm = append(fm, f)
				}
				if len(fm) == 0 {
					keySet := map[string]bool{}
					for _, k := range c.Keys {
						keySet[k.Stream] = true
					}
					for _, f := range in.Fields() {
						if f.Name != flag && !keySet[f.Name] {
							fm = append(fm, FieldMap{Stream: f.Name, Column: f.Name})
						}
					}
				}
				fields, _, ok2 := bindColumns(ic, h, ref, in, fm, "fields")
				if !ok1 || !ok2 {
					return ic.Problems().First()
				}
				commitSize := c.CommitSize
				if commitSize <= 0 {
					commitSize = 1000
				}
				w := &syncWriter{rc: rc, h: h, table: ref, flagIdx: flagIdx, keys: keys, fields: fields, commitSize: commitSize}
				// 插入列 = 键列 + 非键字段列；记录各列的值来源（keyVals 或 setVals 的下标）
				keyCols := map[string]bool{}
				for _, c := range w.keys {
					keyCols[c.column.Name] = true
					w.insMap = append(w.insMap, insRef{key: true, idx: len(w.insCols)})
					w.insCols = append(w.insCols, c)
				}
				for i, c := range w.fields {
					if !keyCols[c.column.Name] {
						w.insMap = append(w.insMap, insRef{idx: i})
						w.insCols = append(w.insCols, c)
					}
				}
				out := rc.Out(core.PortMain)
				err = rc.In(core.PortMain).Each(ctx, func(b core.Batch) error {
					for _, r := range b.Rows {
						if err := w.add(ctx, r); err != nil {
							return err
						}
						if w.size() >= commitSize {
							if err := w.flush(ctx, out); err != nil {
								return err
							}
						}
					}
					return nil
				})
				if err != nil {
					return err
				}
				return w.flush(ctx, out)
			}), nil
		},
	}))
}

type syncRow struct {
	src     core.Row
	flag    string
	keyVals []any
	setVals []any
}

// insRef 指向一列已转换值：key=true 取 keyVals[idx]，否则取 setVals[idx]。
type insRef struct {
	key bool
	idx int
}

type syncWriter struct {
	rc         *core.RunContext
	h          *conn.Handle
	table      conn.TableRef
	flagIdx    int
	keys       []boundColumn
	fields     []boundColumn
	insCols    []boundColumn
	insMap     []insRef
	commitSize int
	pending    []syncRow
	passed     []core.Row
}

func (w *syncWriter) size() int { return len(w.pending) }

func (w *syncWriter) add(ctx context.Context, r core.Row) error {
	raw := r[w.flagIdx]
	flag := ""
	if raw != nil {
		cv, err := core.Convert(raw, core.KindString, core.ConvertOptions{})
		if err != nil {
			return w.reject(ctx, r, err)
		}
		flag = cv.(string)
	}
	switch flag {
	case FlagIdentical:
		w.passed = append(w.passed, r)
		return nil
	case FlagNew, FlagChanged, FlagDeleted:
	default:
		return w.reject(ctx, r, fmt.Errorf("未知标志值 %q", flag))
	}
	keyVals, err := convertCols(w.keys, r)
	if err != nil {
		return w.reject(ctx, r, err)
	}
	for i, v := range keyVals {
		if v == nil {
			return w.reject(ctx, r, fmt.Errorf("查询关键字 %s 为 NULL", w.keys[i].stream.Name))
		}
	}
	var setVals []any
	if flag != FlagDeleted && len(w.fields) > 0 {
		if setVals, err = convertCols(w.fields, r); err != nil {
			return w.reject(ctx, r, err)
		}
	}
	w.pending = append(w.pending, syncRow{src: r, flag: flag, keyVals: keyVals, setVals: setVals})
	return nil
}

func (w *syncWriter) reject(ctx context.Context, r core.Row, err error) error {
	var re *core.RowError
	if errors.As(err, &re) {
		return w.rc.Reject(ctx, r, re.Err, re.Field)
	}
	return w.rc.Reject(ctx, r, err, "")
}

// flush：预览只透传；否则一个事务内按标志批量 插入/更新/删除。
func (w *syncWriter) flush(ctx context.Context, out *core.OutPort) error {
	if len(w.pending) == 0 && len(w.passed) == 0 {
		return nil
	}
	rows, passed := w.pending, w.passed
	w.pending, w.passed = nil, nil
	srcs := make([]core.Row, 0, len(rows)+len(passed))
	for _, r := range rows {
		srcs = append(srcs, r.src)
	}
	srcs = append(srcs, passed...)
	if w.rc.Preview {
		return out.Send(ctx, core.Batch{Schema: out.Schema, Rows: srcs})
	}
	var news, changed, deleted []syncRow
	for _, r := range rows {
		switch r.flag {
		case FlagNew:
			news = append(news, r)
		case FlagChanged:
			changed = append(changed, r)
		case FlagDeleted:
			deleted = append(deleted, r)
		}
	}
	tx, err := w.h.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()
	if err := w.insertRows(ctx, tx, news); err != nil {
		return err
	}
	if err := w.updateRows(ctx, tx, changed); err != nil {
		return err
	}
	if err := w.deleteRows(ctx, tx, deleted); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交失败：%w", err)
	}
	committed = true
	w.rc.Metrics.Output.Add(int64(len(news)))
	w.rc.Metrics.Updated.Add(int64(len(changed) + len(deleted)))
	w.rc.Log.Debug("提交：插入 %d、更新 %d、删除 %d", len(news), len(changed), len(deleted))
	return out.Send(ctx, core.Batch{Schema: out.Schema, Rows: srcs})
}

func (w *syncWriter) insertRows(ctx context.Context, tx conn.Querier, rows []syncRow) error {
	if len(rows) == 0 {
		return nil
	}
	d := w.h.Dialect
	names := columnNames(w.insCols)
	per := conn.RowsPerStatement(d, len(names), w.commitSize)
	for start := 0; start < len(rows); start += per {
		end := min(start+per, len(rows))
		args := make([]any, 0, (end-start)*len(w.insCols))
		for _, r := range rows[start:end] {
			for j, c := range w.insCols {
				var v any
				if w.insMap[j].key {
					v = r.keyVals[w.insMap[j].idx]
				} else {
					v = r.setVals[w.insMap[j].idx]
				}
				b, err := d.Bind(v, c.target)
				if err != nil {
					return err
				}
				args = append(args, b)
			}
		}
		if _, err := tx.ExecContext(ctx, conn.InsertSQL(d, w.table, names, end-start), args...); err != nil {
			return fmt.Errorf("插入 %s 失败：%w", w.table, err)
		}
	}
	return nil
}

func (w *syncWriter) updateRows(ctx context.Context, tx conn.Querier, rows []syncRow) error {
	if len(rows) == 0 {
		return nil
	}
	d := w.h.Dialect
	keys := make([]conn.DerivedCol, len(w.keys))
	for i, c := range w.keys {
		keys[i] = conn.DerivedCol{Name: c.column.Name, Cast: d.ParamCast(c.target)}
	}
	set := make([]conn.DerivedCol, len(w.fields))
	for i, c := range w.fields {
		set[i] = conn.DerivedCol{Name: c.column.Name, Cast: d.ParamCast(c.target)}
	}
	perRow := len(keys) + len(set)
	per := min(w.commitSize, max(1, d.MaxParams()/perRow))
	for start := 0; start < len(rows); start += per {
		end := min(start+per, len(rows))
		args := make([]any, 0, (end-start)*perRow)
		for _, r := range rows[start:end] {
			for i, v := range r.keyVals {
				b, err := d.Bind(v, w.keys[i].target)
				if err != nil {
					return err
				}
				args = append(args, b)
			}
			for i, v := range r.setVals {
				b, err := d.Bind(v, w.fields[i].target)
				if err != nil {
					return err
				}
				args = append(args, b)
			}
		}
		if _, err := tx.ExecContext(ctx, conn.UpdateFromSQL(d, w.table, keys, set, end-start), args...); err != nil {
			return fmt.Errorf("更新 %s 失败：%w", w.table, err)
		}
	}
	return nil
}

func (w *syncWriter) deleteRows(ctx context.Context, tx conn.Querier, rows []syncRow) error {
	if len(rows) == 0 {
		return nil
	}
	d := w.h.Dialect
	nk := len(w.keys)
	per := min(w.commitSize, max(1, d.MaxParams()/nk), lookupKeysPerStatement)
	keyNames := columnNames(w.keys)
	for start := 0; start < len(rows); start += per {
		end := min(start+per, len(rows))
		args := make([]any, 0, (end-start)*nk)
		for _, r := range rows[start:end] {
			for i, v := range r.keyVals {
				b, err := d.Bind(v, w.keys[i].target)
				if err != nil {
					return err
				}
				args = append(args, b)
			}
		}
		if _, err := tx.ExecContext(ctx, conn.DeleteSQL(d, w.table, keyNames, end-start), args...); err != nil {
			return fmt.Errorf("删除 %s 失败：%w", w.table, err)
		}
	}
	return nil
}
