// Mooncell — Agent 管理(仅 admin):注册 / 删除 / 连通性测试 / 自更新(按架构推送升级包)
import React from 'react';
import { useMC, fmtTime } from '../lib/data.js';
import { Btn, Field, Icon, Badge, Spinner, EmptyState, Dialog, Select, toast } from '../components/primitives.jsx';
import { PageHead } from '../components/Shell.jsx';
import {
  listAgentNodes, addAgentNode, removeAgentNode, pingAgentNode,
  listAgentBinaries, uploadAgentBinary, updateAgentNode,
} from '../lib/api.js';

// archOf "linux/amd64" → "amd64"(仅放开 linux x86/arm)
const archOf = (os) => {
  const m = /^linux\/(amd64|arm64)$/.exec(os || "");
  return m ? m[1] : "";
};

function AgentsPage() {
  const store = useMC();
  const [agents, setAgents] = React.useState(null);
  const [open, setOpen] = React.useState(false);
  const [info, setInfo] = React.useState({});   // id -> {ok, version, os}
  const [bins, setBins] = React.useState([]);    // [{arch, version, ...}]
  const [updating, setUpdating] = React.useState({}); // id -> bool

  const binByArch = React.useMemo(() => Object.fromEntries(bins.map((b) => [b.arch, b])), [bins]);

  const reloadBins = React.useCallback(() => { listAgentBinaries().then(setBins); }, []);

  // 拉 agent 列表后,对每台并发探测一次,拿到在线/版本/架构。
  const reload = React.useCallback(() => {
    listAgentNodes().then((list) => {
      const arr = list || [];
      setAgents(arr);
      arr.forEach((a) => pingAgentNode(a.id).then((res) => {
        setInfo((m) => ({ ...m, [a.id]: res && res.ok ? { ok: true, version: res.version, os: res.os } : { ok: false } }));
      }));
    });
  }, []);
  React.useEffect(() => { reload(); reloadBins(); }, [reload, reloadBins]);

  if (!store.can("admin")) {
    return <EmptyState icon="shield" title="无权访问" desc="Agent 管理仅管理员可见" />;
  }

  const onDelete = async (a) => {
    try { await removeAgentNode(a.id); toast(`已移除 Agent ${a.name}`); reload(); }
    catch (e) { toast(e.message || "删除失败", { tone: "error" }); }
  };

  // 推送升级包到某 Agent → 自更新(self-exec 重启)。完成后稍等再探测,刷新版本。
  const doUpdate = async (a) => {
    const inf = info[a.id] || {};
    const arch = archOf(inf.os);
    const target = arch && binByArch[arch];
    if (!target) { toast(`未找到 linux/${arch || "?"} 的升级包,请先上传`, { tone: "warn" }); return; }
    if (!confirm(`将把 ${a.name} 从 ${inf.version || "?"} 更新到 ${target.version}(linux/${arch}),Agent 会就地重启。继续?`)) return;
    setUpdating((m) => ({ ...m, [a.id]: true }));
    try {
      const r = await updateAgentNode(a.id);
      toast(`${a.name} 已更新 → ${r.version || target.version}(重启中)`);
      setTimeout(() => pingAgentNode(a.id).then((res) => {
        setInfo((m) => ({ ...m, [a.id]: res && res.ok ? { ok: true, version: res.version, os: res.os } : { ok: false } }));
      }), 2500);
    } catch (e) {
      toast(e.message || "更新失败", { tone: "error" });
    } finally {
      setUpdating((m) => ({ ...m, [a.id]: false }));
    }
  };

  return (
    <div>
      <PageHead title="Agent 管理" desc="Console 可管多台 Agent · 应用部署时按目标机路由 · 支持按架构统一推送自更新"
        actions={<Btn variant="primary" icon="plus" onClick={() => setOpen(true)}>注册 Agent</Btn>} />

      <div className="card" style={{ overflow: "hidden", marginBottom: 14 }}>
        <table className="table">
          <thead><tr><th>名称</th><th>地址</th><th>类型</th><th>状态</th><th>版本</th><th style={{ width: 150 }}></th></tr></thead>
          <tbody>
            {(agents || []).map((a) => {
              const inf = info[a.id];
              const arch = inf && inf.ok ? archOf(inf.os) : "";
              const target = arch ? binByArch[arch] : null;
              const canUpdate = inf && inf.ok && target;
              const isLatest = canUpdate && target.version === inf.version;
              return (
                <tr key={a.id}>
                  <td><span style={{ fontWeight: 600 }}>{a.name}</span></td>
                  <td><span className="mono" style={{ fontSize: 12 }}>{a.addr}</span></td>
                  <td>{a.id === "default" ? <Badge tone="info">内置默认</Badge> : <Badge tone="default">远端</Badge>}</td>
                  <td>
                    {!inf ? <Spinner size={12} /> :
                      inf.ok ? <Badge tone="success" dot>在线{inf.os ? " · " + inf.os : ""}</Badge> :
                        <Badge tone="error" dot>不可达</Badge>}
                  </td>
                  <td>
                    <span className="mono" style={{ fontSize: 12 }}>{inf && inf.ok ? inf.version : "—"}</span>
                    {isLatest ? <Badge tone="success" style={{ marginLeft: 6 }}>最新</Badge> :
                      canUpdate ? <Badge tone="warn" style={{ marginLeft: 6 }}>可更新 {target.version}</Badge> : null}
                  </td>
                  <td>
                    <div style={{ display: "flex", gap: 6, justifyContent: "flex-end" }}>
                      <Btn size="sm" variant="ghost" icon="zap" title="连通性测试"
                        onClick={() => { setInfo((m) => ({ ...m, [a.id]: undefined })); pingAgentNode(a.id).then((res) => setInfo((m) => ({ ...m, [a.id]: res && res.ok ? { ok: true, version: res.version, os: res.os } : { ok: false } }))); }}></Btn>
                      <Btn size="sm" variant={isLatest ? "ghost" : "primary"} icon="rotate" disabled={!canUpdate || updating[a.id] || isLatest}
                        title={!inf || !inf.ok ? "Agent 不可达" : !target ? `未上传 linux/${arch || "?"} 升级包` : isLatest ? "已是最新" : `更新到 ${target.version}`}
                        onClick={() => doUpdate(a)}>{updating[a.id] ? <Spinner size={12} /> : "更新"}</Btn>
                      {a.id === "default" ? null : <Btn size="sm" variant="ghost" icon="trash" title="移除" onClick={() => onDelete(a)}></Btn>}
                    </div>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
        {agents === null ? <div style={{ padding: 24, textAlign: "center" }}><Spinner size={16} /></div> : null}
      </div>

      <AgentBinariesCard bins={bins} onChanged={reloadBins} />

      <AddAgentDialog open={open} onClose={() => setOpen(false)} onAdded={() => { setOpen(false); reload(); }} />
    </div>
  );
}

// Agent 升级包:按架构上传一次,供推送给同架构的各 Agent。
function AgentBinariesCard({ bins, onChanged }) {
  const [arch, setArch] = React.useState("amd64");
  const [version, setVersion] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const fileRef = React.useRef(null);
  const byArch = Object.fromEntries(bins.map((b) => [b.arch, b]));

  const submit = async () => {
    const file = fileRef.current && fileRef.current.files[0];
    if (!file) { toast("请选择 agent 二进制文件", { tone: "warn" }); return; }
    if (!version.trim()) { toast("请填版本号", { tone: "warn" }); return; }
    setBusy(true);
    try {
      await uploadAgentBinary(file, arch, version.trim());
      toast(`已上传 linux/${arch} ${version.trim()}`);
      setVersion(""); if (fileRef.current) fileRef.current.value = "";
      onChanged();
    } catch (e) {
      toast(e.message || "上传失败", { tone: "error" });
    } finally { setBusy(false); }
  };

  return (
    <div className="card card-pad">
      <h4 style={{ fontSize: 13.5, marginBottom: 4, display: "flex", alignItems: "center", gap: 7 }}><Icon name="server" size={14} style={{ color: "var(--primary)" }} />Agent 升级包</h4>
      <div style={{ fontSize: 12, color: "var(--muted-fg)", marginBottom: 12 }}>按架构上传 agent 二进制(linux amd64 / arm64),上方各 Agent 的「更新」会推送匹配架构的最新包并自更新重启。</div>

      <div style={{ display: "flex", gap: 16, marginBottom: 14, flexWrap: "wrap" }}>
        {["amd64", "arm64"].map((ar) => (
          <div key={ar} className="card" style={{ padding: "10px 14px", boxShadow: "none", background: "var(--bg)", minWidth: 200 }}>
            <div style={{ fontSize: 12.5, fontWeight: 600 }}>linux/{ar}</div>
            {byArch[ar]
              ? <div style={{ fontSize: 12, color: "var(--muted-fg)", marginTop: 3 }}>
                  <span className="mono" style={{ color: "var(--fg)" }}>{byArch[ar].version}</span> · {(byArch[ar].size / 1048576).toFixed(1)} MB · {fmtTime(byArch[ar].time)}
                </div>
              : <div style={{ fontSize: 12, color: "var(--muted-fg)", marginTop: 3 }}>未上传</div>}
          </div>
        ))}
      </div>

      <div style={{ display: "flex", gap: 10, alignItems: "flex-end", flexWrap: "wrap" }}>
        <div style={{ width: 130 }}><Field label="架构"><Select value={arch} onChange={setArch} options={[{ value: "amd64", label: "linux/amd64" }, { value: "arm64", label: "linux/arm64" }]} /></Field></div>
        <div style={{ width: 150 }}><Field label="版本号"><input className="input mono" value={version} onChange={(e) => setVersion(e.target.value)} placeholder="v0.2.0" /></Field></div>
        <div><Field label="二进制文件"><input ref={fileRef} type="file" className="input" style={{ fontSize: 12, paddingTop: 6 }} /></Field></div>
        <Btn variant="primary" icon="upload" disabled={busy} onClick={submit}>{busy ? <Spinner size={12} /> : "上传"}</Btn>
      </div>
      <div style={{ fontSize: 11.5, color: "var(--muted-fg)", marginTop: 10 }}>
        上传时校验确为该架构的 linux ELF;推送后 Agent 端再次校验 sha256 + 架构 + 自检,任一不过即保持旧版(无损)。纯 nohup 模式下 Agent 用 self-exec 同 PID 重启。
      </div>
    </div>
  );
}

function AddAgentDialog({ open, onClose, onAdded }) {
  const [name, setName] = React.useState("");
  const [addr, setAddr] = React.useState("");
  const [token, setToken] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  React.useEffect(() => { if (open) { setName(""); setAddr(""); setToken(""); setBusy(false); } }, [open]);

  const submit = async () => {
    if (!name.trim() || !addr.trim() || !token) { toast("名称/地址/token 不能为空", { tone: "warn" }); return; }
    setBusy(true);
    try {
      await addAgentNode({ name: name.trim(), addr: addr.trim(), token });
      toast(`已注册 Agent ${name.trim()}`);
      onAdded();
    } catch (e) {
      toast(e.message || "注册失败", { tone: "error" });
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onClose={onClose} width={480} title="注册远端 Agent" desc="录入目标机 Agent 的地址与共享 token"
      foot={<React.Fragment>
        <Btn variant="ghost" onClick={onClose}>取消</Btn>
        <Btn variant="primary" icon="check" disabled={busy} onClick={submit}>{busy ? <Spinner size={12} /> : "注册"}</Btn>
      </React.Fragment>}>
      <div style={{ display: "flex", flexDirection: "column", gap: 13 }}>
        <Field label="名称"><input className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder="如 机房B-应用服务器" /></Field>
        <Field label="地址(host:port)"><input className="input mono" value={addr} onChange={(e) => setAddr(e.target.value)} placeholder="10.0.2.15:9100" /></Field>
        <Field label="共享 token"><input className="input mono" type="password" value={token} onChange={(e) => setToken(e.target.value)} placeholder="须与该 Agent config.toml 的 token 一致" /></Field>
        <div style={{ display: "flex", gap: 9, padding: "10px 13px", borderRadius: 9, fontSize: 12.5, background: "var(--info-soft)", color: "var(--info)" }}>
          <Icon name="server" size={15} style={{ flex: "none", marginTop: 1 }} />
          <span>注册后可在新建应用时选择该 Agent 作为部署目标。</span>
        </div>
      </div>
    </Dialog>
  );
}

export { AgentsPage };
