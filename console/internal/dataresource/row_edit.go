// 结果集就地编辑：按主键参数化 UPDATE/DELETE，仅单表且必须有主键。
package dataresource

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// RowEditRequest 是批量行编辑请求。
type RowEditRequest struct {
	Schema string          `json:"schema"`
	Table  string          `json:"table"`
	Updates []RowUpdateOp  `json:"updates"`
	Deletes []RowDeleteOp  `json:"deletes"`
}

// RowUpdateOp 以主键定位一行并更新非主键列。
type RowUpdateOp struct {
	Keys map[string]any `json:"keys"` // 主键列 → 原值
	Set  map[string]any `json:"set"`  // 要更新的列 → 新值
	// Old 为变更列的查询时原值，用于乐观条件（主键 + 原值），避免静默覆盖他人修改。
	Old map[string]any `json:"old,omitempty"`
}

// RowDeleteOp 以主键删除一行。
type RowDeleteOp struct {
	Keys map[string]any `json:"keys"`
}

// RowEditResult 批量编辑结果。
type RowEditResult struct {
	Updated    int   `json:"updated"`
	Deleted    int   `json:"deleted"`
	DurationMs int64 `json:"durationMs"`
}

// ApplyRowEdits 在适配器上执行批量 UPDATE/DELETE（独立事务，自动提交）。
// primaryKeys 必须由服务端从 Describe 重新读取，不得信任客户端。
func ApplyRowEdits(ctx context.Context, adapter DataSourceAdapter, schema, table string, primaryKeys []string, req RowEditRequest) (*RowEditResult, error) {
	if table == "" {
		return nil, fmt.Errorf("表名不能为空")
	}
	if len(primaryKeys) == 0 {
		return nil, fmt.Errorf("表无主键，无法安全执行就地编辑或删除")
	}
	if len(req.Updates) == 0 && len(req.Deletes) == 0 {
		return nil, fmt.Errorf("没有需要保存的变更")
	}

	// 校验表存在并取得列白名单
	obj := MetadataNode{Kind: NodeTable, Schema: schema, Name: table}
	structure, err := adapter.Describe(ctx, obj)
	if err != nil {
		return nil, fmt.Errorf("读取表结构失败: %w", err)
	}
	serverPKs := primaryKeyColumns(structure)
	if len(serverPKs) == 0 {
		return nil, fmt.Errorf("表无主键，无法安全执行就地编辑或删除")
	}
	if !sameStringSet(serverPKs, primaryKeys) {
		// 以服务端为准，但要求客户端声明的 PK 集合一致，避免指错行
		return nil, fmt.Errorf("主键信息不一致，请重新查询后再编辑")
	}
	colSet := map[string]bool{}
	for _, c := range structure.Columns {
		colSet[c.Name] = true
		colSet[strings.ToLower(c.Name)] = true
	}
	pkSet := map[string]bool{}
	for _, pk := range serverPKs {
		pkSet[pk] = true
		pkSet[strings.ToLower(pk)] = true
	}

	qual := qualifyTable(adapter, schema, table)
	start := time.Now()
	tx, err := adapter.Begin(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	updated, deleted := 0, 0
	ph := 0
	nextPH := func() string {
		ph++
		return adapter.Placeholder(ph)
	}

	for i, up := range req.Updates {
		if len(up.Keys) == 0 || len(up.Set) == 0 {
			return nil, fmt.Errorf("第 %d 条更新缺少主键或变更列", i+1)
		}
		setArgs := make([]any, 0, len(up.Set)+len(serverPKs)*2)
		ph = 0
		var setParts []string
		type oldCond struct {
			col string
			val any
		}
		var oldConds []oldCond
		for col, val := range up.Set {
			col = strings.TrimSpace(col)
			if col == "" || pkSet[col] || pkSet[strings.ToLower(col)] {
				return nil, fmt.Errorf("不允许通过就地编辑修改主键列: %s", col)
			}
			if !colSet[col] && !colSet[strings.ToLower(col)] {
				return nil, fmt.Errorf("未知列: %s", col)
			}
			nv, err := normalizeEditValue(val)
			if err != nil {
				return nil, fmt.Errorf("第 %d 条更新列 %s: %w", i+1, col, err)
			}
			real := resolveColumnName(structure, col)
			setParts = append(setParts, adapter.QuoteIdentifier(real)+" = "+nextPH())
			setArgs = append(setArgs, nv)
			if up.Old != nil {
				if ov, ok := lookupKey(up.Old, col); ok {
					onv, err := normalizeEditValue(ov)
					if err != nil {
						return nil, fmt.Errorf("第 %d 条更新列 %s 原值: %w", i+1, col, err)
					}
					oldConds = append(oldConds, oldCond{col: real, val: onv})
				}
			}
		}
		whereSQL, whereArgs, err := buildPKWhere(adapter, serverPKs, up.Keys, &ph)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条更新: %w", i+1, err)
		}
		// 乐观条件接在主键 WHERE 之后，占位符序号连续
		for _, oc := range oldConds {
			if oc.val == nil {
				whereSQL += " AND " + adapter.QuoteIdentifier(oc.col) + " IS NULL"
			} else {
				ph++
				whereSQL += " AND " + adapter.QuoteIdentifier(oc.col) + " = " + adapter.Placeholder(ph)
				whereArgs = append(whereArgs, oc.val)
			}
		}
		sqlText := fmt.Sprintf("UPDATE %s SET %s WHERE %s", qual, strings.Join(setParts, ", "), whereSQL)
		args := append(setArgs, whereArgs...)
		res, err := tx.Exec(ctx, sqlText, args...)
		if err != nil {
			return nil, adapter.NormalizeError(err).toError()
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return nil, fmt.Errorf("第 %d 条更新未命中行（数据可能已被他人修改，请重新查询）", i+1)
		}
		updated += int(n)
	}

	for i, del := range req.Deletes {
		if len(del.Keys) == 0 {
			return nil, fmt.Errorf("第 %d 条删除缺少主键", i+1)
		}
		ph = 0
		// 校验主键值类型
		for _, pk := range serverPKs {
			if v, ok := lookupKey(del.Keys, pk); ok {
				if _, err := normalizeEditValue(v); err != nil {
					return nil, fmt.Errorf("第 %d 条删除: %w", i+1, err)
				}
			}
		}
		whereSQL, whereArgs, err := buildPKWhere(adapter, serverPKs, del.Keys, &ph)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条删除: %w", i+1, err)
		}
		sqlText := fmt.Sprintf("DELETE FROM %s WHERE %s", qual, whereSQL)
		res, err := tx.Exec(ctx, sqlText, whereArgs...)
		if err != nil {
			return nil, adapter.NormalizeError(err).toError()
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return nil, fmt.Errorf("第 %d 条删除未命中行（数据可能已被他人修改，请重新查询）", i+1)
		}
		deleted += int(n)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交失败: %w", err)
	}
	committed = true
	return &RowEditResult{
		Updated:    updated,
		Deleted:    deleted,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

func qualifyTable(adapter DataSourceAdapter, schema, table string) string {
	qt := adapter.QuoteIdentifier(table)
	if schema == "" {
		return qt
	}
	return adapter.QuoteIdentifier(schema) + "." + qt
}

func buildPKWhere(adapter DataSourceAdapter, primaryKeys []string, keys map[string]any, ph *int) (string, []any, error) {
	parts := make([]string, 0, len(primaryKeys))
	args := make([]any, 0, len(primaryKeys))
	for _, pk := range primaryKeys {
		val, ok := lookupKey(keys, pk)
		if !ok {
			return "", nil, fmt.Errorf("缺少主键值: %s", pk)
		}
		nv, err := normalizeEditValue(val)
		if err != nil {
			return "", nil, err
		}
		if nv == nil {
			parts = append(parts, adapter.QuoteIdentifier(pk)+" IS NULL")
			continue
		}
		*ph++
		parts = append(parts, adapter.QuoteIdentifier(pk)+" = "+adapter.Placeholder(*ph))
		args = append(args, nv)
	}
	return strings.Join(parts, " AND "), args, nil
}

func lookupKey(keys map[string]any, name string) (any, bool) {
	if v, ok := keys[name]; ok {
		return v, true
	}
	for k, v := range keys {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return nil, false
}

func resolveColumnName(structure ObjectStructure, name string) string {
	for _, c := range structure.Columns {
		if c.Name == name || strings.EqualFold(c.Name, name) {
			return c.Name
		}
	}
	return name
}

func normalizeEditValue(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	// 拒绝对象/数组（含 binary 占位），避免被静默写成 NULL
	switch v.(type) {
	case map[string]any, []any:
		return nil, fmt.Errorf("不支持的单元格值类型（对象/数组/二进制不可写回）")
	}
	return v, nil
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	mb := map[string]bool{}
	for _, x := range b {
		mb[strings.ToLower(x)] = true
	}
	for _, x := range a {
		if !mb[strings.ToLower(x)] {
			return false
		}
	}
	return true
}
