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

// Agent 自更新:Console 推送新 agent 二进制(已按架构匹配)→ 本机校验 sha256 + ELF 架构 + 自检
// → 备份当前为 <exe>.old → 原子替换自身 → 延迟 syscall.Exec 用新二进制就地重启(同 PID,适配
// nohup 无监管场景)。Console↔Agent 共享 token 已是部署级信任面,自更新不额外扩大信任边界。

// selfUpdateMaxBytes 是自更新二进制上传硬上限(单 Go 二进制通常 10–30MB,给足余量)。
const selfUpdateMaxBytes = 256 << 20

// selfUpdate 处理 POST /api/self-update(token)。
func (a *agent) selfUpdate(w http.ResponseWriter, r *http.Request) {
	// 全局串行:固定临时路径 <exe>.new 与"备份→替换自身"都是非原子临界区,
	// 两个管理员同时推送会互相覆盖,导致最终二进制/sha/版本对不上。并发推送直接 409 拒绝。
	if !a.selfUpdateMu.TryLock() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "已有自更新进行中,请稍后重试"})
		return
	}
	defer a.selfUpdateMu.Unlock()

	r.Body = http.MaxBytesReader(w, r.Body, selfUpdateMaxBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "表单解析失败或超过上限"})
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			r.MultipartForm.RemoveAll()
		}
	}()
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

	// 校验:sha256 一致 + ELF 架构匹配本机 GOARCH(复用部署制品的架构校验)。
	if msg := validateSelfUpdate(newPath, hex.EncodeToString(h.Sum(nil)), wantSha); msg != "" {
		os.Remove(newPath)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	// 自检:新二进制能在本机起来 + 接受当前 config.toml(挡住坏包/动态依赖缺失/配置不兼容;
	// 纯 nohup 无自愈网,自检是关键前置闸)。
	if err := selftestBinary(newPath); err != nil {
		os.Remove(newPath)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "新二进制自检失败,保持旧版: " + err.Error()})
		return
	}
	// 版本核对:以新二进制自报 --version 为权威,与 Console 声明版本比对——防止误标/传错包(上传旧包
	// 却填新版本号会"看起来升级成功")。最终验收只能在 Agent 端做(Console 不能执行跨架构包)。
	realVer, verr := binaryVersion(newPath)
	if verr != nil {
		os.Remove(newPath)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无法读取新二进制版本: " + verr.Error()})
		return
	}
	if version != "" && realVer != version {
		os.Remove(newPath)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("包版本与声明不符:Console 声明 %s,二进制自报 %s", version, realVer)})
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

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version, "restart": "self-exec", "old": agentVersion})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// 延迟 self-exec:先让 200 回到 Console,再用新二进制就地重启(同 PID,端口在新映像启动时重新 bind)。
	go func() {
		time.Sleep(500 * time.Millisecond)
		argv := append([]string{exe}, os.Args[1:]...)
		log.Printf("[self-update] %s → %s,exec 新二进制就地重启", agentVersion, version)
		if err := syscall.Exec(exe, argv, os.Environ()); err != nil {
			// exec 仅在失败时返回(几乎不可能:新二进制已过 --selftest,而 selftest 同样靠 exec)。
			// 当前进程仍是旧映像、继续运行;但磁盘已被换成未经此机实际运行验证的新版,若此时系统重启
			// 会误用它。故把磁盘二进制回滚为已备份的旧版(<exe>.old),让下次重启回到已知可用状态。
			if rerr := os.Rename(exe+".old", exe); rerr != nil {
				log.Printf("[self-update] exec 失败且回滚磁盘二进制失败(磁盘仍为新版,重启前请人工核对): exec=%v rollback=%v", err, rerr)
			} else {
				log.Printf("[self-update] exec 失败,已把磁盘二进制回滚为旧版(继续运行旧映像): %v", err)
			}
		}
	}()
}

// validateSelfUpdate 校验上传的 agent 二进制:sha256 匹配(若提供)+ ELF 架构匹配本机 GOARCH。
func validateSelfUpdate(path, gotSha, wantSha string) string {
	if wantSha != "" && !strings.EqualFold(gotSha, wantSha) {
		return "sha256 不匹配(传输损坏或包不一致)"
	}
	if msg := checkNativeBinary(path); msg != "" {
		return "架构校验失败: " + msg + "(本机 " + runtime.GOOS + "/" + runtime.GOARCH + ")"
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

// binaryVersion 跑 `<bin> --version` 取新二进制自报版本(权威),用于与 Console 声明版本核对。
func binaryVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v (%s)", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
