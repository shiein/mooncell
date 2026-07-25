package dataresource

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDSNBuilders(t *testing.T) {
	r := DataResource{DBType: DriverPostgreSQL, Host: "h", Port: 5432, Username: "u", DatabaseName: "d", SSLMode: "disable"}

	pg := BuildDSN(r, "secret")
	// libpq key=value：值以单引号包裹
	if !strings.Contains(pg, "host='h'") || !strings.Contains(pg, "password='secret'") || !strings.Contains(pg, "sslmode=disable") {
		t.Errorf("pgDSN 不符: %s", pg)
	}

	r.DBType = DriverMySQL
	my := BuildDSN(r, "secret")
	if !strings.Contains(my, "u:secret@tcp(h:5432)/d") {
		t.Errorf("mysqlDSN 不符: %s", my)
	}

	r.DBType = DriverDM
	r.DefaultSchema = "ADMIN" // 保留字：驱动 set schema "ADMIN"；且不得 URL 编码引号
	dm := BuildDSN(r, "secret")
	if !strings.Contains(dm, "dm://u:secret@h:5432") {
		t.Errorf("dmDSN 不符: %s", dm)
	}
	// 驱动不解码 query：必须是字面 ?schema="ADMIN"，不能是 %22ADMIN%22
	if !strings.Contains(dm, `?schema="ADMIN"`) || strings.Contains(dm, "%22") {
		t.Errorf("dmDSN schema 应原样带双引号且不 URL 编码: %s", dm)
	}
	r.DefaultSchema = "APP"
	dmApp := BuildDSN(r, "secret")
	if !strings.Contains(dmApp, `?schema="APP"`) {
		t.Errorf("dmDSN APP 也应加引号: %s", dmApp)
	}

	r.DBType = DriverKingbase
	kb := BuildDSN(r, "secret")
	if !strings.Contains(kb, "host='h'") {
		t.Errorf("kingbase DSN 应兼容 PG 格式: %s", kb)
	}

	r.DBType = DriverVastbase
	vb := BuildDSN(r, "secret")
	if vb != "" {
		t.Errorf("官方 Vastbase pq 未随构建提供时必须拒绝生成 DSN: %s", vb)
	}
}

func TestDSNSpecialChars(t *testing.T) {
	// 密码含空格、@、:、'、\ 等时不得破坏 DSN 字段边界
	pass := `p@ss:w0rd 'with\chars`
	r := DataResource{DBType: DriverPostgreSQL, Host: "h", Port: 5432, Username: "u", DatabaseName: "d", SSLMode: "disable"}
	pg := BuildDSN(r, pass)
	if !strings.Contains(pg, "password='p@ss:w0rd \\'with\\\\chars'") {
		t.Errorf("pgDSN 特殊字符转义不符: %s", pg)
	}

	r.DBType = DriverMySQL
	my := BuildDSN(r, pass)
	// FormatDSN 编码密码后仍保留 tcp 地址；明文中的空格不得裸出现在 userinfo
	if !strings.Contains(my, "@tcp(h:5432)/d") {
		t.Errorf("mysqlDSN 特殊字符不符: %s", my)
	}

	r.DBType = DriverDM
	dm := BuildDSN(r, "p@ss:word")
	// 驱动不解码：密码必须原样（含 @）；LastIndex("@") 仍能切到 host
	if !strings.HasPrefix(dm, "dm://") || !strings.Contains(dm, "u:p@ss:word@h:5432") {
		t.Errorf("dmDSN 特殊字符应保持原样: %s", dm)
	}
	if strings.Contains(dm, "%40") || strings.Contains(dm, "%3A") {
		t.Errorf("dmDSN 不得 URL 编码凭据: %s", dm)
	}
}

func TestDSNSSLRequire(t *testing.T) {
	r := DataResource{DBType: DriverPostgreSQL, Host: "h", Port: 5432, Username: "u", DatabaseName: "d", SSLMode: "require"}
	pg := BuildDSN(r, "p")
	if !strings.Contains(pg, "sslmode=require") {
		t.Errorf("PG SSL require 不符: %s", pg)
	}

	r.DBType = DriverMySQL
	my := BuildDSN(r, "p")
	if !strings.Contains(my, "tls=true") {
		t.Errorf("MySQL TLS true 不符: %s", my)
	}
}

// handlerTestEnv 封装测试用 Service 和 HTTP handler。
func handlerTestEnv(t *testing.T) (*Service, *httptest.Server) {
	t.Helper()
	db := testDB(t)
	keyPath := t.TempDir() + "/test.key"
	credKey, err := LoadOrCreateCredentialKey(keyPath, false)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(db, credKey)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/data-resources", svc.ListResources)
	mux.HandleFunc("POST /api/data-resources", svc.CreateResource)
	mux.HandleFunc("GET /api/data-resources/{id}", svc.GetResource)
	mux.HandleFunc("PUT /api/data-resources/{id}", svc.UpdateResource)
	mux.HandleFunc("DELETE /api/data-resources/{id}", svc.DeleteResource)
	mux.HandleFunc("POST /api/data-resources/test", svc.TestConnectionHandler)
	mux.HandleFunc("POST /api/data-resources/{id}/test", svc.TestExistingConnection)
	server := httptest.NewServer(mux)
	t.Cleanup(func() { server.Close() })

	// 包装注入 admin 用户
	orig := server.Config.Handler
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		orig.ServeHTTP(w, WithUser(r, "admin", "admin"))
	})
	return svc, server
}

func TestCreateAndGetResource(t *testing.T) {
	_, server := handlerTestEnv(t)

	// 创建
	body := `{"name":"PG","dbType":"pgx","host":"127.0.0.1","port":5432,"databaseName":"mydb","username":"u","password":"secret","sslMode":"disable"}`
	resp, err := http.Post(server.URL+"/api/data-resources", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("创建应返回 200,实际 %d", resp.StatusCode)
	}
	var out map[string]any
	jsonDecode(resp.Body, &out)
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatal("创建后未返回 id")
	}
	// 不应返回密码
	if _, hasPassword := out["hasPassword"]; !hasPassword {
		t.Error("应返回 hasPassword 字段")
	}
	if _, hasCipher := out["credentialCipher"]; hasCipher {
		t.Error("不应返回 credentialCipher")
	}

	// 查询单个
	resp2, _ := http.Get(server.URL + "/api/data-resources/" + id)
	if resp2.StatusCode != 200 {
		t.Fatalf("查询应返回 200,实际 %d", resp2.StatusCode)
	}
}

func TestCreateResourceDuplicateName(t *testing.T) {
	_, server := handlerTestEnv(t)
	body := `{"name":"PG","dbType":"pgx","host":"h","port":5432,"databaseName":"d","username":"u","password":"p","sslMode":"disable"}`
	http.Post(server.URL+"/api/data-resources", "application/json", strings.NewReader(body))
	resp, _ := http.Post(server.URL+"/api/data-resources", "application/json", strings.NewReader(body))
	if resp.StatusCode != 409 {
		t.Errorf("重名应返回 409,实际 %d", resp.StatusCode)
	}
}

func TestDeleteResourceNameMismatch(t *testing.T) {
	_, server := handlerTestEnv(t)
	// 先创建
	body := `{"name":"PG","dbType":"pgx","host":"h","port":5432,"databaseName":"d","username":"u","password":"p","sslMode":"disable"}`
	resp, _ := http.Post(server.URL+"/api/data-resources", "application/json", strings.NewReader(body))
	var out map[string]any
	jsonDecode(resp.Body, &out)
	id, _ := out["id"].(string)

	// 名称不匹配应拒绝
	delBody := `{"name":"WRONG"}`
	req, _ := http.NewRequest("DELETE", server.URL+"/api/data-resources/"+id, strings.NewReader(delBody))
	resp2, _ := http.DefaultClient.Do(req)
	if resp2.StatusCode != 400 {
		t.Errorf("名称不匹配应返回 400,实际 %d", resp2.StatusCode)
	}
}

func TestListResourcesAdmin(t *testing.T) {
	_, server := handlerTestEnv(t)
	// 创建两个
	for _, n := range []string{"R1", "R2"} {
		body := `{"name":"` + n + `","dbType":"pgx","host":"h","port":5432,"databaseName":"d","username":"u","password":"p","sslMode":"disable"}`
		http.Post(server.URL+"/api/data-resources", "application/json", strings.NewReader(body))
	}
	resp, _ := http.Get(server.URL + "/api/data-resources")
	var out map[string]any
	jsonDecode(resp.Body, &out)
	res, _ := out["resources"].([]any)
	if len(res) != 2 {
		t.Errorf("admin 应看到 2 个资源,实际 %d", len(res))
	}
}

func TestCreateResourcePasswordRequired(t *testing.T) {
	_, server := handlerTestEnv(t)
	body := `{"name":"PG","dbType":"pgx","host":"h","port":5432,"databaseName":"d","username":"u","password":"","sslMode":"disable"}`
	resp, _ := http.Post(server.URL+"/api/data-resources", "application/json", strings.NewReader(body))
	if resp.StatusCode != 400 {
		t.Errorf("空密码应返回 400,实际 %d", resp.StatusCode)
	}
}

func TestUpdateResourceKeepPassword(t *testing.T) {
	_, server := handlerTestEnv(t)
	// 创建
	body := `{"name":"PG","dbType":"pgx","host":"h","port":5432,"databaseName":"d","username":"u","password":"p","sslMode":"disable"}`
	resp, _ := http.Post(server.URL+"/api/data-resources", "application/json", strings.NewReader(body))
	var out map[string]any
	jsonDecode(resp.Body, &out)
	id, _ := out["id"].(string)

	// 编辑：空密码表示保留
	updBody := `{"name":"PG2","dbType":"pgx","host":"h","port":5432,"databaseName":"d","username":"u","password":"","sslMode":"disable"}`
	req, _ := http.NewRequest("PUT", server.URL+"/api/data-resources/"+id, strings.NewReader(updBody))
	resp2, _ := http.DefaultClient.Do(req)
	if resp2.StatusCode != 200 {
		t.Fatalf("编辑应返回 200,实际 %d", resp2.StatusCode)
	}
	var out2 map[string]any
	jsonDecode(resp2.Body, &out2)
	if out2["name"] != "PG2" {
		t.Errorf("名称应更新为 PG2,实际 %v", out2["name"])
	}
}

func jsonDecode(r interface {
	Read(p []byte) (n int, err error)
}, v any) {
	defer func() { /* ignore close error */ }()
	dec := json.NewDecoder(r.(interface{ Read([]byte) (int, error) }))
	dec.Decode(v)
}
