package conn

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "gitea.com/kingbase/gokb"        // "kingbase"
	_ "gitee.com/chunanyong/dm"        // "dm"
	_ "github.com/go-sql-driver/mysql" // "mysql"
	_ "github.com/jackc/pgx/v5/stdlib" // "pgx"
	_ "modernc.org/sqlite"             // "sqlite"（仅测试方言）

	"mooncell/console/internal/etl/core"
)

// Resolver 按连接 ID 取配置与明文密码（由 service 从 store + secret 解出）。
type Resolver func(ctx context.Context, id string) (Config, string, error)

// Handle 是一个可用连接：配置 + 方言 + 连接池。调用方不得 Close DB。
type Handle struct {
	Config  Config
	Dialect Dialect
	DB      *sql.DB
}

// Location 返回连接时区。
func (h *Handle) Location() *time.Location { return h.Config.Options.Location() }

// Pool 按连接 ID 缓存 *sql.DB。连接配置变更/删除时由 service 调 Invalidate。
type Pool struct {
	resolve Resolver
	mu      sync.Mutex
	dbs     map[string]*Handle
}

// NewPool 创建连接池。
func NewPool(resolve Resolver) *Pool { return &Pool{resolve: resolve, dbs: map[string]*Handle{}} }

// Get 返回连接（首次使用时打开并 Ping）。
func (p *Pool) Get(ctx context.Context, id string) (*Handle, error) {
	p.mu.Lock()
	if h, ok := p.dbs[id]; ok {
		p.mu.Unlock()
		return h, nil
	}
	p.mu.Unlock()

	cfg, password, err := p.resolve(ctx, id)
	if err != nil {
		return nil, err
	}
	h, err := Open(ctx, cfg, password)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if old, ok := p.dbs[id]; ok { // 并发首次打开：保留先到者
		h.DB.Close()
		return old, nil
	}
	p.dbs[id] = h
	return h, nil
}

// Invalidate 关闭并移除某连接（配置修改/删除后调用）。在途查询由 database/sql 自行收尾。
func (p *Pool) Invalidate(id string) {
	p.mu.Lock()
	h := p.dbs[id]
	delete(p.dbs, id)
	p.mu.Unlock()
	if h != nil {
		h.DB.Close()
	}
}

// Close 关闭全部连接。
func (p *Pool) Close() {
	p.mu.Lock()
	dbs := p.dbs
	p.dbs = map[string]*Handle{}
	p.mu.Unlock()
	for _, h := range dbs {
		h.DB.Close()
	}
}

// Open 按配置打开并验证连接（不入池，测试连接也用它）。
func Open(ctx context.Context, cfg Config, password string) (*Handle, error) {
	d, ok := Get(cfg.DBType)
	if !ok {
		return nil, fmt.Errorf("不支持的数据库类型 %q", cfg.DBType)
	}
	dsn, err := d.DSN(cfg, password)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(d.DriverName(), dsn)
	if err != nil {
		return nil, errors.New("打开连接失败") // 不回显 DSN
	}
	maxOpen := cfg.Options.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = 8
	}
	if d.ID() == "sqlite" {
		maxOpen = 1
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(min(2, maxOpen))
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
	pctx, cancel := context.WithTimeout(ctx, cfg.Options.connectTimeout())
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		db.Close()
		return nil, SanitizeError(err, password)
	}
	return &Handle{Config: cfg, Dialect: d, DB: db}, nil
}

// TestResult 是连接测试结果。
type TestResult struct {
	OK        bool   `json:"ok"`
	Version   string `json:"version,omitempty"`
	LatencyMs int64  `json:"latencyMs"`
	Error     string `json:"error,omitempty"`
}

// Test 打开连接、查询版本后关闭。
func Test(ctx context.Context, cfg Config, password string) TestResult {
	start := time.Now()
	h, err := Open(ctx, cfg, password)
	if err != nil {
		return TestResult{Error: err.Error(), LatencyMs: time.Since(start).Milliseconds()}
	}
	defer h.DB.Close()
	var version string
	if err := h.DB.QueryRowContext(ctx, h.Dialect.VersionSQL()).Scan(&version); err != nil {
		return TestResult{Error: SanitizeError(err, password).Error(), LatencyMs: time.Since(start).Milliseconds()}
	}
	return TestResult{OK: true, Version: strings.TrimSpace(version), LatencyMs: time.Since(start).Milliseconds()}
}

// SanitizeError 去掉错误文本中可能回显的密码。
func SanitizeError(err error, password string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if password != "" {
		msg = strings.ReplaceAll(msg, password, "******")
	}
	return errors.New(msg)
}

// ---------------------------------------------------------------- Env 接入

type poolKey struct{}

// EnvEntry 返回注入 core.Env 的键值。
func (p *Pool) EnvEntry() (any, any) { return poolKey{}, p }

// PoolFrom 从步骤上下文的 Env 取连接池。
func PoolFrom(env core.Env) (*Pool, error) {
	if env == nil {
		return nil, errors.New("运行环境未注入连接池")
	}
	p, ok := env.Lookup(poolKey{}).(*Pool)
	if !ok || p == nil {
		return nil, errors.New("运行环境未注入连接池")
	}
	return p, nil
}

// ---------------------------------------------------------------- 设计期探测缓存

type probeEntry struct {
	schema *core.Schema
	err    error
	at     time.Time
}

const probeTTL = 30 * time.Second

var (
	probeMu    sync.Mutex
	probeCache = map[string]probeEntry{}
)

// Probe 推导查询结果字段，带 30s 缓存：设计器每次编辑都会触发校验，避免反复打数据库。
func (p *Pool) Probe(ctx context.Context, id, query string, args []any) (*core.Schema, error) {
	key := id + "\x00" + query + "\x00" + fmt.Sprint(args...)
	probeMu.Lock()
	if e, ok := probeCache[key]; ok && time.Since(e.at) < probeTTL {
		probeMu.Unlock()
		return e.schema, e.err
	}
	probeMu.Unlock()
	h, err := p.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	s, err := QuerySchema(pctx, h, query, args)
	probeMu.Lock()
	if len(probeCache) > 512 { // 简单上限，防止无界增长
		probeCache = map[string]probeEntry{}
	}
	probeCache[key] = probeEntry{schema: s, err: err, at: time.Now()}
	probeMu.Unlock()
	return s, err
}

// ForgetProbes 清空探测缓存（连接配置变更后调用）。
func ForgetProbes() {
	probeMu.Lock()
	probeCache = map[string]probeEntry{}
	probeMu.Unlock()
}
