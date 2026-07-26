// SFTP 路径工具与远端文件名校验。
// 远端路径统一用 path 包（非 filepath），避免 Console 操作系统分隔符干扰。
package serverops

import (
	"path"
	"strings"
	"unicode"
)

// cleanRemotePath 规范化远端路径；拒绝 NUL 与控制字符。
// 相对路径保留为相对；"." 表示远端 cwd。
func cleanRemotePath(p string) (string, error) {
	if p == "" {
		p = "."
	}
	if strings.ContainsRune(p, 0) || hasControl(p) {
		return "", apiErr(CodeValidation, "路径含非法字符", false)
	}
	// 统一为正斜杠风格；path.Clean 处理 . 与 ..
	p = strings.ReplaceAll(p, "\\", "/")
	cleaned := path.Clean(p)
	if cleaned == "" {
		cleaned = "."
	}
	return cleaned, nil
}

// validateFilename 上传文件名：单个组件，禁止 /、.、..、NUL。
func validateFilename(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return apiErr(CodeValidation, "文件名不能为空", false)
	}
	if name == "." || name == ".." {
		return apiErr(CodeValidation, "文件名非法", false)
	}
	if strings.ContainsAny(name, "/\\") || strings.ContainsRune(name, 0) {
		return apiErr(CodeValidation, "文件名不能包含路径分隔符", false)
	}
	for _, r := range name {
		if r < 32 || r == 127 {
			return apiErr(CodeValidation, "文件名含控制字符", false)
		}
		if unicode.IsSpace(r) && r != ' ' {
			return apiErr(CodeValidation, "文件名含非法空白", false)
		}
	}
	if len(name) > 255 {
		return apiErr(CodeValidation, "文件名过长", false)
	}
	return nil
}

// joinRemote 拼接目录与文件名。
func joinRemote(dir, name string) string {
	if dir == "" || dir == "." {
		return name
	}
	return path.Join(dir, name)
}

// uploadTempName 同目录临时 part 文件名。
func uploadTempName(filename, random string) string {
	return "." + filename + ".mooncell-upload-" + random + ".part"
}

func padOctal(v uint32, width int) string {
	const digits = "01234567"
	if width <= 0 {
		width = 3
	}
	buf := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		buf[i] = digits[v&7]
		v >>= 3
	}
	return string(buf)
}
