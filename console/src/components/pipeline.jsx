// Mooncell — 部署流水线:计划构建 + 模拟引擎 + 视图 + 部署/还原对话框
import React from 'react';
import { useMC, tsDir, DEPLOY_TYPES, isProcessType, nextVersion, randSha, AGENT, fmtClock, fmtTime } from '../lib/data.js';
import { Dialog, Btn, Field, Switch, Progress, Badge, Icon, Spinner } from './primitives.jsx';
import { deployViaAgentStream, restoreViaAgentStream } from '../lib/api.js';

// 把 Agent 返回的真实部署/还原结果({result, steps:[{name,ok,logs}]})转成 PipelineView 可渲染的形状,
// 复用与模拟完全一致的视觉。verb 仅用于收尾汇总行文案("部署"/"还原")。
function realToPipe(res, verb = "部署") {
  const steps = (res.steps || []).map((s, i) => ({
    id: "r" + i, label: s.name,
    status: s.ok ? "success" : "failed",
    rollback: s.name.indexOf("回滚") >= 0, elapsed: "",
  }));
  const t = Date.now();
  const lines = [];
  (res.steps || []).forEach((s) => {
    lines.push({ ts: t, text: "▸ " + s.name, cls: "head" });
    (s.logs || []).forEach((l) => lines.push({ ts: t, text: "  " + l, cls: s.ok ? "ok" : "err" }));
  });
  // 流式进行中:末尾补一个"执行中"占位步 + 等待提示,收到 done 后由结果分支替换。
  if (res.streaming) {
    steps.push({ id: "live", label: "执行中…", status: "running", rollback: false, elapsed: "" });
    lines.push({ ts: t, text: "  ⏳ 等待 Agent 推送下一步…", cls: "head" });
    return { steps, lines };
  }
  lines.push({
    ts: t,
    text: res.result === "success" ? `═ ${verb}成功 ═` : res.result === "rolledback" ? `═ ${verb}失败,已自动回滚 ═` : `═ ${verb}失败 ═`,
    cls: res.result === "success" ? "ok" : "err",
  });
  return { steps, lines };
}

function basename(p) { return (p || "").split("/").pop(); }

// ---------- runner 专属日志 ----------
function stopLogs(app) {
  switch (app.runner) {
    case "systemd": return [`systemctl stop deploy-${app.id}.service`, `等待进程退出 (pid ${app.pid || 21433}) … 已退出,耗时 1.2s`];
    case "pm2": return [`pm2 stop ${app.id}`, `pm2: [${app.id}](0) ✓ stopped`];
    case "tomcat": return [`执行 ${app.workdir}/bin/shutdown.sh`, `catalina 进程已退出 (pid ${app.pid || 18250})`];
    default: return [`读取 pidfile /opt/deploy-agent/run/${app.id}.pid (pid ${app.pid || 19211})`, `发送 SIGTERM → 等待优雅退出 … 已退出`];
  }
}
function startLogs(app, pid) {
  switch (app.runner) {
    case "systemd": return [`systemctl daemon-reload`, `systemctl start deploy-${app.id}.service`, `进程已启动 · pid ${pid} · 由 systemd 托管(崩溃自动拉起)`];
    case "pm2": return [`pm2 start ecosystem.config.js --only ${app.id}`, `pm2: [${app.id}](0) ✓ online · pid ${pid}`];
    case "tomcat": return [`执行 ${app.workdir}/bin/startup.sh`, `Tomcat started · pid ${pid}`];
    default: return [`nohup 启动 · 工作目录 ${app.workdir}`, `写入 pidfile · pid ${pid} 由 Agent 托管`];
  }
}

// ---------- 部署计划 ----------
function makeDeployPlan(app, opt) {
  const isStatic = app.type === "static-nginx";
  const ts = tsDir(Date.now());
  const pid = 20000 + Math.floor(Math.random() * 9000);
  const steps = [];

  steps.push({
    id: "verify", label: "校验制品", dur: 1400,
    logs: [
      `接收制品 ${opt.fileName} (${opt.size})`,
      `分块合并完成 · ${opt.chunks}/${opt.chunks} chunks · 断点续传 0 次`,
      { text: `sha256 ${opt.sha}… 与上传声明一致`, cls: "ok" },
    ],
  });
  steps.push({
    id: "backup", label: "备份当前版本", dur: 2000,
    logs: [
      `创建备份目录 backups/${app.id}/${ts}/`,
      `复制当前制品 ${basename(app.path.split(" ")[0])}`,
      ...app.extraFiles.map((f) => `附加文件 ${f}`),
      `写入 meta.json · 关联 ${app.version} · 滚动保留 ${app.backupKeep} 份`,
    ],
  });
  if (!isStatic) steps.push({ id: "stop", label: "停止服务", dur: 1600, logs: stopLogs(app) });
  steps.push({
    id: "replace", label: "替换制品", dur: 1500,
    logs: isStatic ? [
      `解压至 /data/web/${app.id}-releases/${ts}/`,
      `软链切换 /data/web/${app.id} → ${app.id}-releases/${ts}`,
      { text: "原子切换完成 · root 指向软链,无需 reload", cls: "ok" },
    ] : [
      `落盘 tmp/upload_${opt.sha.slice(0, 6)} · 二次校验通过`,
      `rename 原子替换 ${app.path}`,
    ],
  });
  if (!isStatic) steps.push({ id: "start", label: "启动服务", dur: 1800, logs: startLogs(app, pid) });

  const healthOk = !opt.simulateFail;
  steps.push({
    id: "health", label: "健康检查", dur: healthOk ? 2400 : 4200,
    fail: !healthOk,
    logs: isStatic ? [
      `GET http://127.0.0.1/${app.id}/ → 200 OK · 14ms`,
      { text: "健康检查通过 (1/1)", cls: "ok" },
    ] : healthOk ? [
      { text: `GET ${app.health} → 连接被拒绝 · 服务启动中,3s 后重试 (1/5)`, cls: "warn" },
      `GET ${app.health} → 200 OK · 86ms (第 2 次尝试)`,
      { text: "健康检查通过 · status: UP", cls: "ok" },
    ] : [
      { text: `GET ${app.health} → 连接被拒绝 · 3s 后重试 (1/3)`, cls: "warn" },
      { text: `GET ${app.health} → 连接被拒绝 · 3s 后重试 (2/3)`, cls: "warn" },
      { text: `GET ${app.health} → 连接被拒绝 (3/3)`, cls: "err" },
      { text: "健康检查失败 · 已启用自动回滚,开始还原", cls: "err" },
    ],
  });

  if (!healthOk) {
    steps.push({
      id: "rb-restore", label: "回滚 · 还原备份", dur: 1700, rollback: true,
      logs: [
        `读取 backups/${app.id}/${ts}/meta.json · ${app.version}`,
        isStatic ? `软链指回旧目录(原子)` : `rename 还原 ${app.path}`,
      ],
    });
    if (!isStatic) steps.push({ id: "rb-start", label: "回滚 · 重启服务", dur: 1600, rollback: true, logs: startLogs(app, pid + 1) });
    steps.push({
      id: "rb-health", label: "回滚 · 健康检查", dur: 1800, rollback: true,
      logs: [
        `GET ${app.health} → 200 OK · 92ms`,
        { text: `旧版本 ${app.version} 已恢复服务`, cls: "ok" },
      ],
    });
  }
  return { steps, result: healthOk ? "success" : "rolledback" };
}

// ---------- 还原计划 ----------
function makeRestorePlan(app, backup) {
  const isStatic = app.type === "static-nginx";
  const ts = tsDir(Date.now());
  const pid = 20000 + Math.floor(Math.random() * 9000);
  const steps = [
    {
      id: "pre-backup", label: "备份当前版本", dur: 1800,
      logs: [
        `还原前先备份当前 ${app.version}(防止"还原错了回不去")`,
        `创建 backups/${app.id}/${ts}/ · 写入 meta.json`,
      ],
    },
  ];
  if (!isStatic) steps.push({ id: "stop", label: "停止服务", dur: 1500, logs: stopLogs(app) });
  steps.push({
    id: "restore", label: "还原制品", dur: 1600,
    logs: [
      `读取 backups/${app.id}/${backup.dir}/ · ${backup.version} (${backup.size})`,
      `sha256 校验通过`,
      isStatic ? "软链切换至备份目录(原子)" : `rename 原子替换 ${app.path}`,
    ],
  });
  if (!isStatic) steps.push({ id: "start", label: "启动服务", dur: 1700, logs: startLogs(app, pid) });
  steps.push({
    id: "health", label: "健康检查", dur: 2000,
    logs: isStatic ? [{ text: "软链切换完成 · 无需健康检查", cls: "ok" }] : [
      `GET ${app.health} → 200 OK · 74ms`,
      { text: `健康检查通过 · ${backup.version} 已恢复运行`, cls: "ok" },
    ],
  });
  return { steps, result: "success" };
}

// ---------- 模拟引擎 ----------
function usePipeline() {
  const [steps, setSteps] = React.useState([]);
  const [lines, setLines] = React.useState([]);
  const [state, setState] = React.useState("idle"); // idle running success rolledback
  const timers = React.useRef([]);
  const clearAll = () => { timers.current.forEach(clearTimeout); timers.current = []; };
  React.useEffect(() => clearAll, []);
  const at = (t, fn) => timers.current.push(setTimeout(fn, t));

  const start = (plan, onFinish) => {
    clearAll();
    setLines([]); setState("running");
    setSteps(plan.steps.map((s) => ({ ...s, status: "pending", elapsed: null })));
    const push = (text, cls) => setLines((s) => [...s, { ts: Date.now(), text, cls }]);
    let t = 350;
    plan.steps.forEach((s, i) => {
      at(t, () => {
        setSteps((prev) => prev.map((p, j) => (j === i ? { ...p, status: "running" } : p)));
        push(`▸ ${s.label}`, "head");
      });
      const gap = s.dur / (s.logs.length + 1);
      s.logs.forEach((lg, k) => {
        const item = typeof lg === "string" ? { text: lg } : lg;
        at(t + gap * (k + 1), () => push("  " + item.text, item.cls));
      });
      at(t + s.dur, () => {
        const ok = !s.fail;
        setSteps((prev) => prev.map((p, j) => (j === i ? { ...p, status: ok ? "success" : "failed", elapsed: (s.dur / 1000).toFixed(1) + "s" } : p)));
        push(ok ? `  ✓ 完成 (${(s.dur / 1000).toFixed(1)}s)` : `  ✗ 步骤失败 (${(s.dur / 1000).toFixed(1)}s)`, ok ? "ok" : "err");
      });
      t += s.dur + 300;
    });
    at(t, () => {
      setState(plan.result);
      setLines((s) => [...s, {
        ts: Date.now(),
        text: plan.result === "success" ? "═ 部署流水线完成 · 状态: 成功 ═" : "═ 流水线结束 · 部署失败,已自动回滚 ═",
        cls: plan.result === "success" ? "ok" : "err",
      }]);
      onFinish && onFinish(plan.result);
    });
  };
  const reset = () => { clearAll(); setSteps([]); setLines([]); setState("idle"); };
  return { steps, lines, state, start, reset };
}

// ---------- 控制台视图 ----------
function Console({ lines, height = 320, filter, style }) {
  const ref = React.useRef(null);
  React.useEffect(() => {
    if (ref.current) ref.current.scrollTop = ref.current.scrollHeight;
  }, [lines.length]);
  const clsColor = { head: "#E8B08C", ok: "#8FAE9B", err: "#D4796A", warn: "#CCA45A" };
  return (
    <div className="console" ref={ref} style={{ height, ...style }}>
      {lines.map((l, i) => {
        let body = l.text;
        if (filter && filter.trim()) {
          const parts = String(l.text).split(filter);
          if (parts.length > 1) body = parts.map((p, j) => (j === 0 ? [p] : [<mark key={"m" + j}>{filter}</mark>, p])).flat();
        }
        return (
          <span className="ln" key={i} style={l.cls && clsColor[l.cls] ? { color: clsColor[l.cls], fontWeight: l.cls === "head" ? 600 : 400 } : undefined}>
            <span className="ts">{fmtClock(l.ts)}  </span>{l.level ? <span className={"lv-" + l.level}>{l.level.padEnd(5)} </span> : null}{body}
          </span>
        );
      })}
      {lines.length === 0 ? <span className="ln" style={{ color: "#6E6852" }}>等待输出 …</span> : null}
    </div>
  );
}

function StepNode({ step, index }) {
  const st = step.status;
  return (
    <div className="step-node" data-st={st}>
      {st === "running" ? <Spinner size={13} /> :
        st === "success" ? <Icon name="check" size={13} /> :
          st === "failed" ? <Icon name="x" size={13} /> : index + 1}
    </div>
  );
}

function PipelineView({ pipe, height = 340 }) {
  return (
    <div style={{ display: "flex", gap: 18 }}>
      <div style={{ width: 218, flex: "none", paddingTop: 4 }}>
        {pipe.steps.map((s, i) => (
          <div className="step-row" key={s.id}>
            <div className="step-rail">
              <StepNode step={s} index={i} />
              {i < pipe.steps.length - 1 ? <div className="step-line" data-done={String(s.status === "success")}></div> : null}
            </div>
            <div style={{ paddingBottom: 16, minHeight: 44 }}>
              <div style={{
                fontSize: 13, fontWeight: 600, lineHeight: "26px",
                color: s.status === "pending" ? "var(--muted-fg)" : s.status === "failed" ? "var(--error)" : s.rollback && s.status !== "pending" ? "var(--warn)" : "var(--fg)",
              }}>{s.label}</div>
              <div style={{ fontSize: 11, color: "var(--muted-fg)" }} className="mono">
                {s.status === "running" ? "执行中…" : s.elapsed || ""}
              </div>
            </div>
          </div>
        ))}
      </div>
      <Console lines={pipe.lines} height={height} style={{ flex: 1, minWidth: 0 }} />
    </div>
  );
}

// ---------- 上传模拟 ----------
function useUploadSim() {
  const [phase, setPhase] = React.useState("idle"); // idle uploading hashing ready
  const [prog, setProg] = React.useState(0);
  const [speed, setSpeed] = React.useState(0);
  const [file, setFile] = React.useState(null);
  const timer = React.useRef(null);
  React.useEffect(() => () => clearInterval(timer.current), []);

  const begin = (f) => {
    clearInterval(timer.current);
    setFile(f); setPhase("uploading"); setProg(0);
    let p = 0;
    timer.current = setInterval(() => {
      const sp = 22 + Math.random() * 26; // MB/s
      setSpeed(sp);
      p += (sp * 0.12 / f.sizeMB) * 100;
      if (p >= 100) {
        clearInterval(timer.current);
        setProg(100); setPhase("hashing");
        setTimeout(() => setPhase("ready"), 900);
      } else setProg(p);
    }, 120);
  };
  const reset = () => { clearInterval(timer.current); setPhase("idle"); setProg(0); setFile(null); };
  return { phase, prog, speed, file, begin, reset };
}

// ---------- 部署对话框 ----------
function DeployDialog({ app, open, onClose }) {
  const store = useMC();
  const [stage, setStage] = React.useState("upload");
  const [simulateFail, setSimulateFail] = React.useState(false);
  const [version, setVersion] = React.useState("");
  const [drag, setDrag] = React.useState(false);
  const [realFile, setRealFile] = React.useState(null); // 真实上传的 File(go-binary 走真机部署)
  const [real, setReal] = React.useState(null);         // 真实部署结果:{loading}|{error}|{result,steps}
  const up = useUploadSim();
  const pipe = usePipeline();
  const inputRef = React.useRef(null);

  React.useEffect(() => {
    if (open) {
      setStage("upload"); setSimulateFail(false); up.reset(); pipe.reset();
      setRealFile(null); setReal(null);
      setVersion(nextVersion(app && app.version));
    }
  }, [open, app && app.id]);
  const sha = React.useMemo(() => randSha(), [open, up.file && up.file.name]);
  if (!app) return null;

  // 进程类(go-binary/java-jar/python)且上传了真实文件 → 走 Agent 真实部署;否则(其它类型 / 示例制品)沿用模拟。
  const isReal = isProcessType(app.type) && !!realFile;
  const ext = DEPLOY_TYPES[app.type].artifactExt;
  const chunks = up.file ? Math.ceil(up.file.sizeMB / 4) : 0;

  const pickExample = () => {
    setRealFile(null);
    const mb = 8 + Math.random() * 50;
    up.begin({ name: `${app.artifactName}-${version}${ext || ".tar.gz"}`, sizeMB: mb, size: mb.toFixed(1) + " MB" });
  };
  const pickReal = (f) => {
    setRealFile(f);
    const mb = Math.max(1, f.size / 1048576);
    up.begin({ name: f.name, sizeMB: mb, size: mb < 1024 ? mb.toFixed(1) + " MB" : (mb / 1024).toFixed(2) + " GB" });
  };

  const startDeploy = async () => {
    if (isReal) {
      setStage("pipeline"); setReal({ streaming: true, steps: [] });
      // 前端只提交 制品 + version + releaseId;Agent 配置由 Console 据已存应用配置服务端生成。
      const releaseId = (crypto.randomUUID && crypto.randomUUID()) || ("rel-" + Date.now() + "-" + Math.random().toString(36).slice(2));
      const res = await deployViaAgentStream(app.id, version, releaseId, realFile, (type, data) => {
        if (type === "step") setReal((prev) => ({ streaming: true, steps: [...((prev && prev.steps) || []), data] }));
      });
      if (res.error) { setReal({ error: res.error }); return; }
      setReal(res);
      store.finishDeploy(app, { version: res.version || version, size: up.file ? up.file.size : "—", result: res.result || "failed", real: true });
      return;
    }
    const plan = makeDeployPlan(app, { fileName: up.file.name, size: up.file.size, chunks, sha, simulateFail });
    setStage("pipeline");
    pipe.start(plan, (result) => store.finishDeploy(app, { version, size: up.file.size, result }));
  };

  // 统一两条路径的状态:resultKind ∈ idle|running|success|rolledback|error
  const resultKind = real
    ? (real.streaming || real.loading ? "running" : real.error ? "error" : real.result === "success" ? "success" : (real.result || "failed"))
    : pipe.state;
  const running = resultKind === "running";
  const doneState = resultKind === "success" || resultKind === "rolledback";

  return (
    <Dialog open={open} onClose={onClose} noClose={running} width={stage === "pipeline" ? 860 : 560}
      title={`部署 · ${app.name}`}
      desc={stage === "upload"
        ? (isReal ? `${app.type} · 将下发到 Agent 真机部署:备份 → 停止 → 替换 → 启动 → 健康检查` : "上传制品后将自动执行:备份 → 停止 → 替换 → 启动 → 健康检查")
        : `Release ${version} · 操作人 ${store.user}`}
      foot={stage === "upload" ? (
        <React.Fragment>
          <Btn variant="ghost" onClick={onClose}>取消</Btn>
          <Btn variant="primary" icon="zap" disabled={up.phase !== "ready"} onClick={startDeploy}>{isReal ? "开始部署(真机)" : "开始部署"}</Btn>
        </React.Fragment>
      ) : (
        <React.Fragment>
          {resultKind === "success" ? <Btn variant="ghost" onClick={() => { onClose(); store.nav("app-detail", { appId: app.id, tab: "logs" }); }}>查看应用日志</Btn> : null}
          <Btn variant={doneState ? "primary" : "outline"} disabled={running} onClick={onClose}>{running ? "部署执行中…" : "关闭"}</Btn>
        </React.Fragment>
      )}>

      {stage === "upload" ? (
        <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
          {up.phase === "idle" ? (
            <div className="upload-zone" data-drag={String(drag)}
              onClick={() => inputRef.current && inputRef.current.click()}
              onDragOver={(e) => { e.preventDefault(); setDrag(true); }}
              onDragLeave={() => setDrag(false)}
              onDrop={(e) => { e.preventDefault(); setDrag(false); if (e.dataTransfer.files[0]) pickReal(e.dataTransfer.files[0]); }}>
              <Icon name="upload" size={22} style={{ color: "var(--muted-fg)" }} />
              <div style={{ fontWeight: 600, marginTop: 8, fontSize: 13.5 }}>拖拽制品到此处,或点击选择文件</div>
              <div style={{ fontSize: 12, color: "var(--muted-fg)", marginTop: 3 }}>
                {DEPLOY_TYPES[app.type].label} · 分块上传 + 断点续传 + sha256 校验 · 支持大文件
              </div>
              <input type="file" ref={inputRef} style={{ display: "none" }} onChange={(e) => { if (e.target.files[0]) pickReal(e.target.files[0]); }} />
              <div style={{ marginTop: 12 }}>
                <Btn size="sm" onClick={(e) => { e.stopPropagation(); pickExample(); }}>使用示例制品演示</Btn>
              </div>
            </div>
          ) : (
            <div className="card" style={{ padding: 14 }}>
              <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
                <Icon name="fileText" size={18} style={{ color: "var(--primary)" }} />
                <div style={{ flex: 1, minWidth: 0 }}>
                  <div className="mono" style={{ fontSize: 12.5, fontWeight: 600, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{up.file.name}</div>
                  <div style={{ fontSize: 11.5, color: "var(--muted-fg)" }}>{up.file.size} · {chunks} chunks</div>
                </div>
                {up.phase === "ready" ? <Badge tone="success" dot>校验通过</Badge> :
                  up.phase === "hashing" ? <Badge tone="info"><Spinner size={11} /> sha256 校验中</Badge> :
                    <span className="mono" style={{ fontSize: 11.5, color: "var(--muted-fg)" }}>{up.speed.toFixed(0)} MB/s</span>}
              </div>
              <div style={{ marginTop: 10 }}>
                <Progress value={up.phase === "uploading" ? up.prog : 100} color={up.phase === "ready" ? "var(--success)" : undefined} height={6} />
              </div>
              {up.phase === "ready" ? (
                <div className="mono" style={{ fontSize: 11, color: "var(--muted-fg)", marginTop: 8 }}>sha256: {sha}9c2e41ab07d6…</div>
              ) : null}
            </div>
          )}

          <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12 }}>
            <Field label="Release 版本号">
              <input className="input mono" value={version} onChange={(e) => setVersion(e.target.value)} />
            </Field>
            <Field label="部署目标">
              <div className="input mono" style={{ display: "flex", alignItems: "center", fontSize: 12, color: "var(--fg-secondary)", background: "var(--muted)" }}>
                {AGENT.name} · {AGENT.host.split(":")[0]}
              </div>
            </Field>
          </div>

          <div className="card" style={{ padding: "11px 14px", display: "flex", alignItems: "center", gap: 11, background: "var(--bg)" }}>
            <Switch on={simulateFail} onChange={setSimulateFail} />
            <div>
              <div style={{ fontSize: 13, fontWeight: 600 }}>模拟健康检查失败<span style={{ color: "var(--muted-fg)", fontWeight: 400 }}>(原型演示)</span></div>
              <div style={{ fontSize: 11.5, color: "var(--muted-fg)" }}>新版本启动后探活不通过 → 触发自动回滚流程</div>
            </div>
          </div>
        </div>
      ) : real ? (
        <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
          {real.loading ? (
            <div className="card card-pad" style={{ display: "flex", alignItems: "center", gap: 12 }}>
              <Spinner size={16} />
              <div>
                <div style={{ fontWeight: 600, fontSize: 13.5 }}>正在向 Agent 下发部署 · {version}</div>
                <div style={{ fontSize: 12, color: "var(--muted-fg)" }}>备份 → 停止 → 原子替换 → 启动 → 健康检查(失败将自动回滚)</div>
              </div>
            </div>
          ) : real.error ? (
            <div className="card" style={{ padding: "12px 16px", display: "flex", alignItems: "center", gap: 10, background: "var(--error-soft)", color: "var(--error)" }}>
              <Icon name="alert" size={16} /><span style={{ fontSize: 13 }}>部署失败:{real.error}</span>
            </div>
          ) : (
            <React.Fragment>
              {doneState ? (
                <div className="fade-up" style={{
                  borderRadius: 10, padding: "12px 16px", display: "flex", alignItems: "center", gap: 10,
                  background: resultKind === "success" ? "var(--success-soft)" : "var(--warn-soft)",
                  color: resultKind === "success" ? "var(--success)" : "var(--warn)",
                }}>
                  <Icon name={resultKind === "success" ? "check" : "rotate"} size={17} />
                  <div style={{ flex: 1 }}>
                    <div style={{ fontWeight: 650, fontSize: 13.5 }}>
                      {resultKind === "success" ? `部署成功 · ${real.version || version} 已上线` : `部署失败 · 已自动回滚至 ${app.version}`}
                    </div>
                    <div style={{ fontSize: 12, opacity: .85 }}>
                      {resultKind === "success" ? "Agent 已落盘并由 systemd 托管 · 部署前版本已备份" : "旧版本已恢复服务,业务无中断"}
                    </div>
                  </div>
                </div>
              ) : null}
              <PipelineView pipe={realToPipe(real)} />
            </React.Fragment>
          )}
        </div>
      ) : (
        <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
          {doneState ? (
            <div className="fade-up" style={{
              borderRadius: 10, padding: "12px 16px", display: "flex", alignItems: "center", gap: 10,
              background: pipe.state === "success" ? "var(--success-soft)" : "var(--warn-soft)",
              color: pipe.state === "success" ? "var(--success)" : "var(--warn)",
            }}>
              <Icon name={pipe.state === "success" ? "check" : "rotate"} size={17} />
              <div style={{ flex: 1 }}>
                <div style={{ fontWeight: 650, fontSize: 13.5 }}>
                  {pipe.state === "success" ? `部署成功 · ${version} 已上线` : `部署失败 · 已自动回滚至 ${app.version}`}
                </div>
                <div style={{ fontSize: 12, opacity: .85 }}>
                  {pipe.state === "success" ? "部署前版本已自动备份,可随时一键还原" : "失败原因:健康检查超时 · 旧版本已恢复服务,业务无中断"}
                </div>
              </div>
            </div>
          ) : null}
          <PipelineView pipe={pipe} />
        </div>
      )}
    </Dialog>
  );
}

// ---------- 还原对话框 ----------
function RestoreDialog({ app, backup, open, onClose }) {
  const store = useMC();
  const [stage, setStage] = React.useState("confirm");
  const [real, setReal] = React.useState(null); // 真实还原:{streaming,steps}|{error}|{result,steps}
  const pipe = usePipeline();
  React.useEffect(() => { if (open) { setStage("confirm"); setReal(null); pipe.reset(); } }, [open, backup && backup.id]);
  if (!app || !backup) return null;

  // 进程类且该备份是 Agent 上的真实备份 → 走真机还原;否则(其它类型 / mock 备份)沿用模拟。
  const isReal = isProcessType(app.type) && !!backup.real;

  const startRestore = async () => {
    setStage("pipeline");
    if (isReal) {
      setReal({ streaming: true, steps: [] });
      // 前端只提交 backup + version + releaseId;Agent 配置由 Console 据已存应用配置服务端生成。
      const releaseId = (crypto.randomUUID && crypto.randomUUID()) || ("rel-" + Date.now() + "-" + Math.random().toString(36).slice(2));
      const res = await restoreViaAgentStream(app.id, backup.version, backup.dir, releaseId, (type, data) => {
        if (type === "step") setReal((prev) => ({ streaming: true, steps: [...((prev && prev.steps) || []), data] }));
      });
      if (res.error) { setReal({ error: res.error }); return; }
      setReal(res);
      if (res.result === "success") store.finishRestore(app, backup, { real: true });
      return;
    }
    pipe.start(makeRestorePlan(app, backup), () => store.finishRestore(app, backup));
  };

  // 统一两条路径状态:resultKind ∈ idle|running|success|rolledback|error
  const resultKind = real
    ? (real.streaming ? "running" : real.error ? "error" : real.result === "success" ? "success" : (real.result || "failed"))
    : pipe.state;
  const running = resultKind === "running";
  const doneState = resultKind === "success" || resultKind === "rolledback";

  return (
    <Dialog open={open} onClose={onClose} noClose={running} width={stage === "pipeline" ? 860 : 540}
      title={`一键还原 · ${app.name}`}
      desc={isReal ? `${app.type} · 下发 Agent 真机还原到备份 ${backup.dir}` : `还原到备份 ${backup.dir} (${backup.version})`}
      foot={stage === "confirm" ? (
        <React.Fragment>
          <Btn variant="ghost" onClick={onClose}>取消</Btn>
          <Btn variant="primary" icon="rotate" onClick={startRestore}>{isReal ? "开始还原(真机)" : "开始还原"}</Btn>
        </React.Fragment>
      ) : <Btn variant={resultKind === "success" ? "primary" : "outline"} disabled={running} onClick={onClose}>{running ? "还原中…" : "关闭"}</Btn>}>
      {stage === "confirm" ? (
        <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
          <div className="card" style={{ padding: 14, background: "var(--bg)" }}>
            <div style={{ display: "grid", gridTemplateColumns: "auto 1fr", gap: "7px 16px", fontSize: 13 }}>
              <span style={{ color: "var(--muted-fg)" }}>备份目录</span><span className="mono" style={{ fontSize: 12 }}>backups/{app.id}/{backup.dir}/</span>
              <span style={{ color: "var(--muted-fg)" }}>备份版本</span><span className="mono" style={{ fontSize: 12 }}>{backup.version || "—"} · {backup.size}</span>
              <span style={{ color: "var(--muted-fg)" }}>创建时间</span><span>{fmtTime(backup.time)}({backup.auto ? "部署自动备份" : "手动备份"})</span>
              <span style={{ color: "var(--muted-fg)" }}>当前版本</span><span className="mono" style={{ fontSize: 12 }}>{app.version}</span>
            </div>
          </div>
          <div style={{
            display: "flex", gap: 9, padding: "10px 13px", borderRadius: 9, fontSize: 12.5,
            background: "var(--info-soft)", color: "var(--info)",
          }}>
            <Icon name="archive" size={15} style={{ flex: "none", marginTop: 1 }} />
            <span>还原会走与部署相同的流水线;开始前会先备份当前版本 {app.version},防止"还原错了回不去"。</span>
          </div>
        </div>
      ) : real ? (
        <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
          {real.error ? (
            <div className="card" style={{ padding: "12px 16px", display: "flex", alignItems: "center", gap: 10, background: "var(--error-soft)", color: "var(--error)" }}>
              <Icon name="alert" size={16} /><span style={{ fontSize: 13 }}>还原失败:{real.error}</span>
            </div>
          ) : (
            <React.Fragment>
              {doneState ? (
                <div className="fade-up" style={{
                  borderRadius: 10, padding: "12px 16px", display: "flex", alignItems: "center", gap: 10,
                  background: resultKind === "success" ? "var(--success-soft)" : "var(--warn-soft)",
                  color: resultKind === "success" ? "var(--success)" : "var(--warn)",
                }}>
                  <Icon name={resultKind === "success" ? "check" : "rotate"} size={17} />
                  <div style={{ fontWeight: 650, fontSize: 13.5 }}>
                    {resultKind === "success" ? `还原成功 · ${backup.version || "备份"} 已恢复运行` : `还原失败 · 已自动回滚至原版本`}
                  </div>
                </div>
              ) : null}
              <PipelineView pipe={realToPipe(real, "还原")} />
            </React.Fragment>
          )}
        </div>
      ) : (
        <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
          {pipe.state === "success" ? (
            <div className="fade-up" style={{ borderRadius: 10, padding: "12px 16px", display: "flex", alignItems: "center", gap: 10, background: "var(--success-soft)", color: "var(--success)" }}>
              <Icon name="check" size={17} />
              <div style={{ fontWeight: 650, fontSize: 13.5 }}>还原成功 · {backup.version} 已恢复运行</div>
            </div>
          ) : null}
          <PipelineView pipe={pipe} />
        </div>
      )}
    </Dialog>
  );
}

export {
  makeDeployPlan, makeRestorePlan, usePipeline, PipelineView, Console,
  DeployDialog, RestoreDialog, useUploadSim, basename,
};
