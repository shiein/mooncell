#!/bin/sh
# 在临时目录(全新空库)启动预构建的 Console 二进制,供 Playwright E2E 使用。
# 二进制路径 MC_CONSOLE_BIN(默认 /tmp/mc-console-e2e),需在跑 E2E 前 go build 生成。
set -e
PORT="${1:-8799}"
BIN="${MC_CONSOLE_BIN:-/tmp/mc-console-e2e}"
if [ ! -x "$BIN" ]; then
  echo "Console 二进制不存在: $BIN(先 go build -o $BIN .)" >&2
  exit 1
fi
DIR="$(mktemp -d)"
printf '[server]\naddr="127.0.0.1"\nport=%s\n[database]\npath="%s/e2e.db"\n' "$PORT" "$DIR" > "$DIR/config.toml"
cd "$DIR"
exec "$BIN"
