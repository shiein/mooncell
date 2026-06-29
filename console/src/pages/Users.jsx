// Mooncell — 用户管理(仅 admin):列出 / 新建 / 删除用户,角色 admin/operator/viewer
import React from 'react';
import { useMC, fmtTime } from '../lib/data.js';
import { Btn, Field, Select, Icon, Badge, Spinner, EmptyState, Dialog, toast } from '../components/primitives.jsx';
import { PageHead } from '../components/Shell.jsx';
import { useAsync } from '../lib/async.js';
import { listUsers, createUser, deleteUser } from '../lib/api.js';

const ROLE_OPTS = ["admin", "operator", "viewer"];
const ROLE_DESC = { admin: "全权 + 用户管理", operator: "部署/还原/改数据", viewer: "只读" };
const ROLE_TONE = { admin: "error", operator: "info", viewer: "default" };

function UsersPage() {
  const store = useMC();
  const [open, setOpen] = React.useState(false);
  // 四态:loading / ready / error / stale。旧实现 `setUsers(u || [])` 把失败折成空数组,
  // 页面显示"暂无用户"而错误不可见、无法重试——这是 T9 修复的核心。
  const { data: users, error, loading, retry } = useAsync(listUsers, []);

  // 后端已强制 admin;前端再挡一层,非 admin 直接提示。
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

  return (
    <div>
      <PageHead title="用户管理" desc="账号与角色权限 · 后端按角色强制鉴权"
        actions={<Btn variant="primary" icon="plus" onClick={() => setOpen(true)}>新建用户</Btn>} />

      <div className="card" style={{ overflow: "hidden" }}>
        <table className="table">
          <thead><tr><th>用户名</th><th>角色</th><th>权限</th><th>创建时间</th><th style={{ width: 80 }}></th></tr></thead>
          <tbody>
            {(users || []).map((u) => (
              <tr key={u.username}>
                <td><span style={{ fontWeight: 600 }}>{u.username}</span>{u.username === store.user ? <span style={{ fontSize: 11, color: "var(--muted-fg)", marginLeft: 6 }}>(我)</span> : null}</td>
                <td><Badge tone={ROLE_TONE[u.role] || "default"}>{u.role}</Badge></td>
                <td><span style={{ fontSize: 12.5, color: "var(--muted-fg)" }}>{ROLE_DESC[u.role] || "—"}</span></td>
                <td><span style={{ fontSize: 12.5, color: "var(--muted-fg)" }}>{fmtTime(u.createdAt)}</span></td>
                <td>
                  <Btn size="sm" variant="ghost" icon="trash" title="删除用户"
                    disabled={u.username === store.user}
                    onClick={() => onDelete(u)}></Btn>
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

      <CreateUserDialog open={open} onClose={() => setOpen(false)} onCreated={() => { setOpen(false); retry(); }} />
    </div>
  );
}

function CreateUserDialog({ open, onClose, onCreated }) {
  const [u, setU] = React.useState("");
  const [p, setP] = React.useState("");
  const [role, setRole] = React.useState("viewer");
  const [busy, setBusy] = React.useState(false);
  React.useEffect(() => { if (open) { setU(""); setP(""); setRole("viewer"); setBusy(false); } }, [open]);

  const submit = async () => {
    if (!u.trim() || !p) { toast("用户名与密码不能为空", { tone: "warn" }); return; }
    setBusy(true);
    try {
      await createUser({ username: u.trim(), password: p, role });
      toast(`已创建用户 ${u.trim()}(${role})`);
      onCreated();
    } catch (e) {
      toast(e.message || "创建失败", { tone: "error" });
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onClose={onClose} width={460} title="新建用户" desc="角色决定可执行的操作"
      foot={<React.Fragment>
        <Btn variant="ghost" onClick={onClose}>取消</Btn>
        <Btn variant="primary" icon="check" disabled={busy} onClick={submit}>{busy ? <Spinner size={12} /> : "创建"}</Btn>
      </React.Fragment>}>
      <div style={{ display: "flex", flexDirection: "column", gap: 13 }}>
        <Field label="用户名"><input className="input" value={u} onChange={(e) => setU(e.target.value)} placeholder="如 ops-zhang" /></Field>
        <Field label="初始密码"><input className="input" type="password" value={p} onChange={(e) => setP(e.target.value)} placeholder="登录后可自行修改" /></Field>
        <Field label="角色">
          <Select value={role} onChange={setRole} options={ROLE_OPTS} />
        </Field>
        <div style={{ display: "flex", gap: 9, padding: "10px 13px", borderRadius: 9, fontSize: 12.5, background: "var(--info-soft)", color: "var(--info)" }}>
          <Icon name="shield" size={15} style={{ flex: "none", marginTop: 1 }} />
          <span><b>{role}</b> · {ROLE_DESC[role]}</span>
        </div>
      </div>
    </Dialog>
  );
}

export { UsersPage };
