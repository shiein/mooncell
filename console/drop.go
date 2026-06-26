package main

import (
	"bytes"
	_ "embed"
	"net/http"
)

// 独立免登录文件柜投递页(/drop):极简自包含 HTML,不走 SPA、不需登录。
// 仅两件事:匿名上传(可选过期)→ 拿提取码;凭提取码下载。无列表(列表只在登录后 SPA 可见)。
// 上传走 POST /api/pub/cabinet(需 cabinet.anon_upload=true,未开启时页面据 403 文案优雅提示);
// 下载走 GET /api/pubfile/{code}(仅公开文件,匿名上传即公开)。

//go:embed drop.html
var dropHTML []byte

func (a *api) dropPage(w http.ResponseWriter, r *http.Request) {
	// 自包含页含内联 <script>:用 per-response nonce 放行该脚本,仍禁全局 'unsafe-inline' script。
	// 内联 <style> 由 style-src 'unsafe-inline' 覆盖(与全局一致),无需 nonce。
	nonce := randomToken()[:16]
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self' 'nonce-"+nonce+"'; style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'; form-action 'self'")
	w.Write(bytes.Replace(dropHTML, []byte("<script>"), []byte(`<script nonce="`+nonce+`">`), 1))
}
