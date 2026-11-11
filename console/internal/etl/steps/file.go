package steps

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mooncell/console/internal/etl/core"
)

// file_root（R1-B1）是 csv_input/csv_output 等文件类步骤的相对路径根目录：
// path 一律解释为根目录下的相对路径，绝对路径与越界（..、符号链接指向根外）
// 在编译期与运行期都拒绝，对管理员同样生效。由 Service 注入 Env；
// 运行环境没有 file_root 时文件步骤拒绝运行（fail closed，防误配置放开任意路径）。
type fileRootKey struct{}

// FileRootEntry 返回注入 Env 的键值对；root 为目录路径（内部规范化为绝对路径并解析符号链接）。
func FileRootEntry(root string) (any, any) {
	if abs, err := filepath.Abs(root); err == nil {
		if res, err2 := filepath.EvalSymlinks(abs); err2 == nil {
			abs = res
		}
		root = abs
	}
	return fileRootKey{}, root
}

func fileRootOf(env core.Env) (string, bool) {
	if env == nil {
		return "", false
	}
	s, ok := env.Lookup(fileRootKey{}).(string)
	return s, ok && s != ""
}

// cleanRel 词法校验：必须是根目录下的相对路径且不越界；返回清洗后的相对路径。
func cleanRel(p string) (string, bool) {
	if p == "" || filepath.IsAbs(p) {
		return "", false
	}
	c := filepath.Clean(p)
	if c == ".." || strings.HasPrefix(c, ".."+string(filepath.Separator)) {
		return "", false
	}
	return c, true
}

// withinRoot 报告 resolved（已规范化）是否位于 root（已规范化绝对路径）之内。
func withinRoot(root, resolved string) bool {
	return resolved == root || strings.HasPrefix(resolved, root+string(filepath.Separator))
}

// escapesRoot 探测 full 的已存在最近祖先：其符号链接解析结果不在 root 内即越界。
// 路径尚不存在时沿祖先向上找；已存在部分（可能是符号链接目录）解析后仍在根内即安全。
func escapesRoot(root, full string) bool {
	probe := full
	for {
		if _, err := os.Lstat(probe); err == nil {
			res, err := filepath.EvalSymlinks(probe)
			return err == nil && !withinRoot(root, res)
		} else if !os.IsNotExist(err) {
			return false // 权限等异常留给运行期报告
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return false
		}
		probe = parent
	}
}

// checkFilePath 编译期校验：词法越界与「已存在最近祖先解析到根外」都报错。
func checkFilePath(ic *core.InferContext, path, fieldPath string) {
	rel, ok := cleanRel(path)
	if !ok {
		ic.Errorf(fieldPath, "path_escape", "文件路径须为 file_root 下的相对路径，不能是绝对路径或含 .. 越界")
		return
	}
	root, ok := fileRootOf(ic.Env)
	if !ok {
		return // 无根环境只做词法校验（运行期仍会强制）
	}
	if escapesRoot(root, filepath.Join(root, rel)) {
		ic.Errorf(fieldPath, "path_escape", "文件路径经符号链接解析后越出 file_root")
	}
}

// resolveFilePath 运行期解析：词法 + 符号链接校验后返回 root 内绝对路径。
// forWrite=true 时目标文件可不存在——校验父目录（必须已存在，不自动建目录）；
// 若目标文件本身已存在（可能是符号链接），其解析结果也须在根内。
func resolveFilePath(env core.Env, path string, forWrite bool) (string, error) {
	rel, ok := cleanRel(path)
	if !ok {
		return "", fmt.Errorf("文件路径须为 file_root 下的相对路径")
	}
	root, ok := fileRootOf(env)
	if !ok || root == "" {
		return "", fmt.Errorf("运行环境未配置 file_root，文件步骤不可用")
	}
	full := filepath.Join(root, rel)
	if !forWrite {
		res, err := filepath.EvalSymlinks(full)
		if err != nil {
			return "", fmt.Errorf("文件不可用：%w", err)
		}
		if !withinRoot(root, res) {
			return "", fmt.Errorf("文件路径经符号链接解析后越出 file_root")
		}
		return res, nil
	}
	dir := filepath.Dir(full)
	rd, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("父目录不可用：%w", err)
	}
	if !withinRoot(root, rd) {
		return "", fmt.Errorf("文件路径经符号链接解析后越出 file_root")
	}
	if _, err := os.Lstat(full); err == nil {
		if res, err := filepath.EvalSymlinks(full); err != nil || !withinRoot(root, res) {
			return "", fmt.Errorf("文件路径经符号链接解析后越出 file_root")
		}
	}
	return full, nil
}
