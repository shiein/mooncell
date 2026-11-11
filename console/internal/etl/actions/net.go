// 作业项：HTTP 请求、子作业、通知（Webhook）。
package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"mooncell/console/internal/etl/core"
	"mooncell/console/internal/etl/workflow"
)

// ---------------------------------------------------------------- 凭据打开器

// SecretOpener 由服务层注入：把密文还原为明文（连接密码同款 sealed box）。
type SecretOpener interface {
	Open(sealed, aad string) (string, error)
}

type secretKey struct{}

// SecretEntry 返回注入 Env 的键值。
func SecretEntry(o SecretOpener) (any, any) { return secretKey{}, o }

// defIDKey 是 Env 中「当前运行定义 ID」的键。
type defIDKey struct{}

// DefIDEntry 返回注入 Env 的键值：notify 开密的 AAD 必须由运行中的定义
// ID 推导，不信任文档里的 aadSuffix（复制/篡改场景会不一致）。
func DefIDEntry(defID string) (any, any) { return defIDKey{}, defID }

func defIDFrom(env core.Env) string {
	if env == nil {
		return ""
	}
	if s, ok := env.Lookup(defIDKey{}).(string); ok {
		return s
	}
	return ""
}

func openerFrom(env core.Env) SecretOpener {
	if env != nil {
		if o, ok := env.Lookup(secretKey{}).(SecretOpener); ok {
			return o
		}
	}
	return nil
}

// SecretField 是作业项配置中的加密字段约定：
// 前端提交明文 `secret`，服务层在保存定义时 Seal 为 `secretCipher` 并删除明文。
const (
	NotifySecretAADPrefix = "etl/notify/" // AAD = prefix + defID + "/" + nodeID
)

// ---------------------------------------------------------------- HTTP 请求

// HTTPConfig 是「HTTP 请求」作业项：调接口，把状态码/响应字段写入变量。
type HTTPConfig struct {
	URL       string            `json:"url"`    // 支持 ${var}
	Method    string            `json:"method"` // GET/POST/PUT/DELETE，默认 GET
	Headers   map[string]string `json:"headers"`
	Body      string            `json:"body"`
	Timeout   int               `json:"timeout"`   // 秒，默认 30
	StatusVar string            `json:"statusVar"` // HTTP 状态码写变量
	DataPath  string            `json:"dataPath"`  // 响应 JSON 数组/对象路径 → 写入 ResultVar 的原始 JSON 文本
	ResultVar string            `json:"resultVar"` // 响应正文（或 DataPath 命中的子树）写变量
	Extract   []VarExtract      `json:"extract"`   // 从响应 JSON 按点路径取值写变量
}

// VarExtract：响应 JSON 点路径 → 变量。
type VarExtract struct {
	Path string    `json:"path"`
	Var  string    `json:"var"`
	Kind core.Kind `json:"kind"`
}

func httpVarExtracts(c *HTTPConfig) map[string]core.Kind {
	m := map[string]core.Kind{}
	if c.StatusVar != "" {
		m[c.StatusVar] = core.KindInteger
	}
	if c.ResultVar != "" {
		m[c.ResultVar] = core.KindString
	}
	for _, e := range c.Extract {
		if e.Var != "" && e.Kind.Valid() {
			m[e.Var] = e.Kind
		}
	}
	return m
}

func init() {
	// HTTP 请求
	workflow.Actions.Register(workflow.DefineAction(workflow.ActionDefinition[HTTPConfig]{
		Meta: core.Meta{ID: "http", Order: 45, Label: "HTTP 请求", Icon: "globe", Description: "调用 HTTP 接口（${变量} 可插值 URL/头/正文），把状态码与响应 JSON 中的值写入变量。",
			Inputs: []core.PortDecl{{Name: core.PortMain, Label: "输入"}}, Outputs: successPort, Keywords: []string{"HTTP", "REST", "接口"}},
		Defaults: func() HTTPConfig { return HTTPConfig{Method: "GET", Timeout: 30} },
		Outputs:  func(c *HTTPConfig) map[string]core.Kind { return httpVarExtracts(c) },
		Validate: func(vc *workflow.ValidateContext, c *HTTPConfig) error {
			if strings.TrimSpace(c.URL) == "" {
				vc.Errorf("url", "url_required", "请填写 URL")
			} else if u, err := url.ParseRequestURI(core.ExpandVars(c.URL, func(string) (any, bool) { return "x", true })); err != nil || u.Host == "" {
				vc.Errorf("url", "url", "URL 无效")
			}
			for _, name := range actionVarRefs(c.URL, c.Body, c.Headers, nil) {
				if _, ok := vc.VarKinds[name]; !ok {
					vc.Errorf("url", "var_undeclared", "引用了未定义的变量 %q", name)
				}
			}
			m := strings.ToUpper(strings.TrimSpace(c.Method))
			switch m {
			case "", "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD":
			default:
				vc.Errorf("method", "method", "不支持的方法 %q", c.Method)
			}
			for i, e := range c.Extract {
				if !core.ValidVarName(e.Var) {
					vc.Errorf(fmt.Sprintf("extract[%d].var", i), "var", "变量名 %q 无效", e.Var)
				}
				if !e.Kind.Valid() {
					vc.Errorf(fmt.Sprintf("extract[%d].kind", i), "kind", "类型无效")
				}
			}
			return nil
		},
		Execute: func(ctx context.Context, ec *workflow.ExecContext, c *HTTPConfig) (workflow.Outcome, error) {
			resolve := func(name string) (any, bool) {
				v, ok := ec.Vars.Get(name)
				if !ok {
					return nil, false
				}
				return core.FormatValue(v.V), true
			}
			u := core.ExpandVars(c.URL, resolve)
			body := core.ExpandVars(c.Body, resolve)
			method := strings.ToUpper(c.Method)
			if method == "" {
				method = "GET"
			}
			req, err := http.NewRequestWithContext(ctx, method, u, strings.NewReader(body))
			if err != nil {
				return workflow.Outcome{}, fmt.Errorf("构造请求失败：%w", err)
			}
			for k, v := range c.Headers {
				req.Header.Set(k, core.ExpandVars(v, resolve))
			}
			if body != "" && req.Header.Get("Content-Type") == "" {
				req.Header.Set("Content-Type", "application/json")
			}
			timeout := c.Timeout
			if timeout <= 0 {
				timeout = 30
			}
			cli := &http.Client{Timeout: time.Duration(timeout) * time.Second}
			ec.Log.Detail("请求 %s %s", method, u)
			resp, err := cli.Do(req)
			if err != nil {
				return workflow.Outcome{}, fmt.Errorf("请求失败：%w", err)
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
			if err != nil {
				return workflow.Outcome{}, err
			}
			vars := map[string]core.Value{}
			if c.StatusVar != "" {
				vars[c.StatusVar] = core.NewValue(int64(resp.StatusCode))
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return workflow.Outcome{Vars: vars}, fmt.Errorf("HTTP %d：%s", resp.StatusCode, oneLineAction(string(data)))
			}
			if c.ResultVar != "" || len(c.Extract) > 0 {
				var root any
				dec := json.NewDecoder(bytes.NewReader(data))
				dec.UseNumber()
				if err := dec.Decode(&root); err != nil {
					return workflow.Outcome{Vars: vars}, fmt.Errorf("响应不是 JSON：%w", err)
				}
				if c.ResultVar != "" {
					target := root
					if c.DataPath != "" {
						var ok bool
						if target, ok = jsonPathAction(root, c.DataPath); !ok {
							return workflow.Outcome{Vars: vars}, fmt.Errorf("响应中不存在数据路径 %q", c.DataPath)
						}
					}
					if s, ok := target.(string); ok {
						vars[c.ResultVar] = core.NewValue(s)
					} else {
						b, _ := json.Marshal(target)
						vars[c.ResultVar] = core.NewValue(string(b))
					}
				}
				for _, e := range c.Extract {
					v, ok := jsonPathAction(root, e.Path)
					if !ok || v == nil {
						return workflow.Outcome{Vars: vars}, fmt.Errorf("响应中不存在路径 %q", e.Path)
					}
					cv, err := core.Convert(jsonScalarAction(v), e.Kind, core.ConvertOptions{})
					if err != nil {
						return workflow.Outcome{Vars: vars}, fmt.Errorf("路径 %s 的值无法转为%s：%w", e.Path, e.Kind.Label(), err)
					}
					vars[e.Var] = core.Value{Kind: e.Kind, V: cv}
				}
			}
			return workflow.Outcome{Vars: vars, Message: fmt.Sprintf("HTTP %d", resp.StatusCode)}, nil
		},
	}))

	// 子作业
	workflow.Actions.Register(workflow.DefineAction(workflow.ActionDefinition[WorkflowConfig]{
		Meta: core.Meta{ID: "workflow", Order: 35, Label: "子作业", Icon: "workflow", Description: "运行另一个作业（嵌套上限 8 层），作业项状态归入本次运行；参数以表达式映射作业变量。",
			Inputs: []core.PortDecl{{Name: core.PortMain, Label: "输入"}}, Outputs: successPort, Keywords: []string{"Job", "子作业", "嵌套"}},
		Defaults: func() WorkflowConfig { return WorkflowConfig{Params: map[string]string{}} },
		Refs: func(c *WorkflowConfig) []workflow.DefRef {
			return []workflow.DefRef{{ID: c.Workflow, Kind: "workflow"}}
		},
		Validate: func(vc *workflow.ValidateContext, c *WorkflowConfig) error {
			if c.Workflow == "" {
				vc.Errorf("workflow", "workflow_required", "请选择要运行的作业")
				return nil
			}
			r, err := runnerFrom(vc.Env)
			if err != nil {
				return nil
			}
			name, params, err := r.WorkflowParams(vc.Ctx, c.Workflow)
			if err != nil {
				vc.Errorf("workflow", "workflow_missing", "作业不存在或无法读取：%s", err.Error())
				return nil
			}
			declared := map[string]core.Kind{}
			for _, p := range params {
				declared[p.Name] = p.Kind
				if _, ok := c.Params[p.Name]; !ok && p.Required && p.Default == nil {
					vc.Errorf("params."+p.Name, "param_required", "作业「%s」的必填参数 %s 未赋值", name, p.Name)
				}
			}
			for k, src := range c.Params {
				if _, ok := declared[k]; !ok {
					vc.Errorf("params."+k, "param_unknown", "作业「%s」没有参数 %s", name, k)
					continue
				}
				if _, err := compileVarExpr(src, vc.VarKinds); err != nil {
					vc.Errorf("params."+k, "expr", "参数 %s：%s", k, err.Error())
				}
			}
			return nil
		},
		Execute: func(ctx context.Context, ec *workflow.ExecContext, c *WorkflowConfig) (workflow.Outcome, error) {
			r, err := runnerFrom(ec.Env)
			if err != nil {
				return workflow.Outcome{}, err
			}
			kinds := kindsFromVars(ec.Vars)
			params := map[string]core.Value{}
			for k, src := range c.Params {
				vp, err := compileVarExpr(src, kinds)
				if err != nil {
					return workflow.Outcome{}, fmt.Errorf("参数 %s：%w", k, err)
				}
				v, err := vp.eval(ec.Vars)
				if err != nil {
					return workflow.Outcome{}, fmt.Errorf("参数 %s：%w", k, err)
				}
				params[k] = core.NewValue(v)
			}
			sum, err := r.RunWorkflow(ctx, ec, c.Workflow, params)
			if err != nil {
				return workflow.Outcome{}, err
			}
			return workflow.Outcome{Message: fmt.Sprintf("子作业完成：成功 %d、失败 %d、跳过 %d", sum.Succeeded, sum.Failed, sum.Skipped)}, nil
		},
	}))

	// 通知（Webhook）
	workflow.Actions.Register(workflow.DefineAction(workflow.ActionDefinition[NotifyConfig]{
		Meta: core.Meta{ID: "notify", Order: 50, Label: "通知", Icon: "bell", Description: "向 Webhook 发送 JSON 通知（运行结果、变量）。凭据在保存定义时加密存储，运行时注入指定请求头。",
			Inputs: []core.PortDecl{{Name: core.PortMain, Label: "输入"}}, Outputs: successPort, Keywords: []string{"Mail", "Webhook", "通知", "告警"}},
		Defaults: func() NotifyConfig {
			return NotifyConfig{Template: `{"runId":"${RUN_ID}","status":"运行完成","message":""}`}
		},
		Validate: func(vc *workflow.ValidateContext, c *NotifyConfig) error {
			if strings.TrimSpace(c.URL) == "" {
				vc.Errorf("url", "url_required", "请填写 Webhook 地址")
			} else if u, err := url.ParseRequestURI(core.ExpandVars(c.URL, func(string) (any, bool) { return "x", true })); err != nil || u.Host == "" {
				vc.Errorf("url", "url", "Webhook 地址无效")
			}
			for _, name := range actionVarRefs(c.URL, c.Template, c.Headers, nil) {
				if _, ok := vc.VarKinds[name]; !ok {
					vc.Errorf("url", "var_undeclared", "引用了未定义的变量 %q", name)
				}
			}
			return nil
		},
		Execute: func(ctx context.Context, ec *workflow.ExecContext, c *NotifyConfig) (workflow.Outcome, error) {
			resolve := func(name string) (any, bool) {
				v, ok := ec.Vars.Get(name)
				if !ok {
					return nil, false
				}
				return core.FormatValue(v.V), true
			}
			u := core.ExpandVars(c.URL, resolve)
			body := core.ExpandVars(c.Template, resolve)
			req, err := http.NewRequestWithContext(ctx, "POST", u, strings.NewReader(body))
			if err != nil {
				return workflow.Outcome{}, fmt.Errorf("构造请求失败：%w", err)
			}
			req.Header.Set("Content-Type", "application/json")
			for k, v := range c.Headers {
				req.Header.Set(k, core.ExpandVars(v, resolve))
			}
			if c.SecretHeader != "" {
				var secret string
				switch {
				case c.SecretCipher != "":
					op := openerFrom(ec.Env)
					if op == nil {
						return workflow.Outcome{}, errors.New("运行环境未注入凭据打开器")
					}
					suffix := c.AADSuffix
					if id := defIDFrom(ec.Env); id != "" {
						derived := id + "/" + ec.NodeID
						if suffix != "" && suffix != derived {
							return workflow.Outcome{}, fmt.Errorf("凭据 AAD 后缀 %q 与运行中的定义不符", suffix)
						}
						suffix = derived
					}
					pw, err := op.Open(c.SecretCipher, NotifySecretAADPrefix+suffix)
					if err != nil {
						return workflow.Outcome{}, fmt.Errorf("解密凭据失败：%w", err)
					}
					secret = pw
				case c.Secret != "":
					secret = c.Secret
				}
				if secret != "" {
					req.Header.Set(c.SecretHeader, core.ExpandVars(secret, resolve))
				}
			}
			resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
			if err != nil {
				return workflow.Outcome{}, fmt.Errorf("发送失败：%w", err)
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return workflow.Outcome{}, fmt.Errorf("Webhook 返回 HTTP %d", resp.StatusCode)
			}
			return workflow.Outcome{Message: fmt.Sprintf("已发送（HTTP %d）", resp.StatusCode)}, nil
		},
	}))
}

// NotifyConfig 是「通知」作业项（v1：Webhook；邮件需要部署级 SMTP 配置，后续补充）。
type NotifyConfig struct {
	URL          string            `json:"url"`
	Headers      map[string]string `json:"headers"`
	Template     string            `json:"template"`     // 正文模板，支持 ${var}
	SecretHeader string            `json:"secretHeader"` // 凭据写入的请求头（如 Authorization）
	// Secret 仅作提交时的瞬态字段：服务层保存定义时 Seal 为 SecretCipher 并清除；
	// 持久化文档不应再出现它（解码声明它是为了兼容绕过保存路径的文档）。
	Secret       string `json:"secret,omitempty"`
	SecretCipher string `json:"secretCipher,omitempty"`
	// AADSuffix 由保存方写入：defID + "/" + nodeID，作密封的 AAD 末段。
	AADSuffix string `json:"aadSuffix,omitempty"`
}

// WorkflowConfig 是「子作业」配置：嵌套运行另一个作业定义。
type WorkflowConfig struct {
	Workflow string            `json:"workflow"`
	Params   map[string]string `json:"params"` // 参数名 → 变量表达式
}

// ---------------------------------------------------------------- JSON 点路径（与 steps/http.go 同款轻量实现）

func jsonPathAction(v any, path string) (any, bool) {
	if path == "" || path == "$" {
		return v, true
	}
	for _, s := range strings.Split(strings.TrimPrefix(path, "$."), ".") {
		switch cur := v.(type) {
		case map[string]any:
			var ok bool
			if v, ok = cur[s]; !ok {
				return nil, false
			}
		case []any:
			var i int
			if _, err := fmt.Sscanf(s, "%d", &i); err != nil || i < 0 || i >= len(cur) {
				return nil, false
			}
			v = cur[i]
		default:
			return nil, false
		}
	}
	return v, true
}

func jsonScalarAction(v any) any {
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

// actionVarRefs 汇总 URL/正文/请求头/模板中引用的 ${name}。
func actionVarRefs(text, body string, headers map[string]string, extra []string) []string {
	out := append(core.VarRefs(text), core.VarRefs(body)...)
	for k, v := range headers {
		out = append(out, core.VarRefs(k)...)
		out = append(out, core.VarRefs(v)...)
	}
	for _, s := range extra {
		out = append(out, core.VarRefs(s)...)
	}
	return out
}

func oneLineAction(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}
