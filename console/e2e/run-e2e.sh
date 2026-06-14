#!/bin/sh
# 一键前端 E2E:构建前端 + 内嵌的 Console 二进制 → 临时空库起服务 → 跑 Playwright → 收尾。
# 依赖 go / pnpm / curl 在 PATH。端口 8765。
set -e
cd "$(dirname "$0")/.."  # console/
PORT=8765
BIN=/tmp/mc-console-e2e

echo "[e2e] 构建前端 + Console 二进制…"
pnpm build >/dev/null
go build -o "$BIN" .

FAKE_AGENT_PORT=9111
# 假 Agent:让 Console 有真实能力清单与可控错误态(能力过滤 / 备份失败态 E2E)。
node e2e/fake-agent.mjs >/tmp/mc-fake-agent.log 2>&1 &
FAKE_PID=$!

DIR="$(mktemp -d)"
printf '[server]\naddr="127.0.0.1"\nport=%s\n[database]\npath="%s/e2e.db"\n[agent]\naddr="127.0.0.1:%s"\ntoken="tok"\n' "$PORT" "$DIR" "$FAKE_AGENT_PORT" > "$DIR/config.toml"
( cd "$DIR" && "$BIN" >"$DIR/console.log" 2>&1 & echo $! > "$DIR/pid" )

cleanup() {
  [ -f "$DIR/pid" ] && kill -9 "$(cat "$DIR/pid")" 2>/dev/null || true
  kill -9 "$FAKE_PID" 2>/dev/null || true
  pkill -9 -f mc-console-e2e 2>/dev/null || true
  pkill -9 -f "e2e/fake-agent.mjs" 2>/dev/null || true
  rm -rf "$DIR"
}
trap cleanup EXIT INT TERM

echo "[e2e] 等待 Console 就绪…"
i=0
while [ $i -lt 40 ]; do
  if curl -s -o /dev/null "http://127.0.0.1:$PORT/api/session"; then break; fi
  i=$((i + 1)); sleep 0.5
done

pnpm exec playwright test "$@"
