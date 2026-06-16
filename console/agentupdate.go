package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Agent 自更新分发:管理员按架构(linux amd64/arm64)上传一次 agent 二进制到 Console,
// Console 按各 agent 上报的真实架构推送匹配的包到其 /api/self-update。一处上传、多机统一更新。

// agentArchELF 是放开的生产架构 → ELF e_machine(Mooncell agent 统一在 linux x86/arm)。
var agentArchELF = map[string]uint16{"amd64": 0x3E, "arm64": 0xB7}

func (a *api) agentBinPath(arch string) string {
	return filepath.Join(a.agentBinDir, "agent-linux-"+arch)
}
func (a *api) agentBinMetaPath(arch string) string { return a.agentBinPath(arch) + ".json" }

type agentBinMeta struct {
	Arch    string `json:"arch"`
	Version string `json:"version"`
	Sha256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Time    int64  `json:"time"`
}

// elfArch 读 ELF 头返回架构名("amd64"/"arm64");非 ELF 或不识别返回 ""。用于上传时校验文件与声明架构一致。
func elfArch(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 20)
	if n, _ := io.ReadFull(f, head); n < 20 {
		return ""
	}
	if !(head[0] == 0x7f && head[1] == 'E' && head[2] == 'L' && head[3] == 'F') {
		return ""
	}
	var m uint16
	if head[5] == 2 {
		m = uint16(head[18])<<8 | uint16(head[19])
	} else {
		m = uint16(head[19])<<8 | uint16(head[18])
	}
	for arch, em := range agentArchELF {
		if em == m {
			return arch
		}
	}
	return ""
}

// archOf 从 ping 上报的 os 串("linux/amd64")取放开的架构名;非 linux 或不识别架构返回 ""。
// 必须连 OS 一起 fail-closed:Mooncell agent 包只发 linux,"darwin/amd64" 不能被当成 amd64 推 linux 包
// (虽然 Agent 端最终会拒非 ELF,但后端边界也要一致,不把跨 OS 的包送上路)。
func archOf(osStr string) string {
	i := strings.IndexByte(osStr, '/')
	if i < 0 || osStr[:i] != "linux" {
		return ""
	}
	if arch := osStr[i+1:]; agentArchELF[arch] != 0 {
		return arch
	}
	return ""
}

// uploadAgentBinary 处理 POST /api/agent-binary(admin):上传某架构的 agent 二进制(校验确为该架构 ELF)。
func (a *api) uploadAgentBinary(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		if isMaxBytes(err) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "二进制超过 256MB 上限"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "表单解析失败"})
		return
	}
	defer cleanupMultipart(r)
	arch := strings.TrimSpace(r.FormValue("arch"))
	version := strings.TrimSpace(r.FormValue("version"))
	if agentArchELF[arch] == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "arch 仅支持 amd64 / arm64"})
		return
	}
	if version == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "version 不能为空"})
		return
	}
	file, _, err := r.FormFile("binary")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 binary 字段"})
		return
	}
	defer file.Close()
	if err := os.MkdirAll(a.agentBinDir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "创建存储目录失败"})
		return
	}
	// 唯一临时文件(不用固定 <path>.tmp):两个管理员同时上传同一架构时,固定路径会互相覆盖,
	// 可能出现"校验的是 A 写的、rename 的是 B 写一半的"。校验通过后再原子 rename 到正式路径。
	out, err := os.CreateTemp(a.agentBinDir, "agent-"+arch+"-*.tmp")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "落盘失败"})
		return
	}
	tmp := out.Name()
	os.Chmod(tmp, 0o755)
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(out, h), file)
	out.Close()
	if err != nil {
		os.Remove(tmp)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "写入失败"})
		return
	}
	// 校验上传的确是声明架构的 linux ELF(防止把 arm 包错传成 amd64 后推到机器上起不来)。
	if got := elfArch(tmp); got != arch {
		os.Remove(tmp)
		g := got
		if g == "" {
			g = "非 linux ELF"
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "文件与声明架构不符:声明 " + arch + ",实际 " + g})
		return
	}
	if err := os.Rename(tmp, a.agentBinPath(arch)); err != nil {
		os.Remove(tmp)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存失败"})
		return
	}
	meta := agentBinMeta{Arch: arch, Version: version, Sha256: hex.EncodeToString(h.Sum(nil)), Size: size, Time: time.Now().UnixMilli()}
	b, _ := json.Marshal(meta)
	os.WriteFile(a.agentBinMetaPath(arch), b, 0o644)
	a.store.appendAudit(a.sessionUser(r), "上传 Agent 包", "linux/"+arch+" "+version, "成功")
	writeJSON(w, http.StatusOK, meta)
}

// listAgentBinaries 处理 GET /api/agent-binaries:列出已上传的各架构包元数据。
func (a *api) listAgentBinaries(w http.ResponseWriter, r *http.Request) {
	out := []agentBinMeta{}
	for arch := range agentArchELF {
		if b, err := os.ReadFile(a.agentBinMetaPath(arch)); err == nil {
			var m agentBinMeta
			if json.Unmarshal(b, &m) == nil {
				out = append(out, m)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"binaries": out})
}

// updateAgent 处理 POST /api/agents/{id}/update(admin):按目标 agent 上报架构推送匹配的包到其自更新端点。
func (a *api) updateAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cl := a.resolveAgentByID(id)
	if a.unknownAgent(w, cl) {
		return
	}
	// 取 agent 真实架构与当前版本(权威,不信任前端)。
	status, body, err := cl.get("/api/ping")
	if err != nil || status != http.StatusOK {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "Agent 不可达,无法获取架构", "online": false})
		return
	}
	var p struct {
		Os      string `json:"os"`
		Version string `json:"version"`
	}
	json.Unmarshal(body, &p)
	arch := archOf(p.Os)
	if arch == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无法识别 Agent 架构(仅支持 linux amd64/arm64): " + p.Os})
		return
	}
	mb, err := os.ReadFile(a.agentBinMetaPath(arch))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "尚未上传 linux/" + arch + " 的 Agent 包,请先在「Agent 升级包」上传"})
		return
	}
	var meta agentBinMeta
	json.Unmarshal(mb, &meta)
	f, err := os.Open(a.agentBinPath(arch))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Agent 包文件缺失,请重新上传"})
		return
	}
	defer f.Close()

	pr, ct := buildSelfUpdateBody(f, meta.Version, meta.Sha256)
	st, rb, perr := cl.post("/api/self-update", ct, pr)
	user := a.sessionUser(r)
	if perr != nil || st >= 400 {
		a.store.appendAudit(user, "更新 Agent", id+" → "+meta.Version, "失败")
		relayAgent(w, st, rb, perr)
		return
	}
	// Agent 已接受并替换二进制、将 self-exec 就地重启;但 200 是在 exec 之前返回的,新版可能起不来。
	// 必须回探 /api/ping 确认 Agent 以新版本重新上线,才能记"成功"——否则可能升挂了还记成功(nohup 无监管)。
	ok, gotVer := a.waitAgentVersion(cl, meta.Version, 30*time.Second)
	if ok {
		a.store.appendAudit(user, "更新 Agent", id+" "+p.Version+" → "+meta.Version, "成功")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "confirmed": true, "version": gotVer})
		return
	}
	a.store.appendAudit(user, "更新 Agent", id+" → "+meta.Version, "重启未确认")
	writeJSON(w, http.StatusBadGateway, map[string]any{
		"ok": false, "confirmed": false,
		"error": "已推送并替换二进制,但 30s 内未确认 Agent 以新版本重新上线(回探到版本: " + gotVer + ")。" +
			"请检查目标机 Agent 进程;若未恢复,可用备份 <可执行文件>.old 手工回滚。",
	})
}

// waitAgentVersion 自更新推送后回探 Agent:每秒轮询 /api/ping,直到在线且版本==want 或超时。
// 返回是否确认 + 最后一次探到的版本(用于诊断:能 ping 通但版本不对 = 替换/重启出了问题)。
func (a *api) waitAgentVersion(cl *agentClient, want string, timeout time.Duration) (bool, string) {
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		status, body, err := cl.get("/api/ping")
		if err != nil || status != http.StatusOK {
			continue
		}
		var p struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(body, &p) == nil && p.Version != "" {
			last = p.Version
			if p.Version == want {
				return true, p.Version
			}
		}
	}
	return false, last
}

// buildSelfUpdateBody 流式构造发给 Agent /api/self-update 的 multipart(binary + version + sha256)。
func buildSelfUpdateBody(bin io.Reader, version, sha string) (*io.PipeReader, string) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	ct := mw.FormDataContentType()
	go func() {
		var err error
		defer func() { mw.Close(); pw.CloseWithError(err) }()
		if err = mw.WriteField("version", version); err != nil {
			return
		}
		if err = mw.WriteField("sha256", sha); err != nil {
			return
		}
		fw, e := mw.CreateFormFile("binary", "agent")
		if e != nil {
			err = e
			return
		}
		_, err = io.Copy(fw, bin)
	}()
	return pr, ct
}
