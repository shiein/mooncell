// Mooncell — 布局 Shell:侧边栏 + 顶栏
import React from 'react';
import { Icon, Btn, Badge, Progress } from './primitives.jsx';
import { AGENT } from '../lib/data.js';

const NAV_ITEMS = [
  { id: "overview", label: "总览", en: "Overview", icon: "gauge" },
  { id: "apps", label: "应用", en: "Applications", icon: "box" },
  { id: "artifacts", label: "制品仓库", en: "Artifacts", icon: "archive" },
  { id: "cabinet", label: "文件柜", en: "Cabinet", icon: "folder" },
  { id: "audit", label: "审计日志", en: "Audit", icon: "shield" },
  { id: "agents", label: "Agent 管理", en: "Agents", icon: "server", adminOnly: true },
  { id: "users", label: "用户管理", en: "Users", icon: "user", adminOnly: true },
  { id: "system", label: "系统", en: "System", icon: "settings", adminOnly: true },
];

const ROLE_LABEL = { admin: "管理员", operator: "运维", viewer: "只读" };

function MoonLogo({ size = 26 }) {
  return (
    <div style={{
      width: size, height: size, borderRadius: 8, background: "var(--primary)", position: "relative",
      overflow: "hidden", flex: "none", boxShadow: "var(--shadow-sm)",
    }}>
      <div style={{
        position: "absolute", width: size * 0.62, height: size * 0.62, borderRadius: "50%",
        background: "#FFF", top: "19%", left: "19%",
      }}></div>
      <div style={{
        position: "absolute", width: size * 0.52, height: size * 0.52, borderRadius: "50%",
        background: "var(--primary)", top: "12%", left: "34%",
      }}></div>
    </div>
  );
}

function Sidebar({ page, onNav, user, role, onLogout }) {
  const navItems = NAV_ITEMS.filter((n) => !n.adminOnly || role === "admin");
  return (
    <aside className="sidebar">
      <div style={{ display: "flex", alignItems: "center", gap: 10, padding: "4px 8px 14px" }}>
        <MoonLogo />
        <div>
          <div style={{ fontWeight: 650, fontSize: 15, letterSpacing: "-0.01em" }}>Mooncell</div>
          <div style={{ fontSize: 10.5, color: "var(--muted-fg)", letterSpacing: ".04em" }}>内网部署平台</div>
        </div>
      </div>

      <nav style={{ display: "flex", flexDirection: "column", gap: 2 }}>
        {navItems.map((n) => (
          <button key={n.id} className="nav-item" data-active={String(page === n.id || (n.id === "apps" && page === "app-detail"))}
            onClick={() => onNav(n.id)}>
            <Icon name={n.icon} size={16} />
            <span style={{ flex: 1 }}>{n.label}</span>
          </button>
        ))}
      </nav>

      <div style={{ flex: 1 }}></div>

      {/* Agent 状态卡 */}
      <div className="card" style={{ padding: "10px 12px", marginBottom: 10 }}>
        <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 7, whiteSpace: "nowrap" }}>
          <span style={{ width: 7, height: 7, borderRadius: "50%", background: "var(--success)", flex: "none" }} className="pulse-dot"></span>
          <span style={{ fontSize: 12.5, fontWeight: 600, overflow: "hidden", textOverflow: "ellipsis" }}>{AGENT.name}</span>
          <span style={{ fontSize: 10.5, color: "var(--muted-fg)", marginLeft: "auto" }} className="mono">{AGENT.version}</span>
        </div>
        <div className="mono" style={{ fontSize: 10.5, color: "var(--muted-fg)", marginBottom: 8 }}>{AGENT.host}</div>
        <div style={{ display: "flex", alignItems: "center", gap: 7 }}>
          <span style={{ fontSize: 10.5, color: "var(--muted-fg)", flex: "none" }}>磁盘</span>
          <div style={{ flex: 1 }}><Progress value={AGENT.disk} height={5} color={AGENT.disk > 85 ? "var(--error)" : AGENT.disk > 70 ? "var(--warn)" : "var(--success)"} /></div>
          <span className="mono" style={{ fontSize: 10.5, color: "var(--fg-secondary)" }}>{AGENT.disk}%</span>
        </div>
      </div>

      <div style={{ display: "flex", alignItems: "center", gap: 9, padding: "6px 8px" }}>
        <div style={{
          width: 28, height: 28, borderRadius: "50%", background: "var(--primary-soft)", color: "var(--primary)",
          display: "flex", alignItems: "center", justifyContent: "center", fontSize: 12, fontWeight: 650, flex: "none",
        }}>{(user || "A")[0].toUpperCase()}</div>
        <div style={{ flex: 1, minWidth: 0 }}>
          <div style={{ fontSize: 12.5, fontWeight: 600 }}>{user}</div>
          <div style={{ fontSize: 10.5, color: "var(--muted-fg)" }}>{role || "viewer"} · {ROLE_LABEL[role] || "只读"}</div>
        </div>
        <Btn variant="ghost" size="sm" icon="logout" onClick={onLogout} title="退出登录"></Btn>
      </div>
    </aside>
  );
}

function Topbar({ crumbs, theme, onTheme, right }) {
  return (
    <header className="topbar">
      <div style={{ display: "flex", alignItems: "center", gap: 7, fontSize: 13.5, minWidth: 0 }}>
        {crumbs.map((c, i) => (
          <React.Fragment key={i}>
            {i > 0 ? <Icon name="chevronR" size={12} style={{ color: "var(--muted-fg)" }} /> : null}
            {c.onClick
              ? <button className="link-btn" style={{ color: "var(--muted-fg)", fontWeight: 500 }} onClick={c.onClick}>{c.label}</button>
              : <span style={{ fontWeight: 600 }}>{c.label}</span>}
          </React.Fragment>
        ))}
      </div>
      <div style={{ flex: 1 }}></div>
      {right}
      <Badge tone="success" dot><span className="mono" style={{ fontSize: 11 }}>Agent 在线</span></Badge>
      <Btn variant="ghost" icon={theme === "dark" ? "sun" : "moon"} onClick={onTheme} title="切换亮/暗主题"></Btn>
    </header>
  );
}

function Shell({ page, onNav, crumbs, theme, onTheme, user, role, onLogout, children, topRight }) {
  return (
    <div className="shell">
      <Sidebar page={page} onNav={onNav} user={user} role={role} onLogout={onLogout} />
      <div className="main">
        <Topbar crumbs={crumbs} theme={theme} onTheme={onTheme} right={topRight} />
        <div className="content"><div className="content-inner" key={page}>{children}</div></div>
      </div>
    </div>
  );
}

function PageHead({ title, desc, actions }) {
  return (
    <div style={{ display: "flex", alignItems: "flex-end", gap: 16, marginBottom: 20 }}>
      <div style={{ flex: 1 }}>
        <h2 style={{ fontSize: 20, letterSpacing: "-0.015em" }}>{title}</h2>
        {desc ? <p style={{ fontSize: 13, color: "var(--muted-fg)", marginTop: 3 }}>{desc}</p> : null}
      </div>
      {actions}
    </div>
  );
}

export { Shell, Sidebar, Topbar, MoonLogo, PageHead, NAV_ITEMS };
