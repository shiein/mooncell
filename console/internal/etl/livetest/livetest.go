//go:build etl_live

// Package livetest 提供真机集成测试的公共辅助：连接配置来自环境变量。
//
// 用法：为每个被测方言设置一个 JSON 环境变量，缺省即跳过：
//
//	ETL_LIVE_MYSQL='{"host":"127.0.0.1","port":13306,"database":"etl","username":"root","password":"…"}'
//	ETL_LIVE_DM='{"host":"127.0.0.1","port":15236,"username":"SYSDBA","password":"…","defaultSchema":"SYSDBA"}'
//	ETL_LIVE_KINGBASE='{"host":"127.0.0.1","port":15432,"database":"TEST","username":"SYSTEM","password":"…"}'
//	ETL_LIVE_POSTGRES='{"host":"127.0.0.1","port":5432,"database":"postgres","username":"postgres","password":"…"}'
//
// 运行：cd console && GOOS=darwin GOARCH=arm64 go test -tags etl_live ./internal/etl/...
package livetest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"mooncell/console/internal/etl/conn"
	"mooncell/console/internal/etl/core"
)

// liveConfig 是环境变量 JSON 的形态：conn.Config 的全部字段 + 明文密码（仅测试进程内）。
type liveConfig struct {
	conn.Config
	Password string `json:"password"`
}

// Config 读取 ETL_LIVE_<DIALECT>；未设置时 t.Skip。返回连接配置与密码。
func Config(t *testing.T, dialectID string) (conn.Config, string) {
	t.Helper()
	raw := os.Getenv("ETL_LIVE_" + strings.ToUpper(dialectID))
	if raw == "" {
		t.Skipf("未设置 ETL_LIVE_%s，跳过真机测试", strings.ToUpper(dialectID))
	}
	var lc liveConfig
	if err := json.Unmarshal([]byte(raw), &lc); err != nil {
		t.Fatalf("ETL_LIVE_%s 不是合法 JSON：%v", strings.ToUpper(dialectID), err)
	}
	lc.Config.DBType = dialectID
	if lc.Config.ID == "" {
		lc.Config.ID = "live_" + dialectID
	}
	if lc.Config.Host == "" {
		lc.Config.Host = "127.0.0.1"
	}
	return lc.Config, lc.Password
}

// Open 打开一个非池化连接（自动 Ping）。失败即 Fatal。
func Open(t *testing.T, dialectID string) *conn.Handle {
	t.Helper()
	cfg, pw := Config(t, dialectID)
	h, err := conn.Open(context.Background(), cfg, pw)
	if err != nil {
		t.Fatalf("无法连接 %s：%v", dialectID, err)
	}
	t.Cleanup(func() { h.DB.Close() })
	return h
}

// Pool 建一个只识别给定方言连接的连接池（连接 ID = live_<dialect>）。
func Pool(t *testing.T, dialects ...string) *conn.Pool {
	t.Helper()
	cfgs := map[string]conn.Config{}
	pws := map[string]string{}
	for _, d := range dialects {
		cfg, pw := Config(t, d)
		cfgs[cfg.ID] = cfg
		pws[cfg.ID] = pw
	}
	pool := conn.NewPool(func(ctx context.Context, id string) (conn.Config, string, error) {
		cfg, ok := cfgs[id]
		if !ok {
			return conn.Config{}, "", fmt.Errorf("未知连接 %s", id)
		}
		return cfg, pws[id], nil
	})
	t.Cleanup(pool.Close)
	return pool
}

// Env 返回含连接池的 core.Env（供 pipeline/steps 测试用）。
func Env(t *testing.T, dialects ...string) core.Env {
	t.Helper()
	pool := Pool(t, dialects...)
	k, v := pool.EnvEntry()
	return core.MapEnv{k: v}
}

// ConnID 返回方言对应的连接 ID（"live_<dialect>"）。
func ConnID(dialectID string) string { return "live_" + dialectID }
