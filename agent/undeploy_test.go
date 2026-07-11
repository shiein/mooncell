package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// callUndeploy 用给定 query 驱动 undeploy 处理器,返回 HTTP 码与解码后的响应体。
func callUndeploy(t *testing.T, a *agent, id, query string) (int, map[string]any) {
	t.Helper()
	url := "/api/apps/" + id
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodDelete, url, nil)
	req.SetPathValue("id", id)
	rr := httptest.NewRecorder()
	a.undeploy(rr, req)
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体非 JSON: %v, raw=%s", err, rr.Body.String())
	}
	return rr.Code, body
}

// TestUndeployStaticNginxSymlinkRemoved:软链托管下线的正常路径——对外软链被删除后,
// 停后复验通过,返回 200 ok=true。这是「真下线成功」的基准。
func TestUndeployStaticNginxSymlinkRemoved(t *testing.T) {
	root := t.TempDir()
	release := filepath.Join(root, "release")
	if err := os.Mkdir(release, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "current")
	if err := os.Symlink(release, link); err != nil {
		t.Fatal(err)
	}
	a := &agent{cfg: &Config{Paths: PathsConfig{DeployRoots: []string{root}}}}

	code, body := callUndeploy(t, a, "app1", "binPath="+link)

	if code != http.StatusOK {
		t.Fatalf("软链删除成功应返回 200,got %d body=%v", code, body)
	}
	if ok, _ := body["ok"].(bool); !ok {
		t.Fatalf("应 ok=true,got %v", body)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("对外软链应已被删除,Lstat err=%v", err)
	}
}

// TestUndeployReportsStillPresentAsFailure:回归——下线动作被拒(此处用不可写目录令
// os.Remove 失败,等价于 stop 权限不足 / 进程 SIGKILL 免疫的伪成功场景),停后复验探到目标
// 仍存活,必须返回非 2xx 而非旧行为的固定 200。Console 的 status>=300 判定据此中止删元数据,
// 不再留下失管进程。root 下 os.Remove 会绕过权限位,故 root 跳过。
func TestUndeployReportsStillPresentAsFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 绕过目录写权限,无法构造删除失败,跳过")
	}
	root := t.TempDir()
	web := filepath.Join(root, "web")
	if err := os.Mkdir(web, 0o755); err != nil {
		t.Fatal(err)
	}
	release := filepath.Join(root, "release")
	if err := os.Mkdir(release, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(web, "current")
	if err := os.Symlink(release, link); err != nil {
		t.Fatal(err)
	}
	// 去掉父目录写权限:os.Remove(link) 因 EACCES 失败,软链残留。r-x 保留,Lstat/遍历仍可。
	if err := os.Chmod(web, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(web, 0o755) // 还原,否则 t.TempDir 清理失败

	a := &agent{cfg: &Config{Paths: PathsConfig{DeployRoots: []string{root}}}}

	code, body := callUndeploy(t, a, "app1", "binPath="+link)

	if code < 300 {
		t.Fatalf("软链未能移除(下线未生效)应返回非 2xx,却得到 %d body=%v", code, body)
	}
	if ok, _ := body["ok"].(bool); ok {
		t.Fatalf("下线未生效应 ok=false,got %v", body)
	}
	// 软链应仍在(正是复验判定"仍存活"的依据)。
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("前置条件破坏:软链本应残留,Lstat err=%v", err)
	}
}

// TestUndeployStillAlive_SymlinkGuardedByRoots:越界 binPath 软链不能让复验永远判"仍存活"
// ——否则一个配置到 deploy_roots 之外的应用将永远删不掉。清理块跳过它,复验也须跳过。
func TestUndeployStillAlive_SymlinkGuardedByRoots(t *testing.T) {
	outside := t.TempDir() // 不在 DeployRoots 内
	release := filepath.Join(outside, "release")
	if err := os.Mkdir(release, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(outside, "current")
	if err := os.Symlink(release, link); err != nil {
		t.Fatal(err)
	}
	a := &agent{cfg: &Config{Paths: PathsConfig{DeployRoots: []string{t.TempDir()}}}}

	if alive, detail := a.undeployStillAlive("app1", "", "", link); alive {
		t.Fatalf("越界软链不应被判仍存活(否则应用永远删不掉),detail=%s", detail)
	}
}
