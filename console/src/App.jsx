// Mooncell — 根组件:全局 Store + 路由 + 主题
import React from 'react';
import { useTweaks } from './lib/tweaks.js';
import {
  MCStore, INITIAL_APPS, INITIAL_RELEASES, INITIAL_BACKUPS, INITIAL_CABINET, INITIAL_AUDIT,
  tsDir, MC_DAY, fmtBytes,
} from './lib/data.js';
import { ToastHost, toast } from './components/primitives.jsx';
import { Shell } from './components/Shell.jsx';
import { LoginPage } from './pages/Login.jsx';
import { OverviewPage, CabinetPage, AuditPage } from './pages/Overview.jsx';
import { AppsPage } from './pages/Apps.jsx';
import { AppDetailPage } from './pages/AppDetail.jsx';
import { UsersPage } from './pages/Users.jsx';
import { AgentsPage } from './pages/Agents.jsx';
import { logout as apiLogout, getSession, hydrateData, putEntity, deleteEntity, removeCabinetFile, setAppLifecycle } from './lib/api.js';

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
  const [role, setRole] = React.useState("admin");
  const [view, setView] = React.useState("login");
  const user = session || "admin";

  React.useEffect(() => {
    let alive = true;
    getSession().then((s) => {
      if (alive && s) { setSession(s.user); setRole(s.role || "viewer"); setView("console"); }
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
  // 初始为 mock,登录后从后端水合(首启用 mock 作种子,后续取持久化数据);后端不可达则保留 mock。
  const [apps, setApps] = React.useState(INITIAL_APPS);
  const [releases, setReleases] = React.useState(INITIAL_RELEASES);
  const [backups, setBackups] = React.useState(INITIAL_BACKUPS);
  const [cabinet, setCabinet] = React.useState(INITIAL_CABINET);
  const [audit, setAudit] = React.useState(INITIAL_AUDIT);

  const hydratedRef = React.useRef(false);
  React.useEffect(() => {
    if (!session || hydratedRef.current) return;
    hydratedRef.current = true;
    hydrateData({
      apps: INITIAL_APPS, releases: INITIAL_RELEASES, backups: INITIAL_BACKUPS,
      cabinet: INITIAL_CABINET, audit: INITIAL_AUDIT,
    }).then((data) => {
      if (!data) return; // 后端不可达:保留 mock,页面照常 1:1
      const byTimeDesc = (arr) => [...(arr || [])].sort((a, b) => (b.time || 0) - (a.time || 0));
      // 后端可达即以库为准(即便为空):空库必须清空 mock,否则生产环境会一直显示演示应用。
      setApps(data.apps || []); // apps 保持插入顺序
      setReleases(byTimeDesc(data.releases));
      setBackups(byTimeDesc(data.backups));
      setCabinet(byTimeDesc(data.cabinet));
      setAudit(byTimeDesc(data.audit));
    });
  }, [session]);

  // 镜像写:乐观更新已在前端完成,这里把结果落库(失败仅 console 告警,不打断 UI)。
  const persist = (kind, obj) => putEntity(kind, obj);
  const remove = (kind, id) => deleteEntity(kind, id);

  // 审计为服务端只追加(真实操作经 Agent 时由 Console 权威 appendAudit;后端已禁止前端写 kind=audit)。
  // 前端 addAudit 只做乐观显示,不落库——演示/模拟操作的审计仅当次可见,刷新后以服务端权威记录为准。
  const addAudit = (action, target, result) => {
    const a = { id: "a" + Date.now() + Math.random(), time: Date.now(), user, action, target, result, ip: "192.168.10.2" };
    setAudit((s) => [a, ...s]);
  };
  const patchApp = (id, patch) => setApps((s) => s.map((a) => {
    if (a.id !== id) return a;
    const next = { ...a, ...patch };
    persist("app", next);
    return next;
  }));
  // patchAppLocal:仅更新本地显示,不落库。真机操作(部署/还原/启停)的 version/status 由 Console
  // 服务端权威落库(applyAppRuntimeState),前端不再重复 persist——刷新后以服务端记录为准。
  const patchAppLocal = (id, patch) => setApps((s) => s.map((a) => (a.id === id ? { ...a, ...patch } : a)));

  const store = {
    user, role, nav, route,
    apps, releases, backups, cabinet, audit,
    // 角色权限:write = operator/admin 可改;admin = 仅管理员(用户管理)。viewer 只读。
    can: (perm) => (perm === "admin" ? role === "admin" : role !== "viewer"),

    // real:经 Agent 的真机部署。其审计由 Console 服务端权威落库;前端 addAudit 只乐观显示不落库。
    // 三态:success / rolledback(失败已回滚到旧版本)/ failed(失败且未能回滚)——不坍缩。
    finishDeploy(app, { version, size, result, real }) {
      const now = Date.now();
      const backup = { id: "b" + now, appId: app.id, version: app.version, time: now, size: size || "—", auto: true, operator: user, dir: tsDir(now), note: "" };
      const release = { id: "r" + now, appId: app.id, version, status: result, time: now, operator: user, duration: (30 + Math.random() * 45 | 0) + "s", size: size || "—" };
      // 真实部署:release 由 Console 服务端权威落库、backup 在 Agent 真实生成;前端只乐观显示不落库。
      setBackups((s) => [backup, ...s]); if (!real) persist("backup", backup);
      setReleases((s) => [release, ...s]); // release 服务端权威,前端不落库(刷新读服务端记录)
      // 真机操作:status/version 由 Console 服务端权威落库,前端仅本地即时显示(patchAppLocal,不 persist)。
      const setState = real ? patchAppLocal : patchApp;
      if (result === "success") {
        setState(app.id, {
          version, lastDeploy: now,
          status: app.type === "static-nginx" ? "static" : "running",
          // 真实部署:运行态(pid/cpu/mem/uptime)由 Agent status 查询,前端不伪造随机值;模拟部署才填演示值。
          ...(real
            ? { pid: null, uptime: "—", cpu: "—", mem: "—" }
            : { pid: app.type === "static-nginx" ? null : 20000 + (Math.random() * 9000 | 0), uptime: "刚刚", cpu: "1.0%", mem: app.mem === "—" ? "320 MB" : app.mem }),
        });
        addAudit("部署", `${app.name} ${version}`, "成功");
        toast(`${app.name} · ${version} 部署成功`);
      } else if (result === "rolledback") {
        setState(app.id, { status: app.type === "static-nginx" ? "static" : "running", lastDeploy: now });
        if (real) {
          addAudit("部署", `${app.name} ${version}`, "失败·已回滚");
        } else {
          addAudit("部署", `${app.name} ${version}`, "失败");
          addAudit("回滚", `${app.name} → ${app.version}(自动)`, "成功");
        }
        toast(`部署失败 · 已自动回滚至 ${app.version}`, { tone: "warn", icon: "rotate" });
      } else {
        // failed:既没成功也没能回滚(如首次部署失败),应用进入异常态。
        setState(app.id, { status: "failed", lastDeploy: now, pid: null });
        addAudit("部署", `${app.name} ${version}`, "失败");
        toast(`${app.name} · ${version} 部署失败,未能回滚`, { tone: "error", icon: "alert" });
      }
    },

    finishRestore(app, backup, { real } = {}) {
      const now = Date.now();
      const bak = { id: "b" + now, appId: app.id, version: app.version, time: now, size: backup.size, auto: true, operator: user, dir: tsDir(now), note: "还原前自动备份" };
      // 真实还原:还原前备份在 Agent 真实生成、release 由服务端落库;前端只乐观显示。
      setBackups((s) => [bak, ...s]); if (!real) persist("backup", bak);
      // 真机还原:status/version 由 Console 服务端权威落库,前端仅本地即时显示。
      (real ? patchAppLocal : patchApp)(app.id, {
        version: backup.version, lastDeploy: now,
        status: app.type === "static-nginx" ? "static" : "running",
        // 真实还原:运行态由 Agent status 查询,不前端伪造。
        ...(real
          ? { pid: null, uptime: "—", cpu: "—", mem: "—" }
          : { pid: app.type === "static-nginx" ? null : 20000 + (Math.random() * 9000 | 0), uptime: "刚刚" }),
      });
      addAudit("还原", `${app.name} → 备份 ${backup.dir}(${backup.version})`, "成功");
      toast(`${app.name} 已还原至 ${backup.version}`);
    },

    async toggleApp(app, on) {
      const verb = on ? "启动" : "停止";
      // 真机启停(systemd/pm2),用 Agent 返回的真实状态刷新——绝不伪造 pid/cpu/mem。
      // 失败(Agent 不可达 / systemctl 出错 / 无托管单元)返回 null:报错,不写成功状态。
      const st = await setAppLifecycle(app.id, on ? "start" : "stop");
      if (!st) {
        toast(`${app.name} ${verb}失败(Agent 未响应或操作出错)`, { tone: "error", icon: "alert" });
        return;
      }
      // status/pid 由 Console 服务端 applyLifecycleState 权威落库,前端仅本地即时显示。
      patchAppLocal(app.id, {
        status: st.active ? "running" : "stopped",
        pid: st.active && st.pid && st.pid !== "0" ? (Number(st.pid) || st.pid) : null,
        uptime: st.active ? "刚刚" : "—", cpu: "—", mem: "—",
      });
      addAudit(on ? "启动服务" : "停止服务", app.name, "成功"); // 服务端 lifecycle 已权威落审计,这里仅乐观显示
      toast(`${app.name} 已${verb}`);
    },

    addApp(app) {
      setApps((s) => [...s, app]); persist("app", app);
      addAudit("创建应用", app.name, "成功");
      toast(`应用「${app.name}」创建成功,预检通过`);
      nav("app-detail", { appId: app.id });
    },

    updateApp(id, patch) {
      patchApp(id, patch); // patchApp 内部落库合并后的整应用
      const a = apps.find((x) => x.id === id);
      addAudit("修改配置", (a ? a.name : id), "成功");
      toast("配置已保存"); // 是否预检由调用方(配置页)负责并据实提示,这里不谎称"校验通过"
    },

    addManualBackup(app) {
      const now = Date.now();
      const bak = { id: "b" + now, appId: app.id, version: app.version, time: now, size: "≈ " + (10 + Math.random() * 40 | 0) + " MB", auto: false, operator: user, dir: tsDir(now), note: "手动备份" };
      setBackups((s) => [bak, ...s]); persist("backup", bak);
      addAudit("手动备份", app.name, "成功");
      toast(`已创建手动备份 backups/${app.id}/${tsDir(now)}/`);
    },

    deleteBackup(app, b) {
      setBackups((s) => s.filter((x) => x.id !== b.id)); remove("backup", b.id);
      addAudit("删除备份", `${app.name} · ${b.dir}`, "成功");
      toast("备份已删除", { icon: "trash" });
    },

    // 真实上传:后端已落盘 + 写元数据,这里把返回条目插入前端状态(size 转人类可读)。
    pushCabinetFile(meta, anon) {
      const f = { ...meta, size: fmtBytes(meta.size), downloads: meta.downloads || 0 };
      setCabinet((s) => [f, ...s]);
      if (!anon) addAudit("上传文件", "文件柜 · " + f.name, "成功");
      toast(`上传成功 · 提取码 ${f.code}(7 天后过期)`);
    },

    async deleteCabinetFile(f) {
      try { await removeCabinetFile(f.id); }
      catch (e) { toast(e.message || "删除失败", { tone: "error" }); return; }
      setCabinet((s) => s.filter((x) => x.id !== f.id));
      addAudit("删除文件", "文件柜 · " + f.name, "成功");
      toast("文件已删除", { icon: "trash" });
    },

    toggleCabinetPublic(f) {
      const next = { ...f, public: !f.public };
      setCabinet((s) => s.map((x) => (x.id === f.id ? next : x))); persist("cabinet", next);
      toast(f.public ? `「${f.name}」已设为私有` : `「${f.name}」已公开,匿名可见`);
    },
  };

  // ---- auth handlers ----
  // cookie 已由 /api/login 在登录成功时种下,这里只更新前端状态。
  // 只接受后端登录返回的 {user, role};不再有任何前端绕过入口(演示向导已移除)。
  const login = (res) => {
    if (!res || !res.user) return; // 防御:无后端返回不进入主壳
    setSession(res.user); setRole(res.role || "viewer"); setView("console");
    toast(`欢迎回来,${res.user}`);
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
    route.page === "users" ? [{ label: "用户管理" }] :
    route.page === "agents" ? [{ label: "Agent 管理" }] :
    [{ label: "审计日志" }];

  const screenLabel =
    view !== "console" ? (view === "login" ? "登录" : "初始化向导") :
    route.page === "app-detail" ? `应用详情 · ${detailApp ? detailApp.name : ""}` :
    ({ overview: "总览", apps: "应用列表", cabinet: "文件柜", audit: "审计日志", users: "用户管理", agents: "Agent 管理" })[route.page] || route.page;

  return (
    <MCStore.Provider value={store}>
      <div data-screen-label={screenLabel} style={{ height: "100%" }}>
        {view === "login" ? <LoginPage onLogin={login} /> : null}
        {view === "console" ? (
          <Shell page={route.page} onNav={(p) => nav(p)} crumbs={crumbs}
            theme={t.dark ? "dark" : "light"} onTheme={() => setTweak("dark", !t.dark)}
            user={user} role={role} onLogout={logout}>
            {route.page === "overview" ? <OverviewPage /> : null}
            {route.page === "apps" ? <AppsPage /> : null}
            {route.page === "app-detail" ? (
              <AppDetailPage appId={route.appId} tab={route.tab || "overview"}
                onTab={(tab) => nav("app-detail", { appId: route.appId, tab })} />
            ) : null}
            {route.page === "cabinet" ? <CabinetPage /> : null}
            {route.page === "audit" ? <AuditPage /> : null}
            {route.page === "users" ? <UsersPage /> : null}
            {route.page === "agents" ? <AgentsPage /> : null}
          </Shell>
        ) : null}
      </div>
      <ToastHost />
    </MCStore.Provider>
  );
}

export default App;
