#!/bin/sh
# 构建产物兼容性检查：目标浏览器 Chrome 92 不支持的特性不得出现在 dist/assets。
# 在 vite build 后由 `pnpm build` 自动执行；也可单独运行：sh scripts/check-compat.sh
#
# esbuild 按 target 只降「语法」，不降运行时 API，也不改写 CSS 新函数/属性，
# 所以依赖库带进来的新 API/新 CSS 只能在产物上拦截。新增条目时注明最低版本。
#
# 规则：
# - 模式一律是 POSIX ERE（括号等元字符须转义）。grep 本身出错（退出码 2）直接判失败，
#   不能吞掉——否则某条模式写错就会静默失效（:has( 曾因未转义的括号一直不生效）。
# - 豁免只能精确到「确认不会生效」的第三方规则，并写明理由。
set -u
cd "$(dirname "$0")/.."
DIST=dist/assets
[ -d "$DIST" ] || { echo "[compat] $DIST 不存在，先运行 pnpm build"; exit 1; }

fail=0

# scan <描述> <ERE 模式> <文件扩展名 js|css>
scan() {
  desc="$1"; pattern="$2"; ext="$3"
  hits=$(grep -lE -- "$pattern" "$DIST"/*."$ext" 2>/tmp/mc-compat-grep.err)
  rc=$?
  if [ "$rc" -eq 2 ]; then
    echo "[compat] 检查「${desc}」的模式本身出错：$(cat /tmp/mc-compat-grep.err)"
    fail=1
    return
  fi
  if [ -n "$hits" ]; then
    echo "[compat] 禁用特性「${desc}」出现于："
    echo "$hits" | sed 's/^/  /'
    fail=1
  fi
}

# ---- JS 运行时 API（esbuild 不会 polyfill）----
scan "structuredClone（98+，已由 vite define + index.html 兜底改名）" '(^|[^A-Za-z_$])structuredClone' js
scan "Object.hasOwn（93+）"                       'Object\.hasOwn\('                  js
scan "Array#findLast/findLastIndex（97+）"         '\.findLast(Index)?\('              js
scan "Array#toSorted/toReversed/toSpliced（110+）" '\.to(Sorted|Reversed|Spliced)\('   js
scan "Object/Map.groupBy（117+）"                 '(Object|Map)\.groupBy\('           js
scan "AbortSignal.timeout/any（103+/116+）"       'AbortSignal\.(timeout|any)\('      js
scan "Promise.withResolvers（119+）"              'Promise\.withResolvers\('          js
scan "Array.fromAsync（121+）"                    'Array\.fromAsync\('                js
scan "Element#checkVisibility（105+）"            '\.checkVisibility\('               js
scan "Popover API（114+）"                        '\.(showPopover|hidePopover|togglePopover)\(' js

# ---- CSS ----
scan "color-mix()（111+）"                        'color-mix\('                       css
scan ":has() 选择器（105+）"                      ':has\('                            css
scan "容器查询 @container / container-type（105+）" '@container|container-type:'      css
scan "@layer 级联层（99+）"                       '@layer'                            css
scan "accent-color（93+）"                        'accent-color:'                     css
scan "动态视口单位 dvh/svh/lvh（108+）"           '[0-9.](dvh|svh|lvh|dvw|svw|lvw)'   css
scan "text-wrap（114+）"                          'text-wrap:'                        css
scan "oklch/oklab/lab/lch/hwb 颜色（101+/111+）"  '(oklch|oklab|[^a-z-]lab|[^a-z-]lch|hwb)\('  css
scan "相对颜色 rgb(from …)（119+）"               '(rgb|hsl)\(from '                  css
scan "scrollbar-gutter（94+）"                    'scrollbar-gutter:'                 css

# 独立变换属性 translate/scale/rotate（104+）：按规则块检查，豁免确认不生效的第三方规则——
#   .react-flow__resize-control.handle：@xyflow/react 的 NodeResizer 把手，本项目未使用 NodeResizer。
rules=$(grep -ohE '[^{}]*\{[^{}]*' "$DIST"/*.css 2>/dev/null \
  | grep -E '[;{](translate|scale|rotate):' \
  | grep -vE '^\.react-flow__resize-control\.handle\{')
if [ -n "$rules" ]; then
  echo "[compat] 禁用特性「独立变换属性 translate/scale/rotate（104+）」出现于规则："
  echo "$rules" | cut -c1-160 | sed 's/^/  /'
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  echo "[compat] 失败：构建产物含 Chrome 92 不支持的特性（目标基线 Chrome 92）"
  exit 1
fi
echo "[compat] 通过：构建产物无 Chrome 92 禁用特性"
