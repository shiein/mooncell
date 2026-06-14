// Mooncell — Agent 管理(仅 admin):注册 / 删除 / 连通性测试远端 Agent
import React from 'react';
import { useMC, fmtTime } from '../lib/data.js';
import { Btn, Field, Icon, Badge, Spinner, EmptyState, Dialog, toast } from '../components/primitives.jsx';
import { PageHead } from '../components/Shell.jsx';
import { listAgentNodes, addAgentNode, removeAgentNode, pingAgentNode } from '../lib/api.js';

function AgentsPage() {
  const store = useMC();
  const [agents, setAgents] = React.useState(null);
  const [open, setOpen] = React.useState(false);
  const [pings, setPings] = React.useState({}); // id -> 'ok'|'fail'|'...'

  const reload = React.useCallback(() => {
    listAgentNodes().then((a) => setAgents(a || []));
  }, []);
  React.useEffect(() => { reload(); }, [reload]);

  if (!store.can("admin")) {
    return <EmptyState icon="shield" title="无权访问" desc="Agent 管理仅管理员可见" />;
  }

  const ping = async (id) => {
    setPings((p) => ({ ...p, [id]: "..." }));
    const res = await pingAgentNode(id);
    setPings((p) => ({ ...p, [id]: res && res.ok ? "ok" : "fail" }));
    toast(res && res.ok ? `Agent 在线 · ${res.os || ""} · uptime ${res.uptime || 0}s` : "Agent 不可达", { tone: res && res.ok ? "success" : "error" });
  };

  const onDelete = async (a) => {
    try { await removeAgentNode(a.id); toast(`已移除 Agent ${a.name}`); reload(); }
    catch (e) { toast(e.message || "删除失败", { tone: "error" }); }
  };

  return (
    <div>
      <PageHead title="Agent 管理" desc="Console 可管多台 Agent · 应用部署时按目标机路由"
        actions={<Btn variant="primary" icon="plus" onClick={() => setOpen(true)}>注册 Agent</Btn>} />

      <div className="card" style={{ overflow: "hidden" }}>
        <table className="table">
          <thead><tr><th>名称</th><th>地址</th><th>类型</th><th>注册时间</th><th>连通</th><th style={{ width: 100 }}></th></tr></thead>
          <tbody>
            {(agents || []).map((a) => (
              <tr key={a.id}>
                <td><span style={{ fontWeight: 600 }}>{a.name}</span></td>
                <td><span className="mono" style={{ fontSize: 12 }}>{a.addr}</span></td>
                <td>{a.id === "default" ? <Badge tone="info">内置默认</Badge> : <Badge tone="default">远端</Badge>}</td>
                <td><span style={{ fontSize: 12.5, color: "var(--muted-fg)" }}>{a.id === "default" ? "配置内置" : fmtTime(a.createdAt)}</span></td>
                <td>
                  {pings[a.id] === "ok" ? <Badge tone="success" dot>在线</Badge> :
                    pings[a.id] === "fail" ? <Badge tone="error" dot>不可达</Badge> :
                      pings[a.id] === "..." ? <Spinner size={12} /> : <span style={{ fontSize: 12, color: "var(--muted-fg)" }}>—</span>}
                </td>
                <td>
                  <div style={{ display: "flex", gap: 6, justifyContent: "flex-end" }}>
                    <Btn size="sm" variant="ghost" icon="zap" title="连通性测试" onClick={() => ping(a.id)}></Btn>
                    {a.id === "default" ? null : <Btn size="sm" variant="ghost" icon="trash" title="移除" onClick={() => onDelete(a)}></Btn>}
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {agents === null ? <div style={{ padding: 24, textAlign: "center" }}><Spinner size={16} /></div> : null}
      </div>

      <AddAgentDialog open={open} onClose={() => setOpen(false)} onAdded={() => { setOpen(false); reload(); }} />
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
