package consoleapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFailLimiterLockResetAndBound(t *testing.T) {
	l := newFailLimiter(3, time.Minute, 5*time.Minute)
	now := time.Now()
	for i := 0; i < 2; i++ {
		if l.fail("k", now) {
			t.Fatal("未达阈值不应锁定")
		}
	}
	if !l.fail("k", now) {
		t.Fatal("第 3 次失败应锁定")
	}
	if b, wait := l.blocked("k", now); !b || wait <= 0 {
		t.Fatal("应处于锁定期")
	}
	if b, _ := l.blocked("other", now); b {
		t.Fatal("其它 key 不受影响")
	}
	if b, _ := l.blocked("k", now.Add(6*time.Minute)); b {
		t.Fatal("锁定期过后应解除")
	}
	l.fail("r", now)
	l.reset("r")
	l.fail("r", now)
	l.fail("r", now)
	if b, _ := l.blocked("r", now); b {
		t.Fatal("reset 后应重新计数")
	}
	// 内存有界
	small := newFailLimiter(3, time.Minute, time.Minute)
	small.maxKeys = 50
	for i := 0; i < 500; i++ {
		small.fail(strings.Repeat("x", i%7)+string(rune('a'+i%26))+time.Duration(i).String(), now)
	}
	if len(small.m) > 50 {
		t.Fatalf("表项应有界，得 %d", len(small.m))
	}
}

// 登录：同一来源 + 用户名 5 次失败后 429（带 Retry-After），其它用户不受影响，成功清零。
func TestLoginThrottle(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	a := &api{store: s}
	s.createUser("victim", "right-pw", "user")
	s.createUser("other", "right-pw", "user")
	login := func(user, pw, ip string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"username": user, "password": pw})
		req := httptest.NewRequest("POST", "/api/login", strings.NewReader(string(body)))
		req.RemoteAddr = ip + ":40000"
		w := httptest.NewRecorder()
		a.login(w, req)
		return w
	}
	ip := "10.9.8.7"
	for i := 0; i < 4; i++ {
		if c := login("victim", "bad", ip).Code; c != http.StatusUnauthorized {
			t.Fatalf("第 %d 次应 401，得 %d", i+1, c)
		}
	}
	// 第 4 次后一次成功清零，再失败 4 次仍不锁
	if c := login("victim", "right-pw", ip).Code; c != http.StatusOK {
		t.Fatalf("正确密码应 200，得 %d", c)
	}
	for i := 0; i < 4; i++ {
		login("victim", "bad", ip)
	}
	if c := login("victim", "bad", ip).Code; c != http.StatusUnauthorized {
		t.Fatalf("第 5 次失败本身应 401，得 %d", c)
	}
	w := login("victim", "right-pw", ip)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("锁定后即便密码正确也应 429 + Retry-After，得 %d %q", w.Code, w.Header().Get("Retry-After"))
	}
	if c := login("other", "right-pw", ip).Code; c != http.StatusOK {
		t.Fatalf("同来源的其它用户不应受影响，得 %d", c)
	}
	if c := login("victim", "right-pw", "10.9.8.99").Code; c != http.StatusOK {
		t.Fatalf("其它来源登录同一用户不应被锁（防恶意锁定 admin），得 %d", c)
	}
}

// 提取码：查找走索引；未命中与未公开同为 404；连续未命中后 429。
func TestPubCodeIndexedAndThrottled(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	a := &api{store: s}
	far := time.Now().Add(24 * time.Hour).UnixMilli()
	s.putEntity("cabinet", "c1", json.RawMessage(`{"id":"c1","name":"pub.txt","size":1,"code":"ABC234","public":true,"expires":`+jsonInt(far)+`}`))
	s.putEntity("cabinet", "c2", json.RawMessage(`{"id":"c2","name":"priv.txt","size":1,"code":"PRV234","public":false,"expires":`+jsonInt(far)+`}`))

	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT data FROM entities WHERE kind = 'cabinet' AND upper(json_extract(data, '$.code')) = upper(?)`, "x")
	if err != nil {
		t.Fatal(err)
	}
	plan := ""
	for rows.Next() {
		var id, parent, notused int
		var detail string
		rows.Scan(&id, &parent, &notused, &detail)
		plan += detail + ";"
	}
	rows.Close()
	if !strings.Contains(plan, "idx_entities_cabinet_code") {
		t.Fatalf("提取码查找未使用索引: %s", plan)
	}
	if _, ok := s.cabinetByCode("abc234"); !ok {
		t.Fatal("提取码应大小写不敏感")
	}

	get := func(code string) int {
		req := httptest.NewRequest("GET", "/api/pubfile/"+code+"/meta", nil)
		req.SetPathValue("code", code)
		req.RemoteAddr = "10.1.1.1:5000"
		w := httptest.NewRecorder()
		a.pubfileMeta(w, req)
		return w.Code
	}
	if c := get("ABC234"); c != http.StatusOK {
		t.Fatalf("公开文件应 200，得 %d", c)
	}
	if c := get("PRV234"); c != http.StatusNotFound {
		t.Fatalf("未公开应与不存在一样 404（不泄露存在性），得 %d", c)
	}
	for i := 0; i < 25; i++ {
		get("ZZZ999")
	}
	if c := get("ABC234"); c != http.StatusTooManyRequests {
		t.Fatalf("连续未命中后应 429，得 %d", c)
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// 来源 IP：默认不采信转发头；对端为可信反代时取 XFF 最右侧第一个非可信地址（左侧伪造无效）。
func TestClientIPTrustedProxies(t *testing.T) {
	defer SetTrustedProxies(nil)
	req := func(remote, xff string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	SetTrustedProxies(nil)
	if got := clientIP(req("203.0.113.5:1234", "1.2.3.4")); got != "203.0.113.5" {
		t.Fatalf("未配置可信反代时不得采信 XFF，得 %s", got)
	}
	if got := limiterSourceKey(req("127.0.0.1:1234", "")); got != "" {
		t.Fatalf("未配置可信反代时回环来源应跳过兜底限流，得 %q", got)
	}
	if err := SetTrustedProxies([]string{"127.0.0.1", "10.0.0.0/8"}); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ remote, xff, want string }{
		{"127.0.0.1:1", "198.51.100.7", "198.51.100.7"},
		{"127.0.0.1:1", "6.6.6.6, 198.51.100.7", "198.51.100.7"},  // 左侧伪造无效
		{"127.0.0.1:1", "198.51.100.7, 10.1.2.3", "198.51.100.7"}, // 跳过链上可信代理
		{"127.0.0.1:1", "", "127.0.0.1"},                          // 无 XFF
		{"127.0.0.1:1", "garbage", "127.0.0.1"},                   // 非法值不采信
		{"203.0.113.9:1", "198.51.100.7", "203.0.113.9"},          // 非可信对端：忽略 XFF
	}
	for _, c := range cases {
		if got := clientIP(req(c.remote, c.xff)); got != c.want {
			t.Errorf("clientIP(%s, %q) = %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
	if got := limiterSourceKey(req("127.0.0.1:1", "198.51.100.7")); got != "198.51.100.7" {
		t.Fatalf("配置可信反代后兜底限流应按真实来源，得 %q", got)
	}
	if err := SetTrustedProxies([]string{"not-an-ip"}); err == nil {
		t.Fatal("非法配置应报错")
	}
}
