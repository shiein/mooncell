// Mooncell — 应用详情:概览 / 部署记录 / 备份还原 / 实时日志 / 配置
import React from 'react';
import { useMC, DEPLOY_TYPES, isProcessType, isRealType, REL_STATUS, fmtTime, timeAgo, genLogLine, tsDir } from '../lib/data.js';
import { Icon, Btn, Badge, StatusBadge, TypeBadge, Field, Select, Switch, Tabs, EmptyState, Spinner, toast } from '../components/primitives.jsx';
import { Console, DeployDialog, RestoreDialog } from '../components/pipeline.jsx';
import { listAgentBackups, streamAppLogs, getAppStatus } from '../lib/api.js';

function InfoRow({ label, children, mono }) {
  return (
    <React.Fragment>
      <span style={{ color: "var(--muted-fg)", fontSize: 12.5 }}>{label}</span>
      <span className={mono ? "mono" : ""} style={{ fontSize: mono ? 12 : 13, wordBreak: "break-all" }}>{children}</span>
    </React.Fragment>
  );
}

function OverviewTab({ app, releases }) {
  const recent = releases.filter((r) => r.appId === app.id).slice(0, 3);
  const grid = { display: "grid", gridTemplateColumns: "auto 1fr", gap: "9px 18px", alignItems: "baseline" };
  // 真实进程类应用:向 Agent 拉取实时托管状态(systemd/pm2),每 10s 刷新一次。
  // 非真实/静态类沿用 mock 字段。live=null 表示尚未拉到或 Agent 离线。
  const real = isRealType(app.type) && isProcessType(app.type);
  const [live, setLive] = React.useState(null);
  React.useEffect(() => {
    if (!real) { setLive(null); return; }
    let alive = true;
    const tick = () => getAppStatus(app.id).then((s) => { if (alive) setLive(s); });
    tick();
    const iv = setInterval(tick, 10000);
    return () => { alive = false; clearInterval(iv); };
  }, [app.id, real]);
  // 真实应用的进程行优先用 live;未拉到时显式标注,不再回退到 mock 的 pid/uptime。
  const procRow = real
    ? (live ? (live.active ? `pid ${live.pid || "?"} · ${live.state}` : `未运行 · ${live.state || "inactive"}`) : "查询中…")
    : (app.pid ? `pid ${app.pid} · 运行 ${app.uptime}` : "未运行");
  return (
    <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 14 }}>
      <div className="card card-pad">
        <h4 style={{ fontSize: 13.5, marginBottom: 14, display: "flex", alignItems: "center", gap: 7 }}><Icon name="zap" size={14} style={{ color: "var(--primary)" }} />运行状态{real ? <Badge tone="info" style={{ marginLeft: 4 }}>实时</Badge> : null}</h4>
        <div style={grid}>
          <InfoRow label="状态">{real && live ? <StatusBadge status={live.active ? "running" : "stopped"} /> : <StatusBadge status={app.status} />}</InfoRow>
          <InfoRow label="进程" mono>{procRow}</InfoRow>
          <InfoRow label="资源" mono>{real ? "—(Agent 未采集资源指标)" : (app.pid ? `CPU ${app.cpu} · 内存 ${app.mem}` : "—")}</InfoRow>
          <InfoRow label="Runner"><span className="code-chip">{app.runner}</span></InfoRow>
        </div>
        {app.status === "failed" ? (
          <div style={{ marginTop: 14, display: "flex", gap: 8, padding: "9px 12px", borderRadius: 8, background: "var(--error-soft)", color: "var(--error)", fontSize: 12.5 }}>
            <Icon name="alert" size={14} style={{ flex: "none", marginTop: 1 }} />
            <span>最近一次部署健康检查失败,已自动回滚;当前进程异常退出。建议查看实时日志定位后重新部署。</span>
          </div>
        ) : null}
      </div>

      <div className="card card-pad">
        <h4 style={{ fontSize: 13.5, marginBottom: 14, display: "flex", alignItems: "center", gap: 7 }}><Icon name="layers" size={14} style={{ color: "var(--primary)" }} />部署信息</h4>
        <div style={grid}>
          <InfoRow label="当前版本" mono>{app.version}</InfoRow>
          <InfoRow label="最近部署">{app.lastDeploy ? `${fmtTime(app.lastDeploy)}(${timeAgo(app.lastDeploy)})` : "从未部署"}</InfoRow>
          <InfoRow label="制品路径" mono>{app.path}</InfoRow>
          <InfoRow label="备份策略">滚动保留 {app.backupKeep} 份 · 部署前自动备份</InfoRow>
        </div>
      </div>

      <div className="card card-pad">
        <h4 style={{ fontSize: 13.5, marginBottom: 14, display: "flex", alignItems: "center", gap: 7 }}><Icon name="shield" size={14} style={{ color: "var(--primary)" }} />健康检查</h4>
        <div style={grid}>
          <InfoRow label="方式">{app.healthType}</InfoRow>
          <InfoRow label="目标" mono>{app.health}</InfoRow>
          <InfoRow label="策略">超时 3s · 重试 5 次 · 间隔 2s</InfoRow>
          <InfoRow label="最近探活">{app.status === "running" ? <Badge tone="success" dot>30s 前 · 200 OK</Badge> : app.status === "failed" ? <Badge tone="error" dot>5 小时前 · 连接被拒绝</Badge> : <Badge>未启用</Badge>}</InfoRow>
        </div>
      </div>

      <div className="card card-pad">
        <h4 style={{ fontSize: 13.5, marginBottom: 14, display: "flex", alignItems: "center", gap: 7 }}><Icon name="terminal" size={14} style={{ color: "var(--primary)" }} />日志路径</h4>
        <div style={{ display: "flex", flexDirection: "column", gap: 7 }}>
          {app.logPaths.map((p) => (
            <div key={p} className="mono" style={{ fontSize: 12, background: "var(--muted)", borderRadius: 6, padding: "6px 10px", color: "var(--fg-secondary)" }}>{p}</div>
          ))}
        </div>
        <div style={{ fontSize: 11.5, color: "var(--muted-fg)", marginTop: 10 }}>Agent 仅允许读取已声明路径,路径规范化后须落在白名单目录内(防穿越)。</div>
      </div>

      {recent.length > 0 ? (
        <div className="card" style={{ gridColumn: "1 / -1", overflow: "hidden" }}>
          <div style={{ padding: "13px 20px 0", fontSize: 13.5, fontWeight: 600 }}>最近部署</div>
          <table className="table">
            <tbody>
              {recent.map((r) => (
                <tr key={r.id}>
                  <td style={{ width: 110 }}><span className="mono" style={{ fontSize: 12, fontWeight: 600 }}>{r.version}</span></td>
                  <td style={{ width: 130 }}><StatusBadge status={r.status} map={REL_STATUS} pulse={false} /></td>
                  <td><span style={{ fontSize: 12.5, color: "var(--fg-secondary)" }}>{r.operator}</span></td>
                  <td><span className="mono" style={{ fontSize: 12, color: "var(--muted-fg)" }}>{r.size}</span></td>
                  <td><span className="mono" style={{ fontSize: 12, color: "var(--muted-fg)" }}>耗时 {r.duration}</span></td>
                  <td style={{ textAlign: "right" }}><span style={{ fontSize: 12.5, color: "var(--muted-fg)" }}>{fmtTime(r.time)}</span></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
    </div>
  );
}

function ReleasesTab({ app }) {
  const { releases } = useMC();
  const list = releases.filter((r) => r.appId === app.id);
  return (
    <div className="card" style={{ overflow: "hidden" }}>
      <table className="table">
        <thead><tr><th>Release</th><th>状态</th><th>制品大小</th><th>操作人</th><th>耗时</th><th>时间</th></tr></thead>
        <tbody>
          {list.map((r) => (
            <tr key={r.id}>
              <td><span className="mono" style={{ fontSize: 12.5, fontWeight: 600 }}>{r.version}</span></td>
              <td><StatusBadge status={r.status} map={REL_STATUS} pulse={false} /></td>
              <td><span className="mono" style={{ fontSize: 12 }}>{r.size}</span></td>
              <td style={{ fontSize: 12.5 }}>{r.operator}</td>
              <td><span className="mono" style={{ fontSize: 12, color: "var(--muted-fg)" }}>{r.duration}</span></td>
              <td><span style={{ fontSize: 12.5, color: "var(--muted-fg)" }}>{fmtTime(r.time)}({timeAgo(r.time)})</span></td>
            </tr>
          ))}
        </tbody>
      </table>
      {list.length === 0 ? <EmptyState icon="layers" title="还没有部署记录" desc="点击右上角「部署」上传第一个制品" /> : null}
    </div>
  );
}

// 把 Agent 真实备份({dir,version,sha256,time,size:字节})归一到列表展示形状,并标记 real。
function normAgentBackup(b, appId) {
  const mb = b.size ? (b.size / 1048576).toFixed(1) + " MB" : "—";
  return {
    id: "ab-" + b.dir, appId, dir: b.dir, version: b.version || "—", size: mb,
    time: b.time || 0, auto: true, operator: "agent", note: "Agent 真实备份", real: true,
  };
}

function BackupsTab({ app, onRestore }) {
  const store = useMC();
  // go-binary:优先拉取 Agent 真实备份(还原目标须真实存在);拉到(含空数组)即用真实列表,否则回退 mock。
  const [realBaks, setRealBaks] = React.useState(null);
  React.useEffect(() => {
    let alive = true;
    if (isRealType(app.type)) {
      listAgentBackups(app.id, app.agentId).then((arr) => {
        if (alive && arr) setRealBaks(arr.map((b) => normAgentBackup(b, app.id)));
      });
    } else setRealBaks(null);
    return () => { alive = false; };
  }, [app.id, app.type]);
  const list = realBaks !== null ? realBaks : store.backups.filter((b) => b.appId === app.id);
  const [backing, setBacking] = React.useState(false);
  const manualBackup = () => {
    setBacking(true);
    setTimeout(() => { setBacking(false); store.addManualBackup(app); }, 1600);
  };
  return (
    <div>
      <div style={{ display: "flex", alignItems: "center", gap: 12, marginBottom: 12 }}>
        <div style={{ fontSize: 12.5, color: "var(--muted-fg)", flex: 1 }}>
          每次部署/还原前自动备份至 <span className="code-chip">backups/{app.id}/</span> · 滚动保留 {app.backupKeep} 份{realBaks !== null ? " · 来自 Agent 真实备份" : " · 当前占用 " + (list.length > 0 ? "约 " + list.length * 30 + " MB" : "0")}
        </div>
        {store.can("write") ? (
          <Btn size="sm" icon={backing ? undefined : "archive"} disabled={backing || realBaks !== null} title={realBaks !== null ? "真实备份在部署/还原时自动生成" : undefined} onClick={manualBackup}>
            {backing ? <React.Fragment><Spinner size={12} /> 备份中…</React.Fragment> : "手动备份"}
          </Btn>
        ) : null}
      </div>
      <div className="card" style={{ overflow: "hidden" }}>
        <table className="table">
          <thead><tr><th>备份目录</th><th>版本</th><th>大小</th><th>来源</th><th>时间</th><th>操作人</th><th style={{ width: 150 }}></th></tr></thead>
          <tbody>
            {list.map((b) => (
              <tr key={b.id}>
                <td><span className="mono" style={{ fontSize: 12 }}>{b.dir}/</span>{b.note ? <div style={{ fontSize: 11.5, color: "var(--muted-fg)", marginTop: 2 }}>{b.note}</div> : null}</td>
                <td><span className="mono" style={{ fontSize: 12.5, fontWeight: 600 }}>{b.version}</span></td>
                <td><span className="mono" style={{ fontSize: 12 }}>{b.size}</span></td>
                <td><Badge tone={b.auto ? "default" : "info"}>{b.auto ? "自动" : "手动"}</Badge></td>
                <td><span style={{ fontSize: 12.5, color: "var(--muted-fg)" }}>{fmtTime(b.time)}</span></td>
                <td style={{ fontSize: 12.5 }}>{b.operator}</td>
                <td>
                  <div style={{ display: "flex", gap: 6, justifyContent: "flex-end" }}>
                    {store.can("write") ? <Btn size="sm" variant="primary" icon="rotate" onClick={() => onRestore(b)}>还原</Btn> : <span style={{ fontSize: 12, color: "var(--muted-fg)" }}>只读</span>}
                    {store.can("write") && !b.real ? <Btn size="sm" variant="ghost" icon="trash" title="删除备份" onClick={() => store.deleteBackup(app, b)}></Btn> : null}
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {list.length === 0 ? <EmptyState icon="archive" title="暂无备份" desc="首次部署时会自动创建备份" /> : null}
      </div>
    </div>
  );
}

// isConcreteLogPath:可真实 tail 的日志路径——不含 glob(* ? [)、不以 ~ 开头(Agent/Console 都不展开)。
const isConcreteLogPath = (p) => p && !/[*?[]/.test(p) && !p.startsWith("~");

// ---------- 实时日志 ----------
function LogViewer({ app }) {
  const genActive = app.status === "running" || app.status === "static" || app.status === "failed";
  const [realFailed, setRealFailed] = React.useState(false);

  const [lines, setLines] = React.useState([]);
  const [follow, setFollow] = React.useState(true);
  const [filter, setFilter] = React.useState("");
  const [onlyMatch, setOnlyMatch] = React.useState(false);
  // 日志源:__journal__ 跟随进程 journal/pm2(仅进程类有 unit);具体文件为声明的可 tail 日志路径
  // (排除 glob/tilde——Agent tail 与 Console 精确授权都不展开)。static/tomcat 无 journal,只列文件。
  const JOURNAL = "__journal__";
  const canJournal = isProcessType(app.type);
  const fileOpts = (app.logPaths || []).filter(isConcreteLogPath);
  const journalLabel = app.runner === "pm2" ? "进程日志 · pm2" : "进程日志 · journal";
  const logOptions = [
    ...(canJournal ? [{ value: JOURNAL, label: journalLabel }] : []),
    ...fileOpts.map((p) => ({ value: p, label: p })),
  ];
  const [logSrc, setLogSrc] = React.useState(canJournal ? JOURNAL : (fileOpts[0] || ""));
  const followingFile = logSrc !== JOURNAL && logSrc !== "";
  // 可走真实 Agent 流:进程类的 journal,或任意类型选中的具体日志文件。
  const useReal = !realFailed && ((canJournal && logSrc === JOURNAL) || followingFile);

  const append = (line) => setLines((s) => { const n = [...s, line]; return n.length > 400 ? n.slice(-400) : n; });

  // 真实日志:订阅 Agent SSE。logSrc=journal 跟随进程日志;否则跟随选中的声明日志文件。
  // 暂停/切源/离开时 abort,不可达则标记回退模拟。
  React.useEffect(() => {
    if (!useReal || !follow) return;
    const ac = new AbortController();
    let cancelled = false;
    setLines([]); // 重新拉取 tail,避免暂停后重复
    streamAppLogs(app.id, {
      tail: 200, signal: ac.signal, agentId: app.agentId, runner: app.runner,
      path: followingFile ? logSrc : undefined,
      onLine: (l) => { if (!cancelled) append(l); },
    }).then((res) => { if (res && res.error && !cancelled) setRealFailed(true); });
    return () => { cancelled = true; ac.abort(); };
  }, [useReal, follow, app.id, logSrc]);

  // 模拟日志:非进程类应用或真实流回退时使用。
  React.useEffect(() => {
    if (useReal) return;
    setLines(() => {
      const arr = []; let t = Date.now() - 1000 * 60 * 3;
      const n = app.status === "stopped" ? 14 : 28;
      for (let i = 0; i < n; i++) {
        const l = genLogLine(app);
        arr.push({ ts: t, level: l.level, text: l.text });
        t += 2000 + Math.random() * 8000;
      }
      return arr;
    });
    if (!follow || !genActive) return;
    const iv = setInterval(() => {
      const l = genLogLine(app);
      append({ ts: Date.now(), level: l.level, text: l.text });
    }, app.status === "failed" ? 2400 : 850);
    return () => clearInterval(iv);
  }, [useReal, follow, app.id, app.status]);

  const shown = onlyMatch && filter.trim() ? lines.filter((l) => l.text.includes(filter)) : lines;

  return (
    <div>
      <div style={{ display: "flex", gap: 10, alignItems: "center", marginBottom: 10, flexWrap: "wrap" }}>
        <div style={{ width: 300 }}>
          {logOptions.length > 0
            ? <Select value={logSrc} onChange={setLogSrc} options={logOptions} style={{ fontFamily: "var(--font-mono)", fontSize: 12 }} />
            : <span style={{ fontSize: 12, color: "var(--muted-fg)" }} className="mono">无可跟随的日志源(未声明具体日志文件)</span>}
        </div>
        <div style={{ position: "relative", width: 220 }}>
          <Icon name="search" size={13} style={{ position: "absolute", left: 9, top: 8, color: "var(--muted-fg)" }} />
          <input className="input input-sm" style={{ paddingLeft: 28 }} placeholder="关键字过滤 / 高亮" value={filter} onChange={(e) => setFilter(e.target.value)} />
        </div>
        <label style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 12, color: "var(--fg-secondary)", cursor: "pointer" }}>
          <Switch on={onlyMatch} onChange={setOnlyMatch} />仅匹配行
        </label>
        <div style={{ flex: 1 }}></div>
        <Badge tone={follow ? "success" : "default"} dot={follow}>
          {useReal ? (follow ? (followingFile ? "文件 tail -F 实时" : "Agent journal 实时") : "已暂停")
            : genActive ? (follow ? "tail -F 实时跟随(演示)" : "已暂停") : "进程未运行 · 历史日志"}
        </Badge>
        <Btn size="sm" icon={follow ? "pause" : "play"} onClick={() => setFollow(!follow)}>{follow ? "暂停" : "继续"}</Btn>
        <Btn size="sm" icon="download" title="导出最近 7 天进程日志(gzip)" onClick={() => {
          // 下载导出的是进程 journal/pm2 日志;仅进程类可用(文件 tail 源无对应导出)。
          if (!canJournal) { toast("仅进程类应用支持导出 journal/pm2 日志", { tone: "warn" }); return; }
          const since = Math.floor((Date.now() - 7 * 24 * 3600 * 1000) / 1000);
          const a = document.createElement("a");
          a.href = `/api/agent/apps/${encodeURIComponent(app.id)}/logs/download?since=${since}`;
          document.body.appendChild(a); a.click(); a.remove();
          toast("开始导出日志(gzip)");
        }}>下载</Btn>
      </div>
      <Console lines={shown} filter={filter} height={460} />
      <div style={{ display: "flex", justifyContent: "space-between", marginTop: 8, fontSize: 11.5, color: "var(--muted-fg)" }}>
        <span className="mono">{useReal ? (followingFile ? `tail -F ${logSrc}` : (app.runner === "pm2" ? `pm2 logs deploy-${app.id}` : `journalctl -u deploy-${app.id}`)) : (app.logPaths && app.logPaths[0]) || "—"}</span>
        <span>缓冲 {lines.length} / 400 行 · {useReal ? (followingFile ? "tail -F 跟随(轮转安全)" : "journald 跟随(轮转安全)") : "轮转安全(fsnotify 重开文件)"}</span>
      </div>
    </div>
  );
}

// ---------- 配置 ----------
function ConfigTab({ app }) {
  const store = useMC();
  const [edit, setEdit] = React.useState(false);
  const [draft, setDraft] = React.useState(app);
  React.useEffect(() => setDraft(app), [app.id]);
  const set = (k, v) => setDraft({ ...draft, [k]: v });
  const ipt = (k, mono = true) => (
    <input className={"input" + (mono ? " mono" : "")} style={mono ? { fontSize: 12.5 } : undefined}
      disabled={!edit} value={draft[k] || ""} onChange={(e) => set(k, e.target.value)} />
  );
  const save = () => { store.updateApp(app.id, draft); setEdit(false); };

  const sec = { fontSize: 13, fontWeight: 600, margin: "4px 0 0", color: "var(--fg-secondary)" };
  return (
    <div className="card card-pad" style={{ maxWidth: 760 }}>
      <div style={{ display: "flex", alignItems: "center", marginBottom: 16 }}>
        <div style={{ flex: 1, fontSize: 12.5, color: "var(--muted-fg)" }}>
          配置由 <span className="code-chip">{app.type}</span> 类型的 JSON Schema 约束 · 保存前 Agent 端预检
        </div>
        {edit ? (
          <div style={{ display: "flex", gap: 8 }}>
            <Btn size="sm" variant="ghost" onClick={() => { setDraft(app); setEdit(false); }}>取消</Btn>
            <Btn size="sm" variant="primary" icon="check" onClick={save}>保存配置</Btn>
          </div>
        ) : (store.can("write") ? <Btn size="sm" icon="settings" onClick={() => setEdit(true)}>编辑</Btn> : null)}
      </div>
      <div style={{ display: "flex", flexDirection: "column", gap: 13 }}>
        <div style={sec}>基本</div>
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12 }}>
          <Field label="应用名"><input className="input" disabled={!edit} value={draft.name || ""} onChange={(e) => set("name", e.target.value)} /></Field>
          <Field label="Runner">
            <Select value={draft.runner} onChange={(v) => set("runner", v)} disabled={!edit}
              options={DEPLOY_TYPES[app.type].runners} />
          </Field>
          <Field label="启动用户">{ipt("user", false)}</Field>
          <Field label="端口"><input className="input mono" disabled={!edit} value={draft.port || ""} onChange={(e) => set("port", e.target.value)} /></Field>
        </div>
        <div style={sec}>路径与启动</div>
        <Field label="制品路径">{ipt("path")}</Field>
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12 }}>
          <Field label="工作目录">{ipt("workdir")}</Field>
          <Field label={app.type === "java-jar" || app.type === "tomcat-war" ? "JVM 参数" : "启动参数 / 备注"}>{ipt("jvm")}</Field>
        </div>
        <div style={sec}>健康检查</div>
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12 }}>
          <Field label="方式">
            <Select value={draft.healthType} onChange={(v) => set("healthType", v)} disabled={!edit}
              options={["HTTP 200", "端口探活", "进程存活", "无"]} />
          </Field>
          <Field label="目标">{ipt("health")}</Field>
        </div>
        <div style={sec}>备份与钩子</div>
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12 }}>
          <Field label="备份保留份数"><input className="input mono" disabled={!edit} value={draft.backupKeep} onChange={(e) => set("backupKeep", e.target.value)} /></Field>
          <Field label="额外备份文件" hint="随制品一起备份的配置文件">
            <input className="input mono" style={{ fontSize: 12.5 }} disabled={!edit}
              value={(draft.extraFiles || []).join(", ")} onChange={(e) => set("extraFiles", e.target.value.split(/,\s*/).filter(Boolean))} />
          </Field>
        </div>
        <Field label="部署后钩子(仅白名单内置动作)">
          <Select disabled={!edit} value={app.type === "static-nginx" ? "nginx -s reload" : "无"} onChange={() => {}}
            options={["无", "nginx -s reload", "清理缓存目录", "预热请求"]} />
        </Field>
        <Field label="日志路径(每行一条,支持通配)">
          <textarea className="textarea mono" style={{ fontSize: 12.5 }} rows={2} disabled={!edit}
            value={(draft.logPaths || []).join("\n")} onChange={(e) => set("logPaths", e.target.value.split("\n").filter(Boolean))}></textarea>
        </Field>
      </div>
    </div>
  );
}

// ---------- 详情页 ----------
function AppDetailPage({ appId, tab, onTab }) {
  const store = useMC();
  const app = store.apps.find((a) => a.id === appId);
  const [deploying, setDeploying] = React.useState(false);
  const [restoreBackup, setRestoreBackup] = React.useState(null);
  if (!app) return <EmptyState icon="box" title="应用不存在" action={<Btn onClick={() => store.nav("apps")}>返回列表</Btn>} />;

  const relCount = store.releases.filter((r) => r.appId === app.id).length;
  const bakCount = store.backups.filter((b) => b.appId === app.id).length;
  const canRun = app.type !== "static-nginx" && store.can("write");
  const canWrite = store.can("write");

  return (
    <div>
      <div style={{ display: "flex", alignItems: "center", gap: 12, marginBottom: 6 }}>
        <h2 style={{ fontSize: 20, letterSpacing: "-0.015em" }}>{app.name}</h2>
        <TypeBadge type={app.type} />
        <StatusBadge status={app.status} />
        <div style={{ flex: 1 }}></div>
        {canRun && (app.status === "running") ? <Btn icon="stop" onClick={() => store.toggleApp(app, false)}>停止</Btn> : null}
        {canRun && (app.status === "stopped" || app.status === "failed") ? <Btn icon="play" onClick={() => store.toggleApp(app, true)}>启动</Btn> : null}
        {canWrite ? <Btn variant="primary" icon="upload" onClick={() => setDeploying(true)}>部署新版本</Btn> : null}
      </div>
      <div className="mono" style={{ fontSize: 12, color: "var(--muted-fg)", marginBottom: 16 }}>
        {app.id} · {app.path}
      </div>

      <Tabs style={{ marginBottom: 18 }} active={tab} onChange={onTab} tabs={[
        { id: "overview", label: "概览", icon: "gauge" },
        { id: "releases", label: "部署记录", icon: "layers", count: relCount },
        { id: "backups", label: "备份", icon: "archive", count: bakCount },
        { id: "logs", label: "实时日志", icon: "terminal" },
        { id: "config", label: "配置", icon: "settings" },
      ]} />

      {tab === "overview" ? <OverviewTab app={app} releases={store.releases} /> : null}
      {tab === "releases" ? <ReleasesTab app={app} /> : null}
      {tab === "backups" ? <BackupsTab app={app} onRestore={setRestoreBackup} /> : null}
      {tab === "logs" ? <LogViewer app={app} key={app.id + app.status} /> : null}
      {tab === "config" ? <ConfigTab app={app} /> : null}

      <DeployDialog app={app} open={deploying} onClose={() => setDeploying(false)} />
      <RestoreDialog app={app} backup={restoreBackup} open={!!restoreBackup} onClose={() => setRestoreBackup(null)} />
    </div>
  );
}

export { AppDetailPage, LogViewer, OverviewTab, ConfigTab };
