#!/bin/sh
# 一键前端 E2E:构建前端 + 内嵌的 Console 二进制 → Playwright 起临时 Console → 跑用例 → 收尾。
# 依赖 go / pnpm 在 PATH。端口 8765。
set -e
cd "$(dirname "$0")/.."  # console/
BIN=/tmp/mc-console-e2e

echo "[e2e] 构建前端 + Console 二进制…"
pnpm build >/dev/null
# CI/开发终端及 `go env -w` 可能固定了交叉编译目标；E2E 二进制必须按真实宿主机构建。
HOST_GOOS="$(go env GOHOSTOS)"
HOST_GOARCH="$(go env GOHOSTARCH)"
CGO_ENABLED=0 GOOS="$HOST_GOOS" GOARCH="$HOST_GOARCH" go build -o "$BIN" .

export FAKE_AGENT_PORT=9111
# 本地开发环境可能注入 http_proxy/all_proxy。Playwright 的 webServer 就绪探测若走代理，
# 会把代理返回的 4xx 误认成本地服务已启动，随后浏览器直连得到 ERR_CONNECTION_REFUSED。
export NO_PROXY="127.0.0.1,localhost,${NO_PROXY:-}"
export no_proxy="127.0.0.1,localhost,${no_proxy:-}"
# 假 Agent:让 Console 有真实能力清单与可控错误态(能力过滤 / 备份失败态 E2E)。
node e2e/fake-agent.mjs >/tmp/mc-fake-agent.log 2>&1 &
FAKE_PID=$!

cleanup() {
  # Console 由 Playwright webServer 管理；这里只按已知 PID 收尾假 Agent。
  [ -n "$FAKE_PID" ] && kill "$FAKE_PID" 2>/dev/null || true
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

pnpm exec playwright test "$@"
