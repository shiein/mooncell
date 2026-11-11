package steps

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/core"
)

// TableOutputConfig 是「表输出」：多值 INSERT 批量写入，按提交记录数分事务提交。
type TableOutputConfig struct {
	Connection string     `json:"connection"`
	Schema     string     `json:"schema"`
	Table      string     `json:"table"`
	Truncate   bool       `json:"truncate"`   // 开始前清空目标表（与插入不在同一事务，同 Kettle）
	CommitSize int        `json:"commitSize"` // 每个事务的行数，默认 1000
	Fields     []FieldMap `json:"fields"`     // 空 = 按同名映射全部字段
}

func init() {
	core.Steps.Register(core.Define(core.Definition[TableOutputConfig]{
		Meta: core.Meta{
			ID: "table_output", Order: 10, Label: "表输出", Category: core.CategoryOutput, Icon: "table",
			Description: "把记录批量插入目标表，按「提交记录数」分事务提交。可选开始前清空表（全量重灌）。写入成功的记录继续向下游输出。",
			Inputs:      []core.PortDecl{{Name: core.PortMain, Label: "输入", Required: true}},
			Outputs:     []core.PortDecl{{Name: core.PortMain, Label: "已写入"}},
			ErrorPort:   true,
			Keywords:    []string{"Table output", "insert", "写入", "裁剪表", "truncate", "全量"},
		},
		Defaults: func() TableOutputConfig { return TableOutputConfig{CommitSize: 1000} },
		Infer: func(ic *core.InferContext, c *TableOutputConfig) error {
			ic.SetOutput(core.PortMain, ic.Main())
			if strings.TrimSpace(c.Table) == "" {
				ic.Errorf("table", "table_required", "请选择目标表")
				return nil
			}
			if c.CommitSize < 0 {
				ic.Errorf("commitSize", "commit_size", "提交记录数不能为负")
			}
			_, h := handleFor(ic, c.Connection, "connection")
			if h == nil {
				return nil
			}
			bindColumns(ic, h, tableRef(h, c.Schema, c.Table), ic.Main(), c.Fields, "fields")
			return nil
		},
		Open: func(rc *core.RunContext, c *TableOutputConfig) (core.Runner, error) {
			return core.RunnerFunc(func(ctx context.Context) error {
				w, err := newTableWriter(ctx, rc, c.Connection, c.Schema, c.Table, c.Fields, c.CommitSize)
				if err != nil {
					return err
				}
				if c.Truncate && !rc.Preview {
					rc.Log.Info("清空目标表 %s", w.table)
					if _, err := w.h.DB.ExecContext(ctx, w.h.Dialect.TruncateSQL(w.table)); err != nil {
						return fmt.Errorf("清空表失败：%w", err)
					}
				}
				out := rc.Out(core.PortMain)
				err = rc.In(core.PortMain).Each(ctx, func(b core.Batch) error {
					written, err := w.add(ctx, b.Rows)
					if err != nil {
						return err
					}
					return out.Send(ctx, core.Batch{Schema: out.Schema, Rows: written})
				})
				if err != nil {
					w.abort()
					return err
				}
				written, err := w.flush(ctx)
				if err != nil {
					return err
				}
				return out.Send(ctx, core.Batch{Schema: out.Schema, Rows: written})
			}), nil
		},
	}))
}

// tableWriter 攒满 commitSize 行后在一个事务内用多值 INSERT 写入。
type tableWriter struct {
	rc         *core.RunContext
	h          *conn.Handle
	table      conn.TableRef
	cols       []boundColumn
	names      []string
	commitSize int
	perStmt    int

	pending []core.Row
	args    []any
}

func newTableWriter(ctx context.Context, rc *core.RunContext, connID, schema, table string, mapping []FieldMap, commitSize int) (*tableWriter, error) {
	h, err := runHandle(ctx, rc, connID)
	if err != nil {
		return nil, err
	}
	ref := tableRef(h, schema, table)
	ic := core.NewInferContext(ctx, rc.NodeID, rc.Env, nil, map[string]*core.Schema{core.PortMain: rc.In(core.PortMain).Schema})
	cols, _, ok := bindColumns(ic, h, ref, rc.In(core.PortMain).Schema, mapping, "fields")
	if !ok {
		return nil, ic.Problems().First()
	}
	if commitSize <= 0 {
		commitSize = 1000
	}
	return &tableWriter{rc: rc, h: h, table: ref, cols: cols, names: columnNames(cols), commitSize: commitSize,
		perStmt: conn.RowsPerStatement(h.Dialect, len(cols), commitSize)}, nil
}

// add 接收一批行；转换失败的行 Reject；满批则提交。返回本次已提交的行。
func (w *tableWriter) add(ctx context.Context, rows []core.Row) ([]core.Row, error) {
	var committed []core.Row
	for _, r := range rows {
		before := len(w.args)
		var err error
		w.args, err = bindRow(w.h.Dialect, w.cols, r, w.args)
		if err != nil {
			w.args = w.args[:before]
			var re *core.RowError
			if errors.As(err, &re) {
				if rerr := w.rc.Reject(ctx, r, re.Err, re.Field); rerr != nil {
					return committed, rerr
				}
				continue
			}
			return committed, err
		}
		w.pending = append(w.pending, r)
		if len(w.pending) >= w.commitSize {
			c, err := w.flush(ctx)
			committed = append(committed, c...)
			if err != nil {
				return committed, err
			}
		}
	}
	return committed, nil
}

// flush 在一个事务内写入全部待提交行。预览模式只计数不写库。
// 连接 error 端口时批次失败退化为逐行写入：坏行 Reject，其余行照常提交；
// 未连接时维持整步失败的原语义。
func (w *tableWriter) flush(ctx context.Context) ([]core.Row, error) {
	if len(w.pending) == 0 {
		return nil, nil
	}
	rows, args := w.pending, w.args
	w.pending, w.args = nil, nil
	if w.rc.Preview {
		return rows, nil
	}
	if err := w.flushBatch(ctx, rows, args); err != nil {
		if !w.rc.HasOutput(core.PortError) {
			return nil, err
		}
		if !conn.IsDataError(w.h.Dialect, err) {
			// 基础设施错误（断连/超时/取消/语法/权限）：坏行定位没有意义，整步失败
			return nil, err
		}
		w.rc.Log.Warn("%s：批量写入失败（%v），退化逐行写入定位坏行", w.table, err)
		return w.flushRows(ctx, rows, args)
	}
	w.rc.Metrics.Output.Add(int64(len(rows)))
	w.rc.Log.Debug("已提交 %d 行到 %s", len(rows), w.table)
	return rows, nil
}

// flushBatch 单事务多值 INSERT。
func (w *tableWriter) flushBatch(ctx context.Context, rows []core.Row, args []any) error {
	tx, err := w.h.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	ncol := len(w.cols)
	for start := 0; start < len(rows); start += w.perStmt {
		end := min(start+w.perStmt, len(rows))
		q := conn.InsertSQL(w.h.Dialect, w.table, w.names, end-start)
		if _, err := tx.ExecContext(ctx, q, args[start*ncol:end*ncol]...); err != nil {
			tx.Rollback()
			return fmt.Errorf("写入 %s 失败（第 %d–%d 行所在批次已回滚）：%w", w.table, start+1, end, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交失败：%w", err)
	}
	return nil
}

// maxNonDataStreak 逐行阶段允许的连续非数据类错误上限；
// 达到即视为系统性故障（断连/权限/资源），整步失败而非继续吞行。
const maxNonDataStreak = 100

// flushRows 每行单独事务写入（PG 中失败语句会使整个事务失效，不能复用同一事务）。
// 数据类错误的行 Reject 到 error 端口；非数据类错误同样拒行但计入连续计数，
// 连续达到 maxNonDataStreak 即整步失败。返回成功写入的行。
func (w *tableWriter) flushRows(ctx context.Context, rows []core.Row, args []any) ([]core.Row, error) {
	ncol := len(w.cols)
	q := conn.InsertSQL(w.h.Dialect, w.table, w.names, 1)
	written := make([]core.Row, 0, len(rows))
	nonDataStreak := 0
	for i, r := range rows {
		tx, err := w.h.DB.BeginTx(ctx, nil)
		if err != nil {
			return written, err
		}
		_, err = tx.ExecContext(ctx, q, args[i*ncol:(i+1)*ncol]...)
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
		if err != nil {
			if !conn.IsDataError(w.h.Dialect, err) {
				nonDataStreak++
				if nonDataStreak >= maxNonDataStreak {
					return written, fmt.Errorf("连续 %d 行非数据类错误（最近：%w），疑似系统性故障，整步失败", nonDataStreak, err)
				}
			} else {
				nonDataStreak = 0
			}
			if rerr := w.rc.Reject(ctx, r, fmt.Errorf("写入 %s 失败：%w", w.table, err), ""); rerr != nil {
				return written, rerr
			}
			continue
		}
		nonDataStreak = 0
		written = append(written, r)
	}
	w.rc.Metrics.Output.Add(int64(len(written)))
	w.rc.Log.Info("已提交 %d 行到 %s（%d 行被拒转错误输出）", len(written), w.table, len(rows)-len(written))
	return written, nil
}

func (w *tableWriter) abort() { w.pending, w.args = nil, nil }
