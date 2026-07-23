package agentapp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// sha256Hex 复算与 sha256File 同格式(hex 小写)的摘要,供测试构造匹配 meta。
func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// writeBackup 在 BackupDir/<id>/<ts>/ 造一份备份:artifactName(app 或 app.tar.gz)+ meta.json。
func writeBackup(t *testing.T, backupDir, id, ts, artifactName string, artifactBytes []byte, metaVer, metaSha string) {
	t.Helper()
	dir := filepath.Join(backupDir, id, ts)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, artifactName), artifactBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"version":%q,"sha256":%q,"time":1,"operator":"console"}`, metaVer, metaSha)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestResolveBackupVersion_MetaWinsOverClient:单文件备份,sha 一致时——权威版本取 meta,
// 覆盖客户端提交的任意版本;实际比对了 sha256。这是 #4 的核心:还原旧字节不能被写成伪造新版本。
func TestResolveBackupVersion_MetaWinsOverClient(t *testing.T) {
	backupDir := t.TempDir()
	bytes := []byte("real-old-artifact")
	writeBackup(t, backupDir, "app1", "ts1", "app", bytes, "v1-real", sha256Hex(bytes))
	a := &agent{cfg: &Config{Paths: PathsConfig{BackupDir: backupDir}}}

	artifact := filepath.Join(backupDir, "app1", "ts1", "app")
	ver, checked, err := a.resolveBackupVersion("app1", "ts1", artifact, "v2-forged")
	if err != nil {
		t.Fatalf("sha 一致不应报错: %v", err)
	}
	if ver != "v1-real" {
		t.Fatalf("权威版本应取备份 meta v1-real,got %q", ver)
	}
	if !checked {
		t.Fatalf("单文件备份且 meta 有 sha,应实际校验了 sha256")
	}
}

// TestResolveBackupVersion_ShaMismatchRejected:单文件备份 sha 不符(损坏/篡改)→ 报错,
// 调用方据此拒绝还原,不拿坏字节重跑流水线。
func TestResolveBackupVersion_ShaMismatchRejected(t *testing.T) {
	backupDir := t.TempDir()
	writeBackup(t, backupDir, "app1", "ts1", "app", []byte("actual-bytes"), "v1", "deadbeef-wrong-sha")
	a := &agent{cfg: &Config{Paths: PathsConfig{BackupDir: backupDir}}}

	artifact := filepath.Join(backupDir, "app1", "ts1", "app")
	_, _, err := a.resolveBackupVersion("app1", "ts1", artifact, "v2")
	if err == nil {
		t.Fatalf("sha256 不符应报错拒绝还原")
	}
}

// TestResolveBackupVersion_NoMetaFallsBackToClient:老备份无 meta.json → 回退客户端版本、
// 不校验 sha、不阻断还原(保持旧行为的兼容)。
func TestResolveBackupVersion_NoMetaFallsBackToClient(t *testing.T) {
	backupDir := t.TempDir()
	dir := filepath.Join(backupDir, "app1", "ts1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &agent{cfg: &Config{Paths: PathsConfig{BackupDir: backupDir}}}

	ver, checked, err := a.resolveBackupVersion("app1", "ts1", filepath.Join(dir, "app"), "v-client")
	if err != nil {
		t.Fatalf("无 meta 不应报错: %v", err)
	}
	if ver != "v-client" {
		t.Fatalf("无 meta 应回退客户端版本 v-client,got %q", ver)
	}
	if checked {
		t.Fatalf("无 meta.sha256 不应声称校验了 sha")
	}
}

// TestResolveBackupVersion_ArchivedSkipsShaCheck:多文件备份 app.tar.gz——meta.sha256 记的是
// 内部入口文件 sha 而非 tar,不可比,跳过 sha 校验(checked=false)但仍以 meta 版本为准、不误报损坏。
func TestResolveBackupVersion_ArchivedSkipsShaCheck(t *testing.T) {
	backupDir := t.TempDir()
	// tar 内容与 meta.sha256(内部入口文件 sha)故意不同——若误校验会失败。
	writeBackup(t, backupDir, "app1", "ts1", "app.tar.gz", []byte("tarball-bytes"), "v3", "some-inner-file-sha")
	a := &agent{cfg: &Config{Paths: PathsConfig{BackupDir: backupDir}}}

	artifact := filepath.Join(backupDir, "app1", "ts1", "app.tar.gz")
	ver, checked, err := a.resolveBackupVersion("app1", "ts1", artifact, "v-client")
	if err != nil {
		t.Fatalf("多文件备份应跳过 sha 校验、不报错,got %v", err)
	}
	if ver != "v3" {
		t.Fatalf("权威版本应取 meta v3,got %q", ver)
	}
	if checked {
		t.Fatalf("app.tar.gz 不应比对 sha256(无可比真值)")
	}
}
