#!/usr/bin/env bash
# Mooncell 发布打包 —— 在联网构建机产出离线安装 bundle。
#
# 产出:deploy/release/mooncell-<arch>/{mooncell-agent, mooncell-console, install.sh}
# 把该目录整体拷到目标机,运行其中的 install.sh 即可离线安装。
#
# 用法:
#   ./build-release.sh            # 默认 amd64
#   ARCH=arm64 ./build-release.sh # 麒麟/UOS arm64
set -euo pipefail

ARCH="${ARCH:-amd64}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$ROOT/deploy/release/mooncell-$ARCH"

log() { printf '\033[0;36m[build]\033[0m %s\n' "$*"; }

command -v go >/dev/null 2>&1 || { echo "需要 go 工具链"; exit 1; }

log "目标架构:linux/$ARCH"
rm -rf "$OUT"; mkdir -p "$OUT"

log "构建 Console 前端(vite)"
( cd "$ROOT/console" && pnpm install --frozen-lockfile >/dev/null 2>&1 && pnpm build >/dev/null )

log "编译 Console 单二进制(go:embed dist)"
( cd "$ROOT/console" && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -ldflags "-s -w" -o "$OUT/mooncell-console" . )

log "编译 Agent 单二进制"
( cd "$ROOT/agent" && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -ldflags "-s -w" -o "$OUT/mooncell-agent" . )

cp "$ROOT/deploy/install.sh" "$OUT/install.sh"
chmod +x "$OUT/install.sh"

log "完成:$OUT"
ls -lh "$OUT" | awk 'NR>1{print "  "$9" "$5}'
log "拷贝整个目录到目标机,运行 ./install.sh 即可"
