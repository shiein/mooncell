// Mooncell — 应用列表 + 新建应用向导(JSON Schema 动态表单 + 预检)
import React from 'react';
import { useMC, DEPLOY_TYPES, timeAgo } from '../lib/data.js';
import { Dialog, Btn, Field, Select, Switch, Icon, Spinner, TypeBadge, StatusBadge, EmptyState } from '../components/primitives.jsx';
import { DeployDialog } from '../components/pipeline.jsx';
import { PageHead } from '../components/Shell.jsx';
import { listAgentNodes, precheckApp, getAgentCapabilities } from '../lib/api.js';

const APP_SCHEMAS = {
  "java-jar": [
    { key: "path", label: "JAR 目标路径", ph: "/srv/apps/my-app/app.jar", mono: true },
    { key: "jvm", label: "JVM 参数", ph: "-Xms512m -Xmx2g", mono: true },
    { key: "user", label: "启动用户", ph: "appuser" },
    { key: "health", label: "健康检查 URL / 端口", ph: "http://127.0.0.1:8080/actuator/health", mono: true },
    { key: "logs", label: "日志文件路径(用于在线 tail,需具体文件、不支持通配/~)", ph: "/srv/apps/my-app/logs/app.log", mono: true },
  ],
  "tomcat-war": [
    { key: "path", label: "WAR 目标路径(webapps 下)", ph: "/opt/tomcat/webapps/report.war", mono: true },
    { key: "health", label: "健康检查 URL / 端口", ph: "http://127.0.0.1:8081/report/ 或 端口探活 :8081", mono: true },
    { key: "reload", label: "部署后 systemctl restart tomcat", type: "switch", def: false },
  ],
  "go-binary": [
    { key: "path", label: "二进制目标路径", ph: "/srv/apps/my-app/server", mono: true },
    { key: "args", label: "启动参数", ph: "--config config.toml", mono: true },
    { key: "workdir", label: "工作目录", ph: "/srv/apps/my-app", mono: true },
    { key: "health", label: "健康检查", ph: "http://127.0.0.1:80/healthz", mono: true },
  ],
  "python": [
    { key: "interp", label: "解释器路径(支持 venv)", ph: "/srv/apps/my-app/venv/bin/python", mono: true },
    { key: "entry", label: "入口脚本", ph: "main.py", mono: true },
    { key: "args", label: "启动参数", ph: "--port 8090", mono: true },
  ],
  "node": [
    { key: "entry", label: "入口文件", ph: "server.js", mono: true },
    { key: "pm2name", label: "pm2 进程名", ph: "my-app", mono: true },
    { key: "nodePath", label: "node 路径", ph: "/usr/local/bin/node", mono: true },
  ],
  "static-nginx": [
    { key: "path", label: "目标目录", ph: "/data/web/my-app", mono: true },
    { key: "keepRoot", label: "整目录替换(否则仅 dist 内容)", type: "switch", def: true },
    { key: "reload", label: "部署后 nginx -s reload", type: "switch", def: false },
    { key: "nginxBin", label: "nginx 二进制路径", ph: "/usr/sbin/nginx", mono: true },
  ],
};

function CreateAppDialog({ open, onClose }) {
  const store = useMC();
  const [step, setStep] = React.useState(0);
  const [type, setType] = React.useState(null);
  const [form, setForm] = React.useState({});
  const [checks, setChecks] = React.useState([]);
  const [agents, setAgents] = React.useState([{ id: "default", name: "本机 Agent" }]);
  const [caps, setCaps] = React.useState(null); // 选中 Agent 的真实能力清单;null=未知(Agent 不可达)
  const timers = React.useRef([]);
  React.useEffect(() => {
    if (open) {
      setStep(0); setType(null); setForm({}); setChecks([]);
      listAgentNodes().then((a) => { if (a && a.length) setAgents(a); });
    }
    return () => timers.current.forEach(clearTimeout);
  }, [open]);
  // 据选中的 Agent 拉真实能力清单(Runner 过滤用真实能力,不再用 mock AGENT.caps)。
  React.useEffect(() => {
    if (!open) return;
    setCaps(null);
    getAgentCapabilities(form.agentId).then((c) => setCaps(c && c.capabilities ? c.capabilities : null));
  }, [open, form.agentId]);

  const appId = () => (form.name || "new-app").toLowerCase().replace(/[^a-z0-9]+/g, "-").slice(0, 24) || "new-app";
  // 落盘路径:预检与创建共用同一计算,避免"预检校验 A 路径、实际部署落盘 B 路径"不一致。
  // python/node → 入口脚本;static → web root 目录(软链);go/java → 制品文件(不能是目录)。
  const binPathOf = (id) => {
    if (type === "python" || type === "node")
      return `/srv/apps/${id}/${form.entry || (type === "node" ? "server.js" : "app.py")}`;
    if (type === "static-nginx") return form.path || `/srv/apps/${id}`;
    return form.path || `/srv/apps/${id}/app`;
  };

  const runPrecheck = async () => {
    setStep(2);
    setChecks([{ label: "正在向 Agent 预检…", st: "pending" }]);
    const id = appId();
    const binPath = binPathOf(id);
    const params = new URLSearchParams({
      binPath, port: form.port || "", type,
      runner: selectedRunner(),
      agent: form.agentId || "default",
    });
    const res = await precheckApp(params.toString());
    if (!res || !res.checks) {
      setChecks([{ label: "Agent 不可达,无法预检(仍可创建,部署时再校验)", st: "warn" }]);
      return;
    }
    setChecks(res.checks.map((c) => ({ label: c.label, st: c.ok ? "ok" : "fail", note: c.detail || "" })));
  };
  const checksDone = checks.length > 0 && checks.every((c) => c.st !== "pending");
  // 预检有 fail(白名单外/端口占用/运行时缺失)禁止创建;warn(Agent 不可达)允许降级创建。
  const checksBlocked = checks.some((c) => c.st === "fail");

  const create = () => {
    const id = appId();
    // path 即落盘路径,与预检完全一致(binPathOf);interp 是运行时(python 支持 venv、node 自定义路径)。
    const path = binPathOf(id);
    const interp = type === "python" ? (form.interp || "") : type === "node" ? (form.nodePath || "") : "";
    store.addApp({
      id: id + "-" + Math.random().toString(36).slice(2, 5),
      name: form.name || "未命名应用", type, runner: selectedRunner(),
      status: "stopped", version: "—", pid: null, port: +(form.port || 8080),
      path, interp, workdir: form.workdir || `/srv/apps/${id}`,
      health: form.health || "端口探活 :" + (form.port || 8080), healthType: form.health ? "HTTP 200" : "端口探活",
      logPaths: [form.logs || `/srv/apps/${id}/logs/app.log`],
      jvm: form.jvm || form.args || "", user: form.user || "appuser",
      agentId: form.agentId || "default",
      reload: !!form.reload,
      backupKeep: +(form.backupKeep || 5), lastDeploy: null, uptime: "—", mem: "—", cpu: "—",
      artifactName: id, extraFiles: [],
    });
    onClose();
  };

  const typeEntries = Object.entries(DEPLOY_TYPES);
  const schema = type ? APP_SCHEMAS[type] : [];
  const runnersOf = type ? DEPLOY_TYPES[type].runners : [];
  // Runner 可用性据选中 Agent 的真实能力清单:仅静态类(无进程/软链)无运行时依赖恒可用;
  // tomcat/pm2/systemd 等均按 caps 判定。能力未知(caps===null=Agent 不可达)才降级不禁用、留预检兜底;
  // caps 已加载但缺该 key → fail-closed 视为不可用(与 Agent 预检一致,不放过未自检出的能力)。
  const capOk = (r) => {
    if (r === "无进程" || r === "软链") return true;
    if (!caps) return true; // Agent 不可达降级
    const c = caps.find((x) => x.key === r);
    return c ? c.ok : false; // caps 已加载却无此 key → fail-closed
  };
  // selectedRunner:UI 显示值、预检、创建保存共用同一个 Runner——用户手选则用之,
  // 否则取首个能力可用的 Runner(没有则首个)。杜绝「UI 显示 systemd 却提交 pm2」。
  const selectedRunner = () => form.runner || runnersOf.find(capOk) || runnersOf[0] || "systemd";
  // 该类型在选中 Agent 上无任何可用 Runner:禁止预检/创建(否则只会在预检/部署阶段失败)。
  const noAvailableRunner = runnersOf.length > 0 && !runnersOf.some(capOk);
  // Runner 旧选择纠正:能力加载/切类型/换 Agent 后,若已手选的 form.runner 不再属于当前类型
  // 或能力不可用,清空它 → selectedRunner 自动回落首个可用 Runner(避免提交陈旧的不可用值)。
  React.useEffect(() => {
    if (!form.runner) return;
    const r = form.runner;
    const inType = (type ? DEPLOY_TYPES[type].runners : []).includes(r);
    let available = true;
    if (r !== "无进程" && r !== "软链" && caps) {
      const c = caps.find((x) => x.key === r);
      available = c ? c.ok : false; // 与 capOk 一致:caps 已加载却缺 key → 不可用
    }
    if (!inType || !available) setForm((f) => ({ ...f, runner: undefined }));
  }, [caps, type, form.agentId, form.runner]);

  return (
    <Dialog open={open} onClose={onClose} width={620}
      title="新建应用"
      desc={["第 1 步 · 选择部署类型", "第 2 步 · 按 Schema 填写配置", "第 3 步 · Agent 端预检"][step]}
      foot={
        <React.Fragment>
          {step > 0 ? <Btn variant="ghost" icon="chevronL" onClick={() => setStep(step - 1)}>上一步</Btn> : <Btn variant="ghost" onClick={onClose}>取消</Btn>}
          {step === 0 ? <Btn variant="primary" disabled={!type} onClick={() => setStep(1)}>下一步</Btn> : null}
          {step === 1 ? <Btn variant="primary" disabled={!form.name || noAvailableRunner} onClick={runPrecheck}>执行预检</Btn> : null}
          {step === 2 ? <Btn variant="primary" icon="check" disabled={!checksDone || checksBlocked} onClick={create}>创建应用</Btn> : null}
        </React.Fragment>
      }>
      {step === 0 ? (
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 10 }}>
          {typeEntries.map(([k, t]) => (
            <button key={k} onClick={() => setType(k)} className="card" style={{
              padding: "13px 14px", cursor: "pointer", textAlign: "left", font: "inherit",
              borderColor: type === k ? "var(--primary)" : "var(--border)",
              boxShadow: type === k ? "0 0 0 3px var(--primary-soft)" : "var(--shadow-sm)",
            }}>
              <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                <TypeBadge type={k} />
                {type === k ? <Icon name="check" size={14} style={{ color: "var(--primary)", marginLeft: "auto" }} /> : null}
              </div>
              <div style={{ fontSize: 11.5, color: "var(--muted-fg)", marginTop: 7 }} className="mono">
                Runner: {t.runners.join(" / ")}
              </div>
            </button>
          ))}
        </div>
      ) : null}

      {step === 1 ? (
        <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
          <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12 }}>
            <Field label="应用名 *">
              <input className="input" placeholder="如:数据查询平台后端" value={form.name || ""} onChange={(e) => setForm({ ...form, name: e.target.value })} />
            </Field>
            <Field label="Runner" hint="按所选 Agent 真实能力清单过滤,不可用项置灰禁用">
              <Select value={selectedRunner()} onChange={(v) => setForm({ ...form, runner: v })}
                options={runnersOf.map((r) => ({ value: r, label: capOk(r) ? r : r + "(Agent 未检测到)", disabled: !capOk(r) }))} />
            </Field>
          </div>
          {noAvailableRunner ? (
            <div style={{ fontSize: 12, color: "var(--error)", display: "flex", alignItems: "center", gap: 6, background: "var(--error-soft)", borderRadius: 8, padding: "8px 12px" }}>
              <Icon name="alert" size={14} />所选 Agent 不支持该类型所需的任何 Runner,请换一台具备能力的 Agent 或安装对应运行时后再创建。
            </div>
          ) : null}
          {schema.map((f) => f.type === "switch" ? (
            <div key={f.key} style={{ display: "flex", alignItems: "center", gap: 10 }}>
              <Switch on={form[f.key] != null ? form[f.key] : f.def} onChange={(v) => setForm({ ...form, [f.key]: v })} />
              <span style={{ fontSize: 13 }}>{f.label}</span>
            </div>
          ) : (
            <Field key={f.key} label={f.label}>
              <input className={"input" + (f.mono ? " mono" : "")} style={f.mono ? { fontSize: 12.5 } : undefined}
                placeholder={f.ph} value={form[f.key] || ""} onChange={(e) => setForm({ ...form, [f.key]: e.target.value })} />
            </Field>
          ))}
          <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12 }}>
            <Field label="端口">
              <input className="input mono" placeholder="8080" value={form.port || ""} onChange={(e) => setForm({ ...form, port: e.target.value })} />
            </Field>
            <Field label="备份保留份数">
              <input className="input mono" placeholder="5" value={form.backupKeep || ""} onChange={(e) => setForm({ ...form, backupKeep: e.target.value })} />
            </Field>
          </div>
          <Field label="部署目标 Agent" hint="选择该应用部署到哪台 Agent">
            <Select value={form.agentId || "default"} onChange={(v) => setForm({ ...form, agentId: v })}
              options={agents.map((a) => ({ value: a.id, label: a.name + (a.addr ? " · " + a.addr : "") }))} />
          </Field>
          <div style={{ fontSize: 11.5, color: "var(--muted-fg)", background: "var(--muted)", borderRadius: 8, padding: "8px 12px" }}>
            配置由部署类型的 JSON Schema 约束,前端动态渲染、后端与 Agent 双重校验;钩子仅限白名单内置动作,不支持自由脚本。
          </div>
        </div>
      ) : null}

      {step === 2 ? (
        <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
          {checks.map((c, i) => (
            <div key={i} className="card" style={{ padding: "10px 14px", display: "flex", alignItems: "center", gap: 10 }}>
              {c.st === "pending" ? <Spinner size={14} style={{ color: "var(--muted-fg)" }} /> :
                c.st === "fail" ? <Icon name="alert" size={15} style={{ color: "var(--error)" }} /> :
                c.st === "warn" ? <Icon name="alert" size={15} style={{ color: "var(--warn)" }} /> :
                  <Icon name="check" size={15} style={{ color: "var(--success)" }} />}
              <span style={{ fontSize: 13, flex: 1 }} className="mono">{c.label}</span>
              {c.note ? <span style={{ fontSize: 11.5, color: c.st === "fail" ? "var(--error)" : "var(--warn)" }}>{c.note}</span> : null}
            </div>
          ))}
          {checksDone && checksBlocked ? <div className="fade-up" style={{ fontSize: 12.5, color: "var(--error)", display: "flex", alignItems: "center", gap: 6, marginTop: 4 }}><Icon name="alert" size={14} />预检未通过,请修正后重试(白名单/端口/运行时)</div> : null}
          {checksDone && !checksBlocked ? <div className="fade-up" style={{ fontSize: 12.5, color: "var(--success)", display: "flex", alignItems: "center", gap: 6, marginTop: 4 }}><Icon name="check" size={14} />预检通过,可以创建</div> : null}
        </div>
      ) : null}
    </Dialog>
  );
}

function AppsPage() {
  const store = useMC();
  const { apps, releases } = store;
  const [q, setQ] = React.useState("");
  const [typeF, setTypeF] = React.useState("all");
  const [creating, setCreating] = React.useState(false);
  const [deployApp, setDeployApp] = React.useState(null);

  const list = apps.filter((a) =>
    (typeF === "all" || a.type === typeF) &&
    (!q.trim() || a.name.includes(q) || a.id.includes(q.toLowerCase()))
  );
  const counts = {
    running: apps.filter((a) => a.status === "running" || a.status === "static").length,
    failed: apps.filter((a) => a.status === "failed").length,
    stopped: apps.filter((a) => a.status === "stopped").length,
  };

  return (
    <div>
      <PageHead title="应用 Applications" desc={`${apps.length} 个应用 · ${counts.running} 运行 / ${counts.failed} 异常 / ${counts.stopped} 停止`}
        actions={store.can("write") ? <Btn variant="primary" icon="plus" onClick={() => setCreating(true)}>新建应用</Btn> : null} />

      <div style={{ display: "flex", gap: 10, marginBottom: 14 }}>
        <div style={{ position: "relative", width: 280 }}>
          <Icon name="search" size={14} style={{ position: "absolute", left: 10, top: 10, color: "var(--muted-fg)" }} />
          <input className="input" style={{ paddingLeft: 32 }} placeholder="搜索应用名 / ID …" value={q} onChange={(e) => setQ(e.target.value)} />
        </div>
        <div style={{ width: 180 }}>
          <Select value={typeF} onChange={setTypeF}
            options={[{ value: "all", label: "全部类型" }, ...Object.entries(DEPLOY_TYPES).map(([k, t]) => ({ value: k, label: t.label }))]} />
        </div>
      </div>

      <div className="card" style={{ overflow: "hidden" }}>
        <table className="table">
          <thead><tr>
            <th>应用</th><th>类型</th><th>Runner</th><th>状态</th><th>版本</th><th>端口</th><th>最近部署</th><th style={{ width: 130 }}></th>
          </tr></thead>
          <tbody>
            {list.map((a) => (
              <tr key={a.id} className="app-row" onClick={() => store.nav("app-detail", { appId: a.id })}>
                <td>
                  <div style={{ fontWeight: 600 }}>{a.name}</div>
                  <div className="mono" style={{ fontSize: 11, color: "var(--muted-fg)", marginTop: 1 }}>{a.path.split(" ")[0]}</div>
                </td>
                <td><TypeBadge type={a.type} /></td>
                <td><span className="code-chip">{a.runner}</span></td>
                <td><StatusBadge status={a.status} /></td>
                <td><span className="mono" style={{ fontSize: 12 }}>{a.version}</span></td>
                <td><span className="mono" style={{ fontSize: 12, color: "var(--fg-secondary)" }}>{a.port ? ":" + a.port : "—"}</span></td>
                <td><span style={{ fontSize: 12.5, color: "var(--muted-fg)" }}>{a.lastDeploy ? timeAgo(a.lastDeploy) : "从未部署"}</span></td>
                <td onClick={(e) => e.stopPropagation()}>
                  <div style={{ display: "flex", gap: 6, justifyContent: "flex-end" }}>
                    {store.can("write") ? <Btn size="sm" icon="upload" onClick={() => setDeployApp(a)}>部署</Btn> : null}
                    <Btn variant="ghost" size="sm" icon="chevronR" onClick={() => store.nav("app-detail", { appId: a.id })}></Btn>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {list.length === 0 ? <EmptyState icon="search" title="没有匹配的应用" desc="换个关键字或清空筛选条件试试" /> : null}
      </div>

      <CreateAppDialog open={creating} onClose={() => setCreating(false)} />
      <DeployDialog app={deployApp} open={!!deployApp} onClose={() => setDeployApp(null)} />
    </div>
  );
}

export { AppsPage, CreateAppDialog, APP_SCHEMAS };
