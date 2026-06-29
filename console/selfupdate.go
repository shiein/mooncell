package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// Console 自更新:管理员从浏览器直传新 Console 二进制(//go:embed all:dist 内嵌前端,故换一个二进制
// = 前后端一起升级)→ 本机校验 sha256 + ELF 架构 + --selftest + --version → 备份当前为 <exe>.old →
// 原子替换自身 → 延迟 syscall.Exec 用新二进制就地重启(同 PID,适配 nohup 无监管场景)。
// 与 Agent 自更新(agent/update_api.go)同款范式,只是触发由「Console 跨机推送」改为「浏览器直传本机」,
// 少一整条跨机传输链。admin-only:admin 已是最高信任(能推 Agent 包、部署任意制品),不放大边界。

// selfUpdateMaxBytes 是自更新二进制上传硬上限(Console 二进制约 15–40MB,给足余量,与 Agent 同量级)。
const selfUpdateMaxBytes = 256 << 20

// consoleInfo 处理 GET /api/console/info(任意登录):返回当前 Console 版本 + os/arch,
// 供前端展示当前版本 + 升级后轮询确认重启完成(版本变为新版即重启成功)。
func (a *api) consoleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version": consoleVersion,
		"os":      runtime.GOOS + "/" + runtime.GOARCH,
	})
}

// selfUpdate 处理 POST /api/console/self-update(admin):上传新 Console 二进制 → 校验 → 替换自身 → self-exec 重启。
func (a *api) selfUpdate(w http.ResponseWriter, r *http.Request) {
	// 全局串行:固定临时路径 <exe>.new 与"备份→替换自身"都是非原子临界区,
	// 两个管理员同时推送会互相覆盖,导致最终二进制/sha/版本对不上。并发推送直接 409 拒绝。
	if !a.selfUpdateMu.TryLock() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "已有自更新进行中,请稍后重试"})
		return
	}
	defer a.selfUpdateMu.Unlock()

	// 空闲门禁:self-exec 重启会丢内存状态——在飞部署/还原/启停/下线(busy)与活跃分块上传会话
	// (uploads)都会因重启留下半完成操作/孤儿临时文件/续传 404。非空闲直接 409,不重启。
	if a.anyBusy() || a.hasActiveUploads() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "有部署/还原/上传在进行中,请稍后再自更新"})
		return
	}

	// 自更新是 Linux 部署特性(nohup 无监管场景):darwin 上跑的 Console(开发机)拒绝任何上传,
	// 避免把 linux 包错传到 mac 后看似校验通过却无法 self-exec。给清晰错误文案。
	if runtime.GOOS != "linux" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Console 自更新仅在 Linux 部署下可用(当前 " + runtime.GOOS + "/" + runtime.GOARCH + ")"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, selfUpdateMaxBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		if isMaxBytes(err) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "二进制超过 256MB 上限"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "表单解析失败或超过上限"})
		return
	}
	defer cleanupMultipart(r)
	wantSha := r.FormValue("sha256")
	version := r.FormValue("version")
	file, _, err := r.FormFile("binary")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 binary 字段"})
		return
	}
	defer file.Close()

	exe, err := os.Executable()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "无法定位自身可执行文件: " + err.Error()})
		return
	}
	if resolved, e := filepath.EvalSymlinks(exe); e == nil {
		exe = resolved
	}
	newPath := exe + ".new"

	// 落盘新二进制,同时流式算 sha256(不整文件缓存进内存)。
	out, err := os.OpenFile(newPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "创建临时文件失败: " + err.Error()})
		return
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), file); err != nil {
		out.Close()
		os.Remove(newPath)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "写入失败: " + err.Error()})
		return
	}
	out.Close()

	// 校验链(fail-closed,任一不过即删 .new 保旧版):
	// ① sha256:若表单带 sha256 则比对(传输完整性)。
	// ② 架构:elfArch(.new) == runtime.GOARCH(空=非 linux ELF/不识别 → 拒,拦 Mach-O/PE/跨架构)。
	if msg := validateConsoleSelfUpdate(newPath, hex.EncodeToString(h.Sum(nil)), wantSha); msg != "" {
		os.Remove(newPath)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	// ③ 自检:<exe>.new --selftest 退出码 0(加载当前 config,不绑端口/不开 DB,挡住坏包/动态依赖缺失/配置不兼容)。
	if err := selftestBinary(newPath); err != nil {
		os.Remove(newPath)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "新二进制自检失败,保持旧版: " + err.Error()})
		return
	}
	// ④ 版本:<exe>.new --version 读真实版本。允许同版本(管理员常忘记改版本号);仅用于读取展示
	//    与"声明版本 vs 二进制自报版本"的可选核对(传错包防呆),不阻止同版本覆盖。
	realVer, verr := binaryVersion(newPath)
	if verr != nil {
		os.Remove(newPath)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无法读取新二进制版本: " + verr.Error()})
		return
	}
	if version != "" && realVer != version {
		os.Remove(newPath)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("包版本与声明不符:表单声明 %s,二进制自报 %s", version, realVer)})
		return
	}
	version = realVer // 后续响应/日志一律用二进制自报的真实版本,不再信任表单

	// 备份当前 → <exe>.old(供手工回滚:纯 nohup 模式无监管,升级后若新版崩溃需人工 mv 回滚)。
	if err := copyFile(exe, exe+".old", 0o755); err != nil {
		os.Remove(newPath)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "备份当前版本失败: " + err.Error()})
		return
	}
	// 原子替换自身(Linux 允许 rename 覆盖运行中的可执行文件:旧映像继续用旧 inode 运行)。
	if err := os.Rename(newPath, exe); err != nil {
		os.Remove(newPath)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "替换二进制失败: " + err.Error()})
		return
	}

	a.store.appendAudit(a.sessionUser(r), "Console 自更新", consoleVersion+" → "+version, "成功")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version, "restart": "self-exec", "old": consoleVersion})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// 延迟 self-exec:先让 200 回到浏览器,再用新二进制就地重启(同 PID,端口在新映像启动时重新 bind)。
	go func() {
		time.Sleep(500 * time.Millisecond)
		argv := append([]string{exe}, os.Args[1:]...)
		log.Printf("[self-update] Console %s → %s,exec 新二进制就地重启", consoleVersion, version)
		if err := syscall.Exec(exe, argv, os.Environ()); err != nil {
			// exec 仅在失败时返回(几乎不可能:新二进制已过 --selftest,而 selftest 同样靠 exec)。
			// 当前进程仍是旧映像、继续运行;但磁盘已被换成未经此机实际运行验证的新版,若此时系统重启
			// 会误用它。故把磁盘二进制回滚为已备份的旧版(<exe>.old),让下次重启回到已知可用状态
			// (nohup 无自愈网,这步是底线)。
			if rerr := os.Rename(exe+".old", exe); rerr != nil {
				log.Printf("[self-update] exec 失败且回滚磁盘二进制失败(磁盘仍为新版,重启前请人工核对): exec=%v rollback=%v", err, rerr)
			} else {
				log.Printf("[self-update] exec 失败,已把磁盘二进制回滚为旧版(继续运行旧映像): %v", err)
			}
		}
	}()
}

// validateConsoleSelfUpdate 校验上传的 Console 二进制:sha256 匹配(若提供)+ ELF 架构匹配本机 GOARCH。
// elfArch 返回 "" 即非 linux ELF/不识别(Mach-O/PE/跨架构)→ 拒。
func validateConsoleSelfUpdate(path, gotSha, wantSha string) string {
	if wantSha != "" && !strings.EqualFold(gotSha, wantSha) {
		return "sha256 不匹配(传输损坏或包不一致)"
	}
	got := elfArch(path)
	if got == "" {
		return "架构校验失败:非 linux ELF 或不识别格式(本机 " + runtime.GOOS + "/" + runtime.GOARCH + ")"
	}
	if got != runtime.GOARCH {
		return "架构校验失败:上传包为 linux/" + got + ",本机为 " + runtime.GOOS + "/" + runtime.GOARCH
	}
	return ""
}

// selftestBinary 跑 `<bin> --selftest`,退出码 0 视为可在本机执行且能接受当前 config.toml(不绑端口,不冲突)。
func selftestBinary(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--selftest").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// binaryVersion 跑 `<bin> --version` 取新二进制自报版本(权威),用于与表单声明版本核对。
func binaryVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v (%s)", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// copyFile 复制文件内容与权限模式(供备份当前二进制为 <exe>.old)。
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
