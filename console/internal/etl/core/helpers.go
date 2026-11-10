package core

import (
	"context"
	"errors"
)

// RowError 表示「这一行有问题」——可被 Reject 到错误输出；其它 error 视为步骤级致命错误。
type RowError struct {
	Field string
	Err   error
}

func (e *RowError) Error() string { return e.Err.Error() }
func (e *RowError) Unwrap() error { return e.Err }

// RowErrorf 便捷构造行级错误。
func NewRowError(field string, err error) *RowError { return &RowError{Field: field, Err: err} }

// MapRows 构造逐行映射步骤：读 main 输入，fn 返回新行写 main 输出。
//   - 返回 (nil, nil)：丢弃该行；
//   - 返回 *RowError：该行 Reject 到 error 端口（未连接则步骤失败）；
//   - 其它 error：步骤失败。
//
// fn 不得修改入参行（不可变批次规则）；需要改值请 Clone/With。
func MapRows(rc *RunContext, fn func(Row) (Row, error)) Runner {
	return RunnerFunc(func(ctx context.Context) error {
		out := rc.Out(PortMain)
		return rc.In(PortMain).Each(ctx, func(b Batch) error {
			rows := make([]Row, 0, len(b.Rows))
			for _, r := range b.Rows {
				nr, err := fn(r)
				if err != nil {
					var re *RowError
					if errors.As(err, &re) {
						if rerr := rc.Reject(ctx, r, re.Err, re.Field); rerr != nil {
							return rerr
						}
						continue
					}
					return err
				}
				if nr != nil {
					rows = append(rows, nr)
				}
			}
			return out.Send(ctx, Batch{Schema: out.Schema, Rows: rows})
		})
	})
}

// RoutePort 是 Route 的单行判定结果：发往哪个端口（空字符串 = 丢弃）。
type RoutePort = string

// RouteRows 构造逐行分流步骤（如过滤记录）：fn 返回目标端口名。
func RouteRows(rc *RunContext, fn func(Row) (RoutePort, error)) Runner {
	return RunnerFunc(func(ctx context.Context) error {
		return rc.In(PortMain).Each(ctx, func(b Batch) error {
			groups := map[string][]Row{}
			for _, r := range b.Rows {
				port, err := fn(r)
				if err != nil {
					var re *RowError
					if errors.As(err, &re) {
						if rerr := rc.Reject(ctx, r, re.Err, re.Field); rerr != nil {
							return rerr
						}
						continue
					}
					return err
				}
				if port != "" {
					groups[port] = append(groups[port], r)
				}
			}
			for port, rows := range groups {
				out := rc.Out(port)
				if err := out.Send(ctx, Batch{Schema: out.Schema, Rows: rows}); err != nil {
					return err
				}
			}
			return nil
		})
	})
}
