// 稳定错误码与 HTTP 映射。
// 只返回归类后的安全信息；底层 SSH/SFTP 原文仅写服务端 debug 日志（且须清除密码）。
package serverops

import (
	"encoding/json"
	"net/http"
)

// APIError 是统一错误响应：code 供前端分支，message 供展示。
type APIError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	// 分块 offset 冲突时附带权威 nextOffset（字节）。
	NextOffset *int64 `json:"nextOffset,omitempty"`
}

func (e *APIError) Error() string { return e.Message }

// 稳定错误码（设计文档 §17）。
const (
	CodeUnauthorized         = "UNAUTHORIZED"
	CodeForbidden            = "FORBIDDEN"
	CodeBadRequest           = "BAD_REQUEST"
	CodeValidation           = "VALIDATION_ERROR"
	CodeNotFound             = "SERVER_RESOURCE_NOT_FOUND"
	CodeNameDuplicate        = "NAME_DUPLICATE"
	CodeResourceChanged      = "RESOURCE_CHANGED"
	CodeChunkOffsetMismatch  = "CHUNK_OFFSET_MISMATCH"
	CodeRemoteTargetExists   = "REMOTE_TARGET_EXISTS"
	CodeHostKeyMismatch      = "HOST_KEY_MISMATCH"
	CodeHostKeyUnconfirmed   = "HOST_KEY_UNCONFIRMED"
	CodeTransferTooLarge     = "TRANSFER_TOO_LARGE"
	CodeSSHAuthFailed        = "SSH_AUTH_FAILED"
	CodeRemotePartChanged    = "REMOTE_PART_CHANGED"
	CodeAtomicReplaceUnsupp  = "ATOMIC_REPLACE_UNSUPPORTED"
	CodeSessionLimit         = "SESSION_LIMIT_REACHED"
	CodeTransferLimit        = "TRANSFER_LIMIT_REACHED"
	CodeSSHAuthRateLimited   = "SSH_AUTH_RATE_LIMITED"
	CodeSSHConnectionFailed  = "SSH_CONNECTION_FAILED"
	CodeSSHConnectTimeout    = "SSH_CONNECT_TIMEOUT"
	CodeSessionClosed        = "SESSION_CLOSED"
	CodeClientTooSlow        = "CLIENT_TOO_SLOW"
	CodeDBError              = "DB_ERROR"
	CodeInternal             = "INTERNAL_ERROR"
	CodeFeatureDisabled      = "FEATURE_DISABLED"
	CodeMooncellSessionExp   = "MOONCELL_SESSION_EXPIRED"
)

func writeErr(w http.ResponseWriter, status int, code, msg string, retryable bool) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(APIError{Code: code, Message: msg, Retryable: retryable})
}

func writeErrOffset(w http.ResponseWriter, status int, code, msg string, nextOffset int64) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	off := nextOffset
	_ = json.NewEncoder(w).Encode(APIError{
		Code: code, Message: msg, Retryable: true, NextOffset: &off,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOK(w http.ResponseWriter, v any) {
	writeJSON(w, http.StatusOK, v)
}

// writeAPIError 将 *APIError 映射为合适的 HTTP 状态（未知则 500）。
func writeAPIError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	ae, ok := err.(*APIError)
	if !ok {
		writeErr(w, http.StatusInternalServerError, CodeInternal, "内部错误", false)
		return
	}
	status := httpStatusForCode(ae.Code)
	if ae.NextOffset != nil {
		writeErrOffset(w, status, ae.Code, ae.Message, *ae.NextOffset)
		return
	}
	writeErr(w, status, ae.Code, ae.Message, ae.Retryable)
}

func httpStatusForCode(code string) int {
	switch code {
	case CodeUnauthorized, CodeMooncellSessionExp:
		return http.StatusUnauthorized
	case CodeForbidden:
		return http.StatusForbidden
	case CodeBadRequest, CodeValidation:
		return http.StatusBadRequest
	case CodeNotFound:
		return http.StatusNotFound
	case CodeNameDuplicate, CodeResourceChanged, CodeChunkOffsetMismatch, CodeRemoteTargetExists:
		return http.StatusConflict
	case CodeHostKeyMismatch:
		return http.StatusPreconditionFailed
	case CodeTransferTooLarge:
		return http.StatusRequestEntityTooLarge
	case CodeSSHAuthFailed, CodeRemotePartChanged, CodeAtomicReplaceUnsupp, CodeHostKeyUnconfirmed:
		return http.StatusUnprocessableEntity
	case CodeSessionLimit, CodeTransferLimit, CodeSSHAuthRateLimited:
		return http.StatusTooManyRequests
	case CodeSSHConnectionFailed:
		return http.StatusBadGateway
	case CodeSSHConnectTimeout:
		return http.StatusGatewayTimeout
	case CodeFeatureDisabled:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func apiErr(code, msg string, retryable bool) *APIError {
	return &APIError{Code: code, Message: msg, Retryable: retryable}
}
