package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	cfg := &Config{
		Database: DatabaseConfig{Path: filepath.Join(t.TempDir(), "t.db")},
		Session:  SessionConfig{TTLHours: 1},
	}
	return openDB(cfg)
}

// 审计不可伪造:通用 PUT/DELETE /api/data/{kind} 必须拒绝 kind=audit。
func TestPutDeleteEntityRejectsAudit(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	a := &api{store: s}

	for _, kind := range []string{"audit", "release"} {
		for _, m := range []struct {
			name string
			call func(http.ResponseWriter, *http.Request)
			req  *http.Request
		}{
			{"PUT", a.putEntity, httptest.NewRequest("PUT", "/api/data/"+kind+"/x", strings.NewReader(`{"id":"x"}`))},
			{"DELETE", a.deleteEntity, httptest.NewRequest("DELETE", "/api/data/"+kind+"/x", nil)},
		} {
			m.req.SetPathValue("kind", kind)
			m.req.SetPathValue("id", "x")
			w := httptest.NewRecorder()
			m.call(w, m.req)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s kind=%s 应 403,得 %d", m.name, kind, w.Code)
			}
		}
	}

	// 非 audit 实体仍可写
	req := httptest.NewRequest("PUT", "/api/data/app/a1", strings.NewReader(`{"id":"a1"}`))
	req.SetPathValue("kind", "app")
	req.SetPathValue("id", "a1")
	w := httptest.NewRecorder()
	a.putEntity(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("kind=app 应 200,得 %d", w.Code)
	}
}

// 服务端审计/发布记录追加可用,且确实落库。
func TestAppendAuditAndRelease(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	if err := s.appendAudit("admin", "部署", "x v1", "成功"); err != nil {
		t.Fatalf("appendAudit err: %v", err)
	}
	if err := s.appendRelease("x", "v1", "success", "admin"); err != nil {
		t.Fatalf("appendRelease err: %v", err)
	}
	got, err := s.loadEntities()
	if err != nil || len(got["audit"]) != 1 || len(got["release"]) != 1 {
		t.Fatalf("审计/发布各应落库 1 条,得 audit=%d release=%d (err=%v)", len(got["audit"]), len(got["release"]), err)
	}
}

// T5 对账:SSE 没拿到 success 时,向 Agent 查权威幂等记录,recorded=success 则据此记账。
func TestFetchReleaseRecordReconcile(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	a := &api{store: s}

	var gotOp, gotRid string
	recorded := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOp, gotRid = r.URL.Query().Get("op"), r.URL.Query().Get("releaseId")
		w.Header().Set("Content-Type", "application/json")
		if recorded {
			w.Write([]byte(`{"recorded":true,"result":"success","version":"v9"}`))
		} else {
			w.Write([]byte(`{"recorded":false}`))
		}
	}))
	defer srv.Close()
	cl := newAgentClient(AgentConfig{Addr: strings.TrimPrefix(srv.URL, "http://"), Token: "t"})

	// 命中权威成功记录
	res, ver, ok := a.fetchReleaseRecord(cl, "deploy", "app1", "R1")
	if !ok || res != "success" || ver != "v9" {
		t.Fatalf("应对账到 success/v9: ok=%v res=%q ver=%q", ok, res, ver)
	}
	if gotOp != "deploy" || gotRid != "R1" {
		t.Errorf("查询参数透传错误: op=%q rid=%q", gotOp, gotRid)
	}

	// 未记录(如失败/未完成)→ ok=false,不伪造结果
	recorded = false
	if _, _, ok := a.fetchReleaseRecord(cl, "deploy", "app1", "R2"); ok {
		t.Error("未记录时应返回 ok=false")
	}

	// releaseID 为空 / cl 为 nil → 直接 false,不发请求
	if _, _, ok := a.fetchReleaseRecord(cl, "deploy", "app1", ""); ok {
		t.Error("空 releaseID 应返回 false")
	}
	if _, _, ok := a.fetchReleaseRecord(nil, "deploy", "app1", "R1"); ok {
		t.Error("nil cl 应返回 false")
	}
}

// RBAC:operator 不能访问 admin-only;可访问 write 路由;viewer 被 write 路由拦截。
func TestRequireRole(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	s.createUser("op", "pw", "operator")
	s.createUser("vw", "pw", "viewer")
	a := &api{store: s}
	opTok, _ := s.createSession("op")
	vwTok, _ := s.createSession("vw")

	ok := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
	hit := func(h http.HandlerFunc, tok string) int {
		req := httptest.NewRequest("GET", "/x", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
		w := httptest.NewRecorder()
		h(w, req)
		return w.Code
	}

	if c := hit(a.requireRole("admin")(ok), opTok); c != http.StatusForbidden {
		t.Errorf("operator 访问 admin 路由应 403,得 %d", c)
	}
	if c := hit(a.requireRole("admin", "operator")(ok), opTok); c != http.StatusOK {
		t.Errorf("operator 访问 write 路由应 200,得 %d", c)
	}
	if c := hit(a.requireRole("admin", "operator")(ok), vwTok); c != http.StatusForbidden {
		t.Errorf("viewer 访问 write 路由应 403,得 %d", c)
	}
}

// 文件柜过期清理:过期条目(元数据 + 字节)删除,未过期保留。
func TestCabinetExpiryCleanup(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	dir := t.TempDir()
	a := &api{store: s, cabinetDir: dir}
	now := time.Now().UnixMilli()

	os.WriteFile(filepath.Join(dir, "cf_old"), []byte("x"), 0644)
	s.putEntity("cabinet", "cf_old", []byte(fmt.Sprintf(`{"id":"cf_old","expires":%d}`, now-1000)))
	os.WriteFile(filepath.Join(dir, "cf_new"), []byte("y"), 0644)
	s.putEntity("cabinet", "cf_new", []byte(fmt.Sprintf(`{"id":"cf_new","expires":%d}`, now+100000)))

	if n := a.cleanupExpiredCabinet(); n != 1 {
		t.Fatalf("应清理 1 个过期文件,得 %d", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "cf_old")); !os.IsNotExist(err) {
		t.Error("过期文件字节应被删除")
	}
	if _, err := os.Stat(filepath.Join(dir, "cf_new")); err != nil {
		t.Error("未过期文件应保留")
	}
	if _, ok := s.getEntity("cabinet", "cf_old"); ok {
		t.Error("过期条目元数据应被删除")
	}
}

// 幂等键按 (op, app_id, release_id) 隔离:同 releaseId 跨操作/跨 app 不得互相误命中。
func TestDeployIdempotencyIsolation(t *testing.T) {
	s := testStore(t)
	defer s.Close()

	s.putDeploy("deploy", "app-a", "rid-1", "success", "fp1")

	// 同 op+app+rid:命中,指纹一并返回
	if res, fp, ok := s.getDeploy("deploy", "app-a", "rid-1"); !ok || res != "success" || fp != "fp1" {
		t.Fatalf("同键应命中 success/fp1,got %q fp=%q ok=%v", res, fp, ok)
	}
	// 同 rid 不同 op(还原):不得命中
	if _, _, ok := s.getDeploy("restore", "app-a", "rid-1"); ok {
		t.Error("还原复用部署 releaseId 不应命中(op 隔离)")
	}
	// 同 rid 不同 app:不得命中
	if _, _, ok := s.getDeploy("deploy", "app-b", "rid-1"); ok {
		t.Error("不同 app 复用 releaseId 不应命中(app 隔离)")
	}
	// 同键再写不同结果+指纹:覆盖(ON CONFLICT)
	s.putDeploy("deploy", "app-a", "rid-1", "failed", "fp2")
	if res, fp, _ := s.getDeploy("deploy", "app-a", "rid-1"); res != "failed" || fp != "fp2" {
		t.Errorf("同键重写应覆盖为 failed/fp2,got %q fp=%q", res, fp)
	}
}

// 幂等指纹必须覆盖运行配置,否则同 releaseId 改 env/args/venv/reload 会被 Console 误短路。
func TestDeployFingerprintIncludesRuntimeConfig(t *testing.T) {
	base := appConfig{
		Name: "app", Type: "python", Runner: "systemd", Path: "/srv/apps/app/main.py",
		Workdir: "/srv/apps/app", Health: "http://127.0.0.1:8080/health",
		Interp: "/srv/apps/app/venv/bin/python", Jvm: "--port 8080", User: "appuser",
		BackupKeep: 5, Reload: false, Env: map[string]string{"MODE": "prod"},
	}
	fp := deployFingerprint(base, strings.Repeat("a", 64), "v1", "")

	changedArgs := base
	changedArgs.Jvm = "--port 9090"
	if deployFingerprint(changedArgs, strings.Repeat("a", 64), "v1", "") == fp {
		t.Fatal("启动参数变化必须改变 fingerprint")
	}

	changedEnv := base
	changedEnv.Env = map[string]string{"MODE": "staging"}
	if deployFingerprint(changedEnv, strings.Repeat("a", 64), "v1", "") == fp {
		t.Fatal("环境变量变化必须改变 fingerprint")
	}

	changedInterp := base
	changedInterp.Interp = "/srv/apps/app/venv2/bin/python"
	if deployFingerprint(changedInterp, strings.Repeat("a", 64), "v1", "") == fp {
		t.Fatal("解释器/venv 变化必须改变 fingerprint")
	}
}

func TestBuildAgentConfigMapsReloadFlag(t *testing.T) {
	static := buildAgentDeployConfig(appConfig{Type: "static-nginx", Path: "/data/web/site", Reload: true}, "v1", "", "r1")
	if static.ReloadCmd != "nginx-reload" {
		t.Fatalf("static reload=true 应映射 nginx-reload,got %q", static.ReloadCmd)
	}

	tomcat := buildAgentDeployConfig(appConfig{Type: "tomcat-war", Path: "/opt/tomcat/webapps/a.war", Reload: true}, "v1", "", "r1")
	if tomcat.ReloadCmd != "tomcat-restart" {
		t.Fatalf("tomcat reload=true 应映射 tomcat-restart,got %q", tomcat.ReloadCmd)
	}

	disabled := buildAgentDeployConfig(appConfig{Type: "static-nginx", Path: "/data/web/site", Reload: false}, "v1", "", "r1")
	if disabled.ReloadCmd != "" {
		t.Fatalf("reload=false 不应下发 reloadCmd,got %q", disabled.ReloadCmd)
	}
}

// 旧库(单列 release_id 主键)迁移:启动时清空旧表,以复合主键重建,迁移后可正常隔离写入。
func TestMigrateLegacyDeploys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	// 手造旧 schema 并塞一条遗留记录
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Exec(`CREATE TABLE deploys (release_id TEXT PRIMARY KEY, app_id TEXT NOT NULL, result TEXT NOT NULL, created_at INTEGER NOT NULL)`)
	legacy.Exec(`INSERT INTO deploys (release_id, app_id, result, created_at) VALUES ('old','x','success',1)`)
	legacy.Close()

	// openDB 触发迁移
	cfg := &Config{Database: DatabaseConfig{Path: path}, Session: SessionConfig{TTLHours: 1}}
	s := openDB(cfg)
	defer s.Close()

	// 旧记录已随旧表清空(迁移丢弃去重记录,可接受)
	if _, _, ok := s.getDeploy("deploy", "x", "old"); ok {
		t.Error("迁移后旧单列记录应被清空")
	}
	// 新复合主键正常工作
	s.putDeploy("deploy", "x", "new", "success", "fp")
	if res, _, ok := s.getDeploy("deploy", "x", "new"); !ok || res != "success" {
		t.Errorf("迁移后复合主键写入应可用,got %q ok=%v", res, ok)
	}
}

// 复合主键库缺 fingerprint 列时:迁移补列,旧记录指纹为空(命中比对视为不一致),新写入带指纹。
func TestMigrateAddsFingerprint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v2.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// 复合主键但无 fingerprint 列(review5 形态)
	old.Exec(`CREATE TABLE deploys (op TEXT NOT NULL, app_id TEXT NOT NULL, release_id TEXT NOT NULL, result TEXT NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY(op,app_id,release_id))`)
	old.Exec(`INSERT INTO deploys (op,app_id,release_id,result,created_at) VALUES ('deploy','x','r1','success',1)`)
	old.Close()

	s := openDB(&Config{Database: DatabaseConfig{Path: path}, Session: SessionConfig{TTLHours: 1}})
	defer s.Close()

	// 旧记录保留,指纹为空(命中后调用方比对会因空指纹放行给 Agent)
	res, fp, ok := s.getDeploy("deploy", "x", "r1")
	if !ok || res != "success" || fp != "" {
		t.Fatalf("迁移后旧记录应保留且指纹为空,got res=%q fp=%q ok=%v", res, fp, ok)
	}
	// 新写入带指纹
	s.putDeploy("deploy", "x", "r2", "success", "fpX")
	if _, fp2, _ := s.getDeploy("deploy", "x", "r2"); fp2 != "fpX" {
		t.Errorf("迁移后新写入应带指纹,got %q", fp2)
	}
}

// 日志文件 tail 授权:只有应用声明的 logPaths 才放行,其它一律拒绝(防越权读他应用/任意文件)。
func TestAppDeclaresLog(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	a := &api{store: s}

	app := appConfig{Name: "x", Type: "go-binary", LogPaths: []string{"/srv/apps/x/logs/app.log", "/srv/apps/x/logs/err.log"}}
	b, _ := json.Marshal(app)
	s.putEntity("app", "x", b)

	if !a.appDeclaresLog("x", "/srv/apps/x/logs/app.log") {
		t.Error("已声明路径应放行")
	}
	if a.appDeclaresLog("x", "/srv/apps/other/secret.log") {
		t.Error("未声明路径必须拒绝")
	}
	if a.appDeclaresLog("x", "") {
		t.Error("空路径必须拒绝")
	}
	if a.appDeclaresLog("nope", "/srv/apps/x/logs/app.log") {
		t.Error("应用不存在必须拒绝")
	}
	// 规范化后等价的路径应放行(/./、重复斜杠)
	if !a.appDeclaresLog("x", "/srv/apps/x/logs/./app.log") {
		t.Error("规范化后等价的已声明路径应放行")
	}
	// 相对路径 / 穿越必须拒绝(fail-closed)
	if a.appDeclaresLog("x", "srv/apps/x/logs/app.log") {
		t.Error("相对路径必须拒绝")
	}
	if a.appDeclaresLog("x", "/srv/apps/x/logs/../../other/secret.log") {
		t.Error("穿越路径规范化后不在声明内,必须拒绝")
	}
}

func TestAgentLogsRequireExistingApp(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	a := &api{store: s}

	cases := []struct {
		name string
		call func(http.ResponseWriter, *http.Request)
		url  string
	}{
		{"stream", a.agentLogStream, "/api/agent/apps/missing/logs/stream"},
		{"download", a.agentLogDownload, "/api/agent/apps/missing/logs/download"},
		{"file", a.agentLogFileStream, "/api/agent/apps/missing/logs/file/stream?path=/srv/apps/x/app.log"},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", c.url, nil)
		req.SetPathValue("id", "missing")
		w := httptest.NewRecorder()
		c.call(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s: 不存在应用应 404,got %d", c.name, w.Code)
		}
	}
}

// 服务端权威更新应用状态:部署成功切版本+running;失败=failed;启停据 Agent 真实状态。
func TestApplyAppRuntimeState(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	a := &api{store: s}
	put := func(m map[string]any) {
		b, _ := json.Marshal(m)
		s.putEntity("app", "x", b)
	}
	get := func() map[string]any {
		raw, _ := s.getEntity("app", "x")
		var m map[string]any
		json.Unmarshal(raw, &m)
		return m
	}
	// 部署成功:版本切到 v2,状态 running,pid 清空
	put(map[string]any{"id": "x", "type": "go-binary", "version": "v1", "status": "stopped", "pid": float64(123)})
	a.applyAppRuntimeState("x", "v2", "success")
	if m := get(); m["version"] != "v2" || m["status"] != "running" || m["pid"] != nil {
		t.Fatalf("success 应切 v2/running/pid空,got %v", m)
	}
	// 回滚:版本不动(保留 v2),状态 running
	a.applyAppRuntimeState("x", "v3", "rolledback")
	if m := get(); m["version"] != "v2" || m["status"] != "running" {
		t.Errorf("rolledback 应保留旧版本 v2,got version=%v status=%v", get()["version"], m["status"])
	}
	// 失败:status=failed,版本不动
	a.applyAppRuntimeState("x", "v3", "failed")
	if m := get(); m["status"] != "failed" || m["version"] != "v2" {
		t.Errorf("failed 应 status=failed/版本不动,got %v", m)
	}
	// static 类型:成功 → static
	put(map[string]any{"id": "x", "type": "static-nginx", "version": "v1"})
	a.applyAppRuntimeState("x", "v2", "success")
	if get()["status"] != "static" {
		t.Errorf("static 成功应 status=static,got %v", get()["status"])
	}
}

func TestApplyLifecycleState(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	a := &api{store: s}
	b, _ := json.Marshal(map[string]any{"id": "x", "type": "go-binary", "status": "stopped"})
	s.putEntity("app", "x", b)
	// stop 返回 active=false
	a.applyLifecycleState("x", []byte(`{"active":false,"pid":"0"}`))
	raw, _ := s.getEntity("app", "x")
	var m map[string]any
	json.Unmarshal(raw, &m)
	if m["status"] != "stopped" || m["pid"] != nil {
		t.Fatalf("stop 应 stopped/pid空,got %v", m)
	}
	// start 返回 active=true,pid
	a.applyLifecycleState("x", []byte(`{"active":true,"pid":"4321"}`))
	raw, _ = s.getEntity("app", "x")
	json.Unmarshal(raw, &m)
	if m["status"] != "running" || m["pid"] != "4321" {
		t.Errorf("start 应 running/pid=4321,got %v", m)
	}
}
