package main

import (
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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(dropHTML)
}
