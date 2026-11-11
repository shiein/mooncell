package steps

import (
	"bufio"
	"bytes"
	"container/heap"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"mooncell/console/internal/etl/core"
)

// ---------------------------------------------------------------- 值比较

// compareValues 对逻辑值做全序比较：NULL 最小；同类型按自然序；
// 跨数值类型按数值比较（decimal 精确）；其余跨类型按固定类型序，
// 保证排序确定（Kettle 对混合类型键同样只要求确定性）。
func compareValues(a, b any) int {
	if a == nil {
		if b == nil {
			return 0
		}
		return -1
	}
	if b == nil {
		return 1
	}
	if isNumeric(a) && isNumeric(b) {
		return compareNumeric(a, b)
	}
	switch x := a.(type) {
	case bool:
		if y, ok := b.(bool); ok {
			if x == y {
				return 0
			}
			if !x {
				return -1
			}
			return 1
		}
	case string:
		if y, ok := b.(string); ok {
			return bytes.Compare([]byte(x), []byte(y))
		}
	case time.Time:
		if y, ok := b.(time.Time); ok {
			return x.Compare(y)
		}
	case []byte:
		if y, ok := b.([]byte); ok {
			return bytes.Compare(x, y)
		}
	}
	return kindRank(a) - kindRank(b)
}

func isNumeric(v any) bool {
	switch v.(type) {
	case int64, float64, decimal.Decimal:
		return true
	}
	return false
}

func compareNumeric(a, b any) int {
	// int64 与 int64/decimal 的组合精确比较：转 float64 会在 |x|>2^53 时丢精度，
	// 把相邻大整数（如雪花 ID）判为相等，进而被去重/合并吞行。
	if ia, ok := a.(int64); ok {
		switch y := b.(type) {
		case int64:
			return cmpInt(ia, y)
		case decimal.Decimal:
			return decimal.NewFromInt(ia).Cmp(y)
		}
	}
	if ib, ok := b.(int64); ok {
		if x, ok := a.(decimal.Decimal); ok {
			return x.Cmp(decimal.NewFromInt(ib))
		}
	}
	da, oka := a.(decimal.Decimal)
	db, okb := b.(decimal.Decimal)
	if oka && okb {
		return da.Cmp(db)
	}
	var fa, fb float64
	if oka {
		fa = da.InexactFloat64()
	} else {
		fa = toFloat(a)
	}
	if okb {
		fb = db.InexactFloat64()
	} else {
		fb = toFloat(b)
	}
	switch {
	case fa < fb:
		return -1
	case fa > fb:
		return 1
	}
	return 0
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	}
	return 0
}

func kindRank(v any) int {
	switch v.(type) {
	case bool:
		return 1
	case int64, float64, decimal.Decimal:
		return 2
	case string:
		return 3
	case time.Time:
		return 4
	case []byte:
		return 5
	}
	return 6
}

// rowLess 构造按键下标与方向的行比较器。
func rowLess(keys []int, desc []bool) func(a, b core.Row) int {
	return func(a, b core.Row) int {
		for i, k := range keys {
			if c := compareValues(a[k], b[k]); c != 0 {
				if desc[i] {
					return -c
				}
				return c
			}
		}
		return 0
	}
}

// ---------------------------------------------------------------- 排序步骤

// SortField 是一个排序键。
type SortField struct {
	Name string `json:"name"`
	Desc bool   `json:"desc"`
}

// SortRowsConfig 是「排序记录」：内存预算内全内存排序，超出溢写 TempDir 外部归并。
type SortRowsConfig struct {
	Fields []SortField `json:"fields"`
}

// spillChunkFloor 是单个溢写分段的最低行数下限（防止极小预算产生海量小文件）。
const spillChunkFloor = 4096

func init() {
	core.Steps.Register(core.Define(core.Definition[SortRowsConfig]{
		Meta: core.Meta{
			ID: "sort_rows", Order: 30, Label: "排序记录", Category: core.CategoryTransform, Icon: "sort",
			Stateful:    true,
			Description: "按指定字段排序整条流。超出内存预算时溢写临时文件做外部归并，行数不受内存限制。键值按类型自然序比较，NULL 最小（升序排前）。",
			Inputs:      []core.PortDecl{{Name: core.PortMain, Label: "输入", Required: true}},
			Outputs:     []core.PortDecl{{Name: core.PortMain, Label: "输出"}},
			Keywords:    []string{"Sort rows", "排序", "order by", "外部排序"},
		},
		Defaults: func() SortRowsConfig { return SortRowsConfig{} },
		Infer: func(ic *core.InferContext, c *SortRowsConfig) error {
			ic.SetOutput(core.PortMain, ic.Main())
			if len(c.Fields) == 0 {
				ic.Errorf("fields", "fields_required", "至少选择一个排序字段")
				return nil
			}
			for i, f := range c.Fields {
				if ic.Main().Index(f.Name) < 0 {
					ic.Errorf(fmt.Sprintf("fields[%d].name", i), "field_missing", "输入中没有字段 %q", f.Name)
				}
			}
			return nil
		},
		Open: func(rc *core.RunContext, c *SortRowsConfig) (core.Runner, error) {
			var keys []int
			var desc []bool
			for i, f := range c.Fields {
				idx := rc.In(core.PortMain).Schema.Index(f.Name)
				if idx < 0 {
					return nil, fmt.Errorf("排序字段 %q 不在输入中（配置项 fields[%d]）", f.Name, i)
				}
				keys = append(keys, idx)
				desc = append(desc, f.Desc)
			}
			s := &sorter{rc: rc, schema: rc.In(core.PortMain).Schema, less: rowLess(keys, desc)}
			return core.RunnerFunc(s.run), nil
		},
	}))
}

type sorter struct {
	rc     *core.RunContext
	schema *core.Schema
	less   func(a, b core.Row) int

	buf    []core.Row
	bufMem int64
	runs   []string
	dir    string
}

// rowMem 估算一行占用的堆内存（值大小 + 切片头/指针开销）。
func rowMem(r core.Row) int64 {
	var n int64 = 16 + int64(len(r))*16
	for _, v := range r {
		switch x := v.(type) {
		case string:
			n += int64(len(x)) + 16
		case []byte:
			n += int64(len(x)) + 24
		case decimal.Decimal:
			n += 96
		default:
			n += 16
		}
	}
	return n
}

func (s *sorter) run(ctx context.Context) error {
	defer s.cleanup()
	err := s.rc.In(core.PortMain).Each(ctx, func(b core.Batch) error {
		for _, r := range b.Rows {
			m := rowMem(r)
			if len(s.buf) > 0 && s.bufMem+m > s.budget() && len(s.buf) >= spillChunkFloor {
				if err := s.spill(); err != nil {
					return err
				}
			}
			s.buf = append(s.buf, r)
			s.bufMem += m
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.emit(ctx)
}

func (s *sorter) budget() int64 {
	if s.rc.MemoryBudget > 0 {
		return s.rc.MemoryBudget
	}
	return 512 << 20
}

func (s *sorter) cleanup() {
	if s.dir != "" {
		os.RemoveAll(s.dir)
	}
}

// spill 排序内存缓冲并落盘为一个分段文件。
func (s *sorter) spill() error {
	if s.dir == "" {
		d, err := s.rc.TempDir()
		if err != nil {
			return fmt.Errorf("创建排序临时目录失败：%w", err)
		}
		s.dir = d
	}
	sort.Slice(s.buf, func(i, j int) bool { return s.less(s.buf[i], s.buf[j]) < 0 })
	path := filepath.Join(s.dir, fmt.Sprintf("run-%04d.bin", len(s.runs)))
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	for _, r := range s.buf {
		if err := encodeRow(w, s.schema, r); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	s.runs = append(s.runs, path)
	s.buf, s.bufMem = nil, 0
	s.rc.Log.Debug("排序溢写分段 %s", path)
	return nil
}

// emit 归并全部分段与内存缓冲，按 BatchSize 输出。
func (s *sorter) emit(ctx context.Context) error {
	sort.Slice(s.buf, func(i, j int) bool { return s.less(s.buf[i], s.buf[j]) < 0 })
	out := s.rc.Out(core.PortMain)
	sendRows := func(rows []core.Row) error {
		for start := 0; start < len(rows); start += s.rc.BatchSize {
			end := min(start+s.rc.BatchSize, len(rows))
			if err := out.Send(ctx, core.Batch{Schema: out.Schema, Rows: rows[start:end]}); err != nil {
				return err
			}
		}
		return nil
	}
	if len(s.runs) == 0 {
		return sendRows(s.buf)
	}
	readers := make([]*runReader, 0, len(s.runs)+1)
	for _, p := range s.runs {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		readers = append(readers, &runReader{r: bufio.NewReaderSize(f, 1<<20), file: f, schema: s.schema})
	}
	readers = append(readers, &runReader{mem: s.buf, schema: s.schema})
	mh := mergeHeap{less: s.less}
	for _, rr := range readers {
		if err := rr.next(); err != nil {
			return err
		}
		if rr.cur != nil {
			mh.rs = append(mh.rs, rr)
		}
	}
	heap.Init(&mh)
	batch := make([]core.Row, 0, s.rc.BatchSize)
	for mh.Len() > 0 {
		rr := mh.rs[0]
		batch = append(batch, rr.cur)
		if err := rr.next(); err != nil {
			return err
		}
		if rr.cur == nil {
			heap.Pop(&mh)
		} else {
			heap.Fix(&mh, 0)
		}
		if len(batch) == s.rc.BatchSize {
			if err := out.Send(ctx, core.Batch{Schema: out.Schema, Rows: batch}); err != nil {
				return err
			}
			batch = make([]core.Row, 0, s.rc.BatchSize)
		}
	}
	if len(batch) > 0 {
		return out.Send(ctx, core.Batch{Schema: out.Schema, Rows: batch})
	}
	return nil
}

// runReader 从一个分段文件或内存切片按序产出行。
type runReader struct {
	r      *bufio.Reader
	file   *os.File
	mem    []core.Row
	schema *core.Schema
	cur    core.Row
}

func (rr *runReader) next() error {
	if rr.mem != nil {
		if len(rr.mem) == 0 {
			rr.cur = nil
			return nil
		}
		rr.cur = rr.mem[0]
		rr.mem = rr.mem[1:]
		return nil
	}
	row, err := decodeRow(rr.r, rr.schema)
	if err == io.EOF {
		rr.cur = nil
		return nil
	}
	if err != nil {
		return err
	}
	rr.cur = row
	return nil
}

// mergeHeap 按行序最小堆组织各分段读取器。
type mergeHeap struct {
	rs   []*runReader
	less func(a, b core.Row) int
}

func (h *mergeHeap) Len() int { return len(h.rs) }
func (h *mergeHeap) Less(i, j int) bool {
	return h.less(h.rs[i].cur, h.rs[j].cur) < 0
}
func (h *mergeHeap) Swap(i, j int) { h.rs[i], h.rs[j] = h.rs[j], h.rs[i] }
func (h *mergeHeap) Push(x any)    { h.rs = append(h.rs, x.(*runReader)) }
func (h *mergeHeap) Pop() (v any)  { v = h.rs[len(h.rs)-1]; h.rs = h.rs[:len(h.rs)-1]; return v }

// ---------------------------------------------------------------- 行编码

// encodeRow 按 Schema 类型把一行写为自描述二进制流（首字节 NULL 位图）。
// 仅用于排序溢写分段——同 Schema 内逐字段定长/变长写入，无类型标签。
func encodeRow(w *bufio.Writer, s *core.Schema, r core.Row) error {
	var nulls []byte
	if s.Len()%8 == 0 {
		nulls = make([]byte, s.Len()/8)
	} else {
		nulls = make([]byte, s.Len()/8+1)
	}
	var raw bytes.Buffer
	for i := 0; i < s.Len(); i++ {
		if r[i] == nil {
			nulls[i/8] |= 1 << (i % 8)
			continue
		}
		if err := encodeValue(&raw, s.Field(i).Kind, r[i]); err != nil {
			return err
		}
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(len(nulls)+raw.Len())); err != nil {
		return err
	}
	if _, err := w.Write(nulls); err != nil {
		return err
	}
	_, err := w.Write(raw.Bytes())
	return err
}

func decodeRow(r *bufio.Reader, s *core.Schema) (core.Row, error) {
	var sz uint32
	if err := binary.Read(r, binary.LittleEndian, &sz); err != nil {
		return nil, err
	}
	nb := s.Len()/8 + 1
	if s.Len()%8 == 0 {
		nb = s.Len() / 8
	}
	nulls := make([]byte, nb)
	if _, err := io.ReadFull(r, nulls); err != nil {
		return nil, err
	}
	row := make(core.Row, s.Len())
	for i := 0; i < s.Len(); i++ {
		if nulls[i/8]&(1<<(i%8)) != 0 {
			row[i] = nil
			continue
		}
		v, err := decodeValue(r, s.Field(i).Kind)
		if err != nil {
			return nil, err
		}
		row[i] = v
	}
	return row, nil
}

func encodeValue(w io.Writer, k core.Kind, v any) error {
	var b [16]byte
	switch k {
	case core.KindBoolean:
		if v.(bool) {
			b[0] = 1
		}
		_, err := w.Write(b[:1])
		return err
	case core.KindInteger:
		n := binary.PutVarint(b[:], v.(int64))
		_, err := w.Write(b[:n])
		return err
	case core.KindNumber:
		binary.LittleEndian.PutUint64(b[:8], math.Float64bits(v.(float64)))
		_, err := w.Write(b[:8])
		return err
	case core.KindDate, core.KindTimestamp:
		// 秒+纳秒+时区偏移秒三段：UnixNano 对远古时间会溢出，且无时区列的墙上时钟须保留
		t := v.(time.Time)
		_, off := t.Zone()
		n := binary.PutVarint(b[:], t.Unix())
		n += binary.PutVarint(b[n:], int64(t.Nanosecond()))
		n += binary.PutVarint(b[n:], int64(off))
		_, err := w.Write(b[:n])
		return err
	case core.KindDecimal:
		s := v.(decimal.Decimal).String()
		return writeBytes(w, []byte(s))
	case core.KindString:
		return writeBytes(w, []byte(v.(string)))
	case core.KindBinary:
		return writeBytes(w, v.([]byte))
	}
	return fmt.Errorf("字段类型 %s 不支持排序溢写", k)
}

func writeBytes(w io.Writer, data []byte) error {
	var b [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(b[:], uint64(len(data)))
	if _, err := w.Write(b[:n]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

func decodeValue(r *bufio.Reader, k core.Kind) (any, error) {
	switch k {
	case core.KindBoolean:
		c, err := r.ReadByte()
		return c == 1, err
	case core.KindInteger:
		return binary.ReadVarint(r)
	case core.KindNumber:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(b[:])), nil
	case core.KindDate, core.KindTimestamp:
		sec, err := binary.ReadVarint(r)
		if err != nil {
			return nil, err
		}
		ns, err := binary.ReadVarint(r)
		if err != nil {
			return nil, err
		}
		off, err := binary.ReadVarint(r)
		if err != nil {
			return nil, err
		}
		return time.Unix(sec, ns).In(time.FixedZone("", int(off))), nil
	case core.KindDecimal:
		n, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, err
		}
		data := make([]byte, n)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, err
		}
		return decimal.NewFromString(string(data))
	case core.KindString, core.KindBinary:
		n, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, err
		}
		data := make([]byte, n)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, err
		}
		if k == core.KindString {
			return string(data), nil
		}
		return data, nil
	}
	return nil, fmt.Errorf("字段类型 %s 不支持排序溢写", k)
}
