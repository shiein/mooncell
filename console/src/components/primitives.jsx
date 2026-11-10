// Mooncell — UI 原语(shadcn 风格)
import React from 'react';
import { createPortal } from 'react-dom';
import { APP_STATUS, DEPLOY_TYPES } from '../lib/data.js';

const IC = {
  gauge: <g><path d="M12 21a9 9 0 1 1 9-9" fill="none"/><path d="M12 12l4-4" fill="none"/><circle cx="12" cy="12" r="1.6" fill="currentColor" stroke="none"/></g>,
  box: <g><rect x="3.5" y="6" width="17" height="14" rx="2" fill="none"/><path d="M3.5 10h17M8 6V4h8v2" fill="none"/></g>,
  folder: <path d="M3.5 6.5a2 2 0 0 1 2-2h4l2 2.5h7a2 2 0 0 1 2 2v8.5a2 2 0 0 1-2 2h-13a2 2 0 0 1-2-2z" fill="none"/>,
  shield: <g><path d="M12 3.5l7 2.8v5.2c0 4.6-3 7.6-7 9-4-1.4-7-4.4-7-9V6.3z" fill="none"/><path d="M9 11.7l2.2 2.2 3.8-4" fill="none"/></g>,
  server: <g><rect x="3.5" y="4.5" width="17" height="6.5" rx="1.5" fill="none"/><rect x="3.5" y="13" width="17" height="6.5" rx="1.5" fill="none"/><circle cx="7" cy="7.7" r="1" fill="currentColor" stroke="none"/><circle cx="7" cy="16.2" r="1" fill="currentColor" stroke="none"/></g>,
  archive: <g><rect x="3.5" y="4.5" width="17" height="5" rx="1" fill="none"/><path d="M5.5 9.5v8a2 2 0 0 0 2 2h9a2 2 0 0 0 2-2v-8M10 13h4" fill="none"/></g>,
  star: <path d="M12 3.5l2.6 5.3 5.9.9-4.3 4.1 1 5.8-5.2-2.7-5.2 2.7 1-5.8-4.3-4.1 5.9-.9z" fill="none"/>,
  fileText: <g><path d="M6 3.5h8l4 4V20.5h-12z" fill="none"/><path d="M14 3.5V8h4M9 12h6M9 15.5h6" fill="none"/></g>,
  search: <g><circle cx="11" cy="11" r="6.5" fill="none"/><path d="M16 16l4.5 4.5" fill="none"/></g>,
  upload: <g><path d="M12 16V4.5M7.5 8.5L12 4l4.5 4.5" fill="none"/><path d="M4.5 16.5v3h15v-3" fill="none"/></g>,
  download: <g><path d="M12 4.5V16M7.5 12L12 16.5l4.5-4.5" fill="none"/><path d="M4.5 17v3h15v-3" fill="none"/></g>,
  play: <path d="M8 5.5l11 6.5-11 6.5z" fill="currentColor" stroke="none"/>,
  stop: <rect x="6.5" y="6.5" width="11" height="11" rx="1.5" fill="currentColor" stroke="none"/>,
  rotate: <g><path d="M4 12a8 8 0 1 1 2.6 5.9" fill="none"/><path d="M4 13v5h5" fill="none" transform="translate(0,-1)"/></g>,
  check: <path d="M5 12.5l4.5 4.5L19 7.5" fill="none"/>,
  x: <path d="M6 6l12 12M18 6L6 18" fill="none"/>,
  chevronR: <path d="M9 5.5l7 6.5-7 6.5" fill="none"/>,
  chevronD: <path d="M5.5 9l6.5 7 6.5-7" fill="none"/>,
  chevronL: <path d="M15 5.5L8 12l7 6.5" fill="none"/>,
  sun: <g><circle cx="12" cy="12" r="4.5" fill="none"/><path d="M12 2.5v2.5M12 19v2.5M2.5 12h2.5M19 12h2.5M5 5l1.8 1.8M17.2 17.2L19 19M19 5l-1.8 1.8M6.8 17.2L5 19" fill="none"/></g>,
  moon: <path d="M20 13.5A8 8 0 0 1 10.5 4 8 8 0 1 0 20 13.5z" fill="none"/>,
  logout: <g><path d="M14 4.5H6a1.5 1.5 0 0 0-1.5 1.5v12A1.5 1.5 0 0 0 6 19.5h8" fill="none"/><path d="M16 8l4 4-4 4M20 12h-9" fill="none"/></g>,
  terminal: <g><rect x="3" y="4.5" width="18" height="15" rx="2" fill="none"/><path d="M7 9.5l3 2.5-3 2.5M12.5 15H17" fill="none"/></g>,
  trash: <g><path d="M4.5 6.5h15M9.5 6.5v-2h5v2M6.5 6.5l1 13.5h9l1-13.5" fill="none"/><path d="M10 10.5v6M14 10.5v6" fill="none"/></g>,
  plus: <path d="M12 5v14M5 12h14" fill="none"/>,
  clock: <g><circle cx="12" cy="12" r="8.5" fill="none"/><path d="M12 7v5.5l3.5 2" fill="none"/></g>,
  hdd: <g><path d="M4 13.5l2.5-7a2 2 0 0 1 1.9-1.5h7.2a2 2 0 0 1 1.9 1.5l2.5 7" fill="none"/><rect x="3.5" y="13.5" width="17" height="5.5" rx="1.5" fill="none"/><circle cx="7.5" cy="16.2" r=".9" fill="currentColor" stroke="none"/></g>,
  copy: <g><rect x="9" y="9" width="11" height="11" rx="1.5" fill="none"/><path d="M5.5 14.5H4.5V4.5h10v1" fill="none" transform="translate(0,-.5)"/></g>,
  alert: <g><path d="M12 3.5L22 20H2z" fill="none"/><path d="M12 9.5V14M12 16.8v.4" fill="none"/></g>,
  pause: <path d="M8.5 5.5v13M15.5 5.5v13" fill="none"/>,
  user: <g><circle cx="12" cy="8" r="3.8" fill="none"/><path d="M4.5 20.5c1.2-3.8 4-5.5 7.5-5.5s6.3 1.7 7.5 5.5" fill="none"/></g>,
  link: <g><path d="M10 14a4 4 0 0 0 6 .4l2.5-2.5a4 4 0 0 0-5.7-5.7L11.5 7.5" fill="none"/><path d="M14 10a4 4 0 0 0-6-.4L5.5 12a4 4 0 0 0 5.7 5.7l1.3-1.3" fill="none"/></g>,
  eye: <g><path d="M2.5 12S6 5.5 12 5.5 21.5 12 21.5 12 18 18.5 12 18.5 2.5 12 2.5 12z" fill="none"/><circle cx="12" cy="12" r="2.8" fill="none"/></g>,
  settings: <g><circle cx="12" cy="12" r="3" fill="none"/><path d="M12 2.8v3M12 18.2v3M2.8 12h3M18.2 12h3M5.5 5.5l2.1 2.1M16.4 16.4l2.1 2.1M18.5 5.5l-2.1 2.1M7.6 16.4l-2.1 2.1" fill="none"/></g>,
  // 铅笔编辑
  pencil: <g><path d="M14.5 4.5l5 5L8 21H3v-5L14.5 4.5z" fill="none"/><path d="M12.5 6.5l5 5" fill="none"/></g>,
  zap: <path d="M13 3L5 13.5h6L11 21l8-10.5h-6z" fill="none"/>,
  // ---- 数据集成
  flow: <g><rect x="3" y="4" width="7" height="5" rx="1.5" fill="none"/><rect x="14" y="15" width="7" height="5" rx="1.5" fill="none"/><path d="M10 6.5h3.5a2 2 0 0 1 2 2V15" fill="none"/></g>,
  database: <g><ellipse cx="12" cy="6" rx="7.5" ry="2.8" fill="none"/><path d="M4.5 6v12c0 1.5 3.4 2.8 7.5 2.8s7.5-1.3 7.5-2.8V6M4.5 12c0 1.5 3.4 2.8 7.5 2.8s7.5-1.3 7.5-2.8" fill="none"/></g>,
  table: <g><rect x="3.5" y="4.5" width="17" height="15" rx="2" fill="none"/><path d="M3.5 9.5h17M3.5 14.5h17M9.5 9.5v10" fill="none"/></g>,
  filter: <path d="M4 5h16l-6.2 7.4v5.4L10.2 20v-7.6z" fill="none"/>,
  function: <g><path d="M14.5 4.5c-2.5 0-3.2 1.4-3.6 3.6L9.3 17c-.4 2-1.2 2.8-3.3 2.8" fill="none"/><path d="M7.5 10.5h7" fill="none"/><path d="M15 13.5l4 4M19 13.5l-4 4" fill="none"/></g>,
  columns: <g><rect x="3.5" y="4.5" width="17" height="15" rx="2" fill="none"/><path d="M9.2 4.5v15M14.8 4.5v15" fill="none"/></g>,
  fingerprint: <g><path d="M7.5 18.5c1.5-2 2-4.2 2-6.5a2.5 2.5 0 0 1 5 0c0 3-.6 5.6-1.8 7.6" fill="none"/><path d="M5 15c.4-1 .6-2 .6-3a6.4 6.4 0 0 1 12.8 0c0 2-.2 3.8-.7 5.5" fill="none"/><path d="M12 12c0 3-.8 5.7-2.4 8" fill="none"/></g>,
  sparkles: <g><path d="M10 4l1.6 4.4L16 10l-4.4 1.6L10 16l-1.6-4.4L4 10l4.4-1.6z" fill="none"/><path d="M17.5 14.5l.8 2.2 2.2.8-2.2.8-.8 2.2-.8-2.2-2.2-.8 2.2-.8z" fill="none"/></g>,
  upsert: <g><path d="M4.5 8.5h10M11 5l3.5 3.5L11 12" fill="none"/><path d="M19.5 15.5h-10M13 12l-3.5 3.5L13 19" fill="none"/></g>,
  branch: <g><circle cx="6.5" cy="5.5" r="2" fill="none"/><circle cx="6.5" cy="18.5" r="2" fill="none"/><circle cx="17.5" cy="8.5" r="2" fill="none"/><path d="M6.5 7.5v9M17.5 10.5c0 4-5 3-9.4 6.6" fill="none"/></g>,
  variable: <g><path d="M7 4.5C4.8 6.6 4 9 4 12s.8 5.4 3 7.5M17 4.5c2.2 2.1 3 4.5 3 7.5s-.8 5.4-3 7.5" fill="none"/><path d="M9.5 9l5 6M14.5 9l-5 6" fill="none"/></g>,
  pipeline: <g><circle cx="5" cy="12" r="2.2" fill="none"/><circle cx="12" cy="12" r="2.2" fill="none"/><circle cx="19" cy="12" r="2.2" fill="none"/><path d="M7.2 12h2.6M14.2 12h2.6" fill="none"/></g>,
  workflow: <g><rect x="3.5" y="9" width="5.5" height="6" rx="1.4" fill="none"/><rect x="15" y="3.5" width="5.5" height="6" rx="1.4" fill="none"/><rect x="15" y="14.5" width="5.5" height="6" rx="1.4" fill="none"/><path d="M9 12h2.5a1.5 1.5 0 0 0 1.5-1.5V8a1.5 1.5 0 0 1 1.5-1.5H15M13 12v4a1.5 1.5 0 0 0 1.5 1.5H15" fill="none"/></g>,
  globe: <g><circle cx="12" cy="12" r="8.5" fill="none"/><path d="M3.5 12h17M12 3.5c2.4 2.4 3.6 5.2 3.6 8.5s-1.2 6.1-3.6 8.5c-2.4-2.4-3.6-5.2-3.6-8.5s1.2-6.1 3.6-8.5z" fill="none"/></g>,
  bell: <g><path d="M6 16.5V11a6 6 0 0 1 12 0v5.5l1.5 2h-15z" fill="none"/><path d="M10 20.5a2.2 2.2 0 0 0 4 0" fill="none"/></g>,
  circle: <circle cx="12" cy="12" r="7.5" fill="none"/>,
  calendar: <g><rect x="3.5" y="5" width="17" height="15" rx="2" fill="none"/><path d="M3.5 10h17M8 3v4M16 3v4" fill="none"/></g>,
  activity: <path d="M3.5 12h4l2.5-6.5 4 13 2.5-6.5h4" fill="none"/>,
  history: <g><path d="M4 12a8 8 0 1 0 2.4-5.7" fill="none"/><path d="M4 4.5v4h4" fill="none"/><path d="M12 8v4.5l3 1.8" fill="none"/></g>,
  undo: <g><path d="M9 7.5L4.5 12 9 16.5" fill="none"/><path d="M5 12h9.5a5 5 0 0 1 0 10H12" fill="none" transform="translate(0,-3)"/></g>,
  redo: <g><path d="M15 7.5l4.5 4.5-4.5 4.5" fill="none"/><path d="M19 12H9.5a5 5 0 0 0 0 10H12" fill="none" transform="translate(0,-3)"/></g>,
  layout: <g><rect x="3.5" y="4" width="6" height="6" rx="1.4" fill="none"/><rect x="14.5" y="4" width="6" height="6" rx="1.4" fill="none"/><rect x="9" y="14" width="6" height="6" rx="1.4" fill="none"/><path d="M6.5 10v2h11v-2M12 12v2" fill="none"/></g>,
  grip: <g fill="currentColor" stroke="none"><circle cx="9" cy="6" r="1.3"/><circle cx="15" cy="6" r="1.3"/><circle cx="9" cy="12" r="1.3"/><circle cx="15" cy="12" r="1.3"/><circle cx="9" cy="18" r="1.3"/><circle cx="15" cy="18" r="1.3"/></g>,
  minus: <path d="M5 12h14" fill="none"/>,
  maximize: <g><path d="M4.5 9V4.5H9M15 4.5h4.5V9M19.5 15v4.5H15M9 19.5H4.5V15" fill="none"/></g>,
  more: <g fill="currentColor" stroke="none"><circle cx="5.5" cy="12" r="1.6"/><circle cx="12" cy="12" r="1.6"/><circle cx="18.5" cy="12" r="1.6"/></g>,
  save: <g><path d="M5 4.5h11l3.5 3.5v11.5H4.5V5z" fill="none"/><path d="M8 4.5v5h7v-5M8 19.5v-6h8v6" fill="none"/></g>,
  panel: <g><rect x="3.5" y="4.5" width="17" height="15" rx="2" fill="none"/><path d="M3.5 14.5h17" fill="none"/></g>,
  sidebar: <g><rect x="3.5" y="4.5" width="17" height="15" rx="2" fill="none"/><path d="M15 4.5v15" fill="none"/></g>,
  refresh: <g><path d="M19.5 12A7.5 7.5 0 1 1 17 6.4" fill="none"/><path d="M19.5 4v4.5H15" fill="none"/></g>,
  key: <g><circle cx="8" cy="15" r="4" fill="none"/><path d="M11 12l8.5-8.5M16 7l2.5 2.5M14 9l2 2" fill="none"/></g>,
  arrowR: <g><path d="M4.5 12h15M13.5 6l6 6-6 6" fill="none"/></g>,
  dot: <circle cx="12" cy="12" r="3.2" fill="currentColor" stroke="none"/>,
  layers: <g><path d="M12 3.5l9 5-9 5-9-5z" fill="none"/><path d="M3.5 13l8.5 4.7L20.5 13" fill="none"/><path d="M3.5 17l8.5 4.7L20.5 17" fill="none" transform="translate(0,-1)"/></g>,
};

function Icon({ name, size = 16, style, className }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" stroke="currentColor" strokeWidth="1.7"
      strokeLinecap="round" strokeLinejoin="round" fill="none" style={style} className={className} aria-hidden="true">
      {IC[name] || null}
    </svg>
  );
}

function Spinner({ size = 14, style }) {
  return (
    <svg className="spin" width={size} height={size} viewBox="0 0 24 24" style={style} aria-hidden="true">
      <circle cx="12" cy="12" r="9" stroke="currentColor" strokeOpacity=".22" strokeWidth="3" fill="none"></circle>
      <path d="M21 12a9 9 0 0 0-9-9" stroke="currentColor" strokeWidth="3" fill="none" strokeLinecap="round"></path>
    </svg>
  );
}

function Btn({ variant = "outline", size, icon, children, className = "", ...rest }) {
  const cls = ["btn", "btn-" + variant, size ? "btn-" + size : "", !children && icon ? "btn-icon" : "", className].join(" ");
  return (
    <button className={cls} {...rest}>
      {icon ? <Icon name={icon} size={size === "sm" ? 13 : 15} /> : null}
      {children}
    </button>
  );
}

function Badge({ tone = "default", dot, children, className = "", style }) {
  return (
    <span className={`badge ${tone !== "default" ? "badge-" + tone : ""} ${className}`} style={style}>
      {dot ? <span className="dot"></span> : null}{children}
    </span>
  );
}

function StatusBadge({ status, map, pulse }) {
  const m = (map || APP_STATUS)[status] || { label: status, tone: "default" };
  return (
    <Badge tone={m.tone}>
      <span className={"dot" + ((pulse || status === "deploying" || status === "running") ? " pulse-dot" : "")}></span>
      {m.label}
    </Badge>
  );
}

function TypeBadge({ type }) {
  const t = DEPLOY_TYPES[type];
  if (!t) return null;
  return <Badge tone={t.tone}>{t.label}</Badge>;
}

function Field({ label, hint, children, style }) {
  return (
    <div style={style}>
      <label className="field-label">{label}</label>
      {children}
      {hint ? <div className="field-hint">{hint}</div> : null}
    </div>
  );
}

function Select({ options, value, onChange, className = "", ...rest }) {
  return (
    <span className="select-wrap">
      <select className={`select-el ${className}`} value={value} onChange={(e) => onChange && onChange(e.target.value)} {...rest}>
        {options.map((o) => {
          const v = typeof o === "string" ? o : o.value;
          const l = typeof o === "string" ? o : o.label;
          const d = typeof o === "object" && !!o.disabled;
          return <option key={v} value={v} disabled={d}>{l}</option>;
        })}
      </select>
      <Icon name="chevronD" size={14} className="select-caret" aria-hidden="true" />
    </span>
  );
}

function Switch({ on, onChange }) {
  return <button type="button" className="switch" data-on={String(!!on)} onClick={() => onChange && onChange(!on)} aria-pressed={!!on}></button>;
}

// Checkbox:自绘方框 + 勾,替代原生 input[type=checkbox](跨浏览器外观一致,套设计系统 token)。
// 用 button + aria-pressed 而非隐藏 input,避免原生外观在 UOS/麒麟等国产化浏览器上不一致。
function Checkbox({ checked, onChange, disabled, label, ariaLabel }) {
  return (
    <button type="button" className="mc-check" data-on={String(!!checked)} disabled={disabled}
      aria-pressed={!!checked} aria-label={ariaLabel || label}
      onClick={() => onChange && onChange(!checked)}>
      <span className="mc-check-box">
        {checked ? <Icon name="check" size={12} style={{ color: "var(--primary-fg)" }} /> : null}
      </span>
      {label ? <span className="mc-check-label">{label}</span> : null}
    </button>
  );
}

function Tabs({ tabs, active, onChange, style }) {
  return (
    <div className="tabs" style={style}>
      {tabs.map((t) => (
        <button key={t.id} className="tab" data-active={String(t.id === active)} onClick={() => onChange(t.id)}>
          {t.icon ? <Icon name={t.icon} size={14} /> : null}
          {t.label}
          {t.count != null ? <span className="count">{t.count}</span> : null}
        </button>
      ))}
    </div>
  );
}

function Seg({ options, value, onChange }) {
  return (
    <div className="seg">
      {options.map((o) => {
        const v = typeof o === "string" ? o : o.value;
        const l = typeof o === "string" ? o : o.label;
        return <button key={v} data-active={String(v === value)} onClick={() => onChange(v)}>{l}</button>;
      })}
    </div>
  );
}

function Dialog({ open, onClose, title, desc, width, children, foot, noClose }) {
  React.useEffect(() => {
    if (!open) return;
    const fn = (e) => { if (e.key === "Escape" && !noClose) onClose && onClose(); };
    window.addEventListener("keydown", fn);
    // 打开时锁住背景滚动，避免遮罩下页面滚动与弹层裁切感
    const prev = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      window.removeEventListener("keydown", fn);
      document.body.style.overflow = prev;
    };
  }, [open, noClose, onClose]);
  if (!open) return null;
  // portal 到 body：避开 .content / .content-inner 的 overflow、动画 transform 造成的
  // fixed 定位失效、顶栏/PageHead 压住弹层、以及背后容器局部发灰的观感问题。
  return createPortal(
    <div
      className="dialog-overlay"
      role="presentation"
      onMouseDown={(e) => { if (e.target === e.currentTarget && !noClose) onClose && onClose(); }}
    >
      <div
        className="dialog"
        role="dialog"
        aria-modal="true"
        style={width ? { width } : undefined}
        onMouseDown={(e) => e.stopPropagation()}
      >
        <div className="dialog-head">
          <div style={{ minWidth: 0 }}>
            <h3 style={{ fontSize: 16 }}>{title}</h3>
            {desc ? <p style={{ fontSize: 12.5, color: "var(--muted-fg)", marginTop: 3 }}>{desc}</p> : null}
          </div>
          {!noClose ? <Btn variant="ghost" size="sm" icon="x" onClick={onClose} aria-label="关闭"></Btn> : null}
        </div>
        <div className="dialog-body">{children}</div>
        {foot ? <div className="dialog-foot">{foot}</div> : null}
      </div>
    </div>,
    document.body,
  );
}

function Progress({ value, color, height = 8 }) {
  return (
    <div className="progress-track" style={{ height }}>
      <div className="progress-fill" style={{ width: `${Math.min(100, Math.max(0, value))}%`, background: color }}></div>
    </div>
  );
}

function Sparkline({ data, color = "var(--primary)", width = 120, height = 32 }) {
  const max = 100;
  const pts = data.map((v, i) => `${(i / (data.length - 1)) * width},${height - (v / max) * height}`).join(" ");
  return (
    <svg width={width} height={height} style={{ display: "block", overflow: "visible" }} aria-hidden="true">
      <polyline points={pts} fill="none" stroke={color} strokeWidth="1.6" strokeLinejoin="round"></polyline>
    </svg>
  );
}

function EmptyState({ icon = "box", title, desc, action }) {
  return (
    <div style={{ textAlign: "center", padding: "44px 20px", color: "var(--muted-fg)" }}>
      <div style={{ display: "inline-flex", padding: 14, borderRadius: 14, background: "var(--muted)", marginBottom: 12 }}>
        <Icon name={icon} size={22} />
      </div>
      <div style={{ fontWeight: 600, color: "var(--fg)", fontSize: 14 }}>{title}</div>
      {desc ? <p style={{ fontSize: 12.5, marginTop: 4 }}>{desc}</p> : null}
      {action ? <div style={{ marginTop: 14 }}>{action}</div> : null}
    </div>
  );
}

// ---------- Toast ----------
function ToastHost() {
  const [items, setItems] = React.useState([]);
  React.useEffect(() => {
    window.__mcToast = (msg, opts = {}) => {
      const id = Math.random().toString(36).slice(2);
      setItems((s) => [...s, { id, msg, ...opts }]);
      setTimeout(() => setItems((s) => s.filter((t) => t.id !== id)), opts.dur || 3200);
    };
    return () => { window.__mcToast = null; };
  }, []);
  return (
    <div className="toast-wrap">
      {items.map((t) => (
        <div key={t.id} className="toast">
          <span style={{ color: t.tone === "error" ? "var(--error)" : t.tone === "warn" ? "var(--warn)" : "var(--success)", display: "flex" }}>
            <Icon name={t.icon || (t.tone === "error" ? "alert" : "check")} size={15} />
          </span>
          <span>{t.msg}</span>
        </div>
      ))}
    </div>
  );
}
const toast = (msg, opts) => window.__mcToast && window.__mcToast(msg, opts);

// ---------- Confirm(命令式确认框,替代浏览器原生 confirm) ----------
// 用法:const ok = await confirmDialog({ title, message, confirmText, cancelText, tone, icon });
// tone="danger" 用 destructive 红色按钮;返回 Promise<boolean>(取消/Esc/点遮罩 = false)。
function ConfirmHost() {
  const [st, setSt] = React.useState(null); // { ...opts, resolve } | null
  React.useEffect(() => {
    window.__mcConfirm = (opts) => new Promise((resolve) => {
      setSt((prev) => {
        if (prev) prev.resolve(false); // 已有未决确认框:旧的按取消收尾,避免悬挂
        return { ...opts, resolve };
      });
    });
    return () => { window.__mcConfirm = null; };
  }, []);
  if (!st) return null;
  const done = (ok) => { const r = st.resolve; setSt(null); r(ok); };
  const danger = st.tone === "danger";
  return (
    <Dialog open onClose={() => done(false)} width={st.width || 460} title={st.title || "确认操作"}
      foot={<React.Fragment>
        <Btn variant="ghost" onClick={() => done(false)}>{st.cancelText || "取消"}</Btn>
        <Btn variant={danger ? "destructive" : "primary"} icon={st.icon || (danger ? "trash" : "check")}
          onClick={() => done(true)}>{st.confirmText || "确认"}</Btn>
      </React.Fragment>}>
      <div style={{ fontSize: 13, lineHeight: 1.65, color: "var(--fg)", whiteSpace: "pre-wrap" }}>{st.message}</div>
    </Dialog>
  );
}
// confirmDialog:字符串或选项对象皆可;ConfirmHost 未挂载时降级到原生 confirm 兜底。
const confirmDialog = (opts) => {
  const o = typeof opts === "string" ? { message: opts } : (opts || {});
  return window.__mcConfirm ? window.__mcConfirm(o) : Promise.resolve(window.confirm(o.message || ""));
};

function CopyChip({ text, label }) {
  const [ok, setOk] = React.useState(false);
  return (
    <button className="link-btn mono" style={{ display: "inline-flex", alignItems: "center", gap: 5, fontSize: 12 }}
      onClick={async () => {
        if (await copyText(text)) {
          setOk(true); setTimeout(() => setOk(false), 1500);
        }
      }}>
      {label || text}
      <Icon name={ok ? "check" : "copy"} size={12} />
    </button>
  );
}

// 内网 HTTP 页面通常没有 Clipboard API；用受控 textarea 兼容复制，且只在确实成功时提示。
async function copyText(text) {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(String(text));
      return true;
    }
  } catch (e) { /* insecure context / permission denied 时走兼容路径 */ }
  const input = document.createElement('textarea');
  input.value = String(text);
  input.setAttribute('readonly', '');
  input.style.position = 'fixed';
  input.style.opacity = '0';
  document.body.appendChild(input);
  input.select();
  let copied = false;
  try { copied = document.execCommand('copy'); } catch (e) { copied = false; }
  input.remove();
  return copied;
}

export {
  Icon, Spinner, Btn, Badge, StatusBadge, TypeBadge, Field, Select, Switch, Checkbox,
  Tabs, Seg, Dialog, Progress, Sparkline, EmptyState, ToastHost, toast,
  ConfirmHost, confirmDialog, CopyChip, copyText,
};
