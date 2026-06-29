// Mooncell — UI 原语(shadcn 风格)
import React from 'react';
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
  zap: <path d="M13 3L5 13.5h6L11 21l8-10.5h-6z" fill="none"/>,
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
    return () => window.removeEventListener("keydown", fn);
  }, [open, noClose]);
  if (!open) return null;
  return (
    <div className="dialog-overlay" onMouseDown={(e) => { if (e.target === e.currentTarget && !noClose) onClose && onClose(); }}>
      <div className="dialog" style={width ? { width } : undefined}>
        <div className="dialog-head">
          <div>
            <h3 style={{ fontSize: 16 }}>{title}</h3>
            {desc ? <p style={{ fontSize: 12.5, color: "var(--muted-fg)", marginTop: 3 }}>{desc}</p> : null}
          </div>
          {!noClose ? <Btn variant="ghost" size="sm" icon="x" onClick={onClose} aria-label="关闭"></Btn> : null}
        </div>
        <div className="dialog-body">{children}</div>
        {foot ? <div className="dialog-foot">{foot}</div> : null}
      </div>
    </div>
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
      onClick={() => {
        try { navigator.clipboard && navigator.clipboard.writeText(text); } catch (e) {}
        setOk(true); setTimeout(() => setOk(false), 1500);
      }}>
      {label || text}
      <Icon name={ok ? "check" : "copy"} size={12} />
    </button>
  );
}

export {
  Icon, Spinner, Btn, Badge, StatusBadge, TypeBadge, Field, Select, Switch, Checkbox,
  Tabs, Seg, Dialog, Progress, Sparkline, EmptyState, ToastHost, toast,
  ConfirmHost, confirmDialog, CopyChip,
};
