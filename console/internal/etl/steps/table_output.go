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
//
// TODO(P1)：连接 error 端口时，批次失败后退化为逐行写入以定位坏行并 Reject，而非整步失败。
func (w *tableWriter) flush(ctx context.Context) ([]core.Row, error) {
	if len(w.pending) == 0 {
		return nil, nil
	}
	rows, args := w.pending, w.args
	w.pending, w.args = nil, nil
	if w.rc.Preview {
		return rows, nil
	}
	tx, err := w.h.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	ncol := len(w.cols)
	for start := 0; start < len(rows); start += w.perStmt {
		end := min(start+w.perStmt, len(rows))
		q := conn.InsertSQL(w.h.Dialect, w.table, w.names, end-start)
		if _, err := tx.ExecContext(ctx, q, args[start*ncol:end*ncol]...); err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("写入 %s 失败（第 %d–%d 行所在批次已回滚）：%w", w.table, start+1, end, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交失败：%w", err)
	}
	w.rc.Metrics.Output.Add(int64(len(rows)))
	w.rc.Log.Debug("已提交 %d 行到 %s", len(rows), w.table)
	return rows, nil
}

func (w *tableWriter) abort() { w.pending, w.args = nil, nil }
