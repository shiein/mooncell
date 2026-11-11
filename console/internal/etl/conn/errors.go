package conn

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"net"

	"gitea.com/kingbase/gokb"
	dmdrv "gitee.com/chunanyong/dm"
	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	modernc "modernc.org/sqlite"
)

// IsDataError 判断错误是否属「数据类」：约束违反、类型/长度/取值错误——
// 这类错误只影响出错的行，表输出可退化为逐行写入把坏行转错误输出。
// 连接断开、超时、取消、语法/权限/资源等基础设施错误返回 false，应整步失败。
//
// 各驱动错误载体：
//   - postgres：*pgconn.PgError（SQLSTATE；22xxx=数据异常 23xxx=完整性约束）
//   - kingbase：*gokb.Error（同 SQLSTATE 模型，fork 自 lib/pq）
//   - mysql：*mysql.MySQLError.Number（无 SQLSTATE，按错误码白名单）
//   - dm：*dm.DmError.ErrCode（DM8 公开错误码，未识别码按基础设施处理——宁可整步失败也不静默转行）
//   - sqlite：*sqlite.Error.Code()（扩展码取低 8 位基础码）
func IsDataError(d Dialect, err error) bool {
	if err == nil {
		return false
	}
	// 取消/超时/网络/坏连接一律基础设施错误
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) || errors.Is(err, driver.ErrBadConn) {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return false
	}
	switch d.ID() {
	case "postgres":
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			return pgDataClass(pe.Code)
		}
	case "kingbase":
		var ke *gokb.Error
		if errors.As(err, &ke) {
			return pgDataClass(string(ke.Code))
		}
	case "mysql":
		var me *mysql.MySQLError
		if errors.As(err, &me) {
			return mysqlDataErrno(me.Number)
		}
	case "dm":
		var de *dmdrv.DmError
		if errors.As(err, &de) {
			return dmDataErrno(de.ErrCode)
		}
	case "sqlite":
		var se *modernc.Error
		if errors.As(err, &se) {
			return sqliteDataErrno(se.Code())
		}
	}
	return false
}

func pgDataClass(code string) bool {
	if len(code) < 2 {
		return false
	}
	return code[:2] == "22" || code[:2] == "23" // data_exception / integrity_constraint_violation
}

// mysqlDataErrno 是 MySQL 的数据类错误码（go-sql-driver 不暴露 SQLSTATE）。
func mysqlDataErrno(n uint16) bool {
	switch n {
	case 1048, // ER_BAD_NULL_ERROR：列不能为 NULL
		1062, // ER_DUP_ENTRY：唯一键冲突
		1264, // ER_WARN_DATA_OUT_OF_RANGE：超出取值范围
		1265, // WARN_DATA_TRUNCATED：截断
		1292, // ER_TRUNCATED_WRONG_VALUE：错误的日期/数值
		1366, // ER_TRUNCATED_WRONG_VALUE_FOR_FIELD：类型不匹配
		1406, // ER_DATA_TOO_LONG：数据过长
		1451, // ER_ROW_IS_REFERENCED_2：FK 引用不能删/改
		1452, // ER_NO_REFERENCED_ROW_2：FK 引用不存在
		1690, // ER_DATA_OUT_OF_RANGE（部分版本）
		3819, // ER_CHECK_CONSTRAINT_VIOLATED
		4025: // ER_CONSTRAINT_FAILED（表达式默认值约束）
		return true
	}
	return false
}

// dmDataErrno 是 DM8 的数据类错误码：负数为服务端错误，正数为驱动在客户端绑定转换时的错误。
// 未列出的码一律按基础设施处理——失败但可观测，优于静默吞错。
func dmDataErrno(n int32) bool {
	switch n {
	// 服务端（负数）——DM8 真机实测（conn/live_dataerr_test.go 维护）：
	case -6602, // 违反唯一性约束
		-6604, // 违反 CHECK 约束
		-6607, // 违反引用约束（外键）
		-6609, // 违反非空约束
		-6169, // 列长度超出定义
		-6149, // 数值溢出（字面量写入 NUMBER 超精度）
		-6118, // 日期时间格式错误（字面量）
		// 驱动端（正数，绑定参数在客户端转换时报出）：
		6092, // 数据大小已超过可支持范围（数值溢出）
		6007, // 类型转换异常
		6015: // 错误的日期时间类型格式
		return true
	}
	// 未实测的码一律按基础设施处理：宁可整步失败，也不把来路不明的错误当坏行吞掉。
	return false
}

// sqliteDataErrno 是 SQLite 的数据类基础错误码（扩展码取低 8 位）。
func sqliteDataErrno(code int) bool {
	switch code & 0xff {
	case 18, // SQLITE_TOOBIG：串/BLOB 超限
		19, // SQLITE_CONSTRAINT（含 2067 UNIQUE/787 FK/1299 NOTNULL/1544 PK/275 CHECK 等扩展码）
		20: // SQLITE_MISMATCH：类型不匹配
		return true
	}
	return false
}
