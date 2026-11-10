// Package schedule 是数据集成的定时调度。
//
// robfig/cron/v3 只用其解析器（5 段 / 可选秒 / @daily 等描述符）；触发循环自研，
// 以完全掌控重叠（skip/queue/parallel）与错过（skip/run_once）语义，并用 CAS 防止重复触发。
package schedule

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"

	"mooncell/console/internal/etl/store"
)

var parser = cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Location 解析时区，空 = Console 本地时区。
func Location(tz string) (*time.Location, error) {
	if strings.TrimSpace(tz) == "" {
		return time.Local, nil
	}
	return time.LoadLocation(tz)
}

// Parse 校验 cron 表达式并返回其调度。
func Parse(spec, tz string) (cron.Schedule, *time.Location, error) {
	loc, err := Location(tz)
	if err != nil {
		return nil, nil, fmt.Errorf("时区无效：%w", err)
	}
	s, err := parser.Parse(strings.TrimSpace(spec))
	if err != nil {
		return nil, nil, fmt.Errorf("cron 表达式无效：%w", err)
	}
	return s, loc, nil
}

// NextN 返回 after 之后的 n 次触发时间。
func NextN(spec, tz string, after time.Time, n int) ([]time.Time, error) {
	s, loc, err := Parse(spec, tz)
	if err != nil {
		return nil, err
	}
	out := make([]time.Time, 0, n)
	t := after.In(loc)
	for i := 0; i < n; i++ {
		t = s.Next(t)
		if t.IsZero() {
			break
		}
		out = append(out, t)
	}
	return out, nil
}

// Next 返回 after 之后的下一次触发（Unix 毫秒；无下次返回 0）。
func Next(spec, tz string, after time.Time) (int64, error) {
	ts, err := NextN(spec, tz, after, 1)
	if err != nil || len(ts) == 0 {
		return 0, err
	}
	return ts[0].UnixMilli(), nil
}

// Describe 把常见 cron 表达式翻译成中文；复杂表达式返回通用描述。
func Describe(spec string) string {
	spec = strings.TrimSpace(spec)
	switch spec {
	case "@yearly", "@annually":
		return "每年 1 月 1 日 00:00"
	case "@monthly":
		return "每月 1 日 00:00"
	case "@weekly":
		return "每周日 00:00"
	case "@daily", "@midnight":
		return "每天 00:00"
	case "@hourly":
		return "每小时整点"
	}
	if strings.HasPrefix(spec, "@every ") {
		return "每隔 " + strings.TrimPrefix(spec, "@every ")
	}
	f := strings.Fields(spec)
	if len(f) == 6 { // 带秒：秒为 0 时按 5 段描述
		if f[0] != "0" {
			return "自定义（含秒）：" + spec
		}
		f = f[1:]
	}
	if len(f) != 5 {
		return spec
	}
	min, hour, dom, mon, dow := f[0], f[1], f[2], f[3], f[4]
	num := func(s string) (int, bool) { n, err := strconv.Atoi(s); return n, err == nil }
	at := func() string {
		h, ok1 := num(hour)
		m, ok2 := num(min)
		if ok1 && ok2 {
			return fmt.Sprintf("%02d:%02d", h, m)
		}
		return ""
	}
	weekdays := map[string]string{"0": "日", "1": "一", "2": "二", "3": "三", "4": "四", "5": "五", "6": "六", "7": "日",
		"SUN": "日", "MON": "一", "TUE": "二", "WED": "三", "THU": "四", "FRI": "五", "SAT": "六"}
	switch {
	case strings.HasPrefix(min, "*/") && hour == "*" && dom == "*" && mon == "*" && dow == "*":
		return "每 " + strings.TrimPrefix(min, "*/") + " 分钟"
	case min == "*" && hour == "*" && dom == "*" && mon == "*" && dow == "*":
		return "每分钟"
	case hour == "*" && dom == "*" && mon == "*" && dow == "*":
		if m, ok := num(min); ok {
			return fmt.Sprintf("每小时第 %d 分", m)
		}
	case strings.HasPrefix(hour, "*/") && dom == "*" && mon == "*" && dow == "*":
		if m, ok := num(min); ok {
			return fmt.Sprintf("每 %s 小时（第 %d 分）", strings.TrimPrefix(hour, "*/"), m)
		}
	case at() != "" && dom == "*" && mon == "*" && dow == "*":
		return "每天 " + at()
	case at() != "" && dom == "*" && mon == "*" && (dow == "1-5" || strings.EqualFold(dow, "MON-FRI")):
		return "工作日（周一至周五）" + at()
	case at() != "" && dom == "*" && mon == "*":
		var names []string
		for _, d := range strings.Split(dow, ",") {
			if w, ok := weekdays[strings.ToUpper(d)]; ok {
				names = append(names, "周"+w)
			} else {
				return "自定义：" + spec
			}
		}
		return "每" + strings.Join(names, "、") + " " + at()
	case at() != "" && mon == "*" && dow == "*":
		if dom == "L" {
			return "每月最后一天 " + at()
		}
		return "每月 " + dom + " 日 " + at()
	}
	return "自定义：" + spec
}

// ---------------------------------------------------------------- 触发循环

// FireFunc 由 service 实现：按调度创建一次运行。返回 error 只用于日志。
type FireFunc func(ctx context.Context, s store.Schedule, firedAt time.Time) error

// Scheduler 周期扫描到期调度并触发。单 Console 实例假设下用 CAS 防止同一时刻重复触发。
type Scheduler struct {
	st     *store.Store
	fire   FireFunc
	tick   time.Duration
	paused atomic.Bool

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// New 创建调度器。
func New(st *store.Store, fire FireFunc) *Scheduler {
	return &Scheduler{st: st, fire: fire, tick: 5 * time.Second, stop: make(chan struct{}), done: make(chan struct{})}
}

// Pause / Resume 用于 Console 自更新 draining：暂停期间到期的调度在恢复后按错过策略处理。
func (s *Scheduler) Pause()  { s.paused.Store(true) }
func (s *Scheduler) Resume() { s.paused.Store(false) }

// Start 启动循环：先处理停机期间错过的触发，再周期扫描。
func (s *Scheduler) Start(ctx context.Context) {
	go func() {
		defer close(s.done)
		s.recoverMisfires(ctx)
		t := time.NewTicker(s.tick)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if !s.paused.Load() {
					s.scan(ctx, time.Now())
				}
			case <-s.stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Stop 停止循环并等待退出。
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
	<-s.done
}

// recoverMisfires：Console 停机期间错过的触发——run_once 补跑一次，skip 直接跳到下一次。
func (s *Scheduler) recoverMisfires(ctx context.Context) {
	list, err := s.st.ListSchedules("")
	if err != nil {
		log.Printf("[etl] 读取调度失败：%v", err)
		return
	}
	now := time.Now()
	for _, sc := range list {
		if !sc.Enabled {
			continue
		}
		next, err := Next(sc.Cron, sc.Timezone, now)
		if err != nil {
			log.Printf("[etl] 调度 %s 的 cron 无效：%v", sc.ID, err)
			continue
		}
		missed := sc.NextFireAt > 0 && sc.NextFireAt < now.UnixMilli()
		if ok, _ := s.st.MarkFired(sc.ID, sc.NextFireAt, sc.LastFireAt, next); !ok {
			continue
		}
		if missed && sc.Misfire == "run_once" {
			log.Printf("[etl] 调度「%s」在停机期间错过触发，按策略补跑一次", sc.Name)
			if err := s.fire(ctx, sc, now); err != nil {
				log.Printf("[etl] 补跑失败：%v", err)
			}
		}
	}
}

func (s *Scheduler) scan(ctx context.Context, now time.Time) {
	list, err := s.st.ListSchedules("")
	if err != nil {
		log.Printf("[etl] 读取调度失败：%v", err)
		return
	}
	for _, sc := range list {
		if !sc.Enabled || sc.NextFireAt == 0 || sc.NextFireAt > now.UnixMilli() {
			continue
		}
		next, err := Next(sc.Cron, sc.Timezone, now)
		if err != nil {
			continue
		}
		ok, err := s.st.MarkFired(sc.ID, sc.NextFireAt, now.UnixMilli(), next)
		if err != nil || !ok {
			continue
		}
		if err := s.fire(ctx, sc, now); err != nil {
			log.Printf("[etl] 调度「%s」触发失败：%v", sc.Name, err)
		}
	}
}
