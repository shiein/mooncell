package consoleapp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func assertAgentAuthFailure(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("Agent 401 必须转换为 502,避免误触发 Console 退出,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Code   string `json:"code"`
		Error  string `json:"error"`
		Online bool   `json:"online"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析错误响应: %v", err)
	}
	if got.Code != agentAuthFailedCode || got.Online || !strings.Contains(got.Error, "共享 token") {
		t.Fatalf("应返回可识别的 Agent 鉴权错误,实际 %+v", got)
	}
}

func TestRelayAgentDoesNotExposeUpstreamUnauthorized(t *testing.T) {
	rec := httptest.NewRecorder()
	relayAgent(rec, http.StatusUnauthorized, []byte(`{"error":"token 校验失败"}`), nil)
	assertAgentAuthFailure(t, rec)
}

func TestStreamAgentRespDoesNotExposeUpstreamUnauthorized(t *testing.T) {
	rec := httptest.NewRecorder()
	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"error":"token 校验失败"}`)),
	}
	(&api{}).streamAgentResp(rec, resp, nil)
	assertAgentAuthFailure(t, rec)
}
