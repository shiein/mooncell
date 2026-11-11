package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mooncell/console/internal/etl/core"
)

// HTTPField 是 JSON 元素里取的一个字段：点路径 → 类型转换。
type HTTPField struct {
	Name   string    `json:"name"`
	Path   string    `json:"path"` // 相对行元素的点路径（如 user.name）；空 = 元素本身
	Kind   core.Kind `json:"kind"`
	Format string    `json:"format"`
}

// HTTPPage 是分页配置：页码参数按页递增，返回不足 size 行即止。
type HTTPPage struct {
	Param     string `json:"param"`     // 页码参数名
	SizeParam string `json:"sizeParam"` // 页大小参数名（可选）
	Size      int    `json:"size"`      // 页大小（判末页用）
	Start     int    `json:"start"`     // 起始页码（默认 1）
	MaxPages  int    `json:"maxPages"`  // 安全上限（默认 1000）
}

// HTTPInputConfig 是「HTTP 输入」（Kettle HTTP / REST client）：
// 请求 URL 返回 JSON，按 dataPath 找到数组拍平成行，逐字段按点路径取值。
// URL / 头 / 正文支持 ${变量} 文本替换。
type HTTPInputConfig struct {
	URL      string            `json:"url"`
	Method   string            `json:"method"` // GET / POST
	Headers  map[string]string `json:"headers"`
	Body     string            `json:"body"`
	DataPath string            `json:"dataPath"` // 指向数组（或对象 → 单行）；空 = 根
	Fields   []HTTPField       `json:"fields"`
	Page     *HTTPPage         `json:"page"`
	Timeout  int               `json:"timeout"` // 秒，默认 60
}

func init() {
	core.Steps.Register(core.Define(core.Definition[HTTPInputConfig]{
		Meta: core.Meta{
			ID: "http_input", Order: 40, Label: "HTTP 输入", Category: core.CategoryInput, Icon: "globe",
			Description: "请求 HTTP 接口（GET/POST，支持 ${变量} 与页码分页），响应 JSON 按数据路径拍平成行。字段声明点路径与类型，取值失败的行可连错误输出。",
			Outputs:     []core.PortDecl{{Name: core.PortMain, Label: "输出"}},
			ErrorPort:   true,
			Keywords:    []string{"HTTP", "REST", "HTTP client", "接口", "json"},
		},
		Defaults: func() HTTPInputConfig { return HTTPInputConfig{Method: "GET", Timeout: 60} },
		Infer: func(ic *core.InferContext, c *HTTPInputConfig) error {
			if strings.TrimSpace(c.URL) == "" {
				ic.Errorf("url", "url_required", "请填写 URL")
			} else if u, err := url.ParseRequestURI(expandVars(c.URL, func(string) (any, bool) { return "x", true })); err != nil || u.Host == "" {
				ic.Errorf("url", "url", "URL 无效")
			}
			for _, v := range httpVars(c) {
				if _, ok := ic.VarKinds[v]; !ok {
					ic.Errorf("url", "var_undeclared", "引用了未声明的变量 %q，请在转换参数中声明", v)
				}
			}
			s, err := httpSchema(c.Fields)
			if err != nil {
				ic.Errorf("fields", "fields", "%s", err.Error())
				return nil
			}
			ic.SetOutput(core.PortMain, s)
			return nil
		},
		Open: func(rc *core.RunContext, c *HTTPInputConfig) (core.Runner, error) {
			s, err := httpSchema(c.Fields)
			if err != nil {
				return nil, err
			}
			opts := make([]core.ConvertOptions, len(c.Fields))
			paths := make([][]string, len(c.Fields))
			for i, f := range c.Fields {
				opts[i] = core.ConvertOptions{Layout: goLayout(f.Format)}
				paths[i] = splitPath(f.Path)
			}
			method := strings.ToUpper(strings.TrimSpace(c.Method))
			if method == "" {
				method = "GET"
			}
			if method != "GET" && method != "POST" {
				return nil, fmt.Errorf("方法只支持 GET / POST")
			}
			timeout := c.Timeout
			if timeout <= 0 {
				timeout = 60
			}
			cli := &http.Client{Timeout: time.Duration(timeout) * time.Second}
			resolve := func(name string) (any, bool) {
				v, ok := rc.Vars.Get(name)
				if !ok {
					return nil, false
				}
				return core.FormatValue(v.V), true
			}
			return core.RunnerFunc(func(ctx context.Context) error {
				em := rc.Emitter(core.PortMain)
				page, start, maxPages := 1, 1, 1
				var pg *HTTPPage
				if c.Page != nil && c.Page.Param != "" {
					pg = c.Page
					start = pg.Start
					if start <= 0 {
						start = 1
					}
					maxPages = pg.MaxPages
					if maxPages <= 0 {
						maxPages = 1000
					}
					maxPages += start - 1
				}
				for page = start; page <= maxPages; page++ {
					elems, err := fetch(ctx, rc, cli, c, method, resolve, pg, page)
					if err != nil {
						return err
					}
					for _, e := range elems {
						row, rerr := httpRow(e, s, paths, opts)
						if rerr != nil {
							if err := rc.Reject(ctx, padRow(row, s.Len()), rerr, ""); err != nil {
								return err
							}
							continue
						}
						rc.Metrics.Input.Add(1)
						if err := em.Emit(ctx, row); err != nil {
							return err
						}
					}
					if pg == nil || (pg.Size > 0 && len(elems) < pg.Size) {
						break
					}
				}
				return em.Flush(ctx)
			}), nil
		},
	}))
}

func httpSchema(fields []HTTPField) (*core.Schema, error) {
	fs := make([]core.Field, len(fields))
	for i, f := range fields {
		if strings.TrimSpace(f.Name) == "" {
			return nil, fmt.Errorf("第 %d 个字段缺少名称", i+1)
		}
		if !f.Kind.Valid() {
			return nil, fmt.Errorf("字段 %q 的类型无效", f.Name)
		}
		fs[i] = core.NewField(f.Name, f.Kind)
	}
	return core.NewSchema(fs...)
}

// splitPath 把 "a.b[0].c" 形式的点路径切成段（数字段 = 数组下标）。
func splitPath(p string) []string {
	if p == "" || p == "$" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(p, "$."), ".")
}

// jsonPath 按点路径取值：map 段取键、数组段取数字下标；取不到返回 (nil,false)。
func jsonPath(v any, segs []string) (any, bool) {
	for _, s := range segs {
		switch cur := v.(type) {
		case map[string]any:
			var ok bool
			if v, ok = cur[s]; !ok {
				return nil, false
			}
		case []any:
			i, err := strconv.Atoi(s)
			if err != nil || i < 0 || i >= len(cur) {
				return nil, false
			}
			v = cur[i]
		default:
			return nil, false
		}
	}
	return v, true
}

// fetch 发一页请求并按 dataPath 展开为元素列表。
func fetch(ctx context.Context, rc *core.RunContext, cli *http.Client, c *HTTPInputConfig, method string, resolve func(string) (any, bool), pg *HTTPPage, page int) ([]any, error) {
	u := expandVars(c.URL, resolve)
	body := expandVars(c.Body, resolve)
	if pg != nil {
		if method == "GET" {
			sep := "?"
			if strings.Contains(u, "?") {
				sep = "&"
			}
			u += fmt.Sprintf("%s%s=%d", sep, url.QueryEscape(pg.Param), page)
			if pg.SizeParam != "" {
				u += fmt.Sprintf("&%s=%d", url.QueryEscape(pg.SizeParam), pg.Size)
			}
		} else {
			var obj map[string]any
			if body == "" {
				obj = map[string]any{}
			} else if err := json.Unmarshal([]byte(body), &obj); err != nil {
				return nil, fmt.Errorf("POST 正文不是 JSON 对象，无法注入分页参数：%w", err)
			}
			obj[pg.Param] = page
			if pg.SizeParam != "" {
				obj[pg.SizeParam] = pg.Size
			}
			b, _ := json.Marshal(obj)
			body = string(b)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, u, strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败：%w", err)
	}
	for k, v := range c.Headers {
		req.Header.Set(k, expandVars(v, resolve))
	}
	if method == "POST" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rc.Log.Detail("请求 %s %s", method, oneLine(u))
	resp, err := cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败：%w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("读响应失败：%w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d：%s", resp.StatusCode, oneLine(string(data)))
	}
	var root any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("响应不是 JSON：%w", err)
	}
	target, ok := jsonPath(root, splitPath(c.DataPath))
	if c.DataPath != "" && !ok {
		return nil, fmt.Errorf("响应中不存在数据路径 %q", c.DataPath)
	}
	switch t := target.(type) {
	case []any:
		return t, nil
	case nil:
		return nil, nil
	default:
		return []any{t}, nil
	}
}

// httpRow 取一行：每个字段按点路径取值 → Convert 到声明类型。
func httpRow(elem any, s *core.Schema, paths [][]string, opts []core.ConvertOptions) (core.Row, error) {
	row := make(core.Row, s.Len())
	for i := 0; i < s.Len(); i++ {
		v, ok := jsonPath(elem, paths[i])
		if !ok || v == nil {
			continue
		}
		cv, err := core.Convert(jsonScalar(v), s.Field(i).Kind, opts[i])
		if err != nil {
			return row, core.NewRowError(s.Field(i).Name, err)
		}
		row[i] = cv
	}
	return row, nil
}

// jsonScalar 把 JSON 值规范化供 Convert：数字用原始文本（UseNumber），其余标量原样。
func jsonScalar(v any) any {
	switch x := v.(type) {
	case json.Number:
		return x.String()
	case map[string]any, []any:
		b, _ := json.Marshal(x)
		return string(b)
	default:
		return v
	}
}

// expandVars 替换 ${name}（简单文本替换；未知名保持原样由上层按 VarKinds 校验）。
func expandVars(s string, resolve func(string) (any, bool)) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			return b.String() + s
		}
		j := strings.Index(s[i:], "}")
		if j < 0 {
			return b.String() + s
		}
		b.WriteString(s[:i])
		name := s[i+2 : i+j]
		if v, ok := resolve(name); ok {
			if sv, ok := v.(string); ok {
				b.WriteString(sv)
			} else {
				b.WriteString(fmt.Sprint(v))
			}
		} else {
			b.WriteString(s[i : i+j+1])
		}
		s = s[i+j+1:]
	}
}

// httpVars 收集 URL/头/正文里引用的 ${name}。
func httpVars(c *HTTPInputConfig) []string {
	var out []string
	scan := func(s string) {
		for {
			i := strings.Index(s, "${")
			if i < 0 {
				return
			}
			j := strings.Index(s[i:], "}")
			if j < 0 {
				return
			}
			out = append(out, s[i+2:i+j])
			s = s[i+j+1:]
		}
	}
	scan(c.URL)
	scan(c.Body)
	for _, v := range c.Headers {
		scan(v)
	}
	return out
}
