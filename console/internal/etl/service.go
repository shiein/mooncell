package etl

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "mooncell/console/internal/etl/actions" // 注册内置作业项
	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/schedule"
	"mooncell/console/internal/etl/secret"
	_ "mooncell/console/internal/etl/steps" // 注册内置步骤
	"mooncell/console/internal/etl/store"
)

// Config 对应 config.toml 的 [etl]。
type Config struct {
	Enabled           bool
	KeyFile           string // 默认：数据库同目录 mooncell-etl.key
	MaxConcurrentRuns int    // 默认 4
	RunLogLimit       int    // 每次运行落库日志上限，默认 5000
	RetentionDays     int    // 运行记录保留天数，默认 30
	KeepPerDefinition int    // 每个定义至少/至多保留的运行数，默认 200
	TempDir           string // 有状态步骤溢写目录，默认系统临时目录
	// FileRoot：csv_input/csv_output 等文件步骤的相对路径根目录（R1-B1），
	// 默认 <数据库所在目录>/etl-files。文件步骤的 path 只能落在该目录内，对管理员同样生效。
	FileRoot string
	// HTTPAllowLoopback：http_input/http/notify 是否允许访问回环地址（R1-m3）。
	// 默认 false：回环、链路本地、未指定地址在拨号层拒绝，防 SSRF 打本机服务。
	HTTPAllowLoopback bool
}

func (c Config) normalized(dbPath string) Config {
	dir := "."
	if dbPath != "" {
		dir = filepath.Dir(strings.SplitN(dbPath, "?", 2)[0])
	}
	if c.KeyFile == "" {
		c.KeyFile = filepath.Join(dir, "mooncell-etl.key")
	}
	if c.FileRoot == "" {
		c.FileRoot = filepath.Join(dir, "etl-files")
	}
	if c.MaxConcurrentRuns <= 0 {
		c.MaxConcurrentRuns = 4
	}
	if c.RunLogLimit <= 0 {
		c.RunLogLimit = 5000
	}
	if c.RetentionDays <= 0 {
		c.RetentionDays = 30
	}
	if c.KeepPerDefinition <= 0 {
		c.KeepPerDefinition = 200
	}
	return c
}

// AuditFunc 写入 Console 审计。
type AuditFunc func(user, action, target, result string)

// Service 组装数据集成模块：存储、凭据、连接池、运行管理、调度。
type Service struct {
	cfg   Config
	st    *store.Store
	box   *secret.Box
	pool  *conn.Pool
	sched *schedule.Scheduler
	runs  *runManager
	audit AuditFunc

	ctx    context.Context
	cancel context.CancelFunc

	drainMu  sync.Mutex
	draining bool
}

// Migrate 建表；由 Console 启动时无条件调用（功能开关只控制路由与后台任务）。
func Migrate(db *sql.DB) error { return store.Migrate(db) }

// New 创建服务：加载/生成密钥，标记遗留运行为 interrupted。
func New(db *sql.DB, dbPath string, cfg Config) (*Service, error) {
	cfg = cfg.normalized(dbPath)
	box, created, err := secret.LoadOrCreate(cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("加载数据集成密钥失败（%s）：%w", cfg.KeyFile, err)
	}
	if created {
		log.Printf("[etl] 已生成凭据密钥 %s —— 请与数据库一起备份，丢失后已保存的连接密码将无法解密", cfg.KeyFile)
	}
	// 文件根：csv_input/csv_output 的相对路径根，限定文件步骤只能访问该目录内（R1-B1）。
	if err := os.MkdirAll(cfg.FileRoot, 0o750); err != nil {
		return nil, fmt.Errorf("ETL 文件根目录创建失败（%s）：%w", cfg.FileRoot, err)
	}
	if err := os.Chmod(cfg.FileRoot, 0o750); err != nil {
		return nil, fmt.Errorf("ETL 文件根目录权限设置失败（%s）：%w", cfg.FileRoot, err)
	}
	root, err := filepath.EvalSymlinks(cfg.FileRoot)
	if err != nil {
		return nil, fmt.Errorf("ETL 文件根目录不可用（%s）：%w", cfg.FileRoot, err)
	}
	cfg.FileRoot = root
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{cfg: cfg, st: store.New(db), box: box, ctx: ctx, cancel: cancel, audit: func(string, string, string, string) {}}
	s.pool = conn.NewPool(s.resolveConnection)
	s.runs = newRunManager(s)
	if n, err := s.st.MarkInterrupted(); err != nil {
		log.Printf("[etl] 标记中断运行失败：%v", err)
	} else if n > 0 {
		log.Printf("[etl] %d 个运行因 Console 重启被标记为中断，可在运行记录中「从失败处继续」", n)
	}
	s.sched = schedule.New(s.st, s.fireSchedule)
	return s, nil
}

// SetAuditFunc 注入审计回调。
func (s *Service) SetAuditFunc(f AuditFunc) {
	if f != nil {
		s.audit = f
	}
}

// Start 启动调度器与保留裁剪。
func (s *Service) Start() {
	s.sched.Start(s.ctx)
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			cutoff := time.Now().AddDate(0, 0, -s.cfg.RetentionDays).UnixMilli()
			if n, err := s.st.TrimRuns(cutoff, s.cfg.KeepPerDefinition); err != nil {
				log.Printf("[etl] 裁剪运行记录失败：%v", err)
			} else if n > 0 {
				log.Printf("[etl] 已清理 %d 条过期运行记录", n)
			}
			select {
			case <-t.C:
			case <-s.ctx.Done():
				return
			}
		}
	}()
}

// Close 停止调度、取消在途运行、关闭连接池。
func (s *Service) Close() {
	s.sched.Stop()
	s.runs.cancelAll("Console 正在关闭")
	s.cancel()
	s.pool.Close()
}

// ErrBusy 表示有运行中的任务，拒绝 drain。
var ErrBusy = errors.New("数据集成有任务正在运行")

// BeginDrain 供 Console 自更新调用：暂停调度；有在途运行则恢复并返回 ErrBusy（附任务名）。
func (s *Service) BeginDrain() error {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	s.sched.Pause()
	s.draining = true
	if names := s.runs.activeNames(); len(names) > 0 {
		s.sched.Resume()
		s.draining = false
		return fmt.Errorf("%w：%s。请等待完成或取消后再升级", ErrBusy, strings.Join(names, "、"))
	}
	return nil
}

// EndDrain 自更新失败/放弃时恢复。
func (s *Service) EndDrain() {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	s.draining = false
	s.sched.Resume()
}

func (s *Service) isDraining() bool {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	return s.draining
}

// resolveConnection 是连接池的配置/密码来源。
func (s *Service) resolveConnection(ctx context.Context, id string) (conn.Config, string, error) {
	c, err := s.st.GetConnection(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return conn.Config{}, "", fmt.Errorf("连接 %s 不存在", id)
		}
		return conn.Config{}, "", err
	}
	pw, err := s.box.Open(c.SecretCipher, c.ID)
	if err != nil {
		return conn.Config{}, "", fmt.Errorf("连接「%s」：%w", c.Name, err)
	}
	return c.Config, pw, nil
}

// RegisterRoutes 注册 /api/etl/*。wrap 负责鉴权并用 WithUser 注入用户名与角色；
// 定义级 use/edit 授权在各 handler 内逐路由校验。
func (s *Service) RegisterRoutes(mux *http.ServeMux, wrap func(http.HandlerFunc) http.HandlerFunc) {
	h := &handlers{s: s}
	r := func(pattern string, fn http.HandlerFunc) { mux.HandleFunc(pattern, wrap(fn)) }

	r("GET /api/etl/overview", h.overview)
	r("GET /api/etl/catalog", h.catalog)

	r("GET /api/etl/connections", h.listConnections)
	r("POST /api/etl/connections", h.saveConnection)
	r("POST /api/etl/connections/test", h.testDraftConnection)
	r("GET /api/etl/connections/{id}", h.getConnection)
	r("PUT /api/etl/connections/{id}", h.saveConnection)
	r("DELETE /api/etl/connections/{id}", h.deleteConnection)
	r("POST /api/etl/connections/{id}/test", h.testConnection)
	r("GET /api/etl/connections/{id}/schemas", h.listSchemas)
	r("GET /api/etl/connections/{id}/tables", h.listTables)
	r("GET /api/etl/connections/{id}/columns", h.listColumns)

	r("GET /api/etl/definitions", h.listDefinitions)
	r("POST /api/etl/definitions", h.createDefinition)
	r("POST /api/etl/definitions/validate", h.validate)
	r("GET /api/etl/definitions/{id}", h.getDefinition)
	r("PUT /api/etl/definitions/{id}", h.updateDefinition)
	r("DELETE /api/etl/definitions/{id}", h.deleteDefinition)
	r("POST /api/etl/definitions/{id}/duplicate", h.duplicateDefinition)
	r("GET /api/etl/definitions/{id}/export", h.exportDefinition)
	r("POST /api/etl/definitions/import/preview", h.importPreview)
	r("POST /api/etl/definitions/import", h.importDefinition)
	r("GET /api/etl/definitions/{id}/versions", h.listVersions)
	r("GET /api/etl/definitions/{id}/versions/{rev}", h.getVersion)
	r("POST /api/etl/definitions/{id}/versions/{rev}/restore", h.restoreVersion)
	r("POST /api/etl/definitions/{id}/run", h.runDefinition)
	r("POST /api/etl/pipelines/preview", h.preview)

	r("GET /api/etl/runs", h.listRuns)
	r("GET /api/etl/runs/{id}", h.getRun)
	r("GET /api/etl/runs/{id}/logs", h.runLogs)
	r("GET /api/etl/runs/{id}/events", h.runEvents)
	r("POST /api/etl/runs/{id}/cancel", h.cancelRun)
	r("POST /api/etl/runs/{id}/resume", h.resumeRun)

	r("GET /api/etl/schedules", h.listSchedules)
	r("POST /api/etl/schedules", h.saveSchedule)
	r("GET /api/etl/schedules/preview", h.previewSchedule)
	r("PUT /api/etl/schedules/{id}", h.saveSchedule)
	r("DELETE /api/etl/schedules/{id}", h.deleteSchedule)
}

// ---------------------------------------------------------------- 用户上下文

type userKey struct{}

type ctxUser struct {
	name string
	role string
}

// WithUser 把当前用户名与角色注入请求上下文（由 Console 鉴权 wrapper 调用）。
func WithUser(r *http.Request, username, role string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), userKey{}, ctxUser{name: username, role: role}))
}

func userOf(r *http.Request) string {
	u, _ := r.Context().Value(userKey{}).(ctxUser)
	return u.name
}

func isAdmin(r *http.Request) bool {
	u, _ := r.Context().Value(userKey{}).(ctxUser)
	return u.role == "admin"
}
