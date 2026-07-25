// Mooncell — 用户管理(仅 admin):创建普通用户(用户名/密码/授权应用)、列表、改授权、删除
import React from 'react';
import { useMC, fmtTime } from '../lib/data.js';
import { Btn, Field, Icon, Badge, Spinner, EmptyState, Dialog, toast, Checkbox } from '../components/primitives.jsx';
import { PageHead } from '../components/Shell.jsx';
import { useAsync } from '../lib/async.js';
import { listUsers, createUser, updateUser, deleteUser } from '../lib/api.js';
import { listDataResources } from '../lib/dataresource-api.js';

function UsersPage() {
  const store = useMC();
  const [open, setOpen] = React.useState(false);
  const [editUser, setEditUser] = React.useState(null);
  const { data: users, error, loading, retry } = useAsync(listUsers, []);

  if (!store.can("admin")) {
    return <EmptyState icon="shield" title="无权访问" desc="用户管理仅管理员可见" />;
  }

  const onDelete = async (u) => {
    if (u.username === store.user) { toast("不能删除当前登录账号", { tone: "warn" }); return; }
    try {
      await deleteUser(u.username);
      toast(`已删除用户 ${u.username}`);
      retry();
    } catch (e) {
      toast(e.message || "删除失败", { tone: "error" });
    }
  };

  const appName = (id) => {
    const a = (store.apps || []).find((x) => x.id === id);
    return a ? a.name : id;
  };

  return (
    <div>
      <PageHead title="用户管理" desc="创建用户并授权可访问的应用 · 普通用户仅能部署与查看授权应用"
        actions={<Btn variant="primary" icon="plus" onClick={() => setOpen(true)}>新建用户</Btn>} />

      <div className="card" style={{ overflow: "hidden" }}>
        <table className="table">
          <thead><tr><th>用户名</th><th>角色</th><th>授权应用</th><th>数据资源</th><th>创建时间</th><th style={{ width: 120 }}></th></tr></thead>
          <tbody>
            {(users || []).map((u) => (
              <tr key={u.username}>
                <td>
                  <span style={{ fontWeight: 600 }}>{u.username}</span>
                  {u.username === store.user ? <span style={{ fontSize: 11, color: "var(--muted-fg)", marginLeft: 6 }}>(我)</span> : null}
                </td>
                <td><Badge tone={u.role === "admin" ? "error" : "info"}>{u.role === "admin" ? "管理员" : "用户"}</Badge></td>
                <td>
                  {u.role === "admin" ? (
                    <span style={{ fontSize: 12.5, color: "var(--muted-fg)" }}>全部应用</span>
                  ) : (u.appIds || []).length === 0 ? (
                    <span style={{ fontSize: 12.5, color: "var(--muted-fg)" }}>未授权</span>
                  ) : (
                    <div style={{ display: "flex", flexWrap: "wrap", gap: 4 }}>
                      {(u.appIds || []).map((id) => (
                        <span key={id} className="code-chip" style={{ fontSize: 11 }} title={id}>{appName(id)}</span>
                      ))}
                    </div>
                  )}
                </td>
                <td>
                  {u.role === "admin" ? (
                    <span style={{ fontSize: 12.5, color: "var(--muted-fg)" }}>全部资源</span>
                  ) : !(u.dataResourceGrants || []).length ? (
                    <span style={{ fontSize: 12.5, color: "var(--muted-fg)" }}>未授权</span>
                  ) : (
                    <span style={{ fontSize: 12.5 }}>{(u.dataResourceGrants || []).length} 项</span>
                  )}
                </td>
                <td><span style={{ fontSize: 12.5, color: "var(--muted-fg)" }}>{fmtTime(u.createdAt)}</span></td>
                <td>
                  <div style={{ display: "flex", gap: 4, justifyContent: "flex-end" }}>
                    {u.role !== "admin" ? (
                      <Btn size="sm" variant="ghost" icon="settings" title="编辑授权" onClick={() => setEditUser(u)}></Btn>
                    ) : null}
                    <Btn size="sm" variant="ghost" icon="trash" title="删除用户"
                      disabled={u.username === store.user}
                      onClick={() => onDelete(u)}></Btn>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {loading ? <div style={{ padding: 24, textAlign: "center" }}><Spinner size={16} /></div> : null}
        {!loading && error ? (
          <EmptyState icon="alert" title="加载用户列表失败" desc={error.message || "请稍后重试"}
            action={<Btn variant="primary" icon="rotate" onClick={retry}>重试</Btn>} />
        ) : null}
        {!loading && !error && users && users.length === 0 ? <EmptyState icon="user" title="暂无用户" /> : null}
      </div>

      <CreateUserDialog open={open} apps={store.apps || []} onClose={() => setOpen(false)}
        onCreated={() => { setOpen(false); retry(); }} />
      <EditUserDialog user={editUser} apps={store.apps || []} open={!!editUser}
        onClose={() => setEditUser(null)} onSaved={() => { setEditUser(null); retry(); }} />
    </div>
  );
}

function AppPicker({ apps, selected, onChange }) {
  if (!apps.length) {
    return <div style={{ fontSize: 12.5, color: "var(--muted-fg)" }}>暂无应用可授权,请先在「应用」中创建</div>;
  }
  const toggle = (id) => {
    const set = new Set(selected);
    set.has(id) ? set.delete(id) : set.add(id);
    onChange([...set]);
  };
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6, maxHeight: 220, overflowY: "auto",
      border: "1px solid var(--border)", borderRadius: 8, padding: "8px 10px" }}>
      {apps.map((a) => (
        <label key={a.id} style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 13, cursor: "pointer" }}>
          <Checkbox checked={selected.includes(a.id)} onChange={() => toggle(a.id)} ariaLabel={a.name} />
          <span style={{ fontWeight: 500 }}>{a.name}</span>
          <span className="mono" style={{ fontSize: 11, color: "var(--muted-fg)" }}>{a.id}</span>
        </label>
      ))}
    </div>
  );
}

function DataResourceGrantPicker({ resources, grants, onChange }) {
  const byId = Object.fromEntries((grants || []).map((g) => [g.resourceId, g.accessMode]));
  const setMode = (resourceId, mode) => {
    const next = (grants || []).filter((g) => g.resourceId !== resourceId);
    if (mode) next.push({ resourceId, accessMode: mode });
    onChange(next);
  };
  if (!resources.length) {
    return <div style={{ fontSize: 12.5, color: "var(--muted-fg)" }}>暂无数据资源，请先在「数据资源」中创建</div>;
  }
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6, maxHeight: 220, overflowY: "auto",
      border: "1px solid var(--border)", borderRadius: 8, padding: "8px 10px" }}>
      {resources.map((r) => (
        <div key={r.id} style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 13 }}>
          <span style={{ flex: 1, fontWeight: 500, overflow: "hidden", textOverflow: "ellipsis" }}>{r.name}</span>
          <select className="input" style={{ width: 110, padding: "4px 8px" }}
            value={byId[r.id] || ""}
            onChange={(e) => setMode(r.id, e.target.value)}>
            <option value="">无权限</option>
            <option value="read">只读</option>
            <option value="write">读写</option>
          </select>
        </div>
      ))}
    </div>
  );
}

function CreateUserDialog({ open, onClose, onCreated, apps }) {
  const [u, setU] = React.useState("");
  const [p, setP] = React.useState("");
  const [appIds, setAppIds] = React.useState([]);
  const [drGrants, setDrGrants] = React.useState([]);
  const [resources, setResources] = React.useState([]);
  const [busy, setBusy] = React.useState(false);
  React.useEffect(() => {
    if (open) {
      setU(""); setP(""); setAppIds([]); setDrGrants([]); setBusy(false);
      listDataResources().then(setResources).catch(() => setResources([]));
    }
  }, [open]);

  const submit = async () => {
    if (!u.trim() || !p) { toast("用户名与密码不能为空", { tone: "warn" }); return; }
    setBusy(true);
    try {
      await createUser({
        username: u.trim(), password: p, appIds,
        dataResourceGrants: drGrants.map((g) => ({ resourceId: g.resourceId, accessMode: g.accessMode })),
      });
      toast(`已创建用户 ${u.trim()}`);
      onCreated();
    } catch (e) {
      toast(e.message || "创建失败", { tone: "error" });
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onClose={onClose} width={520} title="新建用户" desc="普通用户仅能部署与查看授权的应用,并访问已授权数据资源"
      foot={<React.Fragment>
        <Btn variant="ghost" onClick={onClose}>取消</Btn>
        <Btn variant="primary" icon="check" disabled={busy} onClick={submit}>{busy ? <Spinner size={12} /> : "创建"}</Btn>
      </React.Fragment>}>
      <div style={{ display: "flex", flexDirection: "column", gap: 13 }}>
        <Field label="用户名"><input className="input" value={u} onChange={(e) => setU(e.target.value)} placeholder="如 ops-zhang" autoComplete="off" /></Field>
        <Field label="初始密码"><input className="input" type="password" value={p} onChange={(e) => setP(e.target.value)} placeholder="登录密码" autoComplete="new-password" /></Field>
        <Field label="授权访问的应用" hint="可多选;未选则登录后看不到任何应用">
          <AppPicker apps={apps} selected={appIds} onChange={setAppIds} />
        </Field>
        <Field label="数据资源授权" hint="只读须该资源最近测试通过只读事务认证。PostgreSQL/Kingbase 可见范围为整个库的所有 schema，实际权限由数据库账号决定；推荐资源使用只读账号">
          <DataResourceGrantPicker resources={resources} grants={drGrants} onChange={setDrGrants} />
        </Field>
      </div>
    </Dialog>
  );
}

function EditUserDialog({ user, open, onClose, onSaved, apps }) {
  const [p, setP] = React.useState("");
  const [appIds, setAppIds] = React.useState([]);
  const [drGrants, setDrGrants] = React.useState([]);
  const [resources, setResources] = React.useState([]);
  const [busy, setBusy] = React.useState(false);
  React.useEffect(() => {
    if (open && user) {
      setP("");
      setAppIds([...(user.appIds || [])]);
      setDrGrants([...(user.dataResourceGrants || [])].map((g) => ({
        resourceId: g.resourceId, accessMode: g.accessMode,
      })));
      setBusy(false);
      listDataResources().then(setResources).catch(() => setResources([]));
    }
  }, [open, user]);

  const submit = async () => {
    setBusy(true);
    try {
      const payload = {
        appIds,
        dataResourceGrants: drGrants.map((g) => ({ resourceId: g.resourceId, accessMode: g.accessMode })),
      };
      if (p.trim()) payload.password = p.trim();
      await updateUser(user.username, payload);
      toast(`已更新用户 ${user.username}`);
      onSaved();
    } catch (e) {
      toast(e.message || "更新失败", { tone: "error" });
      setBusy(false);
    }
  };

  if (!user) return null;
  return (
    <Dialog open={open} onClose={onClose} width={520} title={`编辑 · ${user.username}`} desc="可重置密码并调整应用与数据资源授权"
      foot={<React.Fragment>
        <Btn variant="ghost" onClick={onClose}>取消</Btn>
        <Btn variant="primary" icon="check" disabled={busy} onClick={submit}>{busy ? <Spinner size={12} /> : "保存"}</Btn>
      </React.Fragment>}>
      <div style={{ display: "flex", flexDirection: "column", gap: 13 }}>
        <Field label="新密码(留空不改)"><input className="input" type="password" value={p} onChange={(e) => setP(e.target.value)} placeholder="不修改请留空" autoComplete="new-password" /></Field>
        <Field label="授权访问的应用">
          <AppPicker apps={apps} selected={appIds} onChange={setAppIds} />
        </Field>
        <Field label="数据资源授权" hint="只读须该资源最近测试通过只读事务认证。PostgreSQL/Kingbase 可见范围为整个库的所有 schema，实际权限由数据库账号决定；推荐资源使用只读账号">
          <DataResourceGrantPicker resources={resources} grants={drGrants} onChange={setDrGrants} />
        </Field>
      </div>
    </Dialog>
  );
}

export { UsersPage };
