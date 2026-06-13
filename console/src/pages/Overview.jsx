// Mooncell — 总览(系统监控)/ 文件柜 / 审计日志
import React from 'react';
import { useMC, AGENT, genSeries, timeAgo, fmtTime, tsDir, MC_NOW, MC_DAY } from '../lib/data.js';
import { Btn, Icon, Badge, Progress, Sparkline, Seg, Switch, CopyChip, EmptyState, Select, toast } from '../components/primitives.jsx';
import { PageHead } from '../components/Shell.jsx';

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

function OverviewPage() {
  const store = useMC();
  const [cpuS, setCpuS] = React.useState(() => genSeries(AGENT.cpu, 9));
  const [memS, setMemS] = React.useState(() => genSeries(AGENT.mem, 4));
  const [testing, setTesting] = React.useState(false);
  React.useEffect(() => {
    const iv = setInterval(() => {
      setCpuS((s) => [...s.slice(1), Math.max(3, Math.min(95, s[s.length - 1] + (Math.random() - 0.5) * 14)) | 0]);
      setMemS((s) => [...s.slice(1), Math.max(20, Math.min(92, s[s.length - 1] + (Math.random() - 0.5) * 5)) | 0]);
    }, 2000);
    return () => clearInterval(iv);
  }, []);
  const cpu = cpuS[cpuS.length - 1], mem = memS[memS.length - 1];
  const d = AGENT.diskDetail;
  const failedApps = store.apps.filter((a) => a.status === "failed");

  const testConn = () => {
    setTesting(true);
    setTimeout(() => { setTesting(false); toast("Agent 连通性正常 · 延迟 2ms · token 校验通过"); }, 1100);
  };

  return (
    <div>
      <PageHead title="总览 Overview" desc={`${AGENT.name} · ${AGENT.host} · ${AGENT.os}`}
        actions={<Btn icon="zap" disabled={testing} onClick={testConn}>{testing ? "测试中…" : "连通性测试"}</Btn>} />

      <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr 1fr", gap: 14, marginBottom: 14 }}>
        <StatCard label="CPU" value={cpu} unit="%" series={cpuS} color="var(--info)" />
        <StatCard label="内存" value={mem} unit="%" series={memS} color="var(--cyan)"
          extra={<div style={{ fontSize: 11.5, color: "var(--muted-fg)" }}>15.8 GB / 32 GB</div>} />
        <div className="card card-pad" style={{ display: "flex", flexDirection: "column", gap: 8 }}>
          <div style={{ display: "flex", alignItems: "center" }}>
            <span style={{ fontSize: 12.5, color: "var(--muted-fg)", fontWeight: 500 }}>磁盘水位</span>
            {AGENT.disk > 85 ? <Badge tone="error" style={{ marginLeft: "auto" }}>禁止部署</Badge> :
              AGENT.disk > 70 ? <Badge tone="warn" style={{ marginLeft: "auto" }}>接近阈值</Badge> : null}
          </div>
          <div className="stat-num">{AGENT.disk}<span style={{ fontSize: 14, color: "var(--muted-fg)", fontWeight: 500 }}> % · {d.used}/{d.total} GB</span></div>
          <Progress value={AGENT.disk} height={7} color={AGENT.disk > 85 ? "var(--error)" : AGENT.disk > 70 ? "var(--warn)" : "var(--success)"} />
          <div style={{ display: "flex", gap: 12, fontSize: 11.5, color: "var(--muted-fg)", whiteSpace: "nowrap" }}>
            <span>备份 {d.backups} GB</span><span>文件柜 {d.cabinet} GB</span><span>日志 {d.logs} GB</span>
          </div>
        </div>
      </div>

      <div style={{ display: "grid", gridTemplateColumns: "5fr 4fr", gap: 14 }}>
        <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
          {/* Agent 能力清单 */}
          <div className="card card-pad">
            <div style={{ display: "flex", alignItems: "center", marginBottom: 12 }}>
              <h4 style={{ fontSize: 13.5, display: "flex", alignItems: "center", gap: 7 }}><Icon name="server" size={14} style={{ color: "var(--primary)" }} />Agent 能力清单</h4>
              <span style={{ marginLeft: "auto", fontSize: 11.5, color: "var(--muted-fg)" }}>启动自检上报 · 前端按能力过滤可选 Runner</span>
            </div>
            <div style={{ display: "grid", gridTemplateColumns: "repeat(4, 1fr)", gap: 8 }}>
              {AGENT.caps.map((c) => (
                <div key={c.key} className="card" style={{ padding: "9px 12px", boxShadow: "none", background: c.ok ? "var(--bg)" : "var(--muted)", opacity: c.ok ? 1 : .6 }}>
                  <div style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 12.5, fontWeight: 600 }}>
                    <Icon name={c.ok ? "check" : "x"} size={12} style={{ color: c.ok ? "var(--success)" : "var(--muted-fg)" }} />{c.label}
                  </div>
                  <div className="mono" style={{ fontSize: 10.5, color: "var(--muted-fg)", marginTop: 2 }}>{c.ver}</div>
                </div>
              ))}
            </div>
            <div style={{ display: "grid", gridTemplateColumns: "auto 1fr", gap: "6px 16px", fontSize: 12.5, marginTop: 14, color: "var(--fg-secondary)" }}>
              <span style={{ color: "var(--muted-fg)" }}>运行时长</span><span className="mono" style={{ fontSize: 12 }}>{AGENT.uptime}</span>
              <span style={{ color: "var(--muted-fg)" }}>Token</span><span className="mono" style={{ fontSize: 12 }}>{AGENT.token}</span>
              <span style={{ color: "var(--muted-fg)" }}>安全边界</span><span>仅类型化 API · 无任意 shell 接口 · 钩子白名单</span>
            </div>
          </div>

          {/* 应用健康 */}
          <div className="card card-pad">
            <h4 style={{ fontSize: 13.5, marginBottom: 12, display: "flex", alignItems: "center", gap: 7 }}><Icon name="box" size={14} style={{ color: "var(--primary)" }} />应用健康</h4>
            <div style={{ display: "flex", gap: 18, marginBottom: failedApps.length ? 12 : 0 }}>
              {[["运行中", store.apps.filter((a) => a.status === "running" || a.status === "static").length, "var(--success)"],
                ["异常", failedApps.length, "var(--error)"],
                ["已停止", store.apps.filter((a) => a.status === "stopped").length, "var(--muted-fg)"]].map(([l, n, c]) => (
                <div key={l}>
                  <div className="stat-num" style={{ fontSize: 22, color: c }}>{n}</div>
                  <div style={{ fontSize: 11.5, color: "var(--muted-fg)" }}>{l}</div>
                </div>
              ))}
            </div>
            {failedApps.map((a) => (
              <button key={a.id} className="card" onClick={() => store.nav("app-detail", { appId: a.id })} style={{
                width: "100%", padding: "10px 13px", display: "flex", alignItems: "center", gap: 10, cursor: "pointer",
                font: "inherit", textAlign: "left", background: "var(--error-soft)", borderColor: "transparent", boxShadow: "none",
              }}>
                <Icon name="alert" size={14} style={{ color: "var(--error)" }} />
                <span style={{ fontSize: 12.5, fontWeight: 600, color: "var(--error)" }}>{a.name}</span>
                <span style={{ fontSize: 11.5, color: "var(--error)", opacity: .8 }}>健康检查失败 · 已自动回滚</span>
                <Icon name="chevronR" size={13} style={{ color: "var(--error)", marginLeft: "auto" }} />
              </button>
            ))}
          </div>
        </div>

        {/* 最近动态 */}
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
function CabinetPage() {
  const store = useMC();
  const [mode, setMode] = React.useState("login");
  const [code, setCode] = React.useState("");
  const [found, setFound] = React.useState(null);
  const [uploading, setUploading] = React.useState(false);
  const [prog, setProg] = React.useState(0);

  const simUpload = (name, size) => {
    setUploading(true); setProg(0);
    const iv = setInterval(() => {
      setProg((p) => {
        if (p >= 100) {
          clearInterval(iv); setUploading(false);
          store.addCabinetFile(name, size, mode === "anon");
          return 100;
        }
        return p + 8 + Math.random() * 14;
      });
    }, 110);
  };

  const lookup = () => {
    const f = store.cabinet.find((x) => x.code.toLowerCase() === code.trim().toLowerCase());
    if (f) setFound(f);
    else { setFound(null); toast("提取码不存在或文件已过期", { tone: "error", icon: "alert" }); }
  };

  const zone = (
    <div className="upload-zone" onClick={() => !uploading && simUpload(
      ["巡检报告-" + tsDir(Date.now()) + ".pdf", "hotfix-补丁包.tar.gz", "现场日志导出.zip"][Math.random() * 3 | 0],
      (1 + Math.random() * 80).toFixed(1) + " MB")}>
      {uploading ? (
        <div style={{ maxWidth: 320, margin: "0 auto" }}>
          <div style={{ fontSize: 13, fontWeight: 600, marginBottom: 8 }}>上传中 · {Math.min(100, prog | 0)}%</div>
          <Progress value={prog} height={6} />
        </div>
      ) : (
        <React.Fragment>
          <Icon name="upload" size={20} style={{ color: "var(--muted-fg)" }} />
          <div style={{ fontWeight: 600, marginTop: 7, fontSize: 13.5 }}>拖拽文件到此处上传(点击演示)</div>
          <div style={{ fontSize: 11.5, color: "var(--muted-fg)", marginTop: 3 }}>
            单文件 ≤ 2 GB · 默认 7 天后自动清理 · 上传后获得提取码 + 直链
          </div>
        </React.Fragment>
      )}
    </div>
  );

  return (
    <div>
      <PageHead title="文件柜 Cabinet" desc="内网临时文件中转 · 二进制存储 · 下载强制 attachment"
        actions={<Seg value={mode} onChange={(v) => { setMode(v); setFound(null); }} options={[{ value: "login", label: "登录视图" }, { value: "anon", label: "匿名视图(演示)" }]} />} />

      {mode === "anon" ? (
        <div style={{ maxWidth: 560, margin: "30px auto 0", display: "flex", flexDirection: "column", gap: 14 }}>
          {zone}
          <div className="card card-pad">
            <div style={{ fontSize: 13, fontWeight: 600, marginBottom: 10 }}>凭提取码下载</div>
            <div style={{ display: "flex", gap: 8 }}>
              <input className="input mono" style={{ textTransform: "uppercase", letterSpacing: ".15em" }} placeholder="提取码,如 M3QP"
                value={code} onChange={(e) => setCode(e.target.value)} onKeyDown={(e) => e.key === "Enter" && lookup()} />
              <Btn variant="primary" onClick={lookup}>提取</Btn>
            </div>
            {found ? (
              <div className="card fade-up" style={{ marginTop: 12, padding: 13, display: "flex", alignItems: "center", gap: 10, background: "var(--bg)" }}>
                <Icon name="fileText" size={17} style={{ color: "var(--primary)" }} />
                <div style={{ flex: 1, minWidth: 0 }}>
                  <div className="mono" style={{ fontSize: 12.5, fontWeight: 600 }}>{found.name}</div>
                  <div style={{ fontSize: 11.5, color: "var(--muted-fg)" }}>{found.size} · {timeAgo(found.expires)}过期</div>
                </div>
                <Btn size="sm" variant="primary" icon="download" onClick={() => toast("开始下载(模拟)· Content-Disposition: attachment")}>下载</Btn>
              </div>
            ) : null}
            <div style={{ fontSize: 11.5, color: "var(--muted-fg)", marginTop: 10 }}>匿名用户只能凭提取码访问自己的文件,看不到完整列表。</div>
          </div>
          {store.cabinet.filter((f) => f.public).length > 0 ? (
            <div className="card" style={{ overflow: "hidden" }}>
              <div style={{ padding: "12px 16px 0", fontSize: 13, fontWeight: 600 }}>公开文件</div>
              <table className="table">
                <tbody>
                  {store.cabinet.filter((f) => f.public).map((f) => (
                    <tr key={f.id}>
                      <td><span className="mono" style={{ fontSize: 12 }}>{f.name}</span></td>
                      <td style={{ width: 90 }}><span className="mono" style={{ fontSize: 11.5, color: "var(--muted-fg)" }}>{f.size}</span></td>
                      <td style={{ width: 80, textAlign: "right" }}><Btn size="sm" variant="ghost" icon="download" onClick={() => toast("开始下载(模拟)")}></Btn></td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : null}
        </div>
      ) : (
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
                    <td><Switch on={f.public} onChange={() => store.toggleCabinetPublic(f)} /></td>
                    <td><span className="mono" style={{ fontSize: 12, color: "var(--muted-fg)" }}>{f.downloads}</span></td>
                    <td>
                      <div style={{ display: "flex", gap: 4, justifyContent: "flex-end" }}>
                        <Btn size="sm" variant="ghost" icon="download" title="下载" onClick={() => toast("开始下载(模拟)")}></Btn>
                        <Btn size="sm" variant="ghost" icon="link" title="复制直链" onClick={() => toast("直链已复制: http://" + AGENT.host.split(":")[0] + "/cabinet/" + f.code)}></Btn>
                        <Btn size="sm" variant="ghost" icon="trash" title="删除" onClick={() => store.deleteCabinetFile(f)}></Btn>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            {store.cabinet.length === 0 ? <EmptyState icon="folder" title="文件柜是空的" /> : null}
          </div>
        </div>
      )}
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
