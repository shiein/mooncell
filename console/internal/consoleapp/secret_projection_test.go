package consoleapp

import (
	"encoding/json"
	"testing"
)

func envVarsOf(t *testing.T, raw json.RawMessage) []map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, item := range envVarItems(m) {
		out = append(out, item.(map[string]any))
	}
	return out
}

// TestRedactAppSecrets:读投影抹除 secret 明文,标 hasValue;非 secret 值原样保留。
func TestRedactAppSecrets(t *testing.T) {
	raw := json.RawMessage(`{"id":"a1","envVars":[
		{"name":"DB_PASS","value":"s3cr3t","secret":true},
		{"name":"EMPTY_SECRET","value":"","secret":true},
		{"name":"PORT","value":"8080","secret":false}
	]}`)
	got := envVarsOf(t, redactAppSecrets(raw))

	if got[0]["value"] != "" || got[0]["hasValue"] != true {
		t.Fatalf("有值 secret 应抹成空且 hasValue=true,got %v", got[0])
	}
	if got[1]["value"] != "" || got[1]["hasValue"] != false {
		t.Fatalf("空 secret 应 hasValue=false,got %v", got[1])
	}
	if got[2]["value"] != "8080" {
		t.Fatalf("非 secret 值应原样保留,got %v", got[2])
	}
	if _, ok := got[2]["hasValue"]; ok {
		t.Fatalf("非 secret 不应加 hasValue")
	}
}

// TestPreserveSecretValues:保存时空 secret 值以既有实体同名 secret 值补回;填了新值则以新值为准。
func TestPreserveSecretValues(t *testing.T) {
	old := json.RawMessage(`{"id":"a1","envVars":[
		{"name":"DB_PASS","value":"stored-secret","secret":true},
		{"name":"API_KEY","value":"old-key","secret":true}
	]}`)
	// 前端保存:DB_PASS 留空(应保留 stored-secret),API_KEY 填了新值(应更新)。
	m := map[string]any{
		"id": "a1",
		"envVars": []any{
			map[string]any{"name": "DB_PASS", "value": "", "secret": true},
			map[string]any{"name": "API_KEY", "value": "new-key", "secret": true},
		},
	}
	preserveSecretValues(old, m)
	got := m["envVars"].([]any)
	if got[0].(map[string]any)["value"] != "stored-secret" {
		t.Fatalf("留空 secret 应补回原值 stored-secret,got %v", got[0])
	}
	if got[1].(map[string]any)["value"] != "new-key" {
		t.Fatalf("填了新值应以新值为准,got %v", got[1])
	}
}

// TestSecretRoundTrip:读(redact)→存(preserve)闭环——用户从投影后的数据原样保存,secret 不丢。
func TestSecretRoundTrip(t *testing.T) {
	stored := json.RawMessage(`{"id":"a1","name":"app","envVars":[
		{"name":"DB_PASS","value":"stored-secret","secret":true}
	]}`)
	// 模拟前端拿到的是 redact 后的数据。
	redacted := redactAppSecrets(stored)
	var m map[string]any
	if err := json.Unmarshal(redacted, &m); err != nil {
		t.Fatal(err)
	}
	// 用户没动 secret,原样回存(带 hasValue、空 value)。
	stripEnvHasValue(m)
	preserveSecretValues(stored, m)
	got := m["envVars"].([]any)[0].(map[string]any)
	if got["value"] != "stored-secret" {
		t.Fatalf("读→存闭环 secret 应保留,got %v", got["value"])
	}
	if _, ok := got["hasValue"]; ok {
		t.Fatalf("hasValue 不应落库")
	}
}
