// Mooncell — 根组件:全局 Store + 路由 + 主题
import React from 'react';
import { useTweaks } from './lib/tweaks.js';
import {
  MCStore, INITIAL_APPS, INITIAL_RELEASES, INITIAL_BACKUPS, INITIAL_CABINET, INITIAL_AUDIT,
  tsDir, MC_DAY,
} from './lib/data.js';
import { ToastHost, toast } from './components/primitives.jsx';
import { Shell } from './components/Shell.jsx';
import { LoginPage, SetupWizard } from './pages/Login.jsx';
import { OverviewPage, CabinetPage, AuditPage } from './pages/Overview.jsx';
import { AppsPage } from './pages/Apps.jsx';
import { AppDetailPage } from './pages/AppDetail.jsx';
import { logout as apiLogout, getSession } from './lib/api.js';

const TWEAK_DEFAULTS = {
  "dark": false,
  "logFs": 12,
};

function App() {
  const [t, setTweak] = useTweaks(TWEAK_DEFAULTS);
  React.useEffect(() => {
    document.documentElement.setAttribute("data-theme", t.dark ? "dark" : "light");
    document.documentElement.style.setProperty("--console-fs", t.logFs + "px");
  }, [t.dark, t.logFs]);

  // ---- session & view ----
  // 会话由后端 httpOnly cookie 维持;挂载时向 /api/session 查询当前登录态。
  const [session, setSession] = React.useState(null);
  const [view, setView] = React.useState("login");
  const user = session || "admin";

  React.useEffect(() => {
    let alive = true;
    getSession().then((u) => {
      if (alive && u) { setSession(u); setView("console"); }
    });
    return () => { alive = false; };
  }, []);

  // ---- route ----
  const [route, setRoute] = React.useState(() => {
    try { return JSON.parse(localStorage.getItem("mc_route")) || { page: "overview" }; }
    catch (e) { return { page: "overview" }; }
  });
  const nav = (page, opts = {}) => {
    const r = { page, appId: opts.appId, tab: opts.tab || (page === "app-detail" ? (opts.tab || "overview") : undefined) };
    setRoute(r);
    try { localStorage.setItem("mc_route", JSON.stringify(r)); } catch (e) {}
  };

  // ---- domain state ----
  const [apps, setApps] = React.useState(INITIAL_APPS);
  const [releases, setReleases] = React.useState(INITIAL_RELEASES);
  const [backups, setBackups] = React.useState(INITIAL_BACKUPS);
  const [cabinet, setCabinet] = React.useState(INITIAL_CABINET);
  const [audit, setAudit] = React.useState(INITIAL_AUDIT);

  const addAudit = (action, target, result) => {
    setAudit((s) => [{ id: "a" + Date.now() + Math.random(), time: Date.now(), user, action, target, result, ip: "192.168.10.2" }, ...s]);
  };
  const patchApp = (id, patch) => setApps((s) => s.map((a) => (a.id === id ? { ...a, ...patch } : a)));

  const store = {
    user, nav, route,
    apps, releases, backups, cabinet, audit,

    finishDeploy(app, { version, size, result }) {
      const now = Date.now();
      setBackups((s) => [{ id: "b" + now, appId: app.id, version: app.version, time: now, size: size || "—", auto: true, operator: user, dir: tsDir(now), note: "" }, ...s]);
      setReleases((s) => [{ id: "r" + now, appId: app.id, version, status: result === "success" ? "success" : "rolledback", time: now, operator: user, duration: (30 + Math.random() * 45 | 0) + "s", size: size || "—" }, ...s]);
      if (result === "success") {
        patchApp(app.id, {
          version, lastDeploy: now,
          status: app.type === "static-nginx" ? "static" : "running",
          pid: app.type === "static-nginx" ? null : 20000 + (Math.random() * 9000 | 0),
          uptime: "刚刚", cpu: "1.0%", mem: app.mem === "—" ? "320 MB" : app.mem,
        });
        addAudit("部署", `${app.name} ${version}`, "成功");
        toast(`${app.name} · ${version} 部署成功`);
      } else {
        patchApp(app.id, { status: app.type === "static-nginx" ? "static" : "running", lastDeploy: now });
        addAudit("部署", `${app.name} ${version}`, "失败");
        addAudit("回滚", `${app.name} → ${app.version}(自动)`, "成功");
        toast(`部署失败 · 已自动回滚至 ${app.version}`, { tone: "warn", icon: "rotate" });
      }
    },

    finishRestore(app, backup) {
      const now = Date.now();
      setBackups((s) => [{ id: "b" + now, appId: app.id, version: app.version, time: now, size: backup.size, auto: true, operator: user, dir: tsDir(now), note: "还原前自动备份" }, ...s]);
      patchApp(app.id, {
        version: backup.version, lastDeploy: now,
        status: app.type === "static-nginx" ? "static" : "running",
        pid: app.type === "static-nginx" ? null : 20000 + (Math.random() * 9000 | 0),
        uptime: "刚刚",
      });
      addAudit("还原", `${app.name} → 备份 ${backup.dir}(${backup.version})`, "成功");
      toast(`${app.name} 已还原至 ${backup.version}`);
    },

    toggleApp(app, on) {
      patchApp(app.id, on
        ? { status: "running", pid: 20000 + (Math.random() * 9000 | 0), uptime: "刚刚", cpu: "0.8%", mem: app.mem === "—" ? "280 MB" : app.mem }
        : { status: "stopped", pid: null, uptime: "—", cpu: "—", mem: "—" });
      addAudit(on ? "启动服务" : "停止服务", app.name, "成功");
      toast(`${app.name} 已${on ? "启动" : "停止"}`);
    },

    addApp(app) {
      setApps((s) => [...s, app]);
      addAudit("创建应用", app.name, "成功");
      toast(`应用「${app.name}」创建成功,预检通过`);
      nav("app-detail", { appId: app.id });
    },

    updateApp(id, patch) {
      patchApp(id, patch);
      const a = apps.find((x) => x.id === id);
      addAudit("修改配置", (a ? a.name : id), "成功");
      toast("配置已保存 · Agent 端校验通过");
    },

    addManualBackup(app) {
      const now = Date.now();
      setBackups((s) => [{ id: "b" + now, appId: app.id, version: app.version, time: now, size: "≈ " + (10 + Math.random() * 40 | 0) + " MB", auto: false, operator: user, dir: tsDir(now), note: "手动备份" }, ...s]);
      addAudit("手动备份", app.name, "成功");
      toast(`已创建手动备份 backups/${app.id}/${tsDir(now)}/`);
    },

    deleteBackup(app, b) {
      setBackups((s) => s.filter((x) => x.id !== b.id));
      addAudit("删除备份", `${app.name} · ${b.dir}`, "成功");
      toast("备份已删除", { icon: "trash" });
    },

    addCabinetFile(name, size, anon) {
      const code = Array.from({ length: 4 }, () => "ABCDEFGHJKMNPQRSTWXYZ123456789"[Math.random() * 30 | 0]).join("");
      setCabinet((s) => [{
        id: "cf" + Date.now(), name, size,
        uploader: anon ? "192.168.10.99(匿名)" : user, time: Date.now(),
        expires: Date.now() + 7 * MC_DAY, code, public: false, downloads: 0,
      }, ...s]);
      if (!anon) addAudit("上传文件", "文件柜 · " + name, "成功");
      toast(`上传成功 · 提取码 ${code}(7 天后过期)`);
    },

    deleteCabinetFile(f) {
      setCabinet((s) => s.filter((x) => x.id !== f.id));
      addAudit("删除文件", "文件柜 · " + f.name, "成功");
      toast("文件已删除", { icon: "trash" });
    },

    toggleCabinetPublic(f) {
      setCabinet((s) => s.map((x) => (x.id === f.id ? { ...x, public: !x.public } : x)));
      toast(f.public ? `「${f.name}」已设为私有` : `「${f.name}」已公开,匿名可见`);
    },
  };

  // ---- auth handlers ----
  // cookie 已由 /api/login 在登录成功时种下,这里只更新前端状态。
  const login = (u) => {
    setSession(u); setView("console");
    toast(`欢迎回来,${u}`);
  };
  const logout = async () => {
    await apiLogout();
    setSession(null); setView("login");
  };

  // ---- crumbs ----
  const detailApp = route.page === "app-detail" ? apps.find((a) => a.id === route.appId) : null;
  const crumbs =
    route.page === "overview" ? [{ label: "总览" }] :
    route.page === "apps" ? [{ label: "应用" }] :
    route.page === "app-detail" ? [{ label: "应用", onClick: () => nav("apps") }, { label: detailApp ? detailApp.name : "详情" }] :
    route.page === "cabinet" ? [{ label: "文件柜" }] :
    [{ label: "审计日志" }];

  const screenLabel =
    view !== "console" ? (view === "login" ? "登录" : "初始化向导") :
    route.page === "app-detail" ? `应用详情 · ${detailApp ? detailApp.name : ""}` :
    ({ overview: "总览", apps: "应用列表", cabinet: "文件柜", audit: "审计日志" })[route.page] || route.page;

  return (
    <MCStore.Provider value={store}>
      <div data-screen-label={screenLabel} style={{ height: "100%" }}>
        {view === "login" ? <LoginPage onLogin={login} onWizard={() => setView("wizard")} /> : null}
        {view === "wizard" ? <SetupWizard onDone={login} onBack={() => setView("login")} /> : null}
        {view === "console" ? (
          <Shell page={route.page} onNav={(p) => nav(p)} crumbs={crumbs}
            theme={t.dark ? "dark" : "light"} onTheme={() => setTweak("dark", !t.dark)}
            user={user} onLogout={logout}>
            {route.page === "overview" ? <OverviewPage /> : null}
            {route.page === "apps" ? <AppsPage /> : null}
            {route.page === "app-detail" ? (
              <AppDetailPage appId={route.appId} tab={route.tab || "overview"}
                onTab={(tab) => nav("app-detail", { appId: route.appId, tab })} />
            ) : null}
            {route.page === "cabinet" ? <CabinetPage /> : null}
            {route.page === "audit" ? <AuditPage /> : null}
          </Shell>
        ) : null}
      </div>
      <ToastHost />
    </MCStore.Provider>
  );
}

export default App;
