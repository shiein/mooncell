package serverops

import (
	"net/http"
	"testing"
)

func TestOriginOKExactMatch(t *testing.T) {
	req := func(origin, host string) *http.Request {
		r, _ := http.NewRequest("GET", "http://"+host+"/ws", nil)
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}
	cases := []struct {
		origin string
		host   string
		ok     bool
	}{
		{"", "ops.example.com", true},
		{"https://ops.example.com", "ops.example.com", true},
		{"https://ops.example.com:443", "ops.example.com", true},
		{"http://127.0.0.1:8787", "127.0.0.1:8787", true},
		// 后缀绕过
		{"https://evilops.example.com", "ops.example.com", false},
		{"https://ops.example.com.evil.test", "ops.example.com", false},
		{"https://evil.example.com", "ops.example.com", false},
		// 端口不同
		{"https://ops.example.com:8443", "ops.example.com", false},
		{"http://ops.example.com", "ops.example.com", true}, // scheme 不同但 host:port 在默认端口下：http→80 vs 无端口补 80
	}
	for _, c := range cases {
		got := originOK(req(c.origin, c.host))
		if got != c.ok {
			t.Fatalf("origin=%q host=%q: got %v want %v", c.origin, c.host, got, c.ok)
		}
	}
}

func TestParseSingleRange(t *testing.T) {
	etag := `W/"1-2"`
	// full
	s, e, st, ok := parseSingleRange("", "", etag, 1000)
	if !ok || s != 0 || e != 999 || st != http.StatusOK {
		t.Fatalf("full: %d-%d st=%d ok=%v", s, e, st, ok)
	}
	// suffix
	s, e, st, ok = parseSingleRange("bytes=-500", "", etag, 1000)
	if !ok || s != 500 || e != 999 || st != http.StatusPartialContent {
		t.Fatalf("suffix: %d-%d st=%d ok=%v", s, e, st, ok)
	}
	// open end
	s, e, st, ok = parseSingleRange("bytes=100-", "", etag, 1000)
	if !ok || s != 100 || e != 999 {
		t.Fatalf("open: %d-%d", s, e)
	}
	// invalid number
	_, _, _, ok = parseSingleRange("bytes=abc-10", "", etag, 1000)
	if ok {
		t.Fatal("expected 416 for bad number")
	}
	// start beyond size
	_, _, _, ok = parseSingleRange("bytes=2000-", "", etag, 1000)
	if ok {
		t.Fatal("expected 416 for start>=size")
	}
	// multipart
	_, _, _, ok = parseSingleRange("bytes=0-1,2-3", "", etag, 1000)
	if ok {
		t.Fatal("expected 416 for multipart")
	}
	// If-Range mismatch → full
	s, e, st, ok = parseSingleRange("bytes=0-10", "other", etag, 1000)
	if !ok || st != http.StatusOK || s != 0 || e != 999 {
		t.Fatalf("if-range: %d-%d st=%d", s, e, st)
	}
}
