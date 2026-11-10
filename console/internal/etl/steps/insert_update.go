package steps

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/core"
)

// InsertUpdateConfig 是「插入/更新」（Kettle 语义，不依赖唯一约束）：
// 按查询关键字查目标表，不存在则插入，存在则（值有变化时）更新。
type InsertUpdateConfig struct {
	Connection string        `json:"connection"`
	Schema     string        `json:"schema"`
	Table      string        `json:"table"`
	Keys       []FieldMap    `json:"keys"`       // 查询关键字：流字段 = 表字段
	Fields     []UpdateField `json:"fields"`     // 插入的字段；Update=true 的字段在命中时更新
	CommitSize int           `json:"commitSize"` // 每个事务的行数，默认 1000
	// NullKeys：关键字段为 NULL 时的处理。error（默认）= 该行进错误输出；insert = 视为新行直接插入。
	NullKeys string `json:"nullKeys"`
	// SkipUnchanged：命中且更新字段值都没变时不执行 UPDATE（默认 true，大幅减少重复同步的写入）。
	SkipUnchanged bool `json:"skipUnchanged"`
	// NoUpdate：只插入新行，已存在的行一律不更新（Kettle「不执行任何更新」）。
	NoUpdate bool `json:"noUpdate"`
}

// UpdateField 是插入/更新的字段映射。
type UpdateField struct {
	Stream string `json:"stream"`
	Column string `json:"column"`
	Update bool   `json:"update"`
}

// lookupKeysPerStatement 是单条查找 SQL 的键数上限（IN 列表过长会显著拖慢部分数据库的解析）。
const lookupKeysPerStatement = 500

func init() {
	core.Steps.Register(core.Define(core.Definition[InsertUpdateConfig]{
		Meta: core.Meta{
			ID: "insert_update", Order: 20, Label: "插入 / 更新", Category: core.CategoryOutput, Icon: "upsert",
			Description: "按查询关键字比对目标表：不存在则插入，存在且值有变化则更新。不要求唯一约束，但关键字段应有索引。",
			Inputs:      []core.PortDecl{{Name: core.PortMain, Label: "输入", Required: true}},
			Outputs:     []core.PortDecl{{Name: core.PortMain, Label: "已处理"}},
			ErrorPort:   true,
			Keywords:    []string{"Insert / Update", "upsert", "merge", "增量", "同步"},
		},
		Defaults: func() InsertUpdateConfig {
			return InsertUpdateConfig{CommitSize: 1000, NullKeys: "error", SkipUnchanged: true}
		},
		Infer: func(ic *core.InferContext, c *InsertUpdateConfig) error {
			ic.SetOutput(core.PortMain, ic.Main())
			if strings.TrimSpace(c.Table) == "" {
				ic.Errorf("table", "table_required", "请选择目标表")
				return nil
			}
			if len(c.Keys) == 0 {
				ic.Errorf("keys", "keys_required", "至少需要一个查询关键字")
				return nil
			}
			if c.NullKeys != "" && c.NullKeys != "error" && c.NullKeys != "insert" {
				ic.Errorf("nullKeys", "null_keys", "关键字为空的处理方式无效")
			}
			_, h := handleFor(ic, c.Connection, "connection")
			if h == nil {
				return nil
			}
			ref := tableRef(h, c.Schema, c.Table)
			plan, ok := planInsertUpdate(ic, h, ref, c)
			if !ok {
				return nil
			}
			if len(plan.update) == 0 && !c.NoUpdate {
				ic.Warnf("fields", "no_update_fields", "没有勾选任何「更新」字段：已存在的行不会被修改")
			}
			// 关键字段无索引时，每批查找都是全表扫描
			if idx, err := h.Dialect.Indexes(ic.Ctx, h.DB, ref); err == nil && !coveredByIndex(idx, columnNames(plan.keys)) {
				ic.Warnf("keys", "no_index", "目标表在关键字段（%s）上没有可用索引，大表上查找会很慢，建议建立索引", strings.Join(columnNames(plan.keys), ", "))
			}
			return nil
		},
		Open: func(rc *core.RunContext, c *InsertUpdateConfig) (core.Runner, error) {
			return core.RunnerFunc(func(ctx context.Context) error {
				h, err := runHandle(ctx, rc, c.Connection)
				if err != nil {
					return err
				}
				ref := tableRef(h, c.Schema, c.Table)
				ic := core.NewInferContext(ctx, rc.NodeID, rc.Env, nil, map[string]*core.Schema{core.PortMain: rc.In(core.PortMain).Schema})
				plan, ok := planInsertUpdate(ic, h, ref, c)
				if !ok {
					return ic.Problems().First()
				}
				u := &upserter{rc: rc, h: h, table: ref, cfg: c, plan: plan}
				if u.cfg.CommitSize <= 0 {
					u.cfg.CommitSize = 1000
				}
				// 关键字段有唯一索引且方言支持原生 upsert 时走快速路径；
				// 元数据读取失败按无索引处理，语义不受影响。
				if idx, err := h.Dialect.Indexes(ctx, h.DB, ref); err == nil && upsertSafe(h.Dialect, idx, columnNames(plan.keys)) {
					_, u.upsert = h.Dialect.UpsertSQL(ref, columnNames(plan.insert), columnNames(plan.keys), columnNames(plan.update), 1, true)
				}
				out := rc.Out(core.PortMain)
				err = rc.In(core.PortMain).Each(ctx, func(b core.Batch) error {
					for _, r := range b.Rows {
						if err := u.add(ctx, r); err != nil {
							return err
						}
						if len(u.pending) >= u.cfg.CommitSize {
							if err := u.flush(ctx, out); err != nil {
								return err
							}
						}
					}
					return nil
				})
				if err != nil {
					return err
				}
				return u.flush(ctx, out)
			}), nil
		},
	}))
}

type iuPlan struct {
	keys   []boundColumn // 查询关键字
	insert []boundColumn // 插入列（含关键字）
	update []boundColumn // 更新列（不含关键字）
}

func planInsertUpdate(ic *core.InferContext, h *conn.Handle, ref conn.TableRef, c *InsertUpdateConfig) (*iuPlan, bool) {
	in := ic.Main()
	keys, _, ok1 := bindColumns(ic, h, ref, in, c.Keys, "keys")
	var fm []FieldMap
	for _, f := range c.Fields {
		fm = append(fm, FieldMap{Stream: f.Stream, Column: f.Column})
	}
	var fields []boundColumn
	ok2 := true
	if len(fm) > 0 {
		fields, _, ok2 = bindColumns(ic, h, ref, in, fm, "fields")
	}
	if !ok1 || !ok2 {
		return nil, false
	}
	p := &iuPlan{keys: keys}
	isKey := map[string]bool{}
	for _, k := range keys {
		isKey[k.column.Name] = true
		p.insert = append(p.insert, k)
	}
	for i, f := range fields {
		if isKey[f.column.Name] {
			continue
		}
		p.insert = append(p.insert, f)
		if c.Fields[i].Update {
			p.update = append(p.update, f)
		}
	}
	return p, true
}

func coveredByIndex(idx []conn.IndexMeta, keys []string) bool {
	need := map[string]bool{}
	for _, k := range keys {
		need[strings.ToLower(k)] = true
	}
	for _, ix := range idx {
		// 索引前缀覆盖全部关键字段即可走索引
		n := 0
		for _, c := range ix.Columns {
			if !need[strings.ToLower(c)] {
				break
			}
			n++
		}
		if n == len(need) {
			return true
		}
	}
	return false
}

// iuRow 是已转换为目标类型的待处理行。
type iuRow struct {
	src     core.Row
	key     string
	nullKey bool
	keyVals []any // 目标类型的逻辑值
	insVals []any
	updVals []any
}

type upserter struct {
	rc      *core.RunContext
	h       *conn.Handle
	table   conn.TableRef
	cfg     *InsertUpdateConfig
	plan    *iuPlan
	upsert  bool // 目标有唯一约束且方言支持原生 upsert
	pending []iuRow
}

func convertCols(cols []boundColumn, r core.Row) ([]any, error) {
	out := make([]any, len(cols))
	for i, c := range cols {
		v, err := core.Convert(r[c.streamIdx], c.target.Kind, core.ConvertOptions{})
		if err != nil {
			return nil, core.NewRowError(c.stream.Name, err)
		}
		out[i] = v
	}
	return out, nil
}

func (u *upserter) add(ctx context.Context, r core.Row) error {
	row := iuRow{src: r}
	var err error
	if row.keyVals, err = convertCols(u.plan.keys, r); err == nil {
		if row.insVals, err = convertCols(u.plan.insert, r); err == nil {
			row.updVals, err = convertCols(u.plan.update, r)
		}
	}
	if err == nil {
		for i, v := range row.keyVals {
			if v == nil {
				if u.cfg.NullKeys == "insert" {
					row.nullKey = true
					break
				}
				err = core.NewRowError(u.plan.keys[i].stream.Name, errors.New("查询关键字为 NULL"))
				break
			}
		}
	}
	if err != nil {
		var re *core.RowError
		if errors.As(err, &re) {
			return u.rc.Reject(ctx, r, re.Err, re.Field)
		}
		return err
	}
	if !row.nullKey {
		row.key = string(rowKey(nil, row.keyVals, seq(len(row.keyVals))))
	}
	u.pending = append(u.pending, row)
	return nil
}

func seq(n int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = i
	}
	return s
}

// flush 在一个事务内：批量查找 → 拆分插入/更新 → 写入 → 提交。
func (u *upserter) flush(ctx context.Context, out *core.OutPort) error {
	if len(u.pending) == 0 {
		return nil
	}
	rows := u.pending
	u.pending = nil

	// 批内去重：同键保留最后一次出现（最终状态与逐行处理一致）
	last := map[string]int{}
	for i, r := range rows {
		if !r.nullKey {
			last[r.key] = i
		}
	}
	var unique []iuRow
	for i, r := range rows {
		if r.nullKey || last[r.key] == i {
			unique = append(unique, r)
		}
	}
	if dup := len(rows) - len(unique); dup > 0 {
		u.rc.Log.Detail("本批有 %d 行与后续行关键字重复，按最后一次出现处理", dup)
	}

	if u.rc.Preview {
		return out.Send(ctx, core.Batch{Schema: out.Schema, Rows: srcRows(rows)})
	}

	tx, err := u.h.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()

	doUpd := !u.cfg.NoUpdate && len(u.plan.update) > 0
	if u.upsert && !doUpd {
		// 有唯一约束且只插不更：insert-or-ignore，无需查找，受影响行即实际插入数。
		affected, err := u.upsertAll(ctx, tx, unique, false)
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("提交失败：%w", err)
		}
		committed = true
		u.rc.Metrics.Output.Add(affected)
		u.rc.Log.Debug("提交：upsert 忽略冲突 %d 行，实际插入 %d", len(unique), affected)
		return out.Send(ctx, core.Batch{Schema: out.Schema, Rows: srcRows(rows)})
	}

	existing, err := u.lookup(ctx, tx, unique)
	if err != nil {
		return err
	}

	var inserts, updates []iuRow
	for _, r := range unique {
		cur, found := existing[r.key]
		switch {
		case r.nullKey || !found:
			inserts = append(inserts, r)
		case u.cfg.NoUpdate || len(u.plan.update) == 0:
		case u.cfg.SkipUnchanged && sameValues(cur, r.updVals):
		default:
			updates = append(updates, r)
		}
	}

	if u.upsert {
		// 原生 upsert：插入与更新合并为一条/数条语句（含 nullKey 直接插入的行）。
		if _, err := u.upsertAll(ctx, tx, append(inserts, updates...), true); err != nil {
			return err
		}
	} else {
		if err := u.insert(ctx, tx, inserts); err != nil {
			return err
		}
		if err := u.updateBatch(ctx, tx, updates); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交失败：%w", err)
	}
	committed = true
	u.rc.Metrics.Output.Add(int64(len(inserts)))
	u.rc.Metrics.Updated.Add(int64(len(updates)))
	u.rc.Log.Debug("提交：插入 %d、更新 %d、未变化 %d", len(inserts), len(updates), len(unique)-len(inserts)-len(updates))
	return out.Send(ctx, core.Batch{Schema: out.Schema, Rows: srcRows(rows)})
}

func srcRows(rows []iuRow) []core.Row {
	out := make([]core.Row, len(rows))
	for i, r := range rows {
		out[i] = r.src
	}
	return out
}

// lookup 分块查询已存在的键，返回 键 → 目标表当前的更新列值。
func (u *upserter) lookup(ctx context.Context, tx conn.Querier, rows []iuRow) (map[string][]any, error) {
	d := u.h.Dialect
	nk := len(u.plan.keys)
	per := min(lookupKeysPerStatement, max(1, d.MaxParams()/nk))
	keyNames := columnNames(u.plan.keys)
	updNames := columnNames(u.plan.update)
	found := map[string][]any{}
	var batch []iuRow
	run := func() error {
		if len(batch) == 0 {
			return nil
		}
		q := conn.LookupSQL(d, u.table, keyNames, updNames, len(batch))
		args := make([]any, 0, len(batch)*nk)
		for _, r := range batch {
			for i, v := range r.keyVals {
				b, err := d.Bind(v, u.plan.keys[i].target)
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
		width := nk + len(u.plan.update)
		vals := make([]any, width)
		ptrs := make([]any, width)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		loc := u.h.Location()
		for res.Next() {
			if err := res.Scan(ptrs...); err != nil {
				return err
			}
			keyVals := make([]any, nk)
			for i := 0; i < nk; i++ {
				v, err := d.Normalize(vals[i], u.plan.keys[i].target, loc)
				if err != nil {
					return err
				}
				keyVals[i] = v
			}
			upd := make([]any, len(u.plan.update))
			for i := range upd {
				v, err := d.Normalize(vals[nk+i], u.plan.update[i].target, loc)
				if err != nil {
					return err
				}
				upd[i] = v
			}
			k := string(rowKey(nil, keyVals, seq(nk)))
			if _, dup := found[k]; !dup {
				found[k] = upd
			}
		}
		batch = batch[:0]
		return res.Err()
	}
	for _, r := range rows {
		if r.nullKey {
			continue
		}
		batch = append(batch, r)
		if len(batch) >= per {
			if err := run(); err != nil {
				return nil, err
			}
		}
	}
	return found, run()
}

func (u *upserter) insert(ctx context.Context, tx conn.Querier, rows []iuRow) error {
	if len(rows) == 0 {
		return nil
	}
	d := u.h.Dialect
	names := columnNames(u.plan.insert)
	per := conn.RowsPerStatement(d, len(names), u.cfg.CommitSize)
	for start := 0; start < len(rows); start += per {
		end := min(start+per, len(rows))
		args := make([]any, 0, (end-start)*len(names))
		for _, r := range rows[start:end] {
			for i, v := range r.insVals {
				b, err := d.Bind(v, u.plan.insert[i].target)
				if err != nil {
					return err
				}
				args = append(args, b)
			}
		}
		if _, err := tx.ExecContext(ctx, conn.InsertSQL(d, u.table, names, end-start), args...); err != nil {
			return fmt.Errorf("插入 %s 失败：%w", u.table, err)
		}
	}
	return nil
}

// upsertAll 用原生 upsert 处理全部行：返回受影响行数（doUpdate=false 时即实际插入数）。
func (u *upserter) upsertAll(ctx context.Context, tx conn.Querier, rows []iuRow, doUpdate bool) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	d := u.h.Dialect
	names := columnNames(u.plan.insert)
	per := conn.RowsPerStatement(d, len(names), u.cfg.CommitSize)
	var affected int64
	for start := 0; start < len(rows); start += per {
		end := min(start+per, len(rows))
		q, ok := d.UpsertSQL(u.table, names, columnNames(u.plan.keys), columnNames(u.plan.update), end-start, doUpdate)
		if !ok {
			return 0, fmt.Errorf("方言 %s 不支持 upsert", d.ID())
		}
		args := make([]any, 0, (end-start)*len(names))
		for _, r := range rows[start:end] {
			for i, v := range r.insVals {
				b, err := d.Bind(v, u.plan.insert[i].target)
				if err != nil {
					return 0, err
				}
				args = append(args, b)
			}
		}
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return 0, fmt.Errorf("upsert %s 失败：%w", u.table, err)
		}
		n, _ := res.RowsAffected()
		affected += n
	}
	return affected, nil
}

// updateBatch 用一条 CASE UPDATE 更新多行（通用路径，不要求唯一约束）。
// 每行参数 = set 列数×(键数+1) + 键数，按 MaxParams 分块。
func (u *upserter) updateBatch(ctx context.Context, tx conn.Querier, rows []iuRow) error {
	if len(rows) == 0 {
		return nil
	}
	d := u.h.Dialect
	set, keys := columnNames(u.plan.update), columnNames(u.plan.keys)
	casts := make([]string, len(u.plan.update))
	for i := range casts {
		casts[i] = d.CaseCast(u.plan.update[i].target)
	}
	perRow := len(set)*(len(keys)+1) + len(keys)
	per := min(u.cfg.CommitSize, max(1, d.MaxParams()/perRow))
	for start := 0; start < len(rows); start += per {
		end := min(start+per, len(rows))
		args := make([]any, 0, (end-start)*perRow)
		for i := range set {
			for _, r := range rows[start:end] {
				for j, v := range r.keyVals {
					b, err := d.Bind(v, u.plan.keys[j].target)
					if err != nil {
						return err
					}
					args = append(args, b)
				}
				b, err := d.Bind(r.updVals[i], u.plan.update[i].target)
				if err != nil {
					return err
				}
				args = append(args, b)
			}
		}
		for _, r := range rows[start:end] {
			for i, v := range r.keyVals {
				b, err := d.Bind(v, u.plan.keys[i].target)
				if err != nil {
					return err
				}
				args = append(args, b)
			}
		}
		if _, err := tx.ExecContext(ctx, conn.UpdateCaseSQL(d, u.table, set, keys, casts, end-start), args...); err != nil {
			return fmt.Errorf("更新 %s 失败：%w", u.table, err)
		}
	}
	return nil
}

// exactUniqueIndex 报告索引是否为「唯一/主键且列集合恰为 keys」。
func exactUniqueIndex(ix conn.IndexMeta, keys []string) bool {
	if !ix.Unique && !ix.Primary || len(ix.Columns) != len(keys) {
		return false
	}
	seen := map[string]bool{}
	for _, c := range ix.Columns {
		seen[strings.ToLower(c)] = true
	}
	for _, k := range keys {
		if !seen[strings.ToLower(k)] {
			return false
		}
	}
	return true
}

// uniqueIndexOn 报告关键字段是否被唯一索引/主键精确覆盖（upsert 冲突目标的必要条件）。
func uniqueIndexOn(idx []conn.IndexMeta, keys []string) bool {
	for _, ix := range idx {
		if exactUniqueIndex(ix, keys) {
			return true
		}
	}
	return false
}

// upsertSafe 在 uniqueIndexOn 之上按方言语义收紧：
// MySQL ON DUPLICATE KEY UPDATE 对任意唯一索引冲突都会更新命中行，
// 表上存在键以外的唯一索引时可能更新到与查询关键字不匹配的行（旧路径会插入报错），
// 此时保守回退通用 CASE 批量更新；PG 系冲突目标显式指定，无此问题。
func upsertSafe(d conn.Dialect, idx []conn.IndexMeta, keys []string) bool {
	if !uniqueIndexOn(idx, keys) {
		return false
	}
	if d.ID() != "mysql" {
		return true
	}
	for _, ix := range idx {
		if (ix.Unique || ix.Primary) && !exactUniqueIndex(ix, keys) {
			return false
		}
	}
	return true
}

// sameValues 比较目标表现值与新值（精确小数按数值、时间按时刻比较）。
func sameValues(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameValue(a[i], b[i]) {
			return false
		}
	}
	return true
}

func sameValue(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	switch x := a.(type) {
	case decimal.Decimal:
		y, ok := b.(decimal.Decimal)
		return ok && x.Equal(y)
	case time.Time:
		y, ok := b.(time.Time)
		return ok && x.Equal(y)
	case []byte:
		y, ok := b.([]byte)
		return ok && bytes.Equal(x, y)
	}
	return a == b
}
