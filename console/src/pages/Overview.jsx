// Mooncell — 总览(系统监控)/ 文件柜 / 审计日志
import React from 'react';
import { useMC, AGENT, genSeries, timeAgo, fmtTime, tsDir, MC_NOW, MC_DAY } from '../lib/data.js';
import { Btn, Icon, Badge, Progress, Sparkline, Switch, CopyChip, EmptyState, Select, toast } from '../components/primitives.jsx';
import { PageHead } from '../components/Shell.jsx';
import { useAgents } from '../lib/agent.js';
import { uploadCabinetFile } from '../lib/api.js';

function StatCard({ label, value, unit, series, color, extra }) {
  return (
    <div className="card card-pad" style={{ display: "flex", flexDirection: "column", gap: 6 }}>
      <div style={{ fontSize: 12.5, color: "var(--muted-fg)", fontWeight: 500 }}>{label}</div>
      <div style={{ display: "flex", alignItems: "flex-end", gap: 12 }}>
        <div className="stat-num" style={{ color, flex: "none" }}>{value}<span style={{ fontSize: 14, color: "var(--muted-fg)", fontWeight: 500 }}> {unit}</span></div>
        <div style={{ marginLeft: "auto", flex: "none" }}><Sparkline data={series} color={color} width={130} height={34} /></div>
      </div>
      {extra}
    </div>
  );
}

// AgentBlock — 单台 Agent 的分区块:资源水位(CPU/内存/磁盘)+ 能力清单 + 该机名下应用健康。
// 每块自管 CPU/内存曲线:有真实读数则追加,离线/探测中维持模拟动画(纯前端 dev 也有动效)。
function AgentBlock({ agent, apps }) {
  const store = useMC();
  const { name, addr, system, caps, online } = agent;
  const [cpuS, setCpuS] = React.useState(() => genSeries(AGENT.cpu, 9));
  const [memS, setMemS] = React.useState(() => genSeries(AGENT.mem, 4));

  React.useEffect(() => {
    if (!system) return;
    setCpuS((s) => [...s.slice(1), Math.round(system.cpuPercent)]);
    setMemS((s) => [...s.slice(1), Math.round(system.memPercent)]);
  }, [system]);

  React.useEffect(() => {
    if (online) return;
    const iv = setInterval(() => {
      setCpuS((s) => [...s.slice(1), Math.max(3, Math.min(95, s[s.length - 1] + (Math.random() - 0.5) * 14)) | 0]);
      setMemS((s) => [...s.slice(1), Math.max(20, Math.min(92, s[s.length - 1] + (Math.random() - 0.5) * 5)) | 0]);
    }, 2000);
    return () => clearInterval(iv);
  }, [online]);

  const cpu = cpuS[cpuS.length - 1], mem = memS[memS.length - 1];
  const disk = system ? Math.round(system.diskPercent) : AGENT.disk;
  const diskUsed = system ? system.diskUsedGB : AGENT.diskDetail.used;
  const diskTotal = system ? system.diskTotalGB : AGENT.diskDetail.total;
  const memLabel = system
    ? `${(system.memUsedMB / 1024).toFixed(1)} GB / ${Math.round(system.memTotalMB / 1024)} GB`
    : "—";
  const failed = apps.filter((a) => a.status === "failed");
  const running = apps.filter((a) => a.status === "running" || a.status === "static").length;
  const stopped = apps.filter((a) => a.status === "stopped").length;

  return (
    <div className="card card-pad" style={{ display: "flex", flexDirection: "column", gap: 14 }}>
      <div style={{ display: "flex", alignItems: "center", gap: 9 }}>
        <Icon name="server" size={15} style={{ color: "var(--primary)" }} />
        <span style={{ fontSize: 14, fontWeight: 700 }}>{name}</span>
        {addr ? <span className="mono" style={{ fontSize: 11.5, color: "var(--muted-fg)" }}>{addr}</span> : null}
        {online === false ? <Badge tone="error" dot style={{ marginLeft: "auto" }}>离线 · 显示缓存</Badge>
          : online ? <Badge tone="success" dot style={{ marginLeft: "auto" }}>在线</Badge>
            : <span style={{ marginLeft: "auto", fontSize: 11.5, color: "var(--muted-fg)" }}>探测中…</span>}
      </div>

      <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr 1fr", gap: 12 }}>
        <StatCard label="CPU" value={cpu} unit="%" series={cpuS} color="var(--info)" />
        <StatCard label="内存" value={mem} unit="%" series={memS} color="var(--cyan)"
          extra={<div style={{ fontSize: 11.5, color: "var(--muted-fg)" }}>{memLabel}</div>} />
        <div className="card card-pad" style={{ display: "flex", flexDirection: "column", gap: 8, boxShadow: "none" }}>
          <div style={{ display: "flex", alignItems: "center" }}>
            <span style={{ fontSize: 12.5, color: "var(--muted-fg)", fontWeight: 500 }}>磁盘水位</span>
            {disk > 85 ? <Badge tone="error" style={{ marginLeft: "auto" }}>禁止部署</Badge> :
              disk > 70 ? <Badge tone="warn" style={{ marginLeft: "auto" }}>接近阈值</Badge> : null}
          </div>
          <div className="stat-num">{disk}<span style={{ fontSize: 14, color: "var(--muted-fg)", fontWeight: 500 }}> % · {diskUsed}/{diskTotal} GB</span></div>
          <Progress value={disk} height={7} color={disk > 85 ? "var(--error)" : disk > 70 ? "var(--warn)" : "var(--success)"} />
        </div>
      </div>

      <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 16 }}>
        {/* 能力清单 */}
        <div>
          <div style={{ fontSize: 12.5, color: "var(--muted-fg)", fontWeight: 600, marginBottom: 8 }}>能力清单</div>
          <div style={{ display: "grid", gridTemplateColumns: "repeat(3, 1fr)", gap: 6 }}>
            {caps.map((c) => (
              <div key={c.key} className="card" style={{ padding: "6px 9px", boxShadow: "none", background: c.ok ? "var(--bg)" : "var(--muted)", opacity: c.ok ? 1 : .55 }}>
                <div style={{ display: "flex", alignItems: "center", gap: 5, fontSize: 11.5, fontWeight: 600 }}>
                  <Icon name={c.ok ? "check" : "x"} size={11} style={{ color: c.ok ? "var(--success)" : "var(--muted-fg)" }} />{c.label}
                </div>
                <div className="mono" style={{ fontSize: 10, color: "var(--muted-fg)" }}>{c.ver}</div>
              </div>
            ))}
          </div>
        </div>

        {/* 该 Agent 名下应用健康 */}
        <div>
          <div style={{ fontSize: 12.5, color: "var(--muted-fg)", fontWeight: 600, marginBottom: 8 }}>应用健康 · {apps.length} 个</div>
          {apps.length === 0 ? (
            <div style={{ fontSize: 12, color: "var(--muted-fg)", padding: "6px 0" }}>该 Agent 暂无应用</div>
          ) : (
            <React.Fragment>
              <div style={{ display: "flex", gap: 18, marginBottom: failed.length ? 10 : 0 }}>
                {[["运行中", running, "var(--success)"], ["异常", failed.length, "var(--error)"], ["已停止", stopped, "var(--muted-fg)"]].map(([l, n, c]) => (
                  <div key={l}>
                    <div className="stat-num" style={{ fontSize: 20, color: c }}>{n}</div>
                    <div style={{ fontSize: 11, color: "var(--muted-fg)" }}>{l}</div>
                  </div>
                ))}
              </div>
              {failed.map((a) => (
                <button key={a.id} className="card" onClick={() => store.nav("app-detail", { appId: a.id })} style={{
                  width: "100%", padding: "8px 11px", marginTop: 6, display: "flex", alignItems: "center", gap: 9, cursor: "pointer",
                  font: "inherit", textAlign: "left", background: "var(--error-soft)", borderColor: "transparent", boxShadow: "none",
                }}>
                  <Icon name="alert" size={13} style={{ color: "var(--error)" }} />
                  <span style={{ fontSize: 12, fontWeight: 600, color: "var(--error)" }}>{a.name}</span>
                  <Icon name="chevronR" size={12} style={{ color: "var(--error)", marginLeft: "auto" }} />
                </button>
              ))}
            </React.Fragment>
          )}
        </div>
      </div>
    </div>
  );
}

function OverviewPage() {
  const store = useMC();
  const agents = useAgents();

  // 应用按 agentId 归属(未设视为 default,与 appConfig/路由口径一致)。
  const appsOf = (id) => store.apps.filter((a) => (a.agentId || "default") === id);
  // 防丢:agentId 指向已移除/未知 Agent 的应用,单列一块,避免在分组视图里静默消失。
  const knownIds = new Set((agents || []).map((a) => a.id));
  const orphans = agents ? store.apps.filter((a) => !knownIds.has(a.agentId || "default")) : [];

  return (
    <div>
      <PageHead title="总览 Overview" desc="按 Agent 分组 · 各机资源水位 / 能力清单 / 应用健康" />

      <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        {agents === null ? (
          <div className="card card-pad" style={{ fontSize: 13, color: "var(--muted-fg)" }}>正在加载 Agent 列表…</div>
        ) : (
          agents.map((ag) => <AgentBlock key={ag.id} agent={ag} apps={appsOf(ag.id)} />)
        )}

        {orphans.length > 0 ? (
          <div className="card card-pad" style={{ display: "flex", flexDirection: "column", gap: 10 }}>
            <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
              <Icon name="alert" size={14} style={{ color: "var(--warn)" }} />
              <span style={{ fontSize: 13.5, fontWeight: 700 }}>未知 / 已移除 Agent 名下应用</span>
              <Badge tone="warn" style={{ marginLeft: "auto" }}>{orphans.length} 个</Badge>
            </div>
            <div style={{ fontSize: 12, color: "var(--muted-fg)" }}>这些应用的 agentId 不在已注册 Agent 列表中,请重新指派或确认对应 Agent 是否被移除。</div>
            {orphans.map((a) => (
              <button key={a.id} className="card" onClick={() => store.nav("app-detail", { appId: a.id })} style={{
                width: "100%", padding: "8px 11px", display: "flex", alignItems: "center", gap: 9, cursor: "pointer",
                font: "inherit", textAlign: "left", background: "var(--bg)", boxShadow: "none",
              }}>
                <span style={{ fontSize: 12.5, fontWeight: 600 }}>{a.name}</span>
                <span className="mono" style={{ fontSize: 11, color: "var(--muted-fg)" }}>agentId={a.agentId || "default"}</span>
                <Icon name="chevronR" size={12} style={{ color: "var(--muted-fg)", marginLeft: "auto" }} />
              </button>
            ))}
          </div>
        ) : null}

        {/* 最近动态(全局,跨 Agent) */}
        <div className="card card-pad">
          <h4 style={{ fontSize: 13.5, marginBottom: 14, display: "flex", alignItems: "center", gap: 7 }}><Icon name="clock" size={14} style={{ color: "var(--primary)" }} />最近动态</h4>
          <div style={{ display: "flex", flexDirection: "column" }}>
            {store.audit.slice(0, 7).map((a, i) => (
              <div key={a.id} style={{ display: "flex", gap: 11, paddingBottom: i === 6 ? 0 : 14 }}>
                <div style={{ display: "flex", flexDirection: "column", alignItems: "center", flex: "none" }}>
                  <span style={{
                    width: 8, height: 8, borderRadius: "50%", marginTop: 5,
                    background: a.result.includes("失败") ? "var(--error)" : "var(--success)",
                  }}></span>
                  {i < 6 ? <span style={{ width: 1.5, flex: 1, background: "var(--border)", marginTop: 4 }}></span> : null}
                </div>
                <div style={{ minWidth: 0 }}>
                  <div style={{ fontSize: 12.5 }}>
                    <span style={{ fontWeight: 600 }}>{a.user}</span>
                    <span style={{ color: "var(--fg-secondary)" }}> {a.action} · {a.target}</span>
                  </div>
                  <div style={{ fontSize: 11, color: "var(--muted-fg)", marginTop: 1 }}>{timeAgo(a.time)} · {a.result}</div>
                </div>
              </div>
            ))}
          </div>
        </div>
      </div>
    </div>
  );
}

// ---------- 文件柜 ----------
// 登录视图:看到所有上传(含匿名投递),可改公开/私有、删除。匿名投递走独立免登录页 /drop。
function CabinetPage() {
  const store = useMC();
  const [uploading, setUploading] = React.useState(false);
  const [prog, setProg] = React.useState(0);
  const fileRef = React.useRef(null);
  const canWrite = store.can("write");

  // 真实上传:multipart 到 Console,落盘 + 返回元数据(登录上传默认私有)。
  const realUpload = async (file) => {
    if (!file) return;
    setUploading(true); setProg(45);
    try {
      const meta = await uploadCabinetFile(file, false);
      setProg(100);
      store.pushCabinetFile(meta, false);
    } catch (e) {
      toast(e.message || "上传失败", { tone: "error", icon: "alert" });
    } finally {
      setUploading(false); setProg(0);
      if (fileRef.current) fileRef.current.value = "";
    }
  };

  // 触发浏览器下载(服务端强制 attachment,不会离开当前页)。
  const dl = (url) => {
    const a = document.createElement("a");
    a.href = url; a.rel = "noopener";
    document.body.appendChild(a); a.click(); a.remove();
  };

  const zone = (
    <div className="upload-zone" data-disabled={String(!canWrite)}
      onClick={() => canWrite && !uploading && fileRef.current && fileRef.current.click()}
      onDragOver={(e) => e.preventDefault()}
      onDrop={(e) => { e.preventDefault(); if (canWrite && e.dataTransfer.files[0]) realUpload(e.dataTransfer.files[0]); }}>
      <input type="file" ref={fileRef} style={{ display: "none" }} onChange={(e) => realUpload(e.target.files[0])} />
      {uploading ? (
        <div style={{ maxWidth: 320, margin: "0 auto" }}>
          <div style={{ fontSize: 13, fontWeight: 600, marginBottom: 8 }}>上传中 · {Math.min(100, prog | 0)}%</div>
          <Progress value={prog} height={6} />
        </div>
      ) : (
        <React.Fragment>
          <Icon name="upload" size={20} style={{ color: "var(--muted-fg)" }} />
          <div style={{ fontWeight: 600, marginTop: 7, fontSize: 13.5 }}>{canWrite ? "拖拽文件到此处,或点击选择文件" : "当前角色为只读,无上传权限"}</div>
          <div style={{ fontSize: 11.5, color: "var(--muted-fg)", marginTop: 3 }}>
            单文件 ≤ 200 MB · 默认 7 天后自动清理 · 上传后获得提取码 + 直链
          </div>
        </React.Fragment>
      )}
    </div>
  );

  return (
    <div>
      <PageHead title="文件柜 Cabinet" desc="内网临时文件中转 · 二进制存储 · 下载强制 attachment"
        actions={<Btn icon="link" onClick={() => window.open("/drop", "_blank", "noopener")}>免登录投递页 · /drop</Btn>} />

      <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        {zone}
        <div className="card" style={{ overflow: "hidden" }}>
          <table className="table">
            <thead><tr><th>文件</th><th>大小</th><th>上传者</th><th>上传时间</th><th>过期</th><th>提取码</th><th>公开</th><th>下载</th><th style={{ width: 90 }}></th></tr></thead>
            <tbody>
              {store.cabinet.map((f) => (
                <tr key={f.id}>
                  <td><span className="mono" style={{ fontSize: 12, fontWeight: 500 }}>{f.name}</span></td>
                  <td><span className="mono" style={{ fontSize: 12 }}>{f.size}</span></td>
                  <td style={{ fontSize: 12.5 }}>{f.uploader}</td>
                  <td><span style={{ fontSize: 12, color: "var(--muted-fg)" }}>{timeAgo(f.time)}</span></td>
                  <td><span style={{ fontSize: 12, color: f.expires - MC_NOW < 3 * MC_DAY ? "var(--warn)" : "var(--muted-fg)" }}>{timeAgo(f.expires)}</span></td>
                  <td><CopyChip text={f.code} /></td>
                  <td>{canWrite ? <Switch on={f.public} onChange={() => store.toggleCabinetPublic(f)} /> : <span style={{ fontSize: 12, color: "var(--muted-fg)" }}>{f.public ? "公开" : "私有"}</span>}</td>
                  <td><span className="mono" style={{ fontSize: 12, color: "var(--muted-fg)" }}>{f.downloads}</span></td>
                  <td>
                    <div style={{ display: "flex", gap: 4, justifyContent: "flex-end" }}>
                      <Btn size="sm" variant="ghost" icon="download" title="下载" onClick={() => dl(`/api/cabinet/${f.id}/download`)}></Btn>
                      <Btn size="sm" variant="ghost" icon="link" title={f.public ? "复制公开直链" : "复制下载链接"}
                        onClick={() => { navigator.clipboard?.writeText(location.origin + (f.public ? `/api/pubfile/${f.code}` : `/api/cabinet/${f.id}/download`)); toast("直链已复制到剪贴板"); }}></Btn>
                      {canWrite ? <Btn size="sm" variant="ghost" icon="trash" title="删除" onClick={() => store.deleteCabinetFile(f)}></Btn> : null}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {store.cabinet.length === 0 ? <EmptyState icon="folder" title="文件柜是空的" /> : null}
        </div>
      </div>
    </div>
  );
}

// ---------- 审计日志 ----------
function AuditPage() {
  const store = useMC();
  const [actF, setActF] = React.useState("all");
  const [q, setQ] = React.useState("");
  const acts = [...new Set(store.audit.map((a) => a.action))];
  const list = store.audit.filter((a) =>
    (actF === "all" || a.action === actF) &&
    (!q.trim() || a.target.includes(q) || a.user.includes(q)));
  return (
    <div>
      <PageHead title="审计日志 Audit" desc="所有写操作留痕:谁、何时、对哪个对象、结果" />
      <div style={{ display: "flex", gap: 10, marginBottom: 14 }}>
        <div style={{ position: "relative", width: 260 }}>
          <Icon name="search" size={14} style={{ position: "absolute", left: 10, top: 10, color: "var(--muted-fg)" }} />
          <input className="input" style={{ paddingLeft: 32 }} placeholder="搜索对象 / 用户 …" value={q} onChange={(e) => setQ(e.target.value)} />
        </div>
        <div style={{ width: 160 }}>
          <Select value={actF} onChange={setActF} options={[{ value: "all", label: "全部操作" }, ...acts]} />
        </div>
      </div>
      <div className="card" style={{ overflow: "hidden" }}>
        <table className="table">
          <thead><tr><th>时间</th><th>用户</th><th>操作</th><th>对象</th><th>结果</th><th>来源 IP</th></tr></thead>
          <tbody>
            {list.map((a) => (
              <tr key={a.id}>
                <td><span style={{ fontSize: 12.5, color: "var(--muted-fg)" }}>{fmtTime(a.time)}</span></td>
                <td style={{ fontSize: 12.5, fontWeight: 600 }}>{a.user}</td>
                <td><Badge tone={a.action === "部署" ? "primary" : a.action.includes("删除") ? "error" : a.action === "回滚" || a.action.includes("还原") ? "warn" : "default"}>{a.action}</Badge></td>
                <td style={{ fontSize: 12.5 }}>{a.target}</td>
                <td><Badge tone={a.result.includes("失败") ? "error" : "success"} dot>{a.result}</Badge></td>
                <td><span className="mono" style={{ fontSize: 12, color: "var(--muted-fg)" }}>{a.ip}</span></td>
              </tr>
            ))}
          </tbody>
        </table>
        {list.length === 0 ? <EmptyState icon="shield" title="没有匹配的审计记录" /> : null}
      </div>
    </div>
  );
}

export { OverviewPage, CabinetPage, AuditPage };
