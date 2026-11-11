package steps_test

// 2026-11 外部评审复现项的回归用例：每个用例都断言完整结果集（主键集合/逐 ID 次数/
// 端口互斥），而不是只看运行状态与行数——这些缺陷的共同特征是「运行成功、行数对，数据错」。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/pipeline"
)

func previewOf(t *testing.T, e core.Env, d *pipeline.Definition, id string) pipeline.PreviewResult {
	t.Helper()
	r := pipeline.Preview(context.Background(), d, id, 256, pipeline.CompileOptions{Env: e}, pipeline.RunOptions{Env: e})
	if r.Error != "" {
		t.Fatalf("预览失败：%s %+v", r.Error, r.Problems)
	}
	return r
}

// idCounts 返回 SELECT 第一列（整数）每个值出现的次数。
func idCounts(t *testing.T, db *sql.DB, q string) map[int64]int {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out[id]++
	}
	return out
}

func problemCodes(p core.Problems, sev core.Severity) map[string]bool {
	m := map[string]bool{}
	for _, x := range p {
		if x.Severity == sev {
			m[x.Code] = true
		}
	}
	return m
}

// merge_rows 满批发送后复用底层数组，下游未读的批被下一批覆盖。
func TestRegressMergeRowsBatchNotReused(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE merge_dst (id INTEGER, flag TEXT)`)
	def := &pipeline.Definition{
		Settings: pipeline.Settings{BatchSize: 2, ChannelBatches: 16},
		Nodes: []pipeline.Node{
			node("base", "generate_rows", map[string]any{"sequenceField": "id", "limit": 0}),
			node("src", "generate_rows", map[string]any{"sequenceField": "id", "limit": 12}),
			node("m", "merge_rows", map[string]any{"keys": []string{"id"}}),
			node("dst", "table_output", map[string]any{"connection": "db", "table": "merge_dst"}),
		},
		Hops: []pipeline.Hop{hop("base", "main", "m"), {ID: "src-info", From: "src", FromPort: "main", To: "m", ToPort: "info"}, hop("m", "main", "dst")},
	}
	if res := run(t, e, def, nil); res.Err != nil {
		t.Fatal(res.Err)
	}
	got := idCounts(t, db, `SELECT id FROM merge_dst`)
	want := map[int64]int{}
	for i := int64(1); i <= 12; i++ {
		want[i] = 1
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("合并输出的主键集合应为 1..12 各一次，实际 %v", got)
	}
}

// csv_output 预览不得触碰目标文件。
func TestRegressCSVOutputPreviewNoWrite(t *testing.T) {
	e, _ := env(t)
	dir := t.TempDir()
	e = withFileRoot(e, dir)
	existing := filepath.Join(dir, "exists.csv")
	if err := os.WriteFile(existing, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path   string
		append bool
	}{{"exists.csv", false}, {"exists.csv", true}, {"absent.csv", false}} {
		def := &pipeline.Definition{
			Nodes: []pipeline.Node{
				node("g", "generate_rows", map[string]any{"sequenceField": "id", "limit": 3}),
				node("out", "csv_output", map[string]any{"path": tc.path, "header": true, "append": tc.append}),
			},
			Hops: []pipeline.Hop{hop("g", "main", "out")},
		}
		r := previewOf(t, e, def, "out")
		if n := len(r.Ports["main"].Rows); n != 3 {
			t.Fatalf("%+v：预览应透传 3 行，实际 %d", tc, n)
		}
	}
	if b, _ := os.ReadFile(existing); string(b) != "original\n" {
		t.Fatalf("预览改写了已有文件：%q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "absent.csv")); !os.IsNotExist(err) {
		t.Fatalf("预览不应创建文件：%v", err)
	}
}

// 多副本对共享外部资源做「一次性」动作：编译期拒绝。
func TestRegressCopiesOnSharedSinkRejected(t *testing.T) {
	e, db := env(t)
	e = withFileRoot(e, t.TempDir())
	mustExec(t, db, `CREATE TABLE tt (id INTEGER)`)
	gen := node("g", "generate_rows", map[string]any{"sequenceField": "id", "limit": 20})
	compile := func(out pipeline.Node) core.Problems {
		def := &pipeline.Definition{Nodes: []pipeline.Node{gen, out}, Hops: []pipeline.Hop{hop("g", "main", "out")}}
		return pipeline.Compile(context.Background(), def, pipeline.CompileOptions{Env: e}).Problems
	}
	for _, appendMode := range []bool{false, true} {
		out := node("out", "csv_output", map[string]any{"path": "x.csv", "append": appendMode})
		out.Copies = 2
		if !problemCodes(compile(out), core.SeverityError)["csv_copies"] {
			t.Fatalf("csv_output（append=%v）2 副本应编译报错", appendMode)
		}
	}
	out := node("out", "table_output", map[string]any{"connection": "db", "table": "tt", "truncate": true})
	out.Copies = 2
	if !problemCodes(compile(out), core.SeverityError)["truncate_copies"] {
		t.Fatal("table_output truncate + 2 副本应编译报错")
	}
	// 不清表的多副本写入、单副本清表仍可用
	out = node("out", "table_output", map[string]any{"connection": "db", "table": "tt"})
	out.Copies = 2
	if p := compile(out); p.HasErrors() {
		t.Fatalf("不清表的多副本表输出不应报错：%+v", p)
	}
	out = node("out", "table_output", map[string]any{"connection": "db", "table": "tt", "truncate": true})
	if p := compile(out); p.HasErrors() {
		t.Fatalf("单副本清表不应报错：%+v", p)
	}
}

// 相邻大整数（> 2^53）不能被排序比较器判为相等。
func TestRegressBigIntKeys(t *testing.T) {
	e, db := env(t)
	mustExec(t, db, `CREATE TABLE ids (id INTEGER)`)
	mustExec(t, db, `INSERT INTO ids VALUES (9007199254740992),(9007199254740993),(9007199254740993)`)
	def := &pipeline.Definition{
		Nodes: []pipeline.Node{
			node("src", "table_input", map[string]any{"connection": "db", "sql": "SELECT id FROM ids ORDER BY id"}),
			node("u", "unique_sorted", map[string]any{"keys": []string{"id"}}),
		},
		Hops: []pipeline.Hop{hop("src", "main", "u")},
	}
	rows := previewOf(t, e, def, "u").Ports["main"].Rows
	if fmt.Sprint(rows) != "[[9007199254740992] [9007199254740993]]" {
		t.Fatalf("去重应保留两个不同大整数，实际 %v", rows)
	}

	// 合并记录：参照 …992、对比 …993 → 一行 deleted、一行 new（不是 identical/changed）
	mustExec(t, db, `CREATE TABLE ref_t (id INTEGER, v TEXT)`)
	mustExec(t, db, `CREATE TABLE cmp_t (id INTEGER, v TEXT)`)
	mustExec(t, db, `INSERT INTO ref_t VALUES (9007199254740992,'a')`)
	mustExec(t, db, `INSERT INTO cmp_t VALUES (9007199254740993,'a')`)
	def = &pipeline.Definition{
		Nodes: []pipeline.Node{
			node("r", "table_input", map[string]any{"connection": "db", "sql": "SELECT id, v FROM ref_t"}),
			node("c", "table_input", map[string]any{"connection": "db", "sql": "SELECT id, v FROM cmp_t"}),
			node("m", "merge_rows", map[string]any{"keys": []string{"id"}}),
		},
		Hops: []pipeline.Hop{hop("r", "main", "m"), {ID: "c-info", From: "c", FromPort: "main", To: "m", ToPort: "info"}},
	}
	rows = previewOf(t, e, def, "m").Ports["main"].Rows
	if fmt.Sprint(rows) != "[[9007199254740992 a deleted] [9007199254740993 a new]]" {
		t.Fatalf("合并大整数键结果错误：%v", rows)
	}
}

// 时间戳哈希键必须保留小数秒（去重与流查询）。
func TestRegressTimestampHashKeys(t *testing.T) {
	e, _ := env(t)
	dir := t.TempDir()
	e = withFileRoot(e, dir)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("ev.csv", "ts,id\n2026-01-01T00:00:01.100Z,1\n2026-01-01T00:00:01.900Z,2\n2026-01-01T00:00:01.100Z,3\n")
	write("ref.csv", "ts,label\n2026-01-01T00:00:01.900Z,nine\n2026-01-01T00:00:01.100Z,one\n")
	tsFields := func(second string) []map[string]string {
		return []map[string]string{{"name": "ts", "kind": "timestamp"}, {"name": second, "kind": map[string]string{"id": "integer", "label": "string"}[second]}}
	}
	ev := node("ev", "csv_input", map[string]any{"path": "ev.csv", "hasHeader": true, "fields": tsFields("id")})
	ref := node("ref", "csv_input", map[string]any{"path": "ref.csv", "hasHeader": true, "fields": tsFields("label")})

	def := &pipeline.Definition{
		Nodes: []pipeline.Node{ev, node("u", "unique_hash", map[string]any{"keys": []string{"ts"}})},
		Hops:  []pipeline.Hop{hop("ev", "main", "u")},
	}
	var ids []string
	for _, r := range previewOf(t, e, def, "u").Ports["main"].Rows {
		ids = append(ids, r[1])
	}
	if fmt.Sprint(ids) != "[1 2]" {
		t.Fatalf("同一秒内的两个时刻应各保留一行，实际 id=%v", ids)
	}

	def = &pipeline.Definition{
		Nodes: []pipeline.Node{ev, ref, node("lk", "stream_lookup", map[string]any{
			"keys":    []map[string]string{{"stream": "ts", "column": "ts"}},
			"returns": []map[string]string{{"column": "label"}},
		})},
		Hops: []pipeline.Hop{hop("ev", "main", "lk"), {ID: "ref-info", From: "ref", FromPort: "main", To: "lk", ToPort: "info"}},
	}
	got := map[string]string{}
	for _, r := range previewOf(t, e, def, "lk").Ports["main"].Rows {
		got[r[1]] = r[2]
	}
	if want := map[string]string{"1": "one", "2": "nine", "3": "one"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("流查询按小数秒匹配错误：%v", got)
	}
}

// 按键写库（更新/插入更新）的时间键：流值是 UTC、连接时区非 UTC、无时区列按墙上时钟
// 存取——修正后既要区分同一秒内的不同时刻，又不能把「库里已有」误判为未命中。
func TestRegressTimestampKeyWriteMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tz.db")
	cfg := conn.Config{ID: "db", DBType: "sqlite", Database: path, Options: conn.Options{TimeZone: "Asia/Shanghai"}}
	pool := conn.NewPool(func(ctx context.Context, id string) (conn.Config, string, error) { return cfg, "", nil })
	t.Cleanup(pool.Close)
	h, err := pool.Get(context.Background(), "db")
	if err != nil {
		t.Fatal(err)
	}
	db := h.DB
	pk, pv := pool.EnvEntry()
	e := withFileRoot(core.MapEnv{pk: pv}, dir)
	if err := os.WriteFile(filepath.Join(dir, "in.csv"), []byte(
		"ts,v\n2026-01-01T00:00:01.100Z,1\n2026-01-01T00:00:01.900Z,2\n2026-01-01T00:00:01.500Z,5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := node("in", "csv_input", map[string]any{"path": "in.csv", "hasHeader": true,
		"fields": []map[string]string{{"name": "ts", "kind": "timestamp"}, {"name": "v", "kind": "integer"}}})
	state := func(table string) string {
		rows, err := db.Query(`SELECT ts, v FROM ` + table + ` ORDER BY ts`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var ts any
			var v int64
			if err := rows.Scan(&ts, &v); err != nil {
				t.Fatal(err)
			}
			s := fmt.Sprint(ts)
			if tm, ok := ts.(time.Time); ok {
				s = tm.Format("15:04:05.000")
			}
			out = append(out, fmt.Sprintf("%s=%d", s, v))
		}
		return fmt.Sprint(out)
	}

	// 更新：.1 与 .9 已存在（墙上时钟文本，SQLite 方言的绑定格式），.5 不存在 → 进错误端口
	mustExec(t, db, `CREATE TABLE upd (ts TIMESTAMP PRIMARY KEY, v INTEGER)`)
	mustExec(t, db, `INSERT INTO upd VALUES ('2026-01-01 00:00:01.1', 0), ('2026-01-01 00:00:01.9', 0)`)
	def := &pipeline.Definition{
		Nodes: []pipeline.Node{src, node("u", "update", map[string]any{"connection": "db", "table": "upd",
			"keys":   []map[string]string{{"stream": "ts", "column": "ts"}},
			"fields": []map[string]string{{"stream": "v", "column": "v"}}}), node("bad", "dummy", nil)},
		Hops: []pipeline.Hop{hop("in", "main", "u"), hop("u", "error", "bad")},
	}
	res := run(t, e, def, nil)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if got := state("upd"); got != "[00:00:01.100=1 00:00:01.900=2]" {
		t.Fatalf("更新结果错误：%s", got)
	}
	if res.Metrics["bad"].Read != 1 {
		t.Fatalf("只有 .5 应未命中，错误端口实际 %d 行", res.Metrics["bad"].Read)
	}

	// 插入更新：已有两行被更新、.5 被插入，不产生重复
	mustExec(t, db, `CREATE TABLE iu (ts TIMESTAMP, v INTEGER)`)
	mustExec(t, db, `INSERT INTO iu VALUES ('2026-01-01 00:00:01.1', 0), ('2026-01-01 00:00:01.9', 0)`)
	def = &pipeline.Definition{
		Nodes: []pipeline.Node{src, node("iu", "insert_update", map[string]any{"connection": "db", "table": "iu",
			"keys":   []map[string]any{{"stream": "ts", "column": "ts"}},
			"fields": []map[string]any{{"stream": "v", "column": "v", "update": true}}})},
		Hops: []pipeline.Hop{hop("in", "main", "iu")},
	}
	if res := run(t, e, def, nil); res.Err != nil {
		t.Fatal(res.Err)
	}
	if got := state("iu"); got != "[00:00:01.100=1 00:00:01.500=5 00:00:01.900=2]" {
		t.Fatalf("插入更新结果错误：%s", got)
	}
}

// 更新/删除：每个输入行只进一个端口；未命中键（含重复行）不得出现在「已处理」。
func TestRegressKeyWritePortsDisjoint(t *testing.T) {
	for _, op := range []string{"delete", "update"} {
		for _, withErr := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/errPort=%v", op, withErr), func(t *testing.T) {
				e, db := env(t)
				mustExec(t, db, `CREATE TABLE src (id INTEGER, v INTEGER)`)
				mustExec(t, db, `INSERT INTO src VALUES (1,10),(2,20),(2,21),(3,30),(1,11)`)
				mustExec(t, db, `CREATE TABLE target (id INTEGER PRIMARY KEY, v INTEGER)`)
				mustExec(t, db, `INSERT INTO target VALUES (1,0),(3,0)`)
				mustExec(t, db, `CREATE TABLE processed (id INTEGER, v INTEGER)`)
				cfg := map[string]any{"connection": "db", "table": "target", "keys": []map[string]string{{"stream": "id", "column": "id"}}}
				if op == "update" {
					cfg["fields"] = []map[string]string{{"stream": "v", "column": "v"}}
				}
				nodes := []pipeline.Node{
					node("src", "table_input", map[string]any{"connection": "db", "sql": "SELECT id, v FROM src"}),
					node("w", op, cfg),
					node("ok", "table_output", map[string]any{"connection": "db", "table": "processed"}),
				}
				hops := []pipeline.Hop{hop("src", "main", "w"), hop("w", "main", "ok")}
				if withErr {
					mustExec(t, db, `CREATE TABLE rejected (id INTEGER, v INTEGER)`)
					nodes = append(nodes,
						node("sel", "select_fields", map[string]any{"fields": []map[string]any{{"source": "id"}, {"source": "v"}}}),
						node("bad", "table_output", map[string]any{"connection": "db", "table": "rejected"}))
					hops = append(hops, hop("w", "error", "sel"), hop("sel", "main", "bad"))
				}
				res := run(t, e, &pipeline.Definition{Nodes: nodes, Hops: hops}, nil)
				if res.Err != nil {
					t.Fatal(res.Err)
				}
				if got := idCounts(t, db, `SELECT v FROM processed`); !reflect.DeepEqual(got, map[int64]int{10: 1, 11: 1, 30: 1}) {
					t.Fatalf("「已处理」应只含命中键的输入行（v=10/11/30），实际 %v", got)
				}
				if withErr {
					if got := idCounts(t, db, `SELECT v FROM rejected`); !reflect.DeepEqual(got, map[int64]int{20: 1, 21: 1}) {
						t.Fatalf("错误端口应含未命中键的全部行（v=20/21），实际 %v", got)
					}
				}
			})
		}
	}
}

// 下推开关差分：比较逐 ID 次数，而不只是集合或总数。
func TestRegressPushdownEquivalence(t *testing.T) {
	type tc struct {
		name  string
		build func() *pipeline.Definition
	}
	ti := func(extra map[string]any) pipeline.Node {
		cfg := map[string]any{"connection": "db", "sql": "SELECT id FROM src ORDER BY id"}
		for k, v := range extra {
			cfg[k] = v
		}
		return node("i", "table_input", cfg)
	}
	out := node("o", "table_output", map[string]any{"connection": "db", "table": "dst"})
	cases := []tc{
		{"maxRows", func() *pipeline.Definition {
			return &pipeline.Definition{
				Nodes: []pipeline.Node{ti(map[string]any{"maxRows": 2}), node("f", "filter_rows", map[string]any{"condition": "id == 3"}), out},
				Hops:  []pipeline.Hop{hop("i", "main", "f"), hop("f", "true", "o")},
			}
		}},
		{"copyCopies", func() *pipeline.Definition {
			// == 是可完整下推的条件（>= 等对可空列走「超集+行内复核」，不删节点，测不到此缺陷）
			f := node("f", "filter_rows", map[string]any{"condition": "id == 3"})
			f.Copies, f.CopiesDistribution = 2, core.DistCopy
			return &pipeline.Definition{
				Nodes: []pipeline.Node{ti(nil), f, out},
				Hops:  []pipeline.Hop{hop("i", "main", "f"), hop("f", "true", "o")},
			}
		}},
		{"copyCopiesSelect", func() *pipeline.Definition {
			s := node("s", "select_fields", map[string]any{"fields": []map[string]any{{"source": "id"}}})
			s.Copies, s.CopiesDistribution = 3, core.DistCopy
			return &pipeline.Definition{
				Nodes: []pipeline.Node{ti(nil), s, out},
				Hops:  []pipeline.Hop{hop("i", "main", "s"), hop("s", "main", "o")},
			}
		}},
		{"selectErrorPort", func() *pipeline.Definition {
			return &pipeline.Definition{
				Nodes: []pipeline.Node{ti(nil), node("s", "select_fields", map[string]any{"fields": []map[string]any{{"source": "id"}}}),
					out, node("errsel", "select_fields", map[string]any{"fields": []map[string]any{{"source": "id"}}}),
					node("eo", "table_output", map[string]any{"connection": "db", "table": "errdst"})},
				Hops: []pipeline.Hop{hop("i", "main", "s"), hop("s", "main", "o"), hop("s", "error", "errsel"), hop("errsel", "main", "eo")},
			}
		}},
		{"selectFanoutDistribute", func() *pipeline.Definition {
			s := node("s", "select_fields", map[string]any{"fields": []map[string]any{{"source": "id"}}})
			s.Distribution = map[string]core.Distribution{"main": core.DistDistribute}
			return &pipeline.Definition{
				Settings: pipeline.Settings{BatchSize: 1},
				Nodes:    []pipeline.Node{ti(nil), s, out, node("o2", "table_output", map[string]any{"connection": "db", "table": "errdst"})},
				Hops:     []pipeline.Hop{hop("i", "main", "s"), hop("s", "main", "o"), hop("s", "main", "o2")},
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, db := env(t)
			mustExec(t, db, `CREATE TABLE src (id INTEGER)`)
			mustExec(t, db, `INSERT INTO src VALUES (1),(2),(3),(4)`)
			mustExec(t, db, `CREATE TABLE dst (id INTEGER)`)
			mustExec(t, db, `CREATE TABLE errdst (id INTEGER)`)
			collect := func(def *pipeline.Definition) string {
				mustExec(t, db, `DELETE FROM dst`)
				mustExec(t, db, `DELETE FROM errdst`)
				if res := run(t, e, def, nil); res.Err != nil {
					t.Fatal(res.Err)
				}
				return fmt.Sprint(idCounts(t, db, `SELECT id FROM dst`), idCounts(t, db, `SELECT id FROM errdst`))
			}
			def := c.build()
			off := collect(def)
			def.Settings.Pushdown = true
			plan := pipeline.Compile(context.Background(), def, pipeline.CompileOptions{Env: e})
			if plan.Problems.HasErrors() {
				t.Fatalf("编译失败：%+v", plan.Problems)
			}
			opt, notes := pipeline.Pushdown(context.Background(), def, plan, e)
			on := collect(opt)
			if on != off {
				t.Fatalf("下推改变了结果：关闭 %s，开启 %s（notes=%v）", off, on, notes)
			}
		})
	}
}

// 流查询的参照与主流共用上游：编译期拒绝（直连与菱形都算），独立来源照常运行。
func TestRegressStreamLookupSharedUpstream(t *testing.T) {
	e, _ := env(t)
	lk := node("lk", "stream_lookup", map[string]any{
		"keys":    []map[string]string{{"stream": "id", "column": "id"}},
		"returns": []map[string]string{{"column": "id", "name": "matched"}},
	})
	info := func(from string) pipeline.Hop {
		return pipeline.Hop{ID: from + "-info", From: from, FromPort: "main", To: "lk", ToPort: "info"}
	}
	gen := func(id string, n int) pipeline.Node {
		return node(id, "generate_rows", map[string]any{"sequenceField": "id", "limit": n})
	}
	dummy := func(id string) pipeline.Node { return node(id, "dummy", nil) }
	settings := pipeline.Settings{BatchSize: 1, ChannelBatches: 2}
	for name, def := range map[string]*pipeline.Definition{
		"direct": {Settings: settings, Nodes: []pipeline.Node{gen("src", 10), lk}, Hops: []pipeline.Hop{hop("src", "main", "lk"), info("src")}},
		"diamond": {Settings: settings, Nodes: []pipeline.Node{gen("src", 10), dummy("a"), dummy("b"), lk},
			Hops: []pipeline.Hop{hop("src", "main", "a"), hop("src", "main", "b"), hop("a", "main", "lk"), info("b")}},
	} {
		plan := pipeline.Compile(context.Background(), def, pipeline.CompileOptions{Env: e})
		if !problemCodes(plan.Problems, core.SeverityError)["info_shared_upstream"] {
			t.Fatalf("%s：共用上游应编译报错，实际 %+v", name, plan.Problems)
		}
	}
	// 独立参照源：数据量远超通道缓冲也能正常结束
	def := &pipeline.Definition{Settings: settings, Nodes: []pipeline.Node{gen("src", 200), gen("ref", 200), lk},
		Hops: []pipeline.Hop{hop("src", "main", "lk"), info("ref")}}
	plan := pipeline.Compile(context.Background(), def, pipeline.CompileOptions{Env: e})
	if plan.Problems.HasErrors() {
		t.Fatalf("独立参照源不应报错：%+v", plan.Problems)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res := pipeline.Run(ctx, plan, pipeline.RunOptions{Env: e})
	if res.Err != nil || res.Metrics["lk"].Read != 400 {
		t.Fatalf("独立参照源应正常读完两路各 200 行：err=%v metrics=%+v", res.Err, res.Metrics["lk"])
	}
}
