// Mooncell — 登录页(仅真实后端登录,无任何前端绕过入口)
import React from 'react';
import { MoonLogo } from '../components/Shell.jsx';
import { Field, Btn, Spinner, Icon } from '../components/primitives.jsx';
import { login as apiLogin } from '../lib/api.js';

function LoginPage({ onLogin }) {
  const [u, setU] = React.useState("");
  const [p, setP] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [err, setErr] = React.useState("");
  const go = async () => {
    if (!u || !p) return;
    setErr(""); setBusy(true);
    try {
      const res = await apiLogin(u, p); // { user, role }
      onLogin(res);
    } catch (e) {
      setErr(e.message || "登录失败");
      setBusy(false);
    }
  };
  return (
    <div style={{ height: "100vh", display: "flex", alignItems: "center", justifyContent: "center", background: "var(--bg-sunken)", flexDirection: "column", gap: 18 }}>
      <div className="card fade-up" style={{ width: 372, padding: "34px 34px 28px", borderRadius: 16, boxShadow: "var(--shadow-md)" }}>
        <div style={{ display: "flex", flexDirection: "column", alignItems: "center", marginBottom: 24 }}>
          <MoonLogo size={44} />
          <h1 style={{ fontSize: 21, marginTop: 14, letterSpacing: "-0.02em" }}>Mooncell</h1>
          <p style={{ fontSize: 12.5, color: "var(--muted-fg)", marginTop: 3, whiteSpace: "nowrap" }}>内网自动化部署平台 · 上传即部署</p>
        </div>
        <div style={{ display: "flex", flexDirection: "column", gap: 13 }}>
          <Field label="用户名">
            <input className="input" value={u} onChange={(e) => setU(e.target.value)} autoComplete="username" />
          </Field>
          <Field label="密码">
            <input className="input" type="password" value={p} onChange={(e) => setP(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && go()} autoComplete="current-password" />
          </Field>
          {err ? (
            <div style={{ display: "flex", alignItems: "center", gap: 7, fontSize: 12.5, color: "var(--error)" }}>
              <Icon name="alert" size={14} style={{ flex: "none" }} />{err}
            </div>
          ) : null}
          <Btn variant="primary" size="lg" disabled={busy} onClick={go} style={{ marginTop: 4 }}>
            {busy ? <React.Fragment><Spinner size={14} /> 登录中…</React.Fragment> : "登录"}
          </Btn>
        </div>
        <div style={{ textAlign: "center", marginTop: 18, fontSize: 11.5, color: "var(--muted-fg)" }}>
          本地账号 · bcrypt · session + httpOnly cookie
        </div>
      </div>
    </div>
  );
}

export { LoginPage };
