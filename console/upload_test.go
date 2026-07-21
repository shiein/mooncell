package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 分块上传 + 断点续传:顺序追加、乱序拒绝并给 nextIndex、重复幂等、收齐后可读出完整制品。
// 须绑定 appId + 登录会话(uploadStart 鉴权)。
func TestChunkedUploadResume(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	if err := s.createUser("admin", "pw", "admin"); err != nil {
		t.Fatal(err)
	}
	tok, _, _ := s.createSession("admin")
	a := &api{store: s, maxUpload: 64 << 20, uploads: map[string]*uploadSession{}}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/upload/start", a.uploadStart)
	mux.HandleFunc("PUT /api/upload/{uploadId}", a.uploadChunk)
	mux.HandleFunc("GET /api/upload/{uploadId}", a.uploadStatus)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	full := []byte(strings.Repeat("MOONCELL", 1000)) // 8000 字节
	chunks := [][]byte{full[:3000], full[3000:6000], full[6000:]}

	withAuth := func(req *http.Request) {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	}

	// start:无 appId 应 400
	body, _ := json.Marshal(map[string]any{"filename": "app.bin", "size": len(full)})
	req0, _ := http.NewRequest("POST", srv.URL+"/api/upload/start", bytes.NewReader(body))
	req0.Header.Set("Content-Type", "application/json")
	withAuth(req0)
	r0, _ := http.DefaultClient.Do(req0)
	if r0.StatusCode != http.StatusBadRequest {
		t.Fatalf("无 appId 应 400,got %d", r0.StatusCode)
	}
	r0.Body.Close()

	// start
	body, _ = json.Marshal(map[string]any{"filename": "app.bin", "size": len(full), "appId": "demo"})
	req, _ := http.NewRequest("POST", srv.URL+"/api/upload/start", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	withAuth(req)
	r, _ := http.DefaultClient.Do(req)
	var st struct {
		UploadID string `json:"uploadId"`
	}
	json.NewDecoder(r.Body).Decode(&st)
	r.Body.Close()
	if st.UploadID == "" {
		t.Fatal("start 应返回 uploadId")
	}
	put := func(idx int, data []byte) *http.Response {
		req, _ := http.NewRequest("PUT", srv.URL+"/api/upload/"+st.UploadID+"?index="+itoa(idx), bytes.NewReader(data))
		withAuth(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// 块0
	if resp := put(0, chunks[0]); resp.StatusCode != 200 {
		t.Fatalf("块0 应 200,got %d", resp.StatusCode)
	}
	// 乱序:跳到块2 → 409 + nextIndex=1
	resp := put(2, chunks[2])
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("乱序块应 409,got %d", resp.StatusCode)
	}
	var conf struct {
		NextIndex int `json:"nextIndex"`
	}
	json.NewDecoder(resp.Body).Decode(&conf)
	resp.Body.Close()
	if conf.NextIndex != 1 {
		t.Errorf("409 应给 nextIndex=1 供续传,got %d", conf.NextIndex)
	}
	// 重复块0 → 幂等 200 duplicate
	resp = put(0, chunks[0])
	var dup struct {
		Duplicate bool `json:"duplicate"`
		NextIndex int  `json:"nextIndex"`
	}
	json.NewDecoder(resp.Body).Decode(&dup)
	resp.Body.Close()
	if !dup.Duplicate || dup.NextIndex != 1 {
		t.Errorf("重复块应幂等(duplicate=true,nextIndex=1),got %+v", dup)
	}
	// 续传块1、块2
	put(1, chunks[1]).Body.Close()
	put(2, chunks[2]).Body.Close()

	// status:收齐
	reqS, _ := http.NewRequest("GET", srv.URL+"/api/upload/"+st.UploadID, nil)
	withAuth(reqS)
	r2, _ := http.DefaultClient.Do(reqS)
	var stat struct {
		Received int64 `json:"received"`
		Complete bool  `json:"complete"`
	}
	json.NewDecoder(r2.Body).Decode(&stat)
	r2.Body.Close()
	if !stat.Complete || stat.Received != int64(len(full)) {
		t.Fatalf("应收齐 %d 字节,got received=%d complete=%v", len(full), stat.Received, stat.Complete)
	}

	// 读出制品应与原文一致
	f, ok := a.openUploadArtifact(st.UploadID)
	if !ok {
		t.Fatal("收齐后应能打开制品")
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if !bytes.Equal(got, full) {
		t.Errorf("重组制品与原文不一致(len got=%d want=%d)", len(got), len(full))
	}
	// 清理
	a.finishUpload(st.UploadID)
	if _, ok := a.getUpload(st.UploadID); ok {
		t.Error("finishUpload 后会话应删除")
	}
}

// 非 admin 无应用授权时 upload/start 403。
func TestUploadRequiresAppACL(t *testing.T) {
	s := testStore(t)
	defer s.Close()
	if err := s.createUser("u1", "pw", "user"); err != nil {
		t.Fatal(err)
	}
	tok, _, _ := s.createSession("u1")
	a := &api{store: s, maxUpload: 64 << 20, uploads: map[string]*uploadSession{}}

	body, _ := json.Marshal(map[string]any{"filename": "x.bin", "size": 10, "appId": "nope"})
	req, _ := http.NewRequest("POST", "/api/upload/start", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	w := httptest.NewRecorder()
	a.uploadStart(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("无授权应 403,got %d body=%s", w.Code, w.Body.String())
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
