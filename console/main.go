package main

import (
	"embed"
	"os"

	"mooncell/console/internal/consoleapp"
)

// consoleVersion 可在构建时用 -ldflags "-X main.consoleVersion=vX.Y.Z" 覆盖(发布打版用,与 agentVersion 对齐)。
var consoleVersion = "dev"

// 编译期把 vite 构建产物嵌入二进制。运行时从内存映像直接服务,无磁盘 IO。
// 需先 `pnpm build` 生成 dist/ 再 `go build`。
//
//go:embed all:dist
var distFS embed.FS

func main() {
	consoleapp.Run(distFS, consoleVersion, os.Args[1:])
}
