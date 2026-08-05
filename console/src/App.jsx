// Mooncell — 根组件:全局 Store + 路由 + 主题
import React from 'react';
import { useTweaks } from './lib/tweaks.js';
import {
  MCStore, INITIAL_APPS, INITIAL_RELEASES, INITIAL_BACKUPS, INITIAL_CABINET, INITIAL_AUDIT,
  tsDir, MC_DAY, fmtBytes,
} from './lib/data.js';
import { ToastHost, ConfirmHost, toast } from './components/primitives.jsx';
import { Shell } from './components/Shell.jsx';
import { LoginPage } from './pages/Login.jsx';
import { OverviewPage, CabinetPage, AuditPage } from './pages/Overview.jsx';
import { AppsPage } from './pages/Apps.jsx';
import { AppDetailPage } from './pages/AppDetail.jsx';
import { UsersPage } from './pages/Users.jsx';
import { AgentsPage } from './pages/Agents.jsx';
import { SystemPage } from './pages/System.jsx';
import { DataResourcesPage } from './pages/DataResources.jsx';
import { DataWorkspacePage } from './pages/DataWorkspace.jsx';
import { ServerResourcesPage } from './pages/ServerResources.jsx';
import { ServerWorkspacePage } from './pages/ServerWorkspace.jsx';
import { logout as apiLogout, getSession, touchSession, hydrateData, putEntity, saveAppConfig, deleteEntity, appDelete, setUnauthorizedHandler, removeCabinetFile, setCabinetFilePublic, setAppLifecycle } from './lib/api.js';

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
  // features 来自 /api/session 与 /api/login；开关未加载前 fail-closed
  const [features, setFeatures] = React.useState({ serverOperations: false });
  const user = session || "admin";

  // 独立工作台 hash：/#/server-operations/{id} — 不写入 mc_route
  const [workspaceResourceId, setWorkspaceResourceId] = React.useState(() => {
    const h = (typeof location !== "undefined" && location.hash) || "";
    const m = h.match(/^#\/server-operations\/([^/?#]+)/);
    return m ? decodeURIComponent(m[1]) : null;
  });
  React.useEffect(() => {
    const sync = () => {
      const h = location.hash || "";
      const m = h.match(/^#\/server-operations\/([^/?#]+)/);
      setWorkspaceResourceId(m ? decodeURIComponent(m[1]) : null);
    };
    window.addEventListener("hashchange", sync);
    sync();
    return () => window.removeEventListener("hashchange", sync);
  }, []);

  // ---- route ----
  // 非 admin 仅允许应用、数据资源与服务器运维相关页;初始化与切换时同步钳制。
  const clampRoute = React.useCallback((r, roleNow, feat) => {
    if (!r) return { page: "apps" };
    const f = feat || features;
    if (r.page === "server-operations" && !(f && f.serverOperations)) {
      return roleNow === "admin" ? { page: "overview" } : { page: "apps" };
    }
    if (roleNow && roleNow !== "admin") {
      const allowed = new Set(["apps", "app-detail", "data-resources", "data-workspace", "server-operations"]);
      if (!allowed.has(r.page)) return { page: "apps" };
    }
    return r;
  }, [features]);
  const [route, setRoute] = React.useState(() => {
    try { return JSON.parse(localStorage.getItem("mc_route")) || { page: "overview" }; }
    catch (e) { return { page: "overview" }; }
  });
  const nav = (page, opts = {}) => {
    const r = clampRoute({
      page,
      appId: opts.appId,
      tab: opts.tab || (page === "app-detail" ? (opts.tab || "overview") : undefined),
      resourceId: opts.resourceId,
      resource: opts.resource,
      workspace: opts.workspace,
    }, role, features);
    setRoute(r);
    try {
      // 不把完整 resource 对象写入 localStorage（含敏感配置）
      // 服务器工作台使用独立 hash，不写 mc_route
      const persist = { ...r };
      delete persist.resource;
      delete persist.workspace;
      localStorage.setItem("mc_route", JSON.stringify(persist));
    } catch (e) {}
  };
  const resetSessionRoute = React.useCallback(() => {
    const next = { page: "apps" };
    setRoute(next);
    try { localStorage.setItem("mc_route", JSON.stringify(next)); } catch (e) {}
  }, []);

  React.useEffect(() => {
    let alive = true;
    getSession().then((s) => {
      if (alive && s) {
        const r = s.role || "viewer";
        const feat = s.features || { serverOperations: false };
        setSession(s.user);
        setRole(r);
        setFeatures(feat);
        setRoute((cur) => clampRoute(cur, r, feat));
        setView("console");
      }
    });
    return () => { alive = false; };
  }, [clampRoute]);

  // 会话失效统一回登录页:任何请求 401(闲置 1h 超时 / 关浏览器后 cookie 失效)→ 清状态并提示重新登录。
  // 角色数据必须同步清空,否则同一浏览器切换账号时会短暂看到上一账号的应用/审计等缓存。
  React.useEffect(() => {
    setUnauthorizedHandler(() => {
      setSession((cur) => {
        if (cur) {
          setApps([]); setReleases([]); setBackups([]); setCabinet([]); setAudit([]);
          setBackupRevision({});
          setRole("viewer");
          setFeatures({ serverOperations: false });
          resetSessionRoute();
          setView("login");
          toast("会话已过期,请重新登录", { tone: "warn", icon: "alert" });
        }
        return null;
      });
    });
  }, [resetSessionRoute]);

  // 只让真实交互滑动续期；5 秒数据轮询、状态查询和其它后台请求只校验会话。
  React.useEffect(() => {
    if (!session) return;
    let lastTouch = 0;
    const onActivity = () => {
      const now = Date.now();
      if (now - lastTouch < 60_000) return;
      lastTouch = now;
      touchSession();
    };
    const events = ['pointerdown', 'keydown', 'wheel', 'touchstart'];
    for (const event of events) window.addEventListener(event, onActivity, { capture: true, passive: true });
    return () => {
      for (const event of events) window.removeEventListener(event, onActivity, { capture: true });
    };
  }, [session]);

  // 会话/角色就绪后同步钳制(含 localStorage 里的 admin-only 旧路由)。
  React.useEffect(() => {
    if (!session) return;
    setRoute((cur) => {
      const next = clampRoute(cur, role, features);
      if (next === cur || (next.page === cur.page && next.appId === cur.appId)) return cur;
      try { localStorage.setItem("mc_route", JSON.stringify(next)); } catch (e) {}
      return next;
    });
  }, [session, role, features, clampRoute]);

  // ---- domain state ----
  // 角色作用域数据默认空,登录后从后端权威水合。INITIAL_* 只作为 demo seed 发给后端,
  // 绝不在认证/ACL 数据尚未返回时直接渲染,避免账号切换时暴露上一账号或演示数据。
  const [apps, setApps] = React.useState([]);
  const [releases, setReleases] = React.useState([]);
  const [backups, setBackups] = React.useState([]);
  const [cabinet, setCabinet] = React.useState([]);
  const [audit, setAudit] = React.useState([]);
  const [backupRevision, setBackupRevision] = React.useState({});
  React.useEffect(() => {
    if (!session) return;
    let alive = true;
    // 每次会话身份/角色变化先 fail-closed 清空,再加载该身份可见的数据。
    setApps([]); setReleases([]); setBackups([]); setCabinet([]); setAudit([]);
    hydrateData({
      apps: INITIAL_APPS, releases: INITIAL_RELEASES, backups: INITIAL_BACKUPS,
      cabinet: INITIAL_CABINET, audit: INITIAL_AUDIT,
    }).then((data) => {
      if (!alive || !data) return;
      const byTimeDesc = (arr) => [...(arr || [])].sort((a, b) => (b.time || 0) - (a.time || 0));
      // 后端可达即以库为准(即便为空):空库必须清空 mock,否则生产环境会一直显示演示应用。
      setApps(data.apps || []); // apps 保持插入顺序
      setReleases(byTimeDesc(data.releases));
      setBackups(byTimeDesc(data.backups));
      setCabinet(byTimeDesc(data.cabinet));
      setAudit(byTimeDesc(data.audit));
    });
    return () => { alive = false; };
  }, [session, role]);

  // 周期性重拉应用列表:后台巡检更新的 status/pid、其他用户的增改删都会反映,不必重新登录。
  // 只覆盖 apps(不动 releases/audit 等视图,避免打断浏览);AppDetail 编辑用独立 draft,不受影响。
  // 落库均为"await 服务端确认后才更新本地",故此刷新读到的已是含本次改动的权威态,不会回退。
  React.useEffect(() => {
    if (!session) return;
    const iv = setInterval(() => {
      hydrateData({
        apps: INITIAL_APPS, releases: INITIAL_RELEASES, backups: INITIAL_BACKUPS,
        cabinet: INITIAL_CABINET, audit: INITIAL_AUDIT,
      }).then((data) => { if (data && data.apps) setApps(data.apps); });
    }, 5000);
    return () => clearInterval(iv);
  }, [session, role]);

  // 镜像写:乐观更新已在前端完成,这里把结果落库(失败仅 console 告警,不打断 UI)。
  // app 走类型化校验入口(saveAppConfig),其余走通用 putEntity。
  const persist = (kind, obj) => {
    if (kind === "app") {
      return saveAppConfig(obj).then((res) => {
        if (res && res.error) { console.error("[persist] app", res.error); toast("配置落库被拒:" + res.error, { tone: "error", icon: "alert" }); }
        return res;
      });
    }
    return putEntity(kind, obj);
  };
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
    apps, releases, backups, cabinet, audit, backupRevision,
    // 权限:
    // - admin/manage: 仅管理员(用户/Agent/系统/新建删除/改配置)
    // - write: 任意已登录(部署/还原/启停);细粒度应用 ACL 由服务端强制
    can: (perm) => {
      if (perm === "admin" || perm === "manage") return role === "admin";
      if (perm === "write") return !!role;
      return true;
    },

    // real:经 Agent 的真机部署。其审计由 Console 服务端权威落库;前端 addAudit 只乐观显示不落库。
    // 三态:success / rolledback(失败已回滚到旧版本)/ failed(失败且未能回滚)——不坍缩。
    finishDeploy(app, { version, size, result, real }) {
      const now = Date.now();
      const backup = { id: "b" + now, appId: app.id, version: app.version, time: now, size: size || "—", auto: true, operator: user, dir: tsDir(now), note: "" };
      const release = { id: "r" + now, appId: app.id, version, status: result, time: now, operator: user, duration: (30 + Math.random() * 45 | 0) + "s", size: size || "—" };
      // 真实部署:release 由 Console 服务端权威落库、backup 在 Agent 真实生成;前端只乐观显示不落库。
      if (!real) { setBackups((s) => [backup, ...s]); persist("backup", backup); }
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
      if (!real) { setBackups((s) => [bak, ...s]); persist("backup", bak); }
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

    // 批量启停:逐个走真机 lifecycle(权威状态回填,不伪造),汇总成一条 toast。
    // 仅对进程类应用有意义;调用方应已筛选。失败的应用如实计入,不打断其余。
    async bulkToggle(targets, on) {
      const verb = on ? "启动" : "停止";
      let ok = 0, fail = 0;
      for (const app of targets) {
        const st = await setAppLifecycle(app.id, on ? "start" : "stop");
        if (!st) { fail++; continue; }
        patchAppLocal(app.id, {
          status: st.active ? "running" : "stopped",
          pid: st.active && st.pid && st.pid !== "0" ? (Number(st.pid) || st.pid) : null,
          uptime: st.active ? "刚刚" : "—", cpu: "—", mem: "—",
        });
        addAudit(on ? "启动服务" : "停止服务", app.name, "成功");
        ok++;
      }
      toast(`批量${verb}:成功 ${ok}` + (fail ? ` · 失败 ${fail}(Agent 未响应或操作出错)` : ""), fail ? { tone: "warn", icon: "alert" } : undefined);
    },

    // 创建应用:**先 await 落库,成功后才更新本地状态并跳转**——与 updateApp/deleteApp 同口径,
    // 不再乐观骗「已创建」。落库被拒(persist 内已 toast 具体原因)则不插入本地、不跳转,避免「假建、刷新消失」。
    async addApp(app) {
      const res = await persist("app", app);
      if (res && res.error) return res;
      setApps((s) => [...s, app]);
      addAudit("创建应用", app.name, "成功");
      toast(`应用「${app.name}」创建成功,预检通过`);
      nav("app-detail", { appId: app.id });
      return { ok: true };
    },

    // 改配置:先归一化数值类型(服务端 Port int / BackupKeep float64,字符串会反序列化失败 400),
    // 再 await 落库;**落库成功后才更新本地状态并提示**——失败如实报错、保留旧值,不再乐观骗"已保存"。
    async updateApp(id, patch) {
      const norm = { ...patch };
      if (norm.port !== undefined) norm.port = Number(norm.port) || 0;
      if (norm.backupKeep !== undefined) norm.backupKeep = 10;
      const a0 = apps.find((x) => x.id === id) || {};
      const next = { ...a0, ...norm };
      const res = await saveAppConfig(next);
      if (res && res.error) {
        toast("保存失败:" + res.error, { tone: "error", icon: "alert" });
        return res;
      }
      setApps((s) => s.map((x) => (x.id === id ? next : x)));
      addAudit("修改配置", a0.name || id, "成功");
      toast("配置已保存");
      return { ok: true };
    },

    // 删除应用:走服务端权威删除(下线 + 删元数据 + 审计,后端原子完成)。
    // 失败(如 Agent 不可达)则不动本地状态、据实报错——杜绝"前端假删、刷新复现"。
    async deleteApp(app) {
      try { await appDelete(app.id); }
      catch (e) { toast("删除失败:" + (e.message || e), { tone: "error", icon: "alert" }); return { error: e.message }; }
      setApps((s) => s.filter((x) => x.id !== app.id));
      addAudit("删除应用", app.name, "成功");
      toast(`应用「${app.name}」已删除`, { icon: "trash" });
      return { ok: true };
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

    // 真机部署/还原结束后递增应用备份版本号,驱动 BackupsTab 重新读取 Agent 权威列表。
    refreshBackups(appId) {
      setBackupRevision((s) => ({ ...s, [appId]: (s[appId] || 0) + 1 }));
    },

    // 真实上传:后端已落盘 + 写元数据,这里把返回条目插入前端状态(size 转人类可读)。
    pushCabinetFile(meta, anon) {
      const f = { ...meta, size: fmtBytes(meta.size), downloads: meta.downloads || 0 };
      setCabinet((s) => [f, ...s]);
      if (!anon) addAudit("上传文件", "文件柜 · " + f.name, "成功");
      const expiryText = f.expires === 0
        ? "永久存储"
        : `${Math.max(1, Math.ceil((f.expires - Date.now()) / MC_DAY))} 天后过期`;
      toast(`上传成功 · 提取码 ${f.code}（${expiryText}）`);
    },

    async deleteCabinetFile(f) {
      try { await removeCabinetFile(f.id); }
      catch (e) { toast(e.message || "删除失败", { tone: "error" }); return; }
      setCabinet((s) => s.filter((x) => x.id !== f.id));
      addAudit("删除文件", "文件柜 · " + f.name, "成功");
      toast("文件已删除", { icon: "trash" });
    },

    async toggleCabinetPublic(f, forcePublic) {
      const isPublic = typeof forcePublic === "boolean" ? forcePublic : !f.public;
      try {
        const saved = await setCabinetFilePublic(f.id, isPublic);
        const next = { ...f, ...saved, public: isPublic };
        setCabinet((s) => s.map((x) => (x.id === f.id ? next : x)));
        toast(isPublic ? `「${f.name}」已公开,匿名可访问` : `「${f.name}」已设为私有`);
        return next;
      } catch (e) {
        toast(e.message || "更新分享状态失败", { tone: "error" });
        return null;
      }
    },

  };

  // ---- auth handlers ----
  // cookie 已由 /api/login 在登录成功时种下,这里只更新前端状态。
  // 只接受后端登录返回的 {user, role};不再有任何前端绕过入口(演示向导已移除)。
  const login = (res) => {
    if (!res || !res.user) return; // 防御:无后端返回不进入主壳
    const r = res.role || "viewer";
    const feat = res.features || { serverOperations: false };
    // 先清空上一会话的角色数据,再切换身份；同一批 React 更新不会渲染旧账号数据。
    setApps([]); setReleases([]); setBackups([]); setCabinet([]); setAudit([]);
    setBackupRevision({});
    setSession(res.user);
    setRole(r);
    setFeatures(feat);
    resetSessionRoute(); // 认证边界不继承上一账号的数据资源/应用详情对象
    setView("console");
    toast(`欢迎回来,${res.user}`);
  };
  const logout = async () => {
    await apiLogout();
    setApps([]); setReleases([]); setBackups([]); setCabinet([]); setAudit([]);
    setBackupRevision({});
    resetSessionRoute();
    setSession(null); setRole("viewer"); setFeatures({ serverOperations: false }); setView("login");
  };

  // ---- crumbs ----
  const detailApp = route.page === "app-detail" ? apps.find((a) => a.id === route.appId) : null;
  const crumbs =
    route.page === "overview" ? [{ label: "总览" }] :
    route.page === "apps" ? [{ label: "应用" }] :
    route.page === "app-detail" ? [{ label: "应用", onClick: () => nav("apps") }, { label: detailApp ? detailApp.name : "详情" }] :
    route.page === "data-resources" ? [{ label: "数据资源" }] :
    route.page === "data-workspace" ? [{ label: "数据资源", onClick: () => nav("data-resources") }, { label: route.resource?.name || "工作台" }] :
    route.page === "server-operations" ? [{ label: "服务器运维" }] :
    route.page === "cabinet" ? [{ label: "文件柜" }] :
    route.page === "users" ? [{ label: "用户管理" }] :
    route.page === "agents" ? [{ label: "Agent 管理" }] :
    route.page === "system" ? [{ label: "系统" }] :
    [{ label: "审计日志" }];

  const screenLabel =
    view !== "console" ? (view === "login" ? "登录" : "初始化向导") :
    route.page === "app-detail" ? `应用详情 · ${detailApp ? detailApp.name : ""}` :
    route.page === "data-workspace" ? `数据工作台 · ${route.resource?.name || ""}` :
    route.page === "server-operations" ? "服务器运维" :
    ({ overview: "总览", apps: "应用列表", "data-resources": "数据资源", cabinet: "文件柜", audit: "审计日志", users: "用户管理", agents: "Agent 管理", system: "系统" })[route.page] || route.page;

  // 工作台独立全页（已登录 + hash 匹配）；功能关闭时 fail-closed 并给出明确提示。
  if (view === "console" && workspaceResourceId) {
    if (!(features && features.serverOperations)) {
      return (
        <MCStore.Provider value={store}>
          <div data-screen-label="服务器工作台" style={{ height: "100%", display: "flex", alignItems: "center", justifyContent: "center" }}>
            <div style={{ textAlign: "center", color: "var(--muted-fg)", maxWidth: 360 }}>
              <div style={{ fontWeight: 600, color: "var(--fg)", marginBottom: 8 }}>服务器运维未启用</div>
              <div style={{ fontSize: 13 }}>请管理员在 config.toml 中设置 server_operations.enabled=true 并重启 Console。</div>
            </div>
          </div>
          <ToastHost />
          <ConfirmHost />
        </MCStore.Provider>
      );
    }
    return (
      <MCStore.Provider value={store}>
        <div data-screen-label="服务器工作台" style={{ height: "100%" }}>
          <ServerWorkspacePage
            resourceId={workspaceResourceId}
            user={user}
            theme={t.dark ? "dark" : "light"}
            onTheme={() => setTweak("dark", !t.dark)}
            onLogout={logout}
            zmodemMaxMB={Number(features.zmodemMaxTransferMB) > 0 ? Number(features.zmodemMaxTransferMB) : 512}
          />
        </div>
        <ToastHost />
        <ConfirmHost />
      </MCStore.Provider>
    );
  }

  return (
    <MCStore.Provider value={store}>
      <div data-screen-label={screenLabel} style={{ height: "100%" }}>
        {view === "login" ? <LoginPage onLogin={login} /> : null}
        {view === "console" ? (
          <Shell page={route.page} onNav={(p) => nav(p)} crumbs={crumbs}
            theme={t.dark ? "dark" : "light"} onTheme={() => setTweak("dark", !t.dark)}
            user={user} role={role} onLogout={logout} features={features}>
            {route.page === "overview" ? <OverviewPage /> : null}
            {route.page === "apps" ? <AppsPage /> : null}
            {route.page === "app-detail" ? (
              <AppDetailPage appId={route.appId} tab={route.tab || "overview"}
                onTab={(tab) => nav("app-detail", { appId: route.appId, tab })} />
            ) : null}
            {route.page === "data-resources" ? (
              <DataResourcesPage onOpenWorkspace={(res, workspace) => nav("data-workspace", { resourceId: res.id, resource: res, workspace })} />
            ) : null}
            {route.page === "data-workspace" && route.resource ? (
              <DataWorkspacePage resource={route.resource} initialWorkspace={route.workspace} onBack={() => nav("data-resources")} />
            ) : null}
            {route.page === "data-workspace" && !route.resource ? (
              <DataResourcesPage onOpenWorkspace={(res, workspace) => nav("data-workspace", { resourceId: res.id, resource: res, workspace })} />
            ) : null}
            {route.page === "server-operations" ? <ServerResourcesPage /> : null}
            {route.page === "cabinet" ? <CabinetPage /> : null}
            {route.page === "audit" ? <AuditPage /> : null}
            {route.page === "users" ? <UsersPage /> : null}
            {route.page === "agents" ? <AgentsPage /> : null}
            {route.page === "system" ? <SystemPage /> : null}
          </Shell>
        ) : null}
      </div>
      <ToastHost />
      <ConfirmHost />
    </MCStore.Provider>
  );
}

export default App;
