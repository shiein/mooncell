package sqltext

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func pg(i int) string { return fmt.Sprintf("$%d", i) }
func q(int) string    { return "?" }

func vars(m map[string]any) Resolver {
	return func(n string) (any, bool) { v, ok := m[n]; return v, ok }
}

func TestRenderBindsAndSkipsLiterals(t *testing.T) {
	sql := `select '${notvar}', "${col}", x::int -- ${c1}
/* ${c2} */ from t where a > ${a} and b = ${ b } and c = ${a}`
	out, args, err := Render(sql, Options{}, pg, vars(map[string]any{"a": int64(1), "b": "x"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a > $1 and b = $2 and c = $3") {
		t.Fatalf("占位符渲染错误：%s", out)
	}
	for _, keep := range []string{"'${notvar}'", `"${col}"`, "-- ${c1}", "/* ${c2} */", "x::int"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("字面量/注释被改写：缺少 %q\n%s", keep, out)
		}
	}
	if !reflect.DeepEqual(args, []any{int64(1), "x", int64(1)}) {
		t.Fatalf("参数错误：%#v", args)
	}
}

func TestRawSubstitutionIsWhitelisted(t *testing.T) {
	ok := map[string]any{"tbl": "ods.orders"}
	out, args, err := Render("select * from #{tbl}", Options{}, q, vars(ok))
	if err != nil || out != "select * from ods.orders" || len(args) != 0 {
		t.Fatalf("合法标识符替换失败：%q %v %v", out, args, err)
	}
	for _, bad := range []any{"orders; drop table x", "a b", "1abc", "a.b.c", int64(3), "o'rders"} {
		if _, _, err := Render("select * from #{tbl}", Options{}, q, vars(map[string]any{"tbl": bad})); err == nil {
			t.Fatalf("非法值 %#v 应被拒绝", bad)
		}
	}
}

func TestMySQLLexing(t *testing.T) {
	opts := Options{BackslashEscapes: true, HashComments: true}
	sql := `select 'it\'s ${x}' # ${y}
from t where id = ${id}`
	out, args, err := Render(sql, opts, q, vars(map[string]any{"id": int64(7)}))
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 1 || !strings.HasSuffix(out, "id = ?") || !strings.Contains(out, `# ${y}`) {
		t.Fatalf("MySQL 词法错误：%q %v", out, args)
	}
}

func TestDollarQuoteAndEscapes(t *testing.T) {
	opts := Options{DollarQuotes: true}
	sql := `select $tag$ ${inside} $tag$, $1, $${literal}, ##{lit2}, ${v}`
	out, args, err := Render(sql, opts, pg, vars(map[string]any{"v": true}))
	if err != nil {
		t.Fatal(err)
	}
	want := `select $tag$ ${inside} $tag$, $1, ${literal}, #{lit2}, $1`
	if out != want || len(args) != 1 {
		t.Fatalf("got %q want %q", out, want)
	}
}

func TestErrors(t *testing.T) {
	cases := []string{"select ${a", "select ${1a}", "select ${missing}"}
	for _, c := range cases {
		if _, _, err := Render(c, Options{}, q, vars(nil)); err == nil {
			t.Fatalf("%q 应报错", c)
		}
	}
	refs, err := Scan("a ${x} #{y} '${z}'", Options{})
	if err != nil || len(refs) != 2 || refs[0].Name != "x" || !refs[1].Raw {
		t.Fatalf("Scan 错误：%+v %v", refs, err)
	}
}
