//go:build etl_live

// 真机方言认证：对每个数据库验证「连接 → DDL/元数据 → 类型读出 → 绑定写回 → 时间语义」全链路。
// 认证结论需要人工确认的点用 t.Logf 输出（go test -v 可见），写回方言文件顶部注释。
package conn_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/livetest"
)

// colCase 描述一个类型覆盖列：DDL 片段、插入值（逻辑值）、期望读回值（nil = 同插入值）。
type colCase struct {
	ddl  string
	vals []any
	want []any
	// check 在断言逻辑值之外追加语义检查（如 MySQL tinyint(1) 应为 boolean）。
	kind *core.Kind
}

type dialectCase struct {
	ddl     string // CREATE TABLE etl_src (...)
	pk      string // 主键列名（大小写与读出列名一致）
	cols    []colCase
	extra   func(t *testing.T, h *conn.Handle)
	ordered string // ORDER BY 表达式（空 = 主键）
}

var shLoc, _ = time.LoadLocation("Asia/Shanghai") // 测试连接时区（与 cfg.Options.TimeZone 一致）

// 逻辑时间值按「无时区时间 = 连接时区墙上时钟」语义构造——normalizeCommon 产出的就是这种形态。
var wall = time.Date(2026, 11, 5, 13, 14, 15, 123000000, shLoc)
var today = time.Date(2026, 11, 5, 0, 0, 0, 0, shLoc)
var far = time.Date(2030, 1, 2, 3, 4, 5, 0, shLoc)

func k(k core.Kind) *core.Kind { return &k }

var cases = map[string]dialectCase{
	"mysql": {
		pk: "id",
		ddl: `CREATE TABLE etl_src (
			id INT PRIMARY KEY,
			i64 BIGINT, u64 BIGINT UNSIGNED, name VARCHAR(100),
			flag TINYINT(1), bitflag BIT(1),
			amount DECIMAL(18,4), price DOUBLE,
			dt DATETIME(3), ts TIMESTAMP NULL, d DATE,
			b BLOB
		)`,
		cols: []colCase{
			{ddl: "i64", vals: []any{int64(-42), nil}},
			{ddl: "u64", vals: []any{dec("7"), dec("18446744073709551615")}}, // unsigned 超 int64 → 精确小数
			{ddl: "name", vals: []any{"赵 一", nil}},
			// tinyint(1) 经 SELECT 探测无法与 tinyint(4) 区分（驱动不暴露声明宽度）→ 读为整数；
			// 经 information_schema（Columns）则识别为布尔。探测路径值断言为 1/0。
			{ddl: "flag", vals: []any{true, false}, want: []any{int64(1), int64(0)}},
			{ddl: "bitflag", vals: []any{true, false}, kind: k(core.KindBoolean)},
			{ddl: "amount", vals: []any{dec("3441.9000"), dec("-0.01")}},
			{ddl: "price", vals: []any{3.25, nil}},
			{ddl: "dt", vals: []any{wall}},
			{ddl: "ts", vals: []any{far}},
			{ddl: "d", vals: []any{today}},
			{ddl: "b", vals: []any{[]byte{0xDE, 0xAD}, nil}},
		},
		extra: mysqlExtra,
	},
	"dm": {
		pk: "ID",
		ddl: `CREATE TABLE etl_src (
			id INT PRIMARY KEY,
			i64 BIGINT, num NUMBER(20,4), numint NUMBER(10),
			name VARCHAR(100), flag BIT,
			dt TIMESTAMP(3), dtz TIMESTAMP WITH TIME ZONE, d DATE,
			b BLOB
		)`,
		cols: []colCase{
			{ddl: "i64", vals: []any{int64(-42), nil}},
			{ddl: "num", vals: []any{dec("1234567890123456.7890"), dec("-0.5")}},
			{ddl: "numint", vals: []any{int64(42), nil}},
			{ddl: "name", vals: []any{"赵 一", nil}},
			{ddl: "flag", vals: []any{true, false}, kind: k(core.KindBoolean)},
			{ddl: "dt", vals: []any{wall}},
			{ddl: "dtz", vals: []any{far}},
			{ddl: "d", vals: []any{today}},
			{ddl: "b", vals: []any{[]byte{0xDE, 0xAD}, nil}},
		},
		extra: dmExtra,
	},
	"kingbase": {
		pk: "id",
		ddl: `CREATE TABLE etl_src (
			id INT PRIMARY KEY,
			i64 BIGINT, amount NUMERIC(18,4), name VARCHAR(100),
			flag BOOLEAN, price DOUBLE PRECISION,
			dt TIMESTAMP(3), dtz TIMESTAMPTZ, d DATE,
			b BYTEA
		)`,
		cols: []colCase{
			{ddl: "i64", vals: []any{int64(-42), nil}},
			{ddl: "amount", vals: []any{dec("3441.9000"), dec("-0.01")}},
			{ddl: "name", vals: []any{"赵 一", nil}},
			{ddl: "flag", vals: []any{true, false}},
			{ddl: "price", vals: []any{3.25, nil}},
			{ddl: "dt", vals: []any{wall}},
			{ddl: "dtz", vals: []any{far}},
			{ddl: "d", vals: []any{today}},
			{ddl: "b", vals: []any{[]byte{0xDE, 0xAD}, nil}},
		},
		extra: kingbaseExtra,
	},
	"postgres": {
		pk: "id",
		ddl: `CREATE TABLE etl_src (
			id INT PRIMARY KEY,
			i64 BIGINT, amount NUMERIC(18,4), name VARCHAR(100),
			flag BOOLEAN, price DOUBLE PRECISION,
			dt TIMESTAMP(3), dtz TIMESTAMPTZ, d DATE,
			b BYTEA
		)`,
		cols: []colCase{
			{ddl: "i64", vals: []any{int64(-42), nil}},
			{ddl: "amount", vals: []any{dec("3441.9000"), dec("-0.01")}},
			{ddl: "name", vals: []any{"赵 一", nil}},
			{ddl: "flag", vals: []any{true, false}},
			{ddl: "price", vals: []any{3.25, nil}},
			{ddl: "dt", vals: []any{wall}},
			{ddl: "dtz", vals: []any{far}},
			{ddl: "d", vals: []any{today}},
			{ddl: "b", vals: []any{[]byte{0xDE, 0xAD}, nil}},
		},
	},
}

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func TestLiveMySQL(t *testing.T)    { liveDialect(t, "mysql") }
func TestLiveDM(t *testing.T)       { liveDialect(t, "dm") }
func TestLiveKingbase(t *testing.T) { liveDialect(t, "kingbase") }
func TestLivePostgres(t *testing.T) { liveDialect(t, "postgres") }

func liveDialect(t *testing.T, id string) {
	t.Helper()
	dc, ok := cases[id]
	if !ok {
		t.Fatalf("无 %s 用例", id)
	}
	cfg, pw := livetest.Config(t, id)
	cfg.Options.TimeZone = "Asia/Shanghai" // 固定连接时区，便于时间语义断言
	ctx := context.Background()
	h, err := conn.Open(ctx, cfg, pw)
	if err != nil {
		t.Fatalf("连接失败：%v", err)
	}
	defer h.DB.Close()
	d := h.Dialect

	var version string
	if err := h.DB.QueryRowContext(ctx, d.VersionSQL()).Scan(&version); err != nil {
		t.Fatalf("版本查询失败：%v", err)
	}
	t.Logf("%s 版本：%s", id, version)

	// 重建覆盖表（无引号名：DM 会存成大写，其余库小写）
	dropTable(t, h, "etl_src")
	if _, err := h.DB.ExecContext(ctx, dc.ddl); err != nil {
		t.Fatalf("建表失败：%v", err)
	}
	t.Cleanup(func() { dropTable(t, h, "etl_src") })
	// 与 steps.tableRef 一致：用户输入经方言折叠（达梦 → 大写）。
	ref := conn.TableRef{Schema: d.FoldIdent(conn.EffectiveSchema(h.Config, "")), Name: d.FoldIdent("etl_src")}

	// ---- 元数据 ----
	cols, err := d.Columns(ctx, h.DB, ref)
	if err != nil {
		t.Fatalf("Columns 失败：%v", err)
	}
	fields := map[string]core.Field{}
	var order []string
	for _, c := range cols {
		f := d.FieldFor(c)
		fields[strings.ToLower(c.Name)] = f
		order = append(order, c.Name)
		t.Logf("列 %-10s 原生=%-24s → %s (len=%d p=%d s=%d null=%v pk=%v)", c.Name, c.DBType, f.Kind, c.Length, c.Precision, c.Scale, c.Nullable, c.PrimaryKey)
	}
	idx, err := d.Indexes(ctx, h.DB, ref)
	if err != nil {
		t.Fatalf("Indexes 失败：%v", err)
	}
	pkOK := false
	for _, ix := range idx {
		if ix.Primary && len(ix.Columns) == 1 && strings.EqualFold(ix.Columns[0], dc.pk) {
			pkOK = true
		}
	}
	if !pkOK {
		t.Errorf("主键索引未发现：%+v", idx)
	}
	tabs, err := d.ListTables(ctx, h.DB, ref.Schema)
	if err != nil {
		t.Fatalf("ListTables 失败：%v", err)
	}
	found := false
	for _, tb := range tabs {
		if strings.EqualFold(tb.Name, "etl_src") {
			found = true
		}
	}
	if !found {
		t.Errorf("ListTables 未包含 etl_src：%+v", tabs)
	}

	// ---- 写入（走 Bind 路径）----
	insertCols := make([]string, 0, len(dc.cols))
	insertFields := make([]core.Field, 0, len(dc.cols))
	for _, cc := range dc.cols {
		f, ok := fields[strings.ToLower(cc.ddlName())]
		if !ok {
			t.Fatalf("列 %s 未在元数据中", cc.ddlName())
		}
		insertCols = append(insertCols, colName(cols, cc.ddlName()))
		insertFields = append(insertFields, f)
	}
	nrows := 0
	for _, cc := range dc.cols {
		if len(cc.vals) > nrows {
			nrows = len(cc.vals)
		}
	}
	for r := 0; r < nrows; r++ {
		args := make([]any, 0, len(dc.cols)+1)
		args = append(args, r+1)
		for i, cc := range dc.cols {
			v := at(cc.vals, r)
			b, err := d.Bind(v, insertFields[i])
			if err != nil {
				t.Fatalf("绑定 %s[%d]=%v 失败：%v", insertCols[i], r, v, err)
			}
			args = append(args, b)
		}
		q := conn.InsertSQL(d, ref, append([]string{colName(cols, "id")}, insertCols...), 1)
		if _, err := h.DB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("插入第 %d 行失败：%v\nSQL: %s", r, err, q)
		}
	}

	// ---- 读出（走 Normalize 路径）----
	orderBy := dc.ordered
	if orderBy == "" {
		orderBy = colName(cols, "id")
	}
	rows, err := h.DB.QueryContext(ctx, fmt.Sprintf("SELECT * FROM %s ORDER BY %s", d.Qualified(ref), orderBy))
	if err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	schema, err := conn.SchemaFromRows(d, rows)
	if err != nil {
		t.Fatalf("SchemaFromRows 失败：%v", err)
	}
	reader := conn.NewRowReader(h, rows, schema)
	var got []core.Row
	for {
		row, ok, err := reader.Next()
		if err != nil {
			t.Fatalf("读行失败：%v", err)
		}
		if !ok {
			break
		}
		got = append(got, row)
	}
	reader.Close()
	if len(got) != nrows {
		t.Fatalf("行数 %d != %d", len(got), nrows)
	}
	for _, cc := range dc.cols {
		f, ok := fields[strings.ToLower(cc.ddlName())]
		if !ok {
			continue
		}
		idx2 := schema.Index(f.Name)
		if idx2 < 0 {
			// 读出列名大小写可能与元数据不同（DM 大写）
			for j := 0; j < schema.Len(); j++ {
				if strings.EqualFold(schema.Field(j).Name, f.Name) {
					idx2 = j
					break
				}
			}
		}
		if idx2 < 0 {
			t.Errorf("读出 schema 缺少列 %s", f.Name)
			continue
		}
		rf := schema.Field(idx2)
		if cc.kind != nil && rf.Kind != *cc.kind {
			t.Errorf("列 %s 期望 kind=%s，读出 %s", cc.ddlName(), *cc.kind, rf.Kind)
		}
		for r := 0; r < nrows; r++ {
			want := at(cc.want, r)
			if want == nil && !cc.wantSet() {
				want = at(cc.vals, r)
			}
			assertLogical(t, fmt.Sprintf("%s[%d]", cc.ddlName(), r), got[r][idx2], want)
		}
	}
	t.Logf("类型往返通过：%d 行 × %d 列", nrows, len(dc.cols))

	if dc.extra != nil {
		dc.extra(t, h)
	}
}

func (c colCase) ddlName() string {
	name := strings.Fields(c.ddl)[0]
	return strings.Trim(name, `"` + "`")
}
func (c colCase) wantSet() bool { return c.want != nil }
func at(vals []any, i int) any {
	if i < len(vals) {
		return vals[i]
	}
	return nil
}

func dropTable(t *testing.T, h *conn.Handle, name string) {
	t.Helper()
	d := h.Dialect
	ref := conn.TableRef{Schema: d.FoldIdent(conn.EffectiveSchema(h.Config, "")), Name: d.FoldIdent(name)}
	// 各库都支持 DROP TABLE IF EXISTS（DM8、Kingbase 兼容）
	_, _ = h.DB.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+d.Qualified(ref))
}

func colName(cols []conn.ColumnMeta, want string) string {
	for _, c := range cols {
		if strings.EqualFold(c.Name, want) {
			return c.Name
		}
	}
	return want
}

// assertLogical 断言读回值与期望逻辑值相等（decimal 按数值、time 按墙上时钟、[]byte 按字节）。
func assertLogical(t *testing.T, where string, got, want any) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Errorf("%s：期望 NULL，实际 %#v", where, got)
		}
		return
	}
	switch w := want.(type) {
	case decimal.Decimal:
		g, ok := got.(decimal.Decimal)
		if !ok || !w.Equal(g) {
			t.Errorf("%s：期望 %s，实际 %#v", where, w, got)
		}
	case time.Time:
		g, ok := got.(time.Time)
		if !ok || !sameWallClock(w, g) {
			t.Errorf("%s：期望墙上时钟 %v，实际 %v", where, w, got)
		}
	case []byte:
		g, ok := got.([]byte)
		if !ok || !bytesEq(w, g) {
			t.Errorf("%s：期望 %x，实际 %#v", where, w, got)
		}
	default:
		if got != want {
			t.Errorf("%s：期望 %#v，实际 %#v", where, want, got)
		}
	}
}

// sameWallClock 无时区语义比较：只看墙上时钟分量。
func sameWallClock(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd &&
		a.Hour() == b.Hour() && a.Minute() == b.Minute() && a.Second() == b.Second() &&
		a.Nanosecond() == b.Nanosecond()
}

func bytesEq(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ------------------------------------------------ 各方言专项认证

func mysqlExtra(t *testing.T, h *conn.Handle) {
	ctx := context.Background()
	// DATETIME 与 TIMESTAMP 的时区行为：两者都按连接时区 Loc 解释（驱动行为）。
	// MySQL TIMESTAMP 在服务器端存 UTC、按会话 tz 转换；驱动 parseTime+Loc 下读回应为同一墙上时钟。
	var dtw, tsw time.Time
	err := h.DB.QueryRowContext(ctx, "SELECT dt, ts FROM etl_src WHERE id = 1").Scan(&dtw, &tsw)
	if err != nil {
		t.Fatalf("时间列查询失败：%v", err)
	}
	t.Logf("DATETIME 读出 %v (loc=%s)；TIMESTAMP 读出 %v (loc=%s)", dtw, dtw.Location(), tsw, tsw.Location())
	// TINYINT(1) 经由 SELECT 探测的类型名
	rows, err := h.DB.QueryContext(ctx, "SELECT flag, bitflag FROM etl_src WHERE 1=0")
	if err == nil {
		cts, _ := rows.ColumnTypes()
		for _, ct := range cts {
			p, s, _ := ct.DecimalSize()
			l, _ := ct.Length()
			t.Logf("探测列 %s: type=%s len=%d p=%d s=%d scan=%s", ct.Name(), ct.DatabaseTypeName(), l, p, s, ct.ScanType())
		}
		rows.Close()
	}
	// unsigned 极值读出
	var u64 any
	if err := h.DB.QueryRowContext(ctx, "SELECT u64 FROM etl_src WHERE id = 2").Scan(&u64); err != nil {
		t.Fatalf("unsigned 查询失败：%v", err)
	}
	t.Logf("u64 max 扫描类型 %T", u64)
	// TRUNCATE 与 ProbeSQL
	probe, err := conn.QuerySchema(ctx, h, "SELECT id, name FROM etl_src WHERE id > 0", nil)
	if err != nil || probe.Len() != 2 {
		t.Fatalf("ProbeSQL 失败：%v", err)
	}
}

func dmExtra(t *testing.T, h *conn.Handle) {
	ctx := context.Background()
	d := h.Dialect
	// ① NUMBER/DECIMAL 扫描出的 Go 类型
	for _, q := range []string{
		"SELECT num FROM etl_src WHERE id = 1",
		"SELECT numint FROM etl_src WHERE id = 1",
		"SELECT dtz FROM etl_src WHERE id = 1",
		"SELECT flag FROM etl_src WHERE id = 1",
		"SELECT d FROM etl_src WHERE id = 1",
	} {
		var v any
		if err := h.DB.QueryRowContext(ctx, q).Scan(&v); err != nil {
			t.Errorf("扫描失败 %s：%v", q, err)
			continue
		}
		t.Logf("%s → Go 类型 %T 值 %#v", q, v, v)
	}
	// ② 单语句参数上限：二分探测（IN 列表占位符）
	lo, hi := 1, 200000
	last := 0
	for lo <= hi {
		mid := (lo + hi) / 2
		args := make([]any, mid)
		for i := range args {
			args[i] = i
		}
		q := fmt.Sprintf("SELECT 1 FROM DUAL WHERE 1 IN (%s)", strings.TrimSuffix(strings.Repeat("?,", mid), ","))
		_, err := h.DB.ExecContext(ctx, q, args...)
		if err == nil {
			last = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	t.Logf("DM 单语句参数上限实测：%d（当前 MaxParams=%d）", last, d.MaxParams())
	// ③ 大结果集流式：插 5000 行后流式读，观察首行延迟与完整性
	dropTable(t, h, "etl_stream")
	if _, err := h.DB.ExecContext(ctx, `CREATE TABLE etl_stream (n INT, s VARCHAR(64))`); err != nil {
		t.Fatalf("建流式表失败：%v", err)
	}
	defer dropTable(t, h, "etl_stream")
	insRef := conn.TableRef{Schema: d.FoldIdent(conn.EffectiveSchema(h.Config, "")), Name: d.FoldIdent("etl_stream")}
	for start := 0; start < 5000; start += 500 {
		args := make([]any, 0, 1000)
		for i := start; i < start+500; i++ {
			args = append(args, i, fmt.Sprintf("s-%d", i))
		}
		if _, err := h.DB.ExecContext(ctx, conn.InsertSQL(d, insRef, []string{d.FoldIdent("n"), d.FoldIdent("s")}, 500), args...); err != nil {
			t.Fatalf("流式表插入失败：%v", err)
		}
	}
	t0 := time.Now()
	rows, err := h.DB.QueryContext(ctx, "SELECT n, s FROM etl_stream")
	if err != nil {
		t.Fatalf("流式查询失败：%v", err)
	}
	var first time.Duration
	n := 0
	for rows.Next() {
		if n == 0 {
			first = time.Since(t0)
		}
		var a int
		var s string
		rows.Scan(&a, &s)
		n++
	}
	t.Logf("DM 流式：首行 %s，读 %d 行共 %s（无需显式 fetch size 即可逐行返回）", first, n, time.Since(t0))
	rows.Close()
	// ④ 未加引号标识符存为大写
	if _, err := h.DB.ExecContext(ctx, `CREATE TABLE "etl_mixed" (MiXeD INT)`); err == nil {
		var cname string
		if err := h.DB.QueryRowContext(ctx, `SELECT column_name FROM all_tab_columns WHERE owner = USER AND table_name = 'etl_mixed'`).Scan(&cname); err == nil {
			t.Logf("引号表名 + 无引号列名 → 存为 %q", cname)
		}
		dropTable(t, h, "etl_mixed")
	}
	// ⑤ TIMESTAMP WITH TIME ZONE 语义
	var tzv any
	if err := h.DB.QueryRowContext(ctx, "SELECT dtz FROM etl_src WHERE id = 1").Scan(&tzv); err == nil {
		t.Logf("TSWTZ 扫描值 %T %#v", tzv, tzv)
	}
}

func kingbaseExtra(t *testing.T, h *conn.Handle) {
	ctx := context.Background()
	// ① ON CONFLICT / pg_catalog / information_schema 兼容性
	_, err := h.DB.ExecContext(ctx, `INSERT INTO etl_src (id) VALUES (1) ON CONFLICT (id) DO NOTHING`)
	t.Logf("ON CONFLICT: err=%v", err)
	// pg_catalog 查询（Indexes 已走过 pg_index + LATERAL）
	var reltuples float64
	err = h.DB.QueryRowContext(ctx, `SELECT reltuples FROM pg_class WHERE relname = 'etl_src'`).Scan(&reltuples)
	t.Logf("pg_class: %v (reltuples=%v)", err, reltuples)
	var ver string
	if err := h.DB.QueryRowContext(ctx, `SELECT version()`).Scan(&ver); err == nil {
		t.Logf("version(): %s", ver)
	}
	// information_schema
	var ntab int
	err = h.DB.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name = 'etl_src'`).Scan(&ntab)
	t.Logf("information_schema.tables: %v (n=%d)", err, ntab)
	// search_path 行为：默认 schema public 是否存在
	var cur string
	if err := h.DB.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&cur); err == nil {
		t.Logf("current_schema=%s", cur)
	}
}
