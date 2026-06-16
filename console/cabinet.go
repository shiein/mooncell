package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// isMaxBytes 判断错误是否为请求体超过 MaxBytesReader 上限。
func isMaxBytes(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

func cleanupMultipart(r *http.Request) {
	if r.MultipartForm != nil {
		r.MultipartForm.RemoveAll()
	}
}

// 文件柜:内网临时文件中转。Console 落盘存二进制(cabinet 目录),元数据复用 entity(kind=cabinet)。
// 上传/删除限 write 角色;按 id 下载需登录(任意角色);公开文件可凭提取码免登录下载。

const cabinetExpiryDays = 7

// allowedExpiryDays 是匿名/登录上传可选的过期天数白名单;白名单外一律回退默认 7 天(fail-safe)。
var allowedExpiryDays = map[int]bool{1: true, 7: true, 30: true}

// parseExpiryDays 解析上传表单里的 expireDays;非白名单值回退默认 7 天。
func parseExpiryDays(s string) int {
	switch strings.TrimSpace(s) {
	case "1":
		return 1
	case "30":
		return 30
	default:
		return cabinetExpiryDays
	}
}

// clientIP 取请求来源 IP:优先反代透传头(X-Forwarded-For 首段 / X-Real-IP),否则 RemoteAddr。
// 内网工具,IP 仅作上传者标识(审计),不作鉴权依据,故接受反代头即可。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		return xr
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// 文件柜单文件上限由 cabinet.max_upload_mb 配置(a.cabinetMaxBytes,默认 200MB)。ParseMultipartForm
// 的参数只是内存阈值,超出会落临时盘且 io.Copy 不限大小;必须用 MaxBytesReader 在传输层截断并回 413。

// genCode 生成易读的 6 位提取码(去掉易混字符)。
func genCode() string {
	const alphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	b := make([]byte, 6)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[n.Int64()]
	}
	return string(b)
}

// storedPath 是某文件柜条目的落盘路径(以 id 命名,避免文件名穿越/冲突)。
func (a *api) storedPath(id string) string {
	return filepath.Join(a.cabinetDir, filepath.Base(id))
}

// storeCabinetFile 落盘 + 写元数据的共享核心;public=true 时上传即公开(匿名场景凭码可下载)。
func (a *api) storeCabinetFile(w http.ResponseWriter, r *http.Request, uploader string, public bool) {
	// 早拦:Content-Length 已超限就立刻回 413,不再读 body。浏览器不发 Expect:100-continue,
	// 若等到 MaxBytesReader 在传输中途截断 + 关连接,客户端只会看到「网络错误」而读不到 413;
	// 提前据声明长度拒绝能让客户端尽量拿到明确响应(客户端另有大小预检兜底)。
	limitMB := a.cabinetMaxBytes >> 20
	if r.ContentLength > a.cabinetMaxBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("文件超过 %d MB 上限", limitMB)})
		return
	}
	// 传输层硬上限:超过即 MaxBytesError,统一回 413,杜绝「文案上限、实际不限」。
	r.Body = http.MaxBytesReader(w, r.Body, a.cabinetMaxBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		if isMaxBytes(err) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("文件超过 %d MB 上限", limitMB)})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "表单解析失败"})
		return
	}
	defer cleanupMultipart(r)
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 file 字段"})
		return
	}
	defer file.Close()

	if err := os.MkdirAll(a.cabinetDir, 0755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "创建存储目录失败"})
		return
	}
	id := fmt.Sprintf("cf%d", time.Now().UnixNano())
	dst, err := os.Create(a.storedPath(id))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "落盘失败"})
		return
	}
	n, err := io.Copy(dst, file)
	dst.Close()
	if err != nil {
		os.Remove(a.storedPath(id))
		if isMaxBytes(err) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("文件超过 %d MB 上限", limitMB)})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "写入失败"})
		return
	}

	now := time.Now()
	expiryDays := parseExpiryDays(r.FormValue("expireDays")) // 表单可选 1/7/30 天,缺省/非法回退 7
	meta := map[string]any{
		"id": id, "name": hdr.Filename, "size": n, "uploader": uploader,
		"time": now.UnixMilli(), "expires": now.Add(time.Duration(expiryDays) * 24 * time.Hour).UnixMilli(),
		"code": genCode(), "public": public, "downloads": 0,
	}
	b, _ := json.Marshal(meta)
	if err := a.store.putEntity("cabinet", id, b); err != nil {
		os.Remove(a.storedPath(id))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "写元数据失败"})
		return
	}
	a.store.appendAudit(uploader, "上传文件", "文件柜 · "+hdr.Filename, "成功")
	writeJSON(w, http.StatusOK, meta)
}

// uploadCabinet 处理 POST /api/cabinet(write):登录用户上传。
func (a *api) uploadCabinet(w http.ResponseWriter, r *http.Request) {
	uploader, _, _ := a.currentUser(r)
	a.storeCabinetFile(w, r, uploader, false)
}

// pubLimits 处理 GET /api/pub/limits(免登录):供 /drop 页展示真实上限、据此做客户端大小预检,
// 并在匿名上传未开启时直接置灰提示。只暴露上限与开关,不泄露其它配置。
func (a *api) pubLimits(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"cabinetMaxMB": a.cabinetMaxBytes >> 20,
		"anonUpload":   a.anonUpload,
	})
}

// uploadCabinetAnon 处理 POST /api/pub/cabinet(免登录,需 cabinet.anon_upload=true):
// 匿名上传,文件即公开(凭返回的提取码下载);上传者记为来源 IP,供登录用户在列表追溯。
func (a *api) uploadCabinetAnon(w http.ResponseWriter, r *http.Request) {
	if !a.anonUpload {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "匿名上传未开启(管理员需在 config.toml 设 cabinet.anon_upload=true)"})
		return
	}
	a.storeCabinetFile(w, r, clientIP(r)+"(匿名)", true)
}

// cleanupExpiredCabinet 删除已过期的文件柜条目(元数据 + 落盘字节);由后台定时任务调用。
func (a *api) cleanupExpiredCabinet() int {
	ids := a.store.expiredCabinet(time.Now().UnixMilli())
	for _, id := range ids {
		a.store.deleteEntity("cabinet", id)
		os.Remove(a.storedPath(id))
	}
	return len(ids)
}

// serveFile 流式回传文件并强制 attachment(防 XSS),顺带把下载计数 +1 落库。
func (a *api) serveFile(w http.ResponseWriter, meta map[string]any) {
	id, _ := meta["id"].(string)
	name, _ := meta["name"].(string)
	f, err := os.Open(a.storedPath(id))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "文件不存在或已清理"})
		return
	}
	defer f.Close()

	if dl, ok := meta["downloads"].(float64); ok {
		meta["downloads"] = dl + 1
		if b, e := json.Marshal(meta); e == nil {
			a.store.putEntity("cabinet", id, b)
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", urlEscape(name)))
	io.Copy(w, f)
}

// downloadCabinet 处理 GET /api/cabinet/{id}/download(登录,任意角色):按 id 下载。
func (a *api) downloadCabinet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	raw, ok := a.store.getEntity("cabinet", id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "文件不存在"})
		return
	}
	var meta map[string]any
	json.Unmarshal(raw, &meta)
	a.serveFile(w, meta)
}

// downloadByCode 处理 GET /api/pubfile/{code}(免登录):仅当文件标记为公开时可凭码下载。
func (a *api) downloadByCode(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	meta, ok := a.store.cabinetByCode(code)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "提取码无效"})
		return
	}
	if pub, _ := meta["public"].(bool); !pub {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "该文件未公开,请登录后下载"})
		return
	}
	a.serveFile(w, meta)
}

// pubfileMeta 处理 GET /api/pubfile/{code}/meta(免登录):仅回元数据(名/大小/过期),不回文件体、不计下载数。
// 供 /drop 页凭码校验并展示文件信息后再触发下载,避免「为校验而整文件下载一遍」。
func (a *api) pubfileMeta(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	meta, ok := a.store.cabinetByCode(code)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "提取码无效"})
		return
	}
	if pub, _ := meta["public"].(bool); !pub {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "该文件未公开,请登录后下载"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": meta["name"], "size": meta["size"], "expires": meta["expires"], "code": meta["code"],
	})
}

// deleteCabinet 处理 DELETE /api/cabinet/{id}(write):删元数据 + 落盘文件。
func (a *api) deleteCabinet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.store.deleteEntity("cabinet", id)
	os.Remove(a.storedPath(id))
	a.store.appendAudit(a.sessionUser(r), "删除文件", "文件柜 · "+id, "成功")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// urlEscape 仅转义 Content-Disposition filename* 所需的少量字符。
func urlEscape(s string) string {
	r := strings.NewReplacer(" ", "%20", "\"", "%22", "\\", "%5C", "\n", "", "\r", "")
	return r.Replace(s)
}
