// Mooncell — 登录页 + 首次初始化向导
import React from 'react';
import { MoonLogo } from '../components/Shell.jsx';
import { Field, Btn, Spinner, Icon, Select } from '../components/primitives.jsx';
import { DEPLOY_TYPES } from '../lib/data.js';
import { login as apiLogin } from '../lib/api.js';

function LoginPage({ onLogin, onWizard }) {
  const [u, setU] = React.useState("admin");
  const [p, setP] = React.useState("jch@9388");
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
      <button className="link-btn" style={{ fontSize: 12.5 }} onClick={onWizard}>演示:首次初始化向导 →</button>
    </div>
  );
}

function WizStep({ n, label, state }) {
  return (
    <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
      <div className="step-node" data-st={state === "done" ? "success" : state === "active" ? "running" : "pending"} style={{ width: 24, height: 24, fontSize: 11 }}>
        {state === "done" ? <Icon name="check" size={12} /> : n}
      </div>
      <span style={{ fontSize: 12.5, fontWeight: state === "active" ? 600 : 500, color: state === "pending" ? "var(--muted-fg)" : "var(--fg)" }}>{label}</span>
    </div>
  );
}

function SetupWizard({ onDone, onBack }) {
  const [step, setStep] = React.useState(0);
  const [admin, setAdmin] = React.useState({ user: "admin", pass: "", pass2: "" });
  const [agent, setAgent] = React.useState({ addr: "192.168.10.21:9100", token: "mc_ag_7f3e2a919d21" });
  const [conn, setConn] = React.useState("idle"); // idle testing ok
  const [app, setApp] = React.useState({ type: "java-jar", name: "", path: "" });
  const labels = ["创建管理员", "添加 Agent", "创建第一个应用", "完成"];

  const testConn = () => {
    setConn("testing");
    setTimeout(() => setConn("ok"), 1400);
  };

  return (
    <div style={{ minHeight: "100vh", display: "flex", alignItems: "center", justifyContent: "center", background: "var(--bg-sunken)", padding: 24 }}>
      <div className="card fade-up" style={{ width: 560, borderRadius: 16, overflow: "hidden" }}>
        <div style={{ padding: "22px 28px 0", display: "flex", alignItems: "center", gap: 12 }}>
          <MoonLogo size={30} />
          <div>
            <h2 style={{ fontSize: 16 }}>首次初始化</h2>
            <p style={{ fontSize: 12, color: "var(--muted-fg)" }}>三步完成 Console 初始化,即可开始部署</p>
          </div>
          <button className="link-btn" style={{ marginLeft: "auto", fontSize: 12, color: "var(--muted-fg)" }} onClick={onBack}>返回登录</button>
        </div>
        <div style={{ display: "flex", gap: 18, padding: "18px 28px", borderBottom: "1px solid var(--border)", flexWrap: "wrap" }}>
          {labels.map((l, i) => <WizStep key={l} n={i + 1} label={l} state={i < step ? "done" : i === step ? "active" : "pending"} />)}
        </div>

        <div style={{ padding: "22px 28px", minHeight: 250 }}>
          {step === 0 ? (
            <div style={{ display: "flex", flexDirection: "column", gap: 13 }}>
              <Field label="管理员用户名">
                <input className="input" value={admin.user} onChange={(e) => setAdmin({ ...admin, user: e.target.value })} />
              </Field>
              <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12 }}>
                <Field label="密码"><input className="input" type="password" value={admin.pass} onChange={(e) => setAdmin({ ...admin, pass: e.target.value })} placeholder="≥ 8 位" /></Field>
                <Field label="确认密码"><input className="input" type="password" value={admin.pass2} onChange={(e) => setAdmin({ ...admin, pass2: e.target.value })} /></Field>
              </div>
              <div style={{ fontSize: 11.5, color: "var(--muted-fg)" }}>账号存于本机 SQLite(bcrypt),角色:admin / operator / viewer。</div>
            </div>
          ) : null}

          {step === 1 ? (
            <div style={{ display: "flex", flexDirection: "column", gap: 13 }}>
              <Field label="Agent 地址" hint="目标机上的常驻服务,Console 主动连接">
                <input className="input mono" style={{ fontSize: 12.5 }} value={agent.addr} onChange={(e) => { setAgent({ ...agent, addr: e.target.value }); setConn("idle"); }} />
              </Field>
              <Field label="Token" hint="来自目标机 /opt/deploy-agent/config.yaml">
                <input className="input mono" style={{ fontSize: 12.5 }} value={agent.token} onChange={(e) => { setAgent({ ...agent, token: e.target.value }); setConn("idle"); }} />
              </Field>
              <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
                <Btn icon={conn === "testing" ? undefined : "zap"} disabled={conn === "testing"} onClick={testConn}>
                  {conn === "testing" ? <React.Fragment><Spinner size={13} /> 测试中…</React.Fragment> : "连通性测试"}
                </Btn>
                {conn === "ok" ? (
                  <span className="fade-up" style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 12.5, color: "var(--success)" }}>
                    <Icon name="check" size={14} />已连通 · 延迟 2ms · 能力: systemd / java / pm2 / nginx / python / node
                  </span>
                ) : null}
              </div>
            </div>
          ) : null}

          {step === 2 ? (
            <div style={{ display: "flex", flexDirection: "column", gap: 13 }}>
              <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12 }}>
                <Field label="部署类型">
                  <Select value={app.type} onChange={(v) => setApp({ ...app, type: v })}
                    options={Object.entries(DEPLOY_TYPES).map(([k, t]) => ({ value: k, label: t.label }))} />
                </Field>
                <Field label="应用名">
                  <input className="input" placeholder="如:数据查询平台后端" value={app.name} onChange={(e) => setApp({ ...app, name: e.target.value })} />
                </Field>
              </div>
              <Field label={app.type === "static-nginx" ? "目标目录" : "制品目标路径"}>
                <input className="input mono" style={{ fontSize: 12.5 }} placeholder={app.type === "static-nginx" ? "/data/web/my-app" : "/srv/apps/my-app/app.jar"}
                  value={app.path} onChange={(e) => setApp({ ...app, path: e.target.value })} />
              </Field>
              <div style={{ fontSize: 11.5, color: "var(--muted-fg)" }}>完整配置(JVM 参数、健康检查、日志路径等)可创建后在「应用 → 配置」中按 Schema 表单补全;保存时 Agent 端预检。</div>
              <button className="link-btn" style={{ alignSelf: "flex-start", fontSize: 12.5 }} onClick={() => setStep(3)}>暂时跳过,稍后创建 →</button>
            </div>
          ) : null}

          {step === 3 ? (
            <div style={{ textAlign: "center", padding: "26px 0" }}>
              <div style={{ display: "inline-flex", padding: 16, borderRadius: "50%", background: "var(--success-soft)", color: "var(--success)", marginBottom: 14 }}>
                <Icon name="check" size={26} />
              </div>
              <h3 style={{ fontSize: 17 }}>初始化完成</h3>
              <p style={{ fontSize: 13, color: "var(--muted-fg)", marginTop: 6 }}>
                管理员已创建 · Agent 已连通{app.name ? ` · 应用「${app.name}」已就绪` : ""}<br />
                日常流程:上传制品 → 自动流水线 → 实时进度 → 失败一键回滚
              </p>
            </div>
          ) : null}
        </div>

        <div style={{ padding: "14px 28px", borderTop: "1px solid var(--border)", display: "flex", justifyContent: "flex-end", gap: 10 }}>
          {step > 0 && step < 3 ? <Btn variant="ghost" icon="chevronL" onClick={() => setStep(step - 1)}>上一步</Btn> : null}
          {step === 0 ? <Btn variant="primary" disabled={!admin.user || !admin.pass || admin.pass !== admin.pass2} onClick={() => setStep(1)}>下一步</Btn> : null}
          {step === 1 ? <Btn variant="primary" disabled={conn !== "ok"} onClick={() => setStep(2)}>下一步</Btn> : null}
          {step === 2 ? <Btn variant="primary" disabled={!app.name || !app.path} onClick={() => setStep(3)}>下一步</Btn> : null}
          {step === 3 ? <Btn variant="primary" icon="chevronR" onClick={() => onDone(admin.user)}>进入控制台</Btn> : null}
        </div>
      </div>
    </div>
  );
}

export { LoginPage, SetupWizard };
