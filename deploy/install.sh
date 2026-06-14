#!/usr/bin/env bash
# Mooncell 离线安装脚本 —— 把 Agent 与 Console 安装为 systemd 常驻服务。
#
# 适用:内网 / 单机 / 离线交付。把本脚本与 mooncell-agent、mooncell-console 两个二进制
# 放在同一目录(由 build-release.sh 产出的 release bundle),在目标机以 root 运行即可。
#
# 用法:
#   ./install.sh install     # 安装(默认);首装生成随机共享 token,写好两端配置并启动
#   ./install.sh upgrade     # 升级:仅替换二进制并重启,保留配置与数据库
#   ./install.sh status      # 查看服务状态与访问信息
#   ./install.sh uninstall   # 卸载服务(默认保留数据;加 --purge 连数据一并删除)
#
# 可用环境变量覆盖默认值:
#   MC_PREFIX            安装根目录(默认 /opt/mooncell)
#   MC_CONSOLE_PORT      Console 端口(默认 8787)
#   MC_AGENT_PORT        Agent 端口(默认 9100)
#   MC_ADMIN_USER        管理员用户名(默认 admin)
#   MC_ADMIN_PASSWORD    管理员密码(默认随机生成并打印)
#   MC_DEPLOY_ROOTS      Agent 允许部署的根目录,逗号分隔(默认 /srv/apps,/data/web)
set -euo pipefail

PREFIX="${MC_PREFIX:-/opt/mooncell}"
CONSOLE_PORT="${MC_CONSOLE_PORT:-8787}"
AGENT_PORT="${MC_AGENT_PORT:-9100}"
ADMIN_USER="${MC_ADMIN_USER:-admin}"
DEPLOY_ROOTS="${MC_DEPLOY_ROOTS:-/srv/apps,/data/web}"

AGENT_DIR="$PREFIX/agent"
CONSOLE_DIR="$PREFIX/console"
SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

log()  { printf '\033[0;36m[mooncell]\033[0m %s\n' "$*"; }
warn() { printf '\033[0;33m[mooncell] 警告:\033[0m %s\n' "$*"; }
die()  { printf '\033[0;31m[mooncell] 错误:\033[0m %s\n' "$*" >&2; exit 1; }

require_root() { [ "$(id -u)" = "0" ] || die "请以 root 运行(systemd 服务安装需要)"; }
require_systemd() { command -v systemctl >/dev/null 2>&1 || die "未检测到 systemd(systemctl),本脚本依赖 systemd 托管服务"; }

# gen_token 生成高熵共享 token;优先 openssl,退化到 /dev/urandom。
gen_token() {
  if command -v openssl >/dev/null 2>&1; then
    echo "mc_$(openssl rand -hex 20)"
  else
    echo "mc_$(head -c 40 /dev/urandom | od -An -tx1 | tr -d ' \n' | head -c 40)"
  fi
}

# toml_roots 把逗号分隔的根目录转成 TOML 数组字面量:a,b → "a", "b"
toml_roots() {
  local out="" r
  IFS=',' read -ra arr <<< "$1"
  for r in "${arr[@]}"; do
    r="$(echo "$r" | xargs)" # trim
    [ -n "$r" ] && out="$out\"$r\", "
  done
  echo "${out%, }"
}

write_unit() {
  local name="$1" desc="$2" wd="$3" exe="$4"
  cat > "/etc/systemd/system/${name}.service" <<EOF
[Unit]
Description=${desc}
After=network.target

[Service]
Type=simple
WorkingDirectory=${wd}
ExecStart=${exe}
Restart=on-failure
RestartSec=3
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
}

do_install() {
  require_root; require_systemd
  [ -f "$SRC_DIR/mooncell-agent" ]   || die "找不到 mooncell-agent(应与本脚本同目录)"
  [ -f "$SRC_DIR/mooncell-console" ] || die "找不到 mooncell-console(应与本脚本同目录)"

  log "安装到 $PREFIX"
  mkdir -p "$AGENT_DIR/backups" "$CONSOLE_DIR"
  install -m 0755 "$SRC_DIR/mooncell-agent"   "$AGENT_DIR/mc-agent"
  install -m 0755 "$SRC_DIR/mooncell-console" "$CONSOLE_DIR/mc-console"

  # 首装才生成 token / 写配置;已存在则保留(支持重复运行 install 不覆盖配置)。
  local token
  if [ -f "$AGENT_DIR/config.toml" ]; then
    token="$(grep -oP 'token\s*=\s*"\K[^"]+' "$AGENT_DIR/config.toml" | head -1)"
    log "复用已有共享 token(配置已存在,不覆盖)"
  else
    token="$(gen_token)"
    log "生成共享 token"
    cat > "$AGENT_DIR/config.toml" <<EOF
# Mooncell Agent 配置(由 install.sh 生成)
[server]
addr = "0.0.0.0"
port = ${AGENT_PORT}

[security]
token = "${token}"

[paths]
deploy_roots = [$(toml_roots "$DEPLOY_ROOTS")]
log_roots    = [$(toml_roots "$DEPLOY_ROOTS"), "/var/log"]
backup_dir   = "${AGENT_DIR}/backups"
EOF
  fi

  local admin_pw="${MC_ADMIN_PASSWORD:-}"
  if [ -f "$CONSOLE_DIR/config.toml" ]; then
    log "Console 配置已存在,保留(管理员账号不变)"
  else
    if [ -z "$admin_pw" ]; then admin_pw="$(gen_token | cut -c4-15)"; GENERATED_PW="$admin_pw"; fi
    cat > "$CONSOLE_DIR/config.toml" <<EOF
# Mooncell Console 配置(由 install.sh 生成)
[server]
addr = "0.0.0.0"
port = ${CONSOLE_PORT}

[database]
path = "${CONSOLE_DIR}/mooncell.db"

[session]
ttl_hours = 168

[admin]
username = "${ADMIN_USER}"
password = "${admin_pw}"

[agent]
addr  = "127.0.0.1:${AGENT_PORT}"
token = "${token}"
EOF
  fi
  chmod 0600 "$AGENT_DIR/config.toml" "$CONSOLE_DIR/config.toml"

  write_unit mooncell-agent   "Mooncell Agent"   "$AGENT_DIR"   "$AGENT_DIR/mc-agent"
  write_unit mooncell-console "Mooncell Console" "$CONSOLE_DIR" "$CONSOLE_DIR/mc-console"
  systemctl daemon-reload
  systemctl enable --now mooncell-agent mooncell-console >/dev/null 2>&1

  sleep 2
  _health_check
  _print_summary "${GENERATED_PW:-}"
}

do_upgrade() {
  require_root; require_systemd
  [ -f "$AGENT_DIR/config.toml" ] || die "未发现已安装实例($PREFIX),请先 install"
  [ -f "$SRC_DIR/mooncell-agent" ] && [ -f "$SRC_DIR/mooncell-console" ] || die "找不到新版二进制"
  log "升级二进制(保留配置与数据库)"
  systemctl stop mooncell-agent mooncell-console || true
  install -m 0755 "$SRC_DIR/mooncell-agent"   "$AGENT_DIR/mc-agent"
  install -m 0755 "$SRC_DIR/mooncell-console" "$CONSOLE_DIR/mc-console"
  systemctl start mooncell-agent mooncell-console
  sleep 2
  _health_check
  log "升级完成"
}

do_uninstall() {
  require_root
  log "停止并移除服务"
  systemctl disable --now mooncell-agent mooncell-console >/dev/null 2>&1 || true
  rm -f /etc/systemd/system/mooncell-agent.service /etc/systemd/system/mooncell-console.service
  systemctl daemon-reload
  if [ "${1:-}" = "--purge" ]; then
    warn "清除全部数据:$PREFIX"
    rm -rf "$PREFIX"
  else
    log "已保留数据目录 $PREFIX(如需彻底清除,加 --purge)"
  fi
}

do_status() {
  systemctl --no-pager status mooncell-agent mooncell-console 2>/dev/null || true
  _print_summary ""
}

_health_check() {
  local token; token="$(grep -oP 'token\s*=\s*"\K[^"]+' "$AGENT_DIR/config.toml" | head -1)"
  if curl -fsS -m 5 -H "Authorization: Bearer $token" "http://127.0.0.1:${AGENT_PORT}/api/ping" >/dev/null 2>&1; then
    log "Agent 健康(:${AGENT_PORT})"
  else
    warn "Agent 未通过健康检查,查看:journalctl -u mooncell-agent -n 50"
  fi
  if curl -fsS -m 5 -o /dev/null "http://127.0.0.1:${CONSOLE_PORT}/api/session" 2>/dev/null \
     || [ "$(curl -s -m 5 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${CONSOLE_PORT}/api/session" 2>/dev/null)" = "401" ]; then
    log "Console 健康(:${CONSOLE_PORT})"
  else
    warn "Console 未通过健康检查,查看:journalctl -u mooncell-console -n 50"
  fi
}

_print_summary() {
  local pw="$1" ip
  ip="$(hostname -I 2>/dev/null | awk '{print $1}')"; [ -n "$ip" ] || ip="<本机IP>"
  echo
  log "============ Mooncell 已就绪 ============"
  log "控制台:  http://${ip}:${CONSOLE_PORT}"
  log "管理员:  ${ADMIN_USER}"
  [ -n "$pw" ] && log "初始密码: ${pw}  (请尽快登录修改)"
  log "服务:    systemctl status mooncell-agent | mooncell-console"
  log "日志:    journalctl -u mooncell-agent -f"
  log "========================================"
}

case "${1:-install}" in
  install)   do_install ;;
  upgrade)   do_upgrade ;;
  uninstall) do_uninstall "${2:-}" ;;
  status)    do_status ;;
  *) die "未知命令:$1(支持 install|upgrade|uninstall|status)" ;;
esac
