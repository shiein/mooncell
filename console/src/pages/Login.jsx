// Mooncell — 登录页(仅真实后端登录,无任何前端绕过入口)
// 布局:宽屏左右分栏(左:产品说明 + 一次部署的流水回放;右:登录表单),窄屏只保留表单。
// 样式集中在 index.css 的「login」段;颜色全部走设计令牌,亮/暗主题自动适配。
import React from 'react';
import { MoonLogo } from '../components/Shell.jsx';
import { Spinner, Icon } from '../components/primitives.jsx';
import { login as apiLogin } from '../lib/api.js';

// 左侧「部署回放」:装饰性示意,阶段与真实部署链路一致(上传 → 校验 → 分发 → 重启 → 探活)。
const REPLAY_STEPS = [
  { label: "上传制品", detail: "order-service-2.14.0.jar", meta: "48.2 MB" },
  { label: "校验", detail: "sha256 一致", meta: "0.4s" },
  { label: "分发到 Agent", detail: "app-prod-02", meta: "3.1s" },
  { label: "重启进程", detail: "systemd · order-service", meta: "6.8s" },
  { label: "健康检查", detail: "HTTP 200 · /health", meta: "1.3s" },
];

// 内联图标:公共图标表里没有锁 / 闭眼,登录页专用,不扩散到 primitives。
function LockIcon({ size = 16 }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" stroke="currentColor" strokeWidth="1.7"
      strokeLinecap="round" strokeLinejoin="round" fill="none" aria-hidden="true">
      <rect x="4.5" y="10.5" width="15" height="10" rx="2" />
      <path d="M8 10.5V7.5a4 4 0 0 1 8 0v3" />
    </svg>
  );
}
function EyeOffIcon({ size = 16 }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" stroke="currentColor" strokeWidth="1.7"
      strokeLinecap="round" strokeLinejoin="round" fill="none" aria-hidden="true">
      <path d="M10.6 5.6A9.6 9.6 0 0 1 12 5.5c6 0 9.5 6.5 9.5 6.5a16 16 0 0 1-2.6 3.4" />
      <path d="M6.6 6.9C4 8.6 2.5 12 2.5 12S6 18.5 12 18.5a9 9 0 0 0 4.6-1.3" />
      <path d="M9.9 9.9a2.8 2.8 0 0 0 4 4" />
      <path d="M3.5 3.5l17 17" />
    </svg>
  );
}

function DeployReplay() {
  const total = REPLAY_STEPS.length;
  return (
    <div className="login-replay" aria-hidden="true">
      <div className="login-replay-head">
        <span className="login-replay-dots"><i /><i /><i /></span>
        <span className="mono">deploy · order-service</span>
        <span className="login-replay-tag mono">v2.14.0</span>
      </div>
      <ol className="login-replay-steps">
        {REPLAY_STEPS.map((s, i) => (
          <li key={s.label} style={{ animationDelay: `${240 + i * 120}ms` }}>
            <span className="login-replay-dot" />
            <span className="login-replay-label">{s.label}</span>
            <span className="login-replay-detail mono">{s.detail}</span>
            <span className="login-replay-meta mono">{s.meta}</span>
          </li>
        ))}
      </ol>
      <div className="login-replay-foot" style={{ animationDelay: `${240 + total * 120}ms` }}>
        <span className="login-replay-ok"><Icon name="check" size={13} />已上线</span>
        <span className="mono">11.6s · 制品已归档</span>
      </div>
    </div>
  );
}

function LoginPage({ onLogin }) {
  const [u, setU] = React.useState("");
  const [p, setP] = React.useState("");
  const [showPwd, setShowPwd] = React.useState(false);
  const [capsOn, setCapsOn] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const [err, setErr] = React.useState("");
  const userRef = React.useRef(null);
  const pwdRef = React.useRef(null);

  const go = async (e) => {
    if (e) e.preventDefault();
    if (busy) return;
    // 空字段:聚焦到缺的那一项,而不是静默无反应
    if (!u) { if (userRef.current) userRef.current.focus(); return; }
    if (!p) { if (pwdRef.current) pwdRef.current.focus(); return; }
    setErr(""); setBusy(true);
    try {
      const res = await apiLogin(u, p); // { user, role }
      if (!res || !res.user) {
        setErr("登录响应异常,请刷新页面后重试");
        return;
      }
      onLogin(res);
    } catch (e2) {
      setErr(e2.message || "登录失败");
    } finally {
      // 成功进入主壳后 Login 会卸载;失败/异常必须清 busy,避免一直「登录中…」
      setBusy(false);
    }
  };

  const trackCaps = (e) => {
    if (e && typeof e.getModifierState === "function") setCapsOn(e.getModifierState("CapsLock"));
  };

  return (
    <div className="login">
      <aside className="login-brand">
        <div className="login-brand-top">
          <MoonLogo size={28} />
          <span className="login-wordmark">Mooncell</span>
        </div>

        <div className="login-brand-body fade-up">
          <h1 className="login-headline">上传即部署</h1>
          <p className="login-lede">
            面向内网服务器的自动化部署平台。制品校验、Agent 分发、进程托管与健康检查,一次上传走完。
          </p>
          <DeployReplay />
        </div>

        <div className="login-brand-foot">内网自动化部署平台</div>
      </aside>

      <main className="login-main">
        <div className="login-panel fade-up">
          <div className="login-compact-brand">
            <MoonLogo size={36} />
            <span className="login-wordmark">Mooncell</span>
          </div>

          <h2 className="login-title">登录控制台</h2>
          <p className="login-subtitle">使用管理员分配的本地账号</p>

          <form className="login-form" onSubmit={go} noValidate>
            <div>
              <label className="field-label" htmlFor="login-username">用户名</label>
              <div className="login-input-wrap">
                <span className="login-input-icon"><Icon name="user" size={16} /></span>
                <input id="login-username" ref={userRef} className="input login-input" value={u}
                  onChange={(e) => setU(e.target.value)} autoComplete="username"
                  autoCapitalize="off" autoCorrect="off" spellCheck={false} autoFocus />
              </div>
            </div>

            <div>
              <label className="field-label" htmlFor="login-password">密码</label>
              <div className="login-input-wrap">
                <span className="login-input-icon"><LockIcon size={16} /></span>
                <input id="login-password" ref={pwdRef} className="input login-input login-input-pwd"
                  type={showPwd ? "text" : "password"} value={p}
                  onChange={(e) => setP(e.target.value)} onKeyDown={trackCaps} onKeyUp={trackCaps}
                  onBlur={() => setCapsOn(false)} autoComplete="current-password" />
                <button type="button" className="login-eye" onClick={() => setShowPwd((v) => !v)}
                  aria-label={showPwd ? "隐藏密码" : "显示密码"} title={showPwd ? "隐藏密码" : "显示密码"}>
                  {showPwd ? <EyeOffIcon size={16} /> : <Icon name="eye" size={16} />}
                </button>
              </div>
              {capsOn ? <div className="login-caps">大写锁定已开启</div> : null}
            </div>

            {err ? (
              <div className="login-error" role="alert">
                <Icon name="alert" size={15} style={{ flex: "none", marginTop: 2 }} />
                <span>{err}</span>
              </div>
            ) : null}

            <button type="submit" className="btn btn-primary btn-lg login-submit" disabled={busy}>
              {busy ? <React.Fragment><Spinner size={14} /> 登录中…</React.Fragment> : "登录"}
            </button>
          </form>

          <div className="login-divider"><span>无需账号</span></div>

          <a href="/drop" className="login-drop">
            <span className="login-drop-icon"><Icon name="upload" size={16} /></span>
            <span className="login-drop-text">
              <span className="login-drop-title">文件投递与取件</span>
              <span className="login-drop-sub">免登录上传文件,或凭提取码取件</span>
            </span>
            <Icon name="chevronR" size={16} className="login-drop-arrow" />
          </a>
        </div>

        <div className="login-legal">会话由 httpOnly Cookie 保持 · 密码以 bcrypt 存储</div>
      </main>
    </div>
  );
}

export { LoginPage };
