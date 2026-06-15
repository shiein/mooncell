// Mooncell — mock 数据与领域模型
import React from 'react';

const MCStore = React.createContext(null);
const useMC = () => React.useContext(MCStore);

const NOW = Date.now();
const MIN = 60e3, HOUR = 3600e3, DAY = 24 * HOUR;
const ago = (ms) => NOW - ms;

// Runner 与 Agent 实际支持对齐:进程类仅 systemd / pm2(不暴露未实现的 nohup);
// static 软链托管、tomcat 容器托管。
const DEPLOY_TYPES = {
  "java-jar":     { label: "Java JAR",     tone: "warn",    runners: ["systemd", "pm2"], artifactExt: ".jar" },
  "tomcat-war":   { label: "Tomcat WAR",   tone: "error",   runners: ["tomcat"], artifactExt: ".war" },
  "native-binary":    { label: "原生二进制",    tone: "cyan",    runners: ["systemd", "pm2"], artifactExt: "" },
  "python":       { label: "Python",       tone: "info",    runners: ["systemd", "pm2"], artifactExt: ".py / .tar.gz" },
  "node":         { label: "Node.js",      tone: "success", runners: ["pm2", "systemd"], artifactExt: ".js / .tar.gz" },
  "static-nginx": { label: "Static / Nginx", tone: "purple", runners: ["软链"], artifactExt: ".tar.gz / .zip" },
};

// 进程类应用:走 systemd / pm2 进程流水线(备份→替换→起停→健康→回滚),支持 Agent 真机部署/还原/日志。
// static-nginx 走软链切换、tomcat-war 走容器,不在内。
const PROCESS_TYPES = ["native-binary", "java-jar", "python", "node"];
const isProcessType = (t) => PROCESS_TYPES.includes(t);

// 所有有真机 Deployer 的类型:进程类 + static-nginx(软链)+ tomcat-war(容器)。
// 决定真机部署/还原/备份是否走 Agent(日志另判,见 isProcessType)。
const REAL_TYPES = [...PROCESS_TYPES, "static-nginx", "tomcat-war"];
const isRealType = (t) => REAL_TYPES.includes(t);

// fmtBytes 把字节数格式化为人类可读;非数字原样返回(兼容旧的字符串 size)。
function fmtBytes(n) {
  if (typeof n !== "number") return n || "—";
  if (n < 1024) return n + " B";
  if (n < 1048576) return (n / 1024).toFixed(1) + " KB";
  if (n < 1073741824) return (n / 1048576).toFixed(1) + " MB";
  return (n / 1073741824).toFixed(2) + " GB";
}

const APP_STATUS = {
  running:   { label: "运行中", tone: "success" },
  stopped:   { label: "已停止", tone: "default" },
  deploying: { label: "部署中", tone: "info" },
  failed:    { label: "异常",   tone: "error" },
  static:    { label: "已发布", tone: "purple" },
};

const REL_STATUS = {
  success:    { label: "成功", tone: "success" },
  failed:     { label: "失败", tone: "error" },
  rolledback: { label: "失败·已回滚", tone: "warn" },
  running:    { label: "进行中", tone: "info" },
};

const INITIAL_APPS = [
  {
    id: "dq-backend", name: "数据查询平台后端", type: "java-jar", runner: "systemd",
    status: "running", version: "v2.4.0", port: 8080, pid: 21433,
    path: "/srv/apps/data-query/app.jar", workdir: "/srv/apps/data-query",
    health: "http://127.0.0.1:8080/actuator/health", healthType: "HTTP 200",
    logPaths: ["/srv/apps/data-query/logs/app.log", "/srv/apps/data-query/logs/error.log"],
    jvm: "-Xms512m -Xmx2g -Dspring.profiles.active=prod", user: "appuser",
    backupKeep: 5, lastDeploy: ago(2 * DAY), uptime: "2d 4h", mem: "1.2 GB", cpu: "3.4%",
    artifactName: "data-query-backend", extraFiles: ["application-prod.yml"],
  },
  {
    id: "report-svc", name: "报表导出服务", type: "tomcat-war", runner: "tomcat",
    status: "running", version: "v1.9.3", port: 8081, pid: 18250,
    path: "/opt/tomcat/webapps/report.war", workdir: "/opt/tomcat",
    health: "端口探活 :8081", healthType: "端口探活",
    logPaths: ["/opt/tomcat/logs/catalina.out", "/srv/logs/report/report.log"],
    jvm: "JAVA_OPTS=-Xmx1g", user: "tomcat",
    backupKeep: 5, lastDeploy: ago(9 * DAY), uptime: "9d 1h", mem: "860 MB", cpu: "1.1%",
    artifactName: "report-svc", extraFiles: [],
  },
  {
    id: "gateway", name: "内网网关服务", type: "native-binary", runner: "systemd",
    status: "running", version: "v3.1.0", pid: 1204, port: 80,
    path: "/srv/apps/gateway/gateway", workdir: "/srv/apps/gateway",
    health: "http://127.0.0.1:80/healthz", healthType: "HTTP 200",
    logPaths: ["/srv/apps/gateway/logs/gateway.log"],
    jvm: "", user: "root",
    backupKeep: 8, lastDeploy: ago(21 * DAY), uptime: "21d 6h", mem: "96 MB", cpu: "0.6%",
    artifactName: "gateway-linux-arm64", extraFiles: ["config.toml"],
  },
  {
    id: "algo-svc", name: "算法分析服务", type: "python", runner: "systemd",
    status: "failed", version: "v0.8.2", pid: null, port: 8090,
    path: "/srv/apps/algo/main.py", workdir: "/srv/apps/algo",
    health: "http://127.0.0.1:8090/ping", healthType: "HTTP 200",
    logPaths: ["/srv/apps/algo/logs/algo.log"],
    jvm: "/srv/apps/algo/venv/bin/python", user: "appuser",
    backupKeep: 5, lastDeploy: ago(5 * HOUR), uptime: "—", mem: "—", cpu: "—",
    artifactName: "algo-svc", extraFiles: ["requirements.txt"],
  },
  {
    id: "msg-push", name: "消息推送服务", type: "node", runner: "pm2",
    status: "running", version: "v1.3.1", pid: 19877, port: 8443,
    path: "/srv/apps/msg-push/server.js", workdir: "/srv/apps/msg-push",
    health: "进程存活 (pm2)", healthType: "进程存活",
    logPaths: ["/srv/apps/msg-push/logs/out.log", "/srv/apps/msg-push/logs/error.log"],
    jvm: "pm2 进程名: msg-push", user: "appuser",
    backupKeep: 5, lastDeploy: ago(3 * DAY), uptime: "3d 2h", mem: "210 MB", cpu: "0.9%",
    artifactName: "msg-push", extraFiles: ["ecosystem.config.js"],
  },
  {
    id: "dq-frontend", name: "数据查询平台前端", type: "static-nginx", runner: "无进程",
    status: "static", version: "v2.4.0", pid: null, port: 443,
    path: "/data/web/dq-frontend → releases/20260610_1421/", workdir: "/data/web",
    health: "—", healthType: "无",
    logPaths: ["/var/log/nginx/dq-access.log", "/var/log/nginx/dq-error.log"],
    jvm: "部署后 nginx -s reload: 否(软链切换)", user: "nginx",
    backupKeep: 5, lastDeploy: ago(2 * DAY), uptime: "—", mem: "—", cpu: "—",
    artifactName: "dq-frontend-dist", extraFiles: [],
  },
  {
    id: "job-scheduler", name: "定时任务调度器", type: "java-jar", runner: "systemd",
    status: "stopped", version: "v1.1.0", pid: null, port: 8085,
    path: "/srv/apps/scheduler/scheduler.jar", workdir: "/srv/apps/scheduler",
    health: "端口探活 :8085", healthType: "端口探活",
    logPaths: ["/srv/apps/scheduler/logs/scheduler.log"],
    jvm: "-Xmx512m", user: "appuser",
    backupKeep: 3, lastDeploy: ago(15 * DAY), uptime: "—", mem: "—", cpu: "—",
    artifactName: "scheduler", extraFiles: [],
  },
  {
    id: "legacy-portal", name: "旧版门户(归档)", type: "static-nginx", runner: "无进程",
    status: "static", version: "v0.9.x", pid: null, port: 8088,
    path: "/data/web/legacy-portal", workdir: "/data/web",
    health: "—", healthType: "无",
    logPaths: ["/var/log/nginx/legacy-access.log"],
    jvm: "", user: "nginx",
    backupKeep: 2, lastDeploy: ago(94 * DAY), uptime: "—", mem: "—", cpu: "—",
    artifactName: "legacy-portal", extraFiles: [],
  },
];

const mkRel = (appId, ver, status, t, operator, dur, size) => ({
  id: appId + "-" + ver, appId, version: ver, status, time: t, operator,
  duration: dur, size, artifact: null,
});

const INITIAL_RELEASES = [
  mkRel("dq-backend", "v2.4.0", "success", ago(2 * DAY), "zhang.wei", "46s", "38.2 MB"),
  mkRel("dq-backend", "v2.3.9", "success", ago(6 * DAY), "zhang.wei", "44s", "38.1 MB"),
  mkRel("dq-backend", "v2.3.8", "rolledback", ago(8 * DAY), "li.na", "2m 18s", "38.1 MB"),
  mkRel("dq-backend", "v2.3.7", "success", ago(13 * DAY), "zhang.wei", "41s", "37.9 MB"),
  mkRel("report-svc", "v1.9.3", "success", ago(9 * DAY), "li.na", "1m 32s", "52.6 MB"),
  mkRel("report-svc", "v1.9.2", "success", ago(22 * DAY), "li.na", "1m 41s", "52.3 MB"),
  mkRel("gateway", "v3.1.0", "success", ago(21 * DAY), "admin", "18s", "14.8 MB"),
  mkRel("algo-svc", "v0.8.2", "failed", ago(5 * HOUR), "wang.lei", "3m 02s", "126 MB"),
  mkRel("algo-svc", "v0.8.1", "success", ago(4 * DAY), "wang.lei", "1m 12s", "124 MB"),
  mkRel("msg-push", "v1.3.1", "success", ago(3 * DAY), "zhang.wei", "29s", "8.4 MB"),
  mkRel("msg-push", "v1.3.0", "success", ago(11 * DAY), "zhang.wei", "31s", "8.4 MB"),
  mkRel("dq-frontend", "v2.4.0", "success", ago(2 * DAY), "zhang.wei", "12s", "6.1 MB"),
  mkRel("dq-frontend", "v2.3.9", "success", ago(6 * DAY), "zhang.wei", "11s", "6.0 MB"),
  mkRel("job-scheduler", "v1.1.0", "success", ago(15 * DAY), "admin", "38s", "21.5 MB"),
];

const mkBak = (appId, ver, t, size, auto, operator, note) => ({
  id: appId + "-b-" + t, appId, version: ver, time: t, size, auto, operator,
  note: note || "", dir: tsDir(t),
});
function tsDir(t) {
  const d = new Date(t);
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}_${p(d.getHours())}${p(d.getMinutes())}`;
}

const INITIAL_BACKUPS = [
  mkBak("dq-backend", "v2.3.9", ago(2 * DAY), "38.1 MB", true, "zhang.wei"),
  mkBak("dq-backend", "v2.3.8", ago(6 * DAY), "38.1 MB", true, "zhang.wei"),
  mkBak("dq-backend", "v2.3.7", ago(8 * DAY), "37.9 MB", true, "li.na"),
  mkBak("dq-backend", "v2.3.7", ago(10 * DAY), "37.9 MB", false, "admin", "升级 JDK 前手动备份"),
  mkBak("report-svc", "v1.9.2", ago(9 * DAY), "52.3 MB", true, "li.na"),
  mkBak("gateway", "v3.0.4", ago(21 * DAY), "14.6 MB", true, "admin"),
  mkBak("algo-svc", "v0.8.1", ago(5 * HOUR), "124 MB", true, "wang.lei"),
  mkBak("algo-svc", "v0.8.0", ago(4 * DAY), "122 MB", true, "wang.lei"),
  mkBak("msg-push", "v1.3.0", ago(3 * DAY), "8.4 MB", true, "zhang.wei"),
  mkBak("dq-frontend", "v2.3.9", ago(2 * DAY), "6.0 MB", true, "zhang.wei"),
  mkBak("dq-frontend", "v2.3.8", ago(6 * DAY), "5.9 MB", true, "zhang.wei"),
  mkBak("job-scheduler", "v1.0.9", ago(15 * DAY), "21.4 MB", true, "admin"),
];

const INITIAL_CABINET = [
  { id: "cf1", name: "patch-dq-20260612.sql", size: "18 KB", uploader: "192.168.10.34(匿名)", time: ago(2 * HOUR), expires: ago(-5 * DAY), code: "K7X2", public: false, downloads: 1 },
  { id: "cf2", name: "report-svc-v1.9.4-rc1.war", size: "52.8 MB", uploader: "li.na", time: ago(6 * HOUR), expires: ago(-7 * DAY), code: "M3QP", public: true, downloads: 3 },
  { id: "cf3", name: "nginx-conf-备份.tar.gz", size: "4.2 KB", uploader: "admin", time: ago(1 * DAY), expires: ago(-6 * DAY), code: "A9F1", public: false, downloads: 0 },
  { id: "cf4", name: "现场排查截图.zip", size: "8.7 MB", uploader: "192.168.10.61(匿名)", time: ago(1 * DAY - 3 * HOUR), expires: ago(-6 * DAY), code: "T5WD", public: false, downloads: 2 },
  { id: "cf5", name: "jdk-11.0.21-linux-aarch64.tar.gz", size: "182 MB", uploader: "admin", time: ago(3 * DAY), expires: ago(-30 * DAY), code: "B2NH", public: true, downloads: 6 },
  { id: "cf6", name: "数据库初始化脚本-v3.sql", size: "236 KB", uploader: "wang.lei", time: ago(5 * DAY), expires: ago(-2 * DAY), code: "R8KC", public: false, downloads: 4 },
];

const INITIAL_AUDIT = [
  { id: "a1", time: ago(5 * HOUR), user: "wang.lei", action: "部署", target: "算法分析服务 v0.8.2", result: "失败", ip: "192.168.10.45" },
  { id: "a2", time: ago(5 * HOUR + 4 * MIN), user: "wang.lei", action: "回滚", target: "算法分析服务 → v0.8.1 备份", result: "成功", ip: "192.168.10.45" },
  { id: "a3", time: ago(8 * HOUR), user: "admin", action: "删除文件", target: "文件柜 · old-dump.sql", result: "成功", ip: "192.168.10.2" },
  { id: "a4", time: ago(1 * DAY), user: "li.na", action: "修改配置", target: "报表导出服务 · JAVA_OPTS", result: "成功", ip: "192.168.10.88" },
  { id: "a5", time: ago(2 * DAY), user: "zhang.wei", action: "部署", target: "数据查询平台后端 v2.4.0", result: "成功", ip: "192.168.10.34" },
  { id: "a6", time: ago(2 * DAY + 10 * MIN), user: "zhang.wei", action: "部署", target: "数据查询平台前端 v2.4.0", result: "成功", ip: "192.168.10.34" },
  { id: "a7", time: ago(3 * DAY), user: "zhang.wei", action: "部署", target: "消息推送服务 v1.3.1", result: "成功", ip: "192.168.10.34" },
  { id: "a8", time: ago(3 * DAY + 2 * HOUR), user: "admin", action: "手动备份", target: "数据查询平台后端", result: "成功", ip: "192.168.10.2" },
  { id: "a9", time: ago(4 * DAY), user: "admin", action: "删除备份", target: "网关服务 · 20260518_0930", result: "成功", ip: "192.168.10.2" },
  { id: "a10", time: ago(5 * DAY), user: "li.na", action: "停止服务", target: "定时任务调度器", result: "成功", ip: "192.168.10.88" },
  { id: "a11", time: ago(6 * DAY), user: "zhang.wei", action: "部署", target: "数据查询平台后端 v2.3.9", result: "成功", ip: "192.168.10.34" },
  { id: "a12", time: ago(8 * DAY), user: "li.na", action: "部署", target: "数据查询平台后端 v2.3.8", result: "失败·已自动回滚", ip: "192.168.10.88" },
];

const AGENT = {
  name: "本机 Agent", host: "192.168.10.21:9100", version: "v1.4.2",
  os: "UOS Server 20 · arm64", uptime: "37d 14h", token: "mc_ag_7f3e····9d21",
  caps: [
    { key: "systemd", label: "systemd", ok: true, ver: "249" },
    { key: "java", label: "Java", ok: true, ver: "11.0.21" },
    { key: "pm2", label: "pm2", ok: true, ver: "5.3.0" },
    { key: "nginx", label: "nginx", ok: true, ver: "1.24.0" },
    { key: "python", label: "Python", ok: true, ver: "3.9.6" },
    { key: "node", label: "Node", ok: true, ver: "18.19.0" },
    { key: "tomcat", label: "Tomcat", ok: false, ver: "未检测到" },
  ],
  cpu: 23, mem: 58, disk: 64,
  diskDetail: { used: 318, total: 500, backups: 42, cabinet: 18, logs: 36 },
};

function genSeries(base, jitter, n = 40) {
  const arr = []; let v = base;
  for (let i = 0; i < n; i++) {
    v = Math.max(2, Math.min(96, v + (Math.random() - 0.5) * jitter * 2));
    arr.push(Math.round(v));
  }
  return arr;
}

// ---------- 运行日志生成 ----------
const LOG_GEN = {
  "java-jar": (app) => {
    const r = Math.random();
    const th = ["http-nio-" + app.port + "-exec-" + (1 + Math.floor(Math.random() * 8)), "scheduling-1", "main"][Math.random() < 0.85 ? 0 : Math.floor(Math.random() * 2) + 1];
    if (r < 0.06) return { level: "WARN", text: `[${th}] c.m.dq.service.QueryService : 慢查询 ${(800 + Math.random() * 2200 | 0)}ms · sql_id=q_${(Math.random() * 999 | 0)}` };
    if (r < 0.09) return { level: "ERROR", text: `[${th}] c.m.dq.ds.DataSourcePool : connection timeout after 5000ms, retrying (1/3)` };
    const acts = [
      `[${th}] c.m.dq.controller.QueryController : query executed in ${(12 + Math.random() * 180 | 0)}ms, rows=${(Math.random() * 500 | 0)}`,
      `[${th}] c.m.dq.cache.LocalCache : cache hit ratio 0.${(80 + Math.random() * 19 | 0)}, entries=${(1000 + Math.random() * 4000 | 0)}`,
      `[${th}] o.s.web.servlet.DispatcherServlet : GET /api/v2/datasets · 200 · ${(8 + Math.random() * 60 | 0)}ms`,
      `[scheduling-1] c.m.dq.task.MetaSyncTask : 元数据同步完成, tables=${(40 + Math.random() * 20 | 0)}, cost=${(200 + Math.random() * 900 | 0)}ms`,
    ];
    return { level: "INFO", text: acts[Math.random() * acts.length | 0] };
  },
  "tomcat-war": (app) => LOG_GEN["java-jar"](app),
  "native-binary": (app) => {
    const r = Math.random();
    if (r < 0.05) return { level: "WARN", text: `proxy: upstream 127.0.0.1:8090 unhealthy, removed from pool` };
    const acts = [
      `proxy: ${["GET", "POST"][Math.random() * 2 | 0]} ${["/api/query", "/api/report/export", "/static/js/app.js", "/api/push/subscribe"][Math.random() * 4 | 0]} → ${[200, 200, 200, 304][Math.random() * 4 | 0]} ${(1 + Math.random() * 40 | 0)}ms`,
      `ratelimit: bucket=default tokens=${(800 + Math.random() * 200 | 0)}/1000`,
    ];
    return { level: "INFO", text: acts[Math.random() * acts.length | 0] };
  },
  "python": () => {
    const r = Math.random();
    if (r < 0.5) return { level: "ERROR", text: `Traceback (most recent call last):\n  File "/srv/apps/algo/main.py", line 84, in handle\n    result = model.predict(batch)\nModuleNotFoundError: No module named 'sklearn'` };
    return { level: "INFO", text: `uvicorn.access: 127.0.0.1 - "POST /analyze HTTP/1.1" 500` };
  },
  "node": () => {
    const r = Math.random();
    if (r < 0.05) return { level: "WARN", text: `ws: client 192.168.10.${(20 + Math.random() * 200 | 0)} heartbeat timeout, closing` };
    const acts = [
      `push: delivered batch=${(Math.random() * 40 | 0)} channel=ops-alerts latency=${(2 + Math.random() * 30 | 0)}ms`,
      `ws: connected clients=${(110 + Math.random() * 40 | 0)}`,
      `queue: depth=${(Math.random() * 12 | 0)} consumed=${(Math.random() * 99 | 0)}/s`,
    ];
    return { level: "INFO", text: acts[Math.random() * acts.length | 0] };
  },
  "static-nginx": () => {
    const ips = `192.168.10.${(20 + Math.random() * 220 | 0)}`;
    const paths = ["/", "/assets/index-Bf2k.js", "/assets/vendor-D81q.js", "/api/v2/query", "/favicon.ico", "/assets/logo.png"];
    const code = [200, 200, 200, 200, 304, 404][Math.random() * 6 | 0];
    return { level: code === 404 ? "WARN" : "INFO", text: `${ips} - "GET ${paths[Math.random() * paths.length | 0]} HTTP/1.1" ${code} ${(200 + Math.random() * 80000 | 0)} "-" "Mozilla/5.0"` };
  },
};

function genLogLine(app) {
  const gen = LOG_GEN[app.type] || LOG_GEN["node"];
  return gen(app);
}

// ---------- 工具 ----------
function fmtTime(t) {
  const d = new Date(t);
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}
function fmtClock(t) {
  const d = new Date(t);
  const p = (n) => String(n).padStart(2, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}
function timeAgo(t) {
  const diff = NOW - t;
  if (diff < 0) {
    const d = -diff;
    if (d < DAY) return `${Math.ceil(d / HOUR)} 小时后`;
    return `${Math.ceil(d / DAY)} 天后`;
  }
  if (diff < MIN) return "刚刚";
  if (diff < HOUR) return `${Math.floor(diff / MIN)} 分钟前`;
  if (diff < DAY) return `${Math.floor(diff / HOUR)} 小时前`;
  return `${Math.floor(diff / DAY)} 天前`;
}
function randSha() {
  const c = "0123456789abcdef"; let s = "";
  for (let i = 0; i < 12; i++) s += c[Math.random() * 16 | 0];
  return s;
}
function nextVersion(v) {
  const m = (v || "v1.0.0").match(/v(\d+)\.(\d+)\.(\d+)/);
  if (!m) return "v1.0.0";
  return `v${m[1]}.${m[2]}.${+m[3] + 1}`;
}

export {
  MCStore, useMC, DEPLOY_TYPES, isProcessType, isRealType, fmtBytes, APP_STATUS, REL_STATUS,
  INITIAL_APPS, INITIAL_RELEASES, INITIAL_BACKUPS, INITIAL_CABINET, INITIAL_AUDIT,
  AGENT, genSeries, genLogLine, fmtTime, fmtClock, timeAgo, randSha, nextVersion, tsDir,
  NOW as MC_NOW, MIN as MC_MIN, HOUR as MC_HOUR, DAY as MC_DAY,
};
