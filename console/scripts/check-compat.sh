#!/bin/sh
# 构建产物兼容性检查（R1-m1）：目标浏览器 Chrome 92 不支持的特性不得出现。
# 在 vite build 后由 `pnpm build` 自动执行；也可单独运行：sh scripts/check-compat.sh
set -e
cd "$(dirname "$0")/.."
DIST=dist/assets
[ -d "$DIST" ] || { echo "[compat] $DIST 不存在，先运行 pnpm build"; exit 1; }

# color-mix()       CSS 函数，Chrome 111+
# :has(             CSS 选择器，Chrome 105+
# structuredClone   JS API，Chrome 98+
# Object.hasOwn     JS API，Chrome 93+
fail=0
check() {
  desc="$1"; pattern="$2"; shift 2
  # -F 固定字符串；-E 正则
  hits=$(grep -rEl "$pattern" "$@" 2>/dev/null || true)
  if [ -n "$hits" ]; then
    echo "[compat] 禁用特性 $desc 出现于："
    echo "$hits" | sed 's/^/  /'
    fail=1
  fi
}

check "color-mix()"      'color-mix'                        "$DIST"
check ":has() 选择器"    ':has('                            "$DIST"
check "structuredClone"  'structuredClone'                  "$DIST"
check "Object.hasOwn"    'Object\.hasOwn('                  "$DIST"

if [ "$fail" -ne 0 ]; then
  echo "[compat] 失败：上述特性需要 Chrome 93+，目标基线为 Chrome 92"
  exit 1
fi
echo "[compat] 通过：构建产物无禁用特性"
