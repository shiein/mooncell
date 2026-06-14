package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// execStart:go-binary / python(venv 解释器)的命令生成。
func TestExecStart(t *testing.T) {
	// go-binary:直跑二进制 + 启动参数
	got, err := execStart(DeployConfig{Type: "go-binary", BinPath: "/srv/apps/x/app", Args: "--port 80"})
	if err != nil || got != "/srv/apps/x/app --port 80" {
		t.Fatalf("go-binary execStart = %q, err=%v", got, err)
	}
	// python:指定 venv 解释器时用之,不查 PATH
	got, err = execStart(DeployConfig{Type: "python", Interpreter: "/srv/apps/x/venv/bin/python", BinPath: "/srv/apps/x/app.py"})
	if err != nil || got != "/srv/apps/x/venv/bin/python /srv/apps/x/app.py" {
		t.Fatalf("python venv execStart = %q, err=%v", got, err)
	}
	// node:指定 node 路径时用之
	got, err = execStart(DeployConfig{Type: "node", Interpreter: "/usr/local/bin/node", BinPath: "/srv/apps/x/server.js"})
	if err != nil || got != "/usr/local/bin/node /srv/apps/x/server.js" {
		t.Fatalf("node execStart = %q, err=%v", got, err)
	}
}

// writePm2Eco:各类型 interpreter 正确(python 吃 venv)。
func TestWritePm2Eco(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		cfg        DeployConfig
		wantInterp string
	}{
		{DeployConfig{Type: "go-binary", BinPath: filepath.Join(dir, "a")}, "none"},
		{DeployConfig{Type: "python", BinPath: filepath.Join(dir, "b"), Interpreter: "/venv/bin/python"}, "/venv/bin/python"},
		{DeployConfig{Type: "python", BinPath: filepath.Join(dir, "c")}, "python3"},
		{DeployConfig{Type: "java-jar", BinPath: filepath.Join(dir, "d")}, "java"},
		{DeployConfig{Type: "node", BinPath: filepath.Join(dir, "e"), Interpreter: "/usr/local/bin/node"}, "/usr/local/bin/node"},
		{DeployConfig{Type: "node", BinPath: filepath.Join(dir, "f")}, "node"},
	}
	for _, c := range cases {
		path, err := writePm2Eco(c.cfg)
		if err != nil {
			t.Fatalf("writePm2Eco err: %v", err)
		}
		var eco struct {
			Apps []map[string]any `json:"apps"`
		}
		b, _ := os.ReadFile(path)
		json.Unmarshal(b, &eco)
		if got := eco.Apps[0]["interpreter"]; got != c.wantInterp {
			t.Errorf("type=%s interpreter = %v, want %v", c.cfg.Type, got, c.wantInterp)
		}
	}
}

// runReload:白名单外动作必须被拒绝且不执行;空动作跳过。
func TestRunReloadWhitelist(t *testing.T) {
	if ran, _, err := runReload(""); ran || err != nil {
		t.Fatalf("空动作应跳过: ran=%v err=%v", ran, err)
	}
	// 任意 shell 串(注入意图)必须被白名单拒绝,而不是当 sh -c 执行
	ran, _, err := runReload("rm -rf / ; curl evil")
	if !ran || err == nil || !strings.Contains(err.Error(), "disallowed") {
		t.Fatalf("白名单外动作应被拒绝: ran=%v err=%v", ran, err)
	}
	// 白名单内动作名应被识别(实际 exec 可能因环境无 nginx 失败,这里只验“被允许而非拒绝”)
	if _, _, err := runReload("nginx-reload"); err != nil && strings.Contains(err.Error(), "disallowed") {
		t.Fatalf("白名单内动作不应被判为 disallowed")
	}
}

// processHealthy:未配置 HTTP 健康检查时,进程未存活必须判失败(杜绝启动失败被判成功)。
func TestProcessHealthyGate(t *testing.T) {
	var logs []string
	if processHealthy("", false, &logs) {
		t.Fatal("无健康检查 + 进程未存活,应判失败")
	}
	logs = nil
	if !processHealthy("", true, &logs) {
		t.Fatal("无健康检查 + 进程存活,应判通过")
	}
}

// copyToTemp:还原源保护——拷贝独立副本,清理后删除。
func TestCopyToTemp(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	os.WriteFile(src, []byte("artifact-bytes"), 0644)

	tmp, cleanup, err := copyToTemp(src)
	if err != nil {
		t.Fatalf("copyToTemp err: %v", err)
	}
	if b, _ := os.ReadFile(tmp); string(b) != "artifact-bytes" {
		t.Fatalf("临时副本内容不符: %q", b)
	}
	// 删掉源后副本仍在(证明独立,免疫滚动清理)
	os.Remove(src)
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("源删除后临时副本应仍存在")
	}
	cleanup()
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("cleanup 后临时副本应被删除")
	}
}

// validateUnitFields:换行/控制字符注入必须被拒绝。
func TestValidateUnitFields(t *testing.T) {
	if err := validateUnitFields(DeployConfig{Name: "ok", BinPath: "/srv/apps/x/app", User: "root"}); err != nil {
		t.Fatalf("正常字段不应报错: %v", err)
	}
	bad := []DeployConfig{
		{Name: "x\n[Service]\nExecStartPre=/evil", BinPath: "/srv/apps/x/app"},
		{Name: "x", BinPath: "/srv/apps/x/app", User: "root\nExecStartPre=evil"},
		{Name: "x", BinPath: "/srv/apps/x/app", Env: map[string]string{"K": "v\nEnvironment=INJECT=1"}},
	}
	for i, c := range bad {
		if err := validateUnitFields(c); err == nil {
			t.Errorf("用例 %d 含换行注入,应被拒绝", i)
		}
	}
}

// processHealthy + rollback:验证空健康检查 + 进程未存活判失败的逻辑(回滚路径同样适用)。

// Agent 侧 releaseId 幂等:记录成功后,同 releaseId 命中缓存;失败不记录。
func TestReleaseIdempotency(t *testing.T) {
	a := &agent{cfg: &Config{Paths: PathsConfig{BackupDir: t.TempDir()}}}
	a.recordRelease("deploy", "app1", "R1", DeployResult{Result: "success", Version: "v1"})
	if cached, ok := a.releaseDone("deploy", "app1", "R1"); !ok || cached.Version != "v1" {
		t.Fatalf("记录后应命中: ok=%v ver=%q", ok, cached.Version)
	}
	// 不同应用不应互相命中
	if _, ok := a.releaseDone("deploy", "app2", "R1"); ok {
		t.Error("不同应用同 releaseId 不应命中")
	}
	// 还原与部署独立命名空间,不应互相命中
	if _, ok := a.releaseDone("restore", "app1", "R1"); ok {
		t.Error("还原不应命中部署的幂等记录")
	}
	// 失败结果不记为命中
	a.recordRelease("deploy", "app1", "R2", DeployResult{Result: "failed"})
	if _, ok := a.releaseDone("deploy", "app1", "R2"); ok {
		t.Error("失败结果不应被当作幂等命中")
	}
}

// flattenSingleTopDir:单一顶层目录上提,散落文件 / 单文件不动。
func TestFlattenSingleTopDir(t *testing.T) {
	// 单一顶层目录 → 内容上提,去掉该层
	d := t.TempDir()
	os.MkdirAll(filepath.Join(d, "myapp-v1"), 0755)
	os.WriteFile(filepath.Join(d, "myapp-v1", "a.txt"), []byte("a"), 0644)
	os.WriteFile(filepath.Join(d, "myapp-v1", "b.txt"), []byte("b"), 0644)
	if err := flattenSingleTopDir(d); err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(d, "a.txt")) || fileExists(filepath.Join(d, "myapp-v1")) {
		t.Error("单一顶层目录应被去掉、内容上提")
	}

	// 散落文件 → 原样保留
	d2 := t.TempDir()
	os.WriteFile(filepath.Join(d2, "index.html"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(d2, "app.js"), []byte("y"), 0644)
	flattenSingleTopDir(d2)
	if !fileExists(filepath.Join(d2, "index.html")) || !fileExists(filepath.Join(d2, "app.js")) {
		t.Error("散落文件应原样保留")
	}

	// 单个顶层文件(非目录)→ 不动
	d3 := t.TempDir()
	os.WriteFile(filepath.Join(d3, "only.py"), []byte("z"), 0644)
	flattenSingleTopDir(d3)
	if !fileExists(filepath.Join(d3, "only.py")) {
		t.Error("单个顶层文件应保留")
	}
}

// extractArchiveSmart:tar.gz 嗅探 + 解包 + 智能去顶层目录(端到端)。
func TestExtractArchiveSmart(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "pkg.tar.gz")
	// 造一个含单一顶层目录 release/ 的 tar.gz
	makeTarGz(t, archive, map[string]string{
		"release/main.py":     "print(1)",
		"release/lib/util.py": "x=1",
	})
	if sniffArchive(archive) != "gzip" {
		t.Fatalf("应嗅探为 gzip")
	}
	dest := filepath.Join(dir, "out")
	if err := extractArchiveSmart(archive, dest, "gzip"); err != nil {
		t.Fatal(err)
	}
	// 顶层 release/ 应被去掉,main.py 直接在 dest 下
	if !fileExists(filepath.Join(dest, "main.py")) || !fileExists(filepath.Join(dest, "lib", "util.py")) {
		t.Error("解包后应智能去掉顶层 release/,入口直达 dest")
	}
}

// 安全解包:拒绝路径穿越(zip-slip)与链接条目。
func TestExtractRejectsTraversalAndLinks(t *testing.T) {
	dir := t.TempDir()
	// 穿越条目
	slip := filepath.Join(dir, "slip.tar.gz")
	makeTarGz(t, slip, map[string]string{"../evil.txt": "pwned"})
	if err := extractArchive(slip, filepath.Join(dir, "out1"), "gzip"); err == nil {
		t.Error("含 ../ 穿越的压缩包应被拒绝")
	}
	if fileExists(filepath.Join(dir, "evil.txt")) {
		t.Fatal("穿越文件不应被写到 dest 外")
	}
	// 软链接条目
	link := filepath.Join(dir, "link.tar.gz")
	makeTarGzCustom(t, link, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{Name: "bad", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	})
	if err := extractArchive(link, filepath.Join(dir, "out2"), "gzip"); err == nil {
		t.Error("含符号链接的压缩包应被拒绝")
	}
}

// 失败安全:多文件部署解包失败(缺入口)时旧目录不被破坏。
func TestSwapDirFailureKeepsOld(t *testing.T) {
	dir := t.TempDir()
	appDir := filepath.Join(dir, "app")
	os.MkdirAll(appDir, 0755)
	os.WriteFile(filepath.Join(appDir, "main.py"), []byte("OLD"), 0644)
	os.WriteFile(filepath.Join(appDir, "keep.txt"), []byte("data"), 0644)
	// 包里没有入口 main.py
	pkg := filepath.Join(dir, "pkg.tar.gz")
	makeTarGz(t, pkg, map[string]string{"other.py": "x"})
	if err := swapDirFromArchive(appDir, pkg, "gzip", "main.py"); err == nil {
		t.Fatal("缺入口应失败")
	}
	// 旧目录与内容必须完好
	if b, _ := os.ReadFile(filepath.Join(appDir, "main.py")); string(b) != "OLD" {
		t.Error("解包失败后旧入口应保留")
	}
	if !fileExists(filepath.Join(appDir, "keep.txt")) {
		t.Error("解包失败后旧目录其它文件应保留")
	}
	// 不应残留 staging/.old
	if fileExists(appDir+".staging") || fileExists(appDir+".old") {
		t.Error("不应残留 staging/.old")
	}
}

func makeTarGzCustom(t *testing.T, path string, fn func(*tar.Writer)) {
	t.Helper()
	f, _ := os.Create(path)
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	fn(tw)
	tw.Close()
	gz.Close()
}

func makeTarGz(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(content))})
		tw.Write([]byte(content))
	}
	tw.Close()
	gz.Close()
}

// withinRoots:路径穿越防护。
func TestWithinRoots(t *testing.T) {
	roots := []string{"/srv/apps"}
	if !withinRoots("/srv/apps/x/app", roots) {
		t.Error("白名单内路径应通过")
	}
	if withinRoots("/srv/apps/../etc/passwd", roots) {
		t.Error("穿越路径应被拒绝")
	}
	if withinRoots("/etc/passwd", roots) {
		t.Error("白名单外路径应被拒绝")
	}
}

// withinRoots:白名单目录内的软链指向白名单外,必须被识破(EvalSymlinks 真实路径边界)。
func TestWithinRootsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatal(err)
	}
	roots := []string{root}

	// 1. 白名单内的普通(尚不存在的)目标:通过
	if !withinRoots(filepath.Join(root, "app", "bin"), roots) {
		t.Error("白名单内普通路径应通过")
	}
	// 2. root 内有个软链 evil → 白名单外目录;经它的路径必须被拒
	link := filepath.Join(root, "evil")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("建软链失败: %v", err)
	}
	if withinRoots(filepath.Join(link, "payload"), roots) {
		t.Error("经白名单内软链逃逸到白名单外的路径必须被拒绝")
	}
	// 3. 直接写到软链目标真实路径(白名单外):拒绝
	if withinRoots(filepath.Join(outside, "x"), roots) {
		t.Error("白名单外真实路径应被拒绝")
	}
}

// static 部署:reload 失败必须阻断部署(不报 success)。测试环境无 nginx,nginx-reload 必失败。
func TestStaticDeployReloadFailureBlocks(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "site")
	arch := filepath.Join(dir, "site.tar.gz")
	makeTarGz(t, arch, map[string]string{"index.html": "<h1>v1</h1>"})
	a := &agent{}
	cfg := DeployConfig{ID: "t-static", Type: "static-nginx", BinPath: binPath, Version: "v1", BackupKeep: 3, ReloadCmd: "nginx-reload"}
	res := a.runDeployStatic(cfg, arch, func(Step) {})
	if res.Result == "success" {
		t.Fatalf("reload 失败时部署不应报 success,got %q", res.Result)
	}
}

// writeExtracted:单文件超出解包上限必须报错,不允许静默截断落一个残缺文件。
func TestExtractRejectsOversizeEntry(t *testing.T) {
	old := maxEntryBytes
	maxEntryBytes = 16 // 临时缩小上限便于测试
	defer func() { maxEntryBytes = old }()

	dir := t.TempDir()
	arch := filepath.Join(dir, "big.tar.gz")
	makeTarGzCustom(t, arch, func(tw *tar.Writer) {
		data := strings.Repeat("A", 64) // 64B > 16B 上限
		tw.WriteHeader(&tar.Header{Name: "huge.bin", Mode: 0644, Size: int64(len(data))})
		tw.Write([]byte(data))
	})
	err := extractArchive(arch, filepath.Join(dir, "out"), "gzip")
	if err == nil || !strings.Contains(err.Error(), "超出解包上限") {
		t.Fatalf("超限文件应报错,got err=%v", err)
	}
}
