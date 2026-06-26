#!/usr/bin/env bash
# Mooncell 发布打包 —— 在联网构建机产出离线安装 bundle。
#
# 产出:deploy/release/mooncell-<arch>/{mooncell-agent, mooncell-console, install.sh}
# 把该目录整体拷到目标机,运行其中的 install.sh 即可离线安装。
#
# 用法:
#   ./build-release.sh            # 默认 amd64
#   ARCH=arm64 ./build-release.sh # 麒麟/UOS arm64
#   VERSION=v0.2.0 ./build-release.sh  # 显式指定版本号(默认取 git describe)
set -euo pipefail

ARCH="${ARCH:-amd64}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$ROOT/deploy/release/mooncell-$ARCH"

# 版本号:优先用环境变量 VERSION;否则取 git describe --tags(无 tag 时退到 commit hash)。
# 注入到 agent/agentVersion 与 console/consoleVersion,供 --version 与自更新版本核对使用。
if [ -z "${VERSION:-}" ]; then
  VERSION="$(cd "$ROOT" && git describe --tags --always 2>/dev/null || echo dev)"
fi

log() { printf '\033[0;36m[build]\033[0m %s\n' "$*"; }

command -v go >/dev/null 2>&1 || { echo "需要 go 工具链"; exit 1; }

log "目标架构:linux/$ARCH  版本:$VERSION"
rm -rf "$OUT"; mkdir -p "$OUT"

log "构建 Console 前端(vite)"
( cd "$ROOT/console" && pnpm install --frozen-lockfile >/dev/null 2>&1 && pnpm build >/dev/null )

log "编译 Console 单二进制(go:embed dist)"
( cd "$ROOT/console" && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath \
    -ldflags "-s -w -X main.consoleVersion=$VERSION" -o "$OUT/mooncell-console" . )

log "编译 Agent 单二进制"
( cd "$ROOT/agent" && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath \
    -ldflags "-s -w -X main.agentVersion=$VERSION" -o "$OUT/mooncell-agent" . )

cp "$ROOT/deploy/install.sh" "$OUT/install.sh"
chmod +x "$OUT/install.sh"

log "完成:$OUT"
ls -lh "$OUT" | awk 'NR>1{print "  "$9" "$5}'
log "拷贝整个目录到目标机,运行 ./install.sh 即可"
