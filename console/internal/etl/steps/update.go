package steps

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/core"
)

// UpdateConfig 是「更新」（Kettle Update）：按键命中目标行则更新映射字段；
// 未命中的行进错误输出（未连接时跳过并记日志），不做插入。
type UpdateConfig struct {
	Connection string     `json:"connection"`
	Schema     string     `json:"schema"`
	Table      string     `json:"table"`
	Keys       []FieldMap `json:"keys"`
	Fields     []FieldMap `json:"fields"`
	CommitSize int        `json:"commitSize"`
}

// DeleteConfig 是「删除」：按键命中目标行则删除；未命中行同「更新」的处理。
type DeleteConfig struct {
	Connection string     `json:"connection"`
	Schema     string     `json:"schema"`
	Table      string     `json:"table"`
	Keys       []FieldMap `json:"keys"`
	CommitSize int        `json:"commitSize"`
}

const (
	keyOpUpdate = "update"
	keyOpDelete = "delete"
)

// keyPlan 是按键写入的执行计划。
type keyPlan struct {
	keys []boundColumn
	set  []boundColumn // update 才有
}

// keyRow 是已转换的待写行。
type keyRow struct {
	src     core.Row
	key     string
	keyVals []any
	setVals []any
}

type keyWriter struct {
	rc         *core.RunContext
	h          *conn.Handle
	table      conn.TableRef
	op         string
	commitSize int
	plan       *keyPlan
	pending    []keyRow
}

// planKeys 解析关键字与更新字段映射并做设计期校验（表存在、字段齐全、索引告警）。
func planKeys(ic *core.InferContext, h *conn.Handle, ref conn.TableRef, in *core.Schema, keys, fields []FieldMap, op string) (*keyPlan, bool) {
	kb, _, ok1 := bindColumns(ic, h, ref, in, keys, "keys")
	p := &keyPlan{keys: kb}
	ok2 := true
	if op == keyOpUpdate {
		p.set, _, ok2 = bindColumns(ic, h, ref, in, fields, "fields")
	}
	if !ok1 || !ok2 {
		return nil, false
	}
	// 目标列既作键又作更新字段：Kettle 允许但更新键本身没有意义且易错
	if idx, err := h.Dialect.Indexes(ic.Ctx, h.DB, ref); err == nil && !coveredByIndex(idx, columnNames(kb)) {
		ic.Warnf("keys", "no_index", "目标表在关键字段（%s）上没有可用索引，大表上查找会很慢，建议建立索引", strings.Join(columnNames(kb), ", "))
	}
	return p, true
}

func (w *keyWriter) add(ctx context.Context, r core.Row) error {
	row := keyRow{src: r}
	var err error
	if row.keyVals, err = convertCols(w.plan.keys, r); err == nil && len(w.plan.set) > 0 {
		row.setVals, err = convertCols(w.plan.set, r)
	}
	if err == nil {
		for i, v := range row.keyVals {
			if v == nil {
				err = core.NewRowError(w.plan.keys[i].stream.Name, errors.New("查询关键字为 NULL"))
				break
			}
		}
	}
	if err != nil {
		var re *core.RowError
		if errors.As(err, &re) {
			return w.rc.Reject(ctx, r, re.Err, re.Field)
		}
		return err
	}
	row.key = string(rowKey(nil, row.keyVals, seq(len(row.keyVals))))
	w.pending = append(w.pending, row)
	return nil
}

// flush 在一个事务内：按键命中检测 → 命中行批量写 → 未命中行处理 → 提交。
func (w *keyWriter) flush(ctx context.Context, out *core.OutPort) error {
	if len(w.pending) == 0 {
		return nil
	}
	rows := w.pending
	w.pending = nil

	last := map[string]int{}
	for i, r := range rows {
		last[r.key] = i
	}
	var uniq []keyRow
	for i, r := range rows {
		if last[r.key] == i {
			uniq = append(uniq, r)
		}
	}
	if w.rc.Preview {
		return out.Send(ctx, core.Batch{Schema: out.Schema, Rows: keySrcRows(rows)})
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

	found, err := w.lookup(ctx, tx, uniq)
	if err != nil {
		return err
	}
	var matched []keyRow
	canErr := w.rc.HasOutput(core.PortError)
	var skipped int
	for _, r := range uniq {
		if found[r.key] {
			matched = append(matched, r)
			continue
		}
		if canErr {
			if err := w.rc.Reject(ctx, r.src, errors.New("关键字在目标表未匹配到记录"), ""); err != nil {
				return err
			}
		} else {
			skipped++
		}
	}
	switch w.op {
	case keyOpUpdate:
		err = w.updateBatch(ctx, tx, matched)
	case keyOpDelete:
		err = w.deleteBatch(ctx, tx, matched)
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交失败：%w", err)
	}
	committed = true
	w.rc.Metrics.Updated.Add(int64(len(matched)))
	if skipped > 0 {
		w.rc.Log.Info("%d 行关键字在 %s 未匹配，已跳过", skipped, w.table)
	}
	w.rc.Log.Debug("提交：%s %d 行", w.op, len(matched))
	return out.Send(ctx, core.Batch{Schema: out.Schema, Rows: keySrcRows(rows)})
}

func keySrcRows(rows []keyRow) []core.Row {
	out := make([]core.Row, len(rows))
	for i, r := range rows {
		out[i] = r.src
	}
	return out
}

// lookup 分块查询已存在的键，返回命中键集合。
func (w *keyWriter) lookup(ctx context.Context, tx conn.Querier, rows []keyRow) (map[string]bool, error) {
	d := w.h.Dialect
	nk := len(w.plan.keys)
	per := min(lookupKeysPerStatement, max(1, d.MaxParams()/nk))
	keyNames := columnNames(w.plan.keys)
	found := map[string]bool{}
	var batch []keyRow
	run := func() error {
		if len(batch) == 0 {
			return nil
		}
		q := conn.LookupSQL(d, w.table, keyNames, nil, len(batch))
		args := make([]any, 0, len(batch)*nk)
		for _, r := range batch {
			for i, v := range r.keyVals {
				b, err := d.Bind(v, w.plan.keys[i].target)
				if err != nil {
					return err
				}
				args = append(args, b)
			}
		}
		res, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("查找已存在记录失败：%w", err)
		}
		defer res.Close()
		vals := make([]any, nk)
		ptrs := make([]any, nk)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		loc := w.h.Location()
		for res.Next() {
			if err := res.Scan(ptrs...); err != nil {
				return err
			}
			keyVals := make([]any, nk)
			for i := 0; i < nk; i++ {
				v, err := d.Normalize(vals[i], w.plan.keys[i].target, loc)
				if err != nil {
					return err
				}
				keyVals[i] = v
			}
			found[string(rowKey(nil, keyVals, seq(nk)))] = true
		}
		batch = batch[:0]
		return res.Err()
	}
	for _, r := range rows {
		batch = append(batch, r)
		if len(batch) >= per {
			if err := run(); err != nil {
				return nil, err
			}
		}
	}
	return found, run()
}

// updateBatch 与 insert_update 同形态：派生行集 + 连接更新，按键命中行更新 set 列。
func (w *keyWriter) updateBatch(ctx context.Context, tx conn.Querier, rows []keyRow) error {
	if len(rows) == 0 {
		return nil
	}
	d := w.h.Dialect
	keys := make([]conn.DerivedCol, len(w.plan.keys))
	for i, c := range w.plan.keys {
		keys[i] = conn.DerivedCol{Name: c.column.Name, Cast: d.ParamCast(c.target)}
	}
	set := make([]conn.DerivedCol, len(w.plan.set))
	for i, c := range w.plan.set {
		set[i] = conn.DerivedCol{Name: c.column.Name, Cast: d.ParamCast(c.target)}
	}
	perRow := len(keys) + len(set)
	per := min(w.commitSize, max(1, d.MaxParams()/perRow))
	for start := 0; start < len(rows); start += per {
		end := min(start+per, len(rows))
		args := make([]any, 0, (end-start)*perRow)
		for _, r := range rows[start:end] {
			for i, v := range r.keyVals {
				b, err := d.Bind(v, w.plan.keys[i].target)
				if err != nil {
					return err
				}
				args = append(args, b)
			}
			for i, v := range r.setVals {
				b, err := d.Bind(v, w.plan.set[i].target)
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

// deleteBatch 按 OR 键条件批量删除命中行。
func (w *keyWriter) deleteBatch(ctx context.Context, tx conn.Querier, rows []keyRow) error {
	if len(rows) == 0 {
		return nil
	}
	d := w.h.Dialect
	nk := len(w.plan.keys)
	per := min(w.commitSize, max(1, d.MaxParams()/nk), lookupKeysPerStatement)
	keyNames := columnNames(w.plan.keys)
	for start := 0; start < len(rows); start += per {
		end := min(start+per, len(rows))
		args := make([]any, 0, (end-start)*nk)
		for _, r := range rows[start:end] {
			for i, v := range r.keyVals {
				b, err := d.Bind(v, w.plan.keys[i].target)
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

// keyStepRunner 装配并运行按键写步骤（update/delete 共用）。
func keyStepRunner(rc *core.RunContext, connID, schema, table string, keys, fields []FieldMap, commitSize int, op string) (core.Runner, error) {
	return core.RunnerFunc(func(ctx context.Context) error {
		h, err := runHandle(ctx, rc, connID)
		if err != nil {
			return err
		}
		ref := tableRef(h, schema, table)
		ic := core.NewInferContext(ctx, rc.NodeID, rc.Env, nil, map[string]*core.Schema{core.PortMain: rc.In(core.PortMain).Schema})
		plan, ok := planKeys(ic, h, ref, rc.In(core.PortMain).Schema, keys, fields, op)
		if !ok {
			return ic.Problems().First()
		}
		if commitSize <= 0 {
			commitSize = 1000
		}
		w := &keyWriter{rc: rc, h: h, table: ref, op: op, commitSize: commitSize, plan: plan}
		out := rc.Out(core.PortMain)
		err = rc.In(core.PortMain).Each(ctx, func(b core.Batch) error {
			for _, r := range b.Rows {
				if err := w.add(ctx, r); err != nil {
					return err
				}
				if len(w.pending) >= w.commitSize {
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
}

func init() {
	core.Steps.Register(core.Define(core.Definition[UpdateConfig]{
		Meta: core.Meta{
			ID: "update", Order: 30, Label: "更新", Category: core.CategoryOutput, Icon: "update",
			Description: "按查询关键字更新目标表已存在的记录。未命中的行进入错误输出（未连接时跳过），不会插入新记录。",
			Inputs:      []core.PortDecl{{Name: core.PortMain, Label: "输入", Required: true}},
			Outputs:     []core.PortDecl{{Name: core.PortMain, Label: "已处理"}},
			ErrorPort:   true,
			Keywords:    []string{"Update", "更新"},
		},
		Defaults: func() UpdateConfig { return UpdateConfig{CommitSize: 1000} },
		Infer: func(ic *core.InferContext, c *UpdateConfig) error {
			ic.SetOutput(core.PortMain, ic.Main())
			if strings.TrimSpace(c.Table) == "" {
				ic.Errorf("table", "table_required", "请选择目标表")
				return nil
			}
			if len(c.Keys) == 0 {
				ic.Errorf("keys", "keys_required", "至少需要一个查询关键字")
				return nil
			}
			if len(c.Fields) == 0 {
				ic.Errorf("fields", "fields_required", "至少需要一个更新字段")
				return nil
			}
			_, h := handleFor(ic, c.Connection, "connection")
			if h == nil {
				return nil
			}
			planKeys(ic, h, tableRef(h, c.Schema, c.Table), ic.Main(), c.Keys, c.Fields, keyOpUpdate)
			return nil
		},
		Open: func(rc *core.RunContext, c *UpdateConfig) (core.Runner, error) {
			return keyStepRunner(rc, c.Connection, c.Schema, c.Table, c.Keys, c.Fields, c.CommitSize, keyOpUpdate)
		},
	}))
	core.Steps.Register(core.Define(core.Definition[DeleteConfig]{
		Meta: core.Meta{
			ID: "delete", Order: 40, Label: "删除", Category: core.CategoryOutput, Icon: "delete",
			Description: "按查询关键字删除目标表中匹配的记录。未命中的行进入错误输出（未连接时跳过）。",
			Inputs:      []core.PortDecl{{Name: core.PortMain, Label: "输入", Required: true}},
			Outputs:     []core.PortDecl{{Name: core.PortMain, Label: "已处理"}},
			ErrorPort:   true,
			Keywords:    []string{"Delete", "删除"},
		},
		Defaults: func() DeleteConfig { return DeleteConfig{CommitSize: 1000} },
		Infer: func(ic *core.InferContext, c *DeleteConfig) error {
			ic.SetOutput(core.PortMain, ic.Main())
			if strings.TrimSpace(c.Table) == "" {
				ic.Errorf("table", "table_required", "请选择目标表")
				return nil
			}
			if len(c.Keys) == 0 {
				ic.Errorf("keys", "keys_required", "至少需要一个查询关键字")
				return nil
			}
			_, h := handleFor(ic, c.Connection, "connection")
			if h == nil {
				return nil
			}
			planKeys(ic, h, tableRef(h, c.Schema, c.Table), ic.Main(), c.Keys, nil, keyOpDelete)
			return nil
		},
		Open: func(rc *core.RunContext, c *DeleteConfig) (core.Runner, error) {
			return keyStepRunner(rc, c.Connection, c.Schema, c.Table, c.Keys, nil, c.CommitSize, keyOpDelete)
		},
	}))
}
