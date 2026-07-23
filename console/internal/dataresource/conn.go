// 连接测试：验证外部数据库可达性、版本、只读事务支持。
//
// 设计文档第三节：连接测试超时 10 秒，返回
//
//	ok, latencyMs, serverVersion, currentDatabase, readOnlyTxSupported, errorCode
package dataresource

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// fmtError import placeholder removed

// 最近连接测试状态（写入 data_resources.last_test_status）。
// 设计：只读事务认证未通过时不得向普通用户授予 read。
const (
	TestStatusOK     = "ok"       // 连通且只读事务可用
	TestStatusOKNoRO = "ok_no_ro" // 连通但只读事务不可用
	TestStatusFail   = "fail"     // 连接失败
)

// TestResult 是连接测试的返回结构。
type TestResult struct {
	OK                  bool   `json:"ok"`
	LatencyMs           int64  `json:"latencyMs"`
	ServerVersion       string `json:"serverVersion"`
	CurrentDatabase     string `json:"currentDatabase"`
	ReadOnlyTxSupported bool   `json:"readOnlyTxSupported"`
	ErrorCode           string `json:"errorCode,omitempty"`
}

// PersistStatus 返回应写入 last_test_status 的值。
func (r TestResult) PersistStatus() string {
	if !r.OK {
		return TestStatusFail
	}
	if r.ReadOnlyTxSupported {
		return TestStatusOK
	}
	return TestStatusOKNoRO
}

// SupportsReadGrant 是否允许对该资源授予普通用户 read（需最近测试通过只读事务）。
func SupportsReadGrant(lastTestStatus string) bool {
	return lastTestStatus == TestStatusOK
}

// testTimeout 连接测试超时时间。
const testTimeout = 10 * time.Second

// TestConnection 测试给定资源配置的连接。password 为明文密码。
// 不使用连接池：临时打开 *sql.DB，测试后关闭。
func TestConnection(r DataResource, password string) TestResult {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	driverName := DriverName(r.DBType)
	dsn := BuildDSN(r, password)
	if dsn == "" {
		return TestResult{ErrorCode: "UNSUPPORTED_DB_TYPE"}
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return TestResult{ErrorCode: "OPEN_FAILED", LatencyMs: time.Since(start).Milliseconds()}
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(testTimeout)

	// Ping
	if err := db.PingContext(ctx); err != nil {
		return TestResult{
			LatencyMs: time.Since(start).Milliseconds(),
			ErrorCode: classifyConnError(err),
		}
	}

	// 查询版本和当前数据库
	version, currentDB := queryServerInfo(ctx, db, r.DBType)

	// 测试只读事务支持：只查询当前事务/会话状态，不执行任何可能落库的写探针。
	roSupported := testReadOnlyTx(ctx, db, r.DBType)

	return TestResult{
		OK:                  true,
		LatencyMs:           time.Since(start).Milliseconds(),
		ServerVersion:       version,
		CurrentDatabase:     currentDB,
		ReadOnlyTxSupported: roSupported,
	}
}

// queryServerInfo 查询数据库版本和当前数据库名。失败时返回空字符串，不阻断测试结果。
func queryServerInfo(ctx context.Context, db *sql.DB, dbType string) (version, currentDB string) {
	versionSQL, currentDBSQL := serverInfoSQL(dbType)
	if versionSQL != "" {
		var v string
		if err := db.QueryRowContext(ctx, versionSQL).Scan(&v); err == nil {
			version = v
		}
	}
	if currentDBSQL != "" {
		var c string
		if err := db.QueryRowContext(ctx, currentDBSQL).Scan(&c); err == nil {
			currentDB = c
		}
	}
	return
}

// serverInfoSQL 返回各数据库的版本查询和当前数据库查询 SQL。
func serverInfoSQL(dbType string) (versionSQL, currentDBSQL string) {
	switch dbType {
	case DriverPostgreSQL, DriverKingbase:
		return "SELECT version()", "SELECT current_database()"
	case DriverMySQL:
		return "SELECT VERSION()", "SELECT DATABASE()"
	case DriverDM:
		return "SELECT * FROM v$version WHERE rownum = 1", "SELECT name FROM v$database WHERE rownum = 1"
	}
	return "", ""
}

// testReadOnlyTx 测试数据库是否真正进入只读事务/会话。
// 不使用 DDL/DML 探针：权限错误、语法错误不能证明只读生效，且部分数据库 DDL 可能隐式提交。
func testReadOnlyTx(ctx context.Context, db *sql.DB, dbType string) bool {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false
	}
	defer tx.Rollback()

	switch dbType {
	case DriverPostgreSQL, DriverKingbase:
		var state string
		if err := tx.QueryRowContext(ctx, "SHOW transaction_read_only").Scan(&state); err != nil {
			return false
		}
		return isReadOnlyState(state)
	case DriverMySQL:
		var state any
		err := tx.QueryRowContext(ctx, "SELECT @@transaction_read_only").Scan(&state)
		if err != nil {
			// MySQL 5.7/部分兼容实现使用旧变量名。
			err = tx.QueryRowContext(ctx, "SELECT @@tx_read_only").Scan(&state)
		}
		return err == nil && isReadOnlyState(state)
	case DriverDM:
		// DM 官方 V$SESSIONS.RDONLY 表示当前会话是否只读；SESSID() 返回当前连接 ID。
		var state string
		if err := tx.QueryRowContext(ctx,
			"SELECT RDONLY FROM V$SESSIONS WHERE SESS_ID = SESSID()").Scan(&state); err != nil {
			return false
		}
		return isReadOnlyState(state)
	default:
		return false
	}
}

func isReadOnlyState(v any) bool {
	switch value := v.(type) {
	case bool:
		return value
	case int64:
		return value == 1
	case int:
		return value == 1
	case uint64:
		return value == 1
	case uint:
		return value == 1
	case []byte:
		return isReadOnlyState(string(value))
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "on", "true", "yes", "y":
			return true
		}
	}
	return false
}

// classifyConnError 将底层连接错误归类为稳定的错误码（不含敏感信息）。
func classifyConnError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "context deadline exceeded") || strings.Contains(msg, "i/o timeout"):
		return "CONN_TIMEOUT"
	case strings.Contains(msg, "authentication") || strings.Contains(msg, "auth") || strings.Contains(msg, "password") || strings.Contains(msg, "login"):
		return "AUTH_FAILED"
	case strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host") || strings.Contains(msg, "network is unreachable"):
		return "CONN_REFUSED"
	case strings.Contains(msg, "tls") || strings.Contains(msg, "ssl") || strings.Contains(msg, "certificate"):
		return "TLS_ERROR"
	default:
		return "CONN_ERROR"
	}
}

// ErrorDescription 返回错误码的可读描述（供 API 直接返回）。
func ErrorDescription(code string) string {
	switch code {
	case "CONN_TIMEOUT":
		return "连接超时"
	case "AUTH_FAILED":
		return "认证失败：用户名或密码错误"
	case "CONN_REFUSED":
		return "无法连接：主机不可达或端口未开放"
	case "TLS_ERROR":
		return "TLS/SSL 错误"
	case "UNSUPPORTED_DB_TYPE":
		return "不支持的数据库类型"
	case "OPEN_FAILED":
		return "驱动初始化失败"
	default:
		return "连接失败"
	}
}
