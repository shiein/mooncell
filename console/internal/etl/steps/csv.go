package steps

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"

	"mooncell/console/internal/etl/core"
)

// CSVField 是 CSV 列声明：位置对应文件第 N 列，按 Kind 转换（Format 用于日期文本）。
type CSVField struct {
	Name   string    `json:"name"`
	Kind   core.Kind `json:"kind"`
	Format string    `json:"format"`
}

func csvFieldsSchema(cols []CSVField) (*core.Schema, error) {
	fields := make([]core.Field, len(cols))
	for i, f := range cols {
		if strings.TrimSpace(f.Name) == "" {
			return nil, fmt.Errorf("第 %d 列缺少名称", i+1)
		}
		if !f.Kind.Valid() {
			return nil, fmt.Errorf("字段 %q 的类型无效", f.Name)
		}
		fields[i] = core.NewField(f.Name, f.Kind)
	}
	return core.NewSchema(fields...)
}

func csvDelim(d string) (rune, error) {
	d = strings.ReplaceAll(d, `\t`, "\t")
	r := []rune(d)
	if d == "" {
		return ',', nil
	}
	if len(r) != 1 {
		return 0, fmt.Errorf("分隔符必须是单个字符")
	}
	return r[0], nil
}

// decodeReader 按编码包装读取器（utf-8 原样，gbk 解码为 utf-8）。
func decodeReader(r io.Reader, enc string) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "", "utf-8", "utf8":
		return r, nil
	case "gbk", "gb18030":
		return transform.NewReader(r, simplifiedchinese.GBK.NewDecoder()), nil
	}
	return nil, fmt.Errorf("不支持的编码 %q（支持 utf-8 / gbk）", enc)
}

func encodeWriter(w io.Writer, enc string) (io.Writer, error) {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "", "utf-8", "utf8":
		return w, nil
	case "gbk", "gb18030":
		return transform.NewWriter(w, simplifiedchinese.GBK.NewEncoder()), nil
	}
	return nil, fmt.Errorf("不支持的编码 %q（支持 utf-8 / gbk）", enc)
}

// ---------------------------------------------------------------- CSV 输入

// CSVInputConfig 是「CSV 输入」：读定界文本文件，按声明的字段类型逐行转换。
type CSVInputConfig struct {
	Path      string     `json:"path"`
	Delimiter string     `json:"delimiter"`
	Encoding  string     `json:"encoding"`
	HasHeader bool       `json:"hasHeader"`
	Fields    []CSVField `json:"fields"`
}

func init() {
	core.Steps.Register(core.Define(core.Definition[CSVInputConfig]{
		Meta: core.Meta{
			ID: "csv_input", Order: 30, Label: "CSV 输入", Category: core.CategoryInput, Icon: "file-text",
			Description: "读取定界文本文件（默认逗号分隔、首行表头）。列按位置对应声明字段，逐行按类型转换，失败行可连错误输出。",
			Outputs:     []core.PortDecl{{Name: core.PortMain, Label: "输出"}},
			ErrorPort:   true,
			Keywords:    []string{"CSV file input", "Text file input", "csv", "文本文件输入"},
		},
		Defaults: func() CSVInputConfig { return CSVInputConfig{Delimiter: ",", Encoding: "utf-8", HasHeader: true} },
		Infer: func(ic *core.InferContext, c *CSVInputConfig) error {
			if strings.TrimSpace(c.Path) == "" {
				ic.Errorf("path", "path_required", "请填写文件路径")
			} else {
				checkFilePath(ic, c.Path, "path")
			}
			if _, err := csvDelim(c.Delimiter); err != nil {
				ic.Errorf("delimiter", "delimiter", "%s", err.Error())
			}
			s, err := csvFieldsSchema(c.Fields)
			if err != nil {
				ic.Errorf("fields", "fields", "%s", err.Error())
				return nil
			}
			ic.SetOutput(core.PortMain, s)
			return nil
		},
		Open: func(rc *core.RunContext, c *CSVInputConfig) (core.Runner, error) {
			s, err := csvFieldsSchema(c.Fields)
			if err != nil {
				return nil, err
			}
			delim, err := csvDelim(c.Delimiter)
			if err != nil {
				return nil, err
			}
			opts := make([]core.ConvertOptions, len(c.Fields))
			for i, f := range c.Fields {
				opts[i] = core.ConvertOptions{Layout: goLayout(f.Format)}
			}
			return core.RunnerFunc(func(ctx context.Context) error {
				p, err := resolveFilePath(rc.Env, c.Path, false)
				if err != nil {
					return err
				}
				f, err := os.Open(p)
				if err != nil {
					return fmt.Errorf("打开文件失败：%w", err)
				}
				defer f.Close()
				rd, err := decodeReader(f, c.Encoding)
				if err != nil {
					return err
				}
				r := csv.NewReader(rd)
				r.Comma = delim
				r.FieldsPerRecord = -1
				em := rc.Emitter(core.PortMain)
				line := 0
				for {
					rec, err := r.Read()
					if err == io.EOF {
						break
					}
					line++
					if err != nil {
						return fmt.Errorf("解析第 %d 行失败：%w", line, err)
					}
					if line == 1 && c.HasHeader {
						continue
					}
					row, rerr := csvRow(rec, s, opts)
					if rerr != nil {
						if err := rc.Reject(ctx, padRow(row, s.Len()), fmt.Errorf("第 %d 行：%w", line, rerr), ""); err != nil {
							return err
						}
						continue
					}
					rc.Metrics.Input.Add(1)
					if err := em.Emit(ctx, row); err != nil {
						return err
					}
				}
				return em.Flush(ctx)
			}), nil
		},
	}))
}

func padRow(r core.Row, n int) core.Row {
	for len(r) < n {
		r = append(r, nil)
	}
	return r
}

func csvRow(rec []string, s *core.Schema, opts []core.ConvertOptions) (core.Row, error) {
	if len(rec) > s.Len() {
		return nil, fmt.Errorf("列数 %d 超过字段数 %d", len(rec), s.Len())
	}
	row := make(core.Row, s.Len())
	for i := 0; i < s.Len(); i++ {
		if i >= len(rec) {
			break
		}
		text := rec[i]
		if text == "" {
			continue // 空串 → NULL
		}
		v, err := core.Convert(text, s.Field(i).Kind, opts[i])
		if err != nil {
			return row, core.NewRowError(s.Field(i).Name, err)
		}
		row[i] = v
	}
	return row, nil
}

// ---------------------------------------------------------------- CSV 输出

// CSVOutputConfig 是「CSV 输出」：把流写为定界文本文件。
type CSVOutputConfig struct {
	Path      string `json:"path"`
	Delimiter string `json:"delimiter"`
	Encoding  string `json:"encoding"`
	Header    bool   `json:"header"` // 首行写字段名
	Append    bool   `json:"append"` // 追加而非覆盖
}

func init() {
	core.Steps.Register(core.Define(core.Definition[CSVOutputConfig]{
		Meta: core.Meta{
			ID: "csv_output", Order: 70, Label: "CSV 输出", Category: core.CategoryOutput, Icon: "file-export",
			Description: "把流写为定界文本文件（默认逗号分隔、含表头）。NULL 写为空串，数值/日期按字段格式输出。",
			Inputs:      []core.PortDecl{{Name: core.PortMain, Label: "输入", Required: true}},
			Outputs:     []core.PortDecl{{Name: core.PortMain, Label: "输出"}},
			Keywords:    []string{"Text file output", "csv", "文本文件输出", "导出"},
		},
		Defaults: func() CSVOutputConfig { return CSVOutputConfig{Delimiter: ",", Encoding: "utf-8", Header: true} },
		Infer: func(ic *core.InferContext, c *CSVOutputConfig) error {
			ic.SetOutput(core.PortMain, ic.Main())
			if strings.TrimSpace(c.Path) == "" {
				ic.Errorf("path", "path_required", "请填写文件路径")
			} else {
				checkFilePath(ic, c.Path, "path")
			}
			if _, err := csvDelim(c.Delimiter); err != nil {
				ic.Errorf("delimiter", "delimiter", "%s", err.Error())
			}
			return nil
		},
		Open: func(rc *core.RunContext, c *CSVOutputConfig) (core.Runner, error) {
			delim, err := csvDelim(c.Delimiter)
			if err != nil {
				return nil, err
			}
			return core.RunnerFunc(func(ctx context.Context) error {
				p, err := resolveFilePath(rc.Env, c.Path, true)
				if err != nil {
					return err
				}
				flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
				if c.Append {
					flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
				}
				f, err := os.OpenFile(p, flag, 0o644)
				if err != nil {
					return fmt.Errorf("打开文件失败：%w", err)
				}
				wr, err := encodeWriter(f, c.Encoding)
				if err != nil {
					f.Close()
					return err
				}
				// wr 均为 io.Closer（*os.File 或 transform.Writer，后者关闭时会冲刷并关闭底层文件）。
				closer := wr.(io.Closer)
				w := csv.NewWriter(wr)
				w.Comma = delim
				in := rc.In(core.PortMain)
				if c.Header && !c.Append {
					names := make([]string, in.Schema.Len())
					for i, f := range in.Schema.Fields() {
						names[i] = f.Name
					}
					if err := w.Write(names); err != nil {
						f.Close()
						return err
					}
				}
				rec := make([]string, in.Schema.Len())
				out := rc.Out(core.PortMain)
				err = in.Each(ctx, func(b core.Batch) error {
					for _, row := range b.Rows {
						for i := 0; i < in.Schema.Len(); i++ {
							rec[i] = core.FormatFieldValue(row[i], in.Schema.Field(i))
						}
						if err := w.Write(rec); err != nil {
							return err
						}
					}
					w.Flush()
					if err := w.Error(); err != nil {
						return err
					}
					return out.Send(ctx, b)
				})
				if cerr := closer.Close(); err == nil {
					err = cerr
				}
				if err != nil {
					return fmt.Errorf("写文件失败：%w", err)
				}
				return nil
			}), nil
		},
	}))
}
