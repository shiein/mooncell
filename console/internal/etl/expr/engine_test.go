package expr

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"mooncell/console/internal/etl/core"
)

var testSchema = core.MustSchema(
	core.NewField("name", core.KindString),
	core.NewField("qty", core.KindInteger),
	core.NewField("price", core.KindDecimal),
	core.NewField("rate", core.KindNumber),
	core.NewField("created_at", core.KindTimestamp),
	core.NewField("订单 备注", core.KindString),
	core.NewField("year", core.KindInteger), // 与函数同名
	core.NewField("地区", core.KindString),
)

func eval(t *testing.T, src string, row core.Row) (any, core.Kind) {
	t.Helper()
	p, err := Compile(src, Env{Schema: testSchema, Vars: map[string]core.Kind{"maxTs": core.KindTimestamp}})
	if err != nil {
		t.Fatalf("编译 %q 失败：%v", src, err)
	}
	vars := core.NewVars(nil)
	vars.Set("maxTs", core.NewValue(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	v, err := p.NewEvaluator(vars).Eval(row)
	if err != nil {
		t.Fatalf("求值 %q 失败：%v", src, err)
	}
	return v, p.Kind
}

func row() core.Row {
	return core.Row{"  ada ", int64(3), decimal.RequireFromString("19.90"), 0.1,
		time.Date(2026, 11, 10, 9, 30, 0, 0, time.UTC), "加急", int64(2025), "华东"}
}

func TestDecimalStaysExact(t *testing.T) {
	v, k := eval(t, "price * qty", row())
	if k != core.KindDecimal {
		t.Fatalf("类型推导应为 decimal，实际 %q", k)
	}
	if d := v.(decimal.Decimal); d.String() != "59.7" {
		t.Fatalf("精确小数结果错误：%s", d)
	}
	v, _ = eval(t, "price + 0.1 >= 20 and price + 0.1 < 20.0001", row())
	if v != true {
		t.Fatalf("decimal 比较错误：%v", v)
	}
	v, _ = eval(t, "price == 19.9", row())
	if v != true {
		t.Fatalf("decimal 相等比较应忽略尾零：%v", v)
	}
}

func TestTypesAndFunctions(t *testing.T) {
	cases := []struct {
		src  string
		want any
		kind core.Kind
	}{
		{`upper(trim(name))`, "ADA", core.KindString},
		{`length(地区)`, int64(2), core.KindInteger},
		{`qty * 2 + 1`, int64(7), core.KindInteger},
		{`field("订单 备注") + "!"`, "加急!", ""},
		{`field("year")`, int64(2025), ""},
		{`formatDate(created_at, "yyyy-MM-dd HH:mm")`, "2026-11-10 09:30", ""},
		{`created_at > var("maxTs")`, true, core.KindBoolean},
		{`decode(qty, 3, "三", "其它")`, "三", ""},
		{`substr("20261110", 1, 4)`, "2026", ""},
		{`qty > 1 ? "多" : "少"`, "多", core.KindString},
		{`地区 in ["华东", "华南"]`, true, core.KindBoolean},
	}
	for _, c := range cases {
		v, k := eval(t, c.src, row())
		if v != c.want {
			t.Errorf("%s = %#v，期望 %#v", c.src, v, c.want)
		}
		if c.kind != "" && k != c.kind {
			t.Errorf("%s 推导类型 %q，期望 %q", c.src, k, c.kind)
		}
	}
}

func TestNullSemantics(t *testing.T) {
	r := row()
	r[1] = nil // qty = NULL
	if v, _ := eval(t, `qty ?? 0`, r); v != int64(0) {
		t.Fatalf("?? 兜底失败：%#v", v)
	}
	if v, _ := eval(t, `upper(field("订单 备注"))`, core.Row{nil, nil, nil, nil, nil, nil, nil, nil}); v != nil {
		t.Fatalf("字符串函数对 NULL 应返回 NULL：%#v", v)
	}
	p, _ := Compile(`qty + 1`, Env{Schema: testSchema})
	if _, err := p.NewEvaluator(nil).Eval(r); err == nil {
		t.Fatal("NULL 参与算术应报错（让该行进错误输出）")
	}
}

func TestCompileErrorsAreHelpful(t *testing.T) {
	for src, want := range map[string]string{
		`nmae + 1`:     "未知的字段或函数",
		`field("不存在")`: "不存在",
		`var("nope")`:  "未声明",
		`upper(qty`:    "",
		`now(1)`:       "参数",
	} {
		_, err := Compile(src, Env{Schema: testSchema, Vars: map[string]core.Kind{}})
		if err == nil {
			t.Errorf("%q 应编译失败", src)
			continue
		}
		if want != "" && !strings.Contains(err.Error(), want) {
			t.Errorf("%q 错误提示 %q 未包含 %q", src, err.Error(), want)
		}
	}
}

func TestGoLayout(t *testing.T) {
	for in, want := range map[string]string{
		"yyyy-MM-dd HH:mm:ss":       "2006-01-02 15:04:05",
		"yyyyMMdd":                  "20060102",
		"yyyy-MM-dd'T'HH:mm:ss.SSS": "2006-01-02T15:04:05.000",
	} {
		if got := GoLayout(in); got != want {
			t.Errorf("GoLayout(%q)=%q want %q", in, got, want)
		}
	}
}

func TestCatalogIsComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range Catalog() {
		if d.Description == "" || d.Category == "" || len(d.Examples) == 0 || d.Insert == "" {
			t.Errorf("函数 %s 文档不完整", d.Name)
		}
		if seen[d.Name] {
			t.Errorf("函数重复：%s", d.Name)
		}
		seen[d.Name] = true
	}
}
