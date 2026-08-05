// 数据资源列表：admin CRUD + 普通用户进入工作台
import React from 'react';
import { useMC } from '../lib/data.js';
import { Btn, Field, Badge, Spinner, EmptyState, Dialog, toast } from '../components/primitives.jsx';
import { PageHead } from '../components/Shell.jsx';
import { useAsync } from '../lib/async.js';
import {
  listDataResources, listDrivers, createDataResource, updateDataResource,
  deleteDataResource, testDataResource, testDataResourceConfig, testDataResourceDraft,
  createWorkspace,
} from '../lib/dataresource-api.js';

/** 各驱动默认端口（与后端 DefaultPort / DriverCatalog.defaultPort 对齐） */
const DEFAULT_PORTS = {
  pgx: 5432,
  mysql: 3306,
  dm: 5236,
  kingbase: 54321,
  opengauss: 5432,
};

const DB_TYPE_LABEL = {
  pgx: 'PostgreSQL',
  mysql: 'MySQL',
  dm: '达梦 DM8',
  kingbase: 'KingbaseES',
  opengauss: 'Vastbase G100（当前构建不可用）',
};

/** 达梦无独立 catalog：实例 → schema/用户模式 → 对象（两层） */
function isDM(dbType) {
  return dbType === 'dm';
}

function defaultPortFor(dbType, drivers) {
  const fromApi = (drivers || []).find((d) => d.id === dbType)?.defaultPort;
  if (fromApi > 0) return fromApi;
  return DEFAULT_PORTS[dbType] || 5432;
}

function defaultSchemaFor(dbType) {
  if (dbType === 'pgx' || dbType === 'kingbase' || dbType === 'opengauss') return 'public';
  return '';
}

const EMPTY_RESOURCE_FORM = {
  name: '',
  dbType: 'pgx',
  host: '127.0.0.1',
  port: DEFAULT_PORTS.pgx,
  databaseName: '',
  defaultSchema: 'public',
  username: '',
  password: '',
  sslMode: 'disable',
};

/** 合并受控 state 与 DOM（兼容浏览器自动填充未触发 onChange 的情况） */
function mergeResourceForm(state, formEl) {
  const out = { ...EMPTY_RESOURCE_FORM, ...state };
  if (!formEl) return out;
  const fd = new FormData(formEl);
  for (const key of ['name', 'dbType', 'host', 'port', 'databaseName', 'defaultSchema', 'username', 'password', 'sslMode']) {
    if (!fd.has(key)) continue;
    const v = fd.get(key);
    if (typeof v === 'string') out[key] = v;
  }
  return out;
}

function validateResourceForm(f, { requirePassword }) {
  const missing = [];
  if (!String(f.name || '').trim()) missing.push('名称');
  if (!String(f.host || '').trim()) missing.push('主机');
  if (!isDM(f.dbType) && !String(f.databaseName || '').trim()) missing.push('数据库名');
  if (!String(f.username || '').trim()) missing.push('用户名');
  if (requirePassword && !String(f.password || '')) missing.push('密码');
  const port = Number(f.port);
  if (!Number.isFinite(port) || port < 1 || port > 65535) {
    return { ok: false, message: '端口必须在 1–65535 范围内' };
  }
  if (missing.length) {
    return { ok: false, message: `请填写：${missing.join('、')}` };
  }
  return { ok: true };
}

function resourcePayloadFromForm(f) {
  const dbType = f.dbType || 'pgx';
  let databaseName = String(f.databaseName || '').trim();
  let defaultSchema = String(f.defaultSchema || '').trim();
  // 达梦：表单只填 schema；同步写入 databaseName 便于列表展示与兼容旧字段
  if (isDM(dbType)) {
    const schema = defaultSchema || databaseName;
    databaseName = schema;
    defaultSchema = schema;
  }
  return {
    name: String(f.name || '').trim(),
    dbType,
    host: String(f.host || '').trim(),
    port: Number(f.port) || defaultPortFor(dbType, []),
    databaseName,
    defaultSchema,
    username: String(f.username || '').trim(),
    sslMode: f.sslMode || 'disable',
    password: f.password || '',
  };
}

function modeLabel(m) {
  if (m === 'admin') return '管理员';
  if (m === 'write') return '读写';
  if (m === 'read') return '只读';
  return m || '—';
}

function catalogColumn(r) {
  if (isDM(r.dbType)) {
    return r.defaultSchema || r.databaseName || r.username || '—';
  }
  return r.databaseName || '—';
}

function DataResourcesPage({ onOpenWorkspace }) {
  const store = useMC();
  const isAdmin = store.can('admin');
  const { data: resources, error, loading, retry } = useAsync(listDataResources, []);
  const [drivers, setDrivers] = React.useState([]);
  const [edit, setEdit] = React.useState(null); // null | {} create | resource edit
  const [delRes, setDelRes] = React.useState(null);
  const [passwordAction, setPasswordAction] = React.useState(null); // {resource, mode: connect|test}
  const [connectingResourceId, setConnectingResourceId] = React.useState(null);

  const openResource = async (resource) => {
    setConnectingResourceId(resource.id);
    try {
      const workspace = await createWorkspace(resource.id, '');
      onOpenWorkspace(resource, workspace);
    } catch (e) {
      if (e && e.code === 'PASSWORD_REQUIRED') {
        setPasswordAction({ resource, mode: 'connect' });
      } else {
        toast(e.message || '连接失败', { tone: 'error' });
      }
    } finally {
      setConnectingResourceId(null);
    }
  };

  React.useEffect(() => {
    listDrivers().then(setDrivers).catch(() => setDrivers([]));
  }, []);

  return (
    <div>
      <PageHead
        title="数据资源"
        desc={isAdmin ? '管理外部数据库连接 · 授权用户可在工作台执行 SQL' : '进入已授权的数据库工作台'}
        actions={isAdmin ? (
          <Btn variant="primary" icon="plus" onClick={() => setEdit({})}>新建资源</Btn>
        ) : null}
      />

      <div className="card" style={{ overflow: 'hidden' }}>
        <table className="table">
          <thead>
            <tr>
              <th>名称</th>
              <th>类型</th>
              <th>地址</th>
              <th>库 / Schema</th>
              <th>权限</th>
              <th>最近测试</th>
              <th style={{ width: isAdmin ? 200 : 100 }}></th>
            </tr>
          </thead>
          <tbody>
            {(resources || []).map((r) => (
              <tr key={r.id}>
                <td style={{ fontWeight: 600 }}>{r.name}</td>
                <td>
                  <span className="code-chip" style={{ fontSize: 11 }}>
                    {DB_TYPE_LABEL[r.dbType] || r.dbType}
                    {r.dbType === 'opengauss' ? ' · 已停用' : ''}
                  </span>
                </td>
                <td className="mono" style={{ fontSize: 12.5 }}>{r.host}:{r.port}</td>
                <td className="mono" style={{ fontSize: 12.5 }}>{catalogColumn(r)}</td>
                <td><Badge tone={r.accessMode === 'read' ? 'info' : 'success'}>{modeLabel(r.accessMode)}</Badge></td>
                <td style={{ fontSize: 12.5, color: 'var(--muted-fg)' }}>{r.lastTestInfo || '未测试'}</td>
                <td>
                  <div style={{ display: 'flex', gap: 4, justifyContent: 'flex-end' }}>
                    <Btn size="sm" variant="primary" disabled={connectingResourceId === r.id}
                      onClick={() => openResource(r)}>{connectingResourceId === r.id ? <Spinner size={12} /> : '进入'}</Btn>
                    {isAdmin ? (
                      <React.Fragment>
                        <Btn size="sm" variant="ghost" icon="activity" title="测试连接" onClick={() => setPasswordAction({ resource: r, mode: 'test' })} />
                        <Btn size="sm" variant="ghost" icon="pencil" title="编辑" onClick={() => setEdit(r)} />
                        <Btn size="sm" variant="ghost" icon="trash" title="删除" onClick={() => setDelRes(r)} />
                      </React.Fragment>
                    ) : null}
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {loading ? <div style={{ padding: 24, textAlign: 'center' }}><Spinner size={16} /></div> : null}
        {!loading && error ? (
          <EmptyState icon="alert" title="加载失败" desc={error.message || '请重试'}
            action={<Btn variant="primary" icon="rotate" onClick={retry}>重试</Btn>} />
        ) : null}
        {!loading && !error && resources && resources.length === 0 ? (
          <EmptyState icon="box" title="暂无数据资源"
            desc={isAdmin ? '点击「新建资源」添加数据库连接' : '管理员尚未为你授权任何数据资源'} />
        ) : null}
      </div>

      <ResourceDialog
        open={!!edit}
        resource={edit && edit.id ? edit : null}
        drivers={drivers}
        onClose={() => setEdit(null)}
        onSaved={() => { setEdit(null); retry(); }}
      />
      <DeleteDialog
        resource={delRes}
        open={!!delRes}
        onClose={() => setDelRes(null)}
        onDeleted={() => { setDelRes(null); retry(); }}
      />
      <ConnectionPasswordDialog
        action={passwordAction}
        open={!!passwordAction}
        onClose={() => setPasswordAction(null)}
        onConnected={(resource, workspace) => {
          setPasswordAction(null);
          onOpenWorkspace(resource, workspace);
        }}
        onTested={() => { setPasswordAction(null); retry(); }}
      />
    </div>
  );
}

function ResourceDialog({ open, resource, drivers, onClose, onSaved }) {
  const isEdit = !!(resource && resource.id);
  const formRef = React.useRef(null);
  const [form, setForm] = React.useState(EMPTY_RESOURCE_FORM);
  const [busy, setBusy] = React.useState(false);
  const [testing, setTesting] = React.useState(false);

  React.useEffect(() => {
    if (!open) return;
    if (isEdit) {
      const dbType = resource.dbType || 'pgx';
      setForm({
        ...EMPTY_RESOURCE_FORM,
        name: resource.name || '',
        dbType,
        host: resource.host || '',
        port: resource.port || defaultPortFor(dbType, drivers),
        databaseName: resource.databaseName || '',
        defaultSchema: resource.defaultSchema || (isDM(dbType) ? (resource.databaseName || '') : ''),
        username: resource.username || '',
        password: '',
        sslMode: resource.sslMode || 'disable',
      });
    } else {
      setForm({ ...EMPTY_RESOURCE_FORM });
    }
    setBusy(false);
    setTesting(false);
  }, [open, resource, isEdit, drivers]);

  const set = (k, v) => setForm((f) => ({ ...f, [k]: v }));

  const onDbTypeChange = (nextType) => {
    setForm((f) => {
      const prevType = f.dbType || 'pgx';
      const prevDefault = defaultPortFor(prevType, drivers);
      const nextDefault = defaultPortFor(nextType, drivers);
      const curPort = Number(f.port);
      // 仅当端口仍是「上一类型默认端口」或空时自动切换，避免覆盖用户手改
      const shouldBumpPort = !curPort || curPort === prevDefault;
      const next = {
        ...f,
        dbType: nextType,
        port: shouldBumpPort ? nextDefault : f.port,
      };
      if (isDM(nextType)) {
        // 达梦：schema 字段用 defaultSchema；若从 PG 切过来，可用原 databaseName
        if (!next.defaultSchema && next.databaseName) next.defaultSchema = next.databaseName;
        next.sslMode = 'disable';
      } else if (isDM(prevType)) {
        // 从达梦切回：库名字段用 schema 填充
        if (!next.databaseName && next.defaultSchema) next.databaseName = next.defaultSchema;
        if (!next.defaultSchema) next.defaultSchema = defaultSchemaFor(nextType);
      } else if (!next.defaultSchema) {
        next.defaultSchema = defaultSchemaFor(nextType);
      }
      return next;
    });
  };

  const currentForm = () => mergeResourceForm(form, formRef.current);

  const onTestConfig = async () => {
    const f = currentForm();
    const v = validateResourceForm(f, { requirePassword: true });
    if (!v.ok) {
      toast(v.message, { tone: 'warn' });
      return;
    }
    const payload = resourcePayloadFromForm(f);
    setTesting(true);
    try {
      let res;
      if (isEdit) {
        // 始终测试「当前表单配置 + 本次输入密码」，密码不会保存。
        res = await testDataResourceDraft(resource.id, payload);
      } else {
        res = await testDataResourceConfig(payload);
      }
      if (res.ok) {
        toast(`连接成功 · ${res.latencyMs || 0}ms` + (res.readOnlyTxSupported ? ' · 只读事务可用' : ' · 只读事务不可用'));
      } else {
        toast(res.error || res.errorCode || '连接失败', { tone: 'error' });
      }
    } catch (e) {
      toast(e.message || '测试失败', { tone: 'error' });
    } finally {
      setTesting(false);
    }
  };

  const submit = async () => {
    const f = currentForm();
    // 同步 DOM 值回 state，避免自动填充后界面有值但 state 空
    setForm((prev) => ({ ...prev, ...f }));
    const v = validateResourceForm(f, { requirePassword: false });
    if (!v.ok) {
      toast(v.message, { tone: 'warn' });
      return;
    }
    setBusy(true);
    try {
      const payload = resourcePayloadFromForm(f);
      delete payload.password; // 资源配置永不携带或保存数据库密码
      if (isEdit) await updateDataResource(resource.id, payload);
      else await createDataResource(payload);
      toast(isEdit ? '已更新资源' : '已创建资源');
      onSaved();
    } catch (e) {
      toast(e.message || '保存失败', { tone: 'error' });
      setBusy(false);
    }
  };

  const opts = drivers.length
    ? drivers
    : ['pgx', 'mysql', 'dm', 'kingbase'].map((id) => ({
      id,
      label: DB_TYPE_LABEL[id],
      defaultPort: DEFAULT_PORTS[id],
      hasCatalog: id !== 'dm',
    }));

  const actionBusy = busy || testing;
  const dm = isDM(form.dbType);

  return (
    <Dialog open={open} onClose={onClose} width={520}
      title={isEdit ? `编辑 · ${resource.name}` : '新建数据资源'}
      desc="只保存连接地址和用户名；数据库密码仅用于本次测试，不会保存"
      foot={<React.Fragment>
        <Btn variant="ghost" onClick={onClose} disabled={actionBusy}>取消</Btn>
        <Btn variant="outline" icon="activity" disabled={actionBusy} onClick={onTestConfig}>
          {testing ? <Spinner size={12} /> : '测试连接'}
        </Btn>
        <Btn variant="primary" icon="check" disabled={actionBusy} onClick={submit}>
          {busy ? <Spinner size={12} /> : '保存'}
        </Btn>
      </React.Fragment>}>
      <form
        ref={formRef}
        autoComplete="off"
        onSubmit={(e) => { e.preventDefault(); submit(); }}
        style={{ display: 'flex', flexDirection: 'column', gap: 12 }}
      >
        <Field label="名称">
          <input className="input" name="name" value={form.name || ''} onChange={(e) => set('name', e.target.value)} autoComplete="off" />
        </Field>
        <Field label="数据库类型">
          <select className="input" name="dbType" value={form.dbType || 'pgx'} onChange={(e) => onDbTypeChange(e.target.value)}>
            {opts.map((d) => (
              <option key={d.id} value={d.id}>{d.label}{d.experimental ? '（实验性）' : ''}</option>
            ))}
          </select>
        </Field>
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 100px', gap: 10 }}>
          <Field label="主机">
            <input className="input" name="host" value={form.host || ''} onChange={(e) => set('host', e.target.value)} autoComplete="off" />
          </Field>
          <Field label="端口">
            <input className="input" name="port" type="number" value={form.port ?? ''} onChange={(e) => set('port', e.target.value)} />
          </Field>
        </div>
        {dm ? (
          <Field
            label="默认 Schema（用户模式）"
            hint="达梦无独立「库」层：写入连接 DSN，变更后会重建连接池。可留空，默认与用户名相同"
          >
            <input
              className="input"
              name="defaultSchema"
              value={form.defaultSchema || ''}
              onChange={(e) => set('defaultSchema', e.target.value)}
              placeholder="如 SYSDBA 或业务用户名"
              autoComplete="off"
            />
          </Field>
        ) : (
          <React.Fragment>
            <Field label="数据库名" hint="必填 · 目标 database / catalog，不是 schema">
              <input className="input" name="databaseName" value={form.databaseName || ''} onChange={(e) => set('databaseName', e.target.value)} autoComplete="off" />
            </Field>
            <Field
              label="默认 Schema"
              hint={
                form.dbType === 'pgx' || form.dbType === 'kingbase'
                  ? '可选 · 常用 public。注意：授权可见范围为整个 database 的所有 schema，实际权限由数据库账号决定；推荐资源使用只读账号作为第三层保护'
                  : '可选 · MySQL 可留空'
              }
            >
              <input className="input" name="defaultSchema" value={form.defaultSchema || ''} onChange={(e) => set('defaultSchema', e.target.value)} autoComplete="off" />
            </Field>
          </React.Fragment>
        )}
        <Field label="用户名">
          <input className="input" name="username" value={form.username || ''} onChange={(e) => set('username', e.target.value)} autoComplete="off" />
        </Field>
        <Field label="测试密码（不保存）" hint="仅点击“测试连接”时使用；保存资源不会提交此密码">
          <input className="input" name="password" type="password" value={form.password || ''}
            onChange={(e) => set('password', e.target.value)} autoComplete="off" data-1p-ignore="true" data-lpignore="true" />
        </Field>
        <Field label="SSL" hint={dm ? '达梦当前仅支持 disable' : undefined}>
          <select
            className="input"
            name="sslMode"
            value={form.sslMode || 'disable'}
            onChange={(e) => set('sslMode', e.target.value)}
            disabled={dm}
          >
            <option value="disable">disable</option>
            {!dm ? <option value="require">require</option> : null}
          </select>
        </Field>
      </form>
    </Dialog>
  );
}

function DeleteDialog({ open, resource, onClose, onDeleted }) {
  const [name, setName] = React.useState('');
  const [busy, setBusy] = React.useState(false);
  React.useEffect(() => { if (open) { setName(''); setBusy(false); } }, [open]);
  if (!resource) return null;
  const submit = async () => {
    // 与后端 EqualFold 一致：大小写不敏感确认
    if (name.trim().toLowerCase() !== String(resource.name || '').toLowerCase()) {
      toast('请输入完整资源名称以确认删除', { tone: 'warn' });
      return;
    }
    setBusy(true);
    try {
      await deleteDataResource(resource.id, name.trim());
      toast('已删除');
      onDeleted();
    } catch (e) {
      toast(e.message || '删除失败', { tone: 'error' });
      setBusy(false);
    }
  };
  return (
    <Dialog open={open} onClose={onClose} width={420} title="删除数据资源"
      desc={`输入资源名称「${resource.name}」确认删除`}
      foot={<React.Fragment>
        <Btn variant="ghost" onClick={onClose}>取消</Btn>
        <Btn variant="destructive" icon="trash" disabled={busy} onClick={submit}>{busy ? <Spinner size={12} /> : '删除'}</Btn>
      </React.Fragment>}>
      <Field label="资源名称"><input className="input" value={name} onChange={(e) => setName(e.target.value)} /></Field>
    </Dialog>
  );
}

function ConnectionPasswordDialog({ action, open, onClose, onConnected, onTested }) {
  const [password, setPassword] = React.useState('');
  const [busy, setBusy] = React.useState(false);
  React.useEffect(() => {
    if (open) {
      setPassword('');
      setBusy(false);
    }
  }, [open, action]);
  if (!action?.resource) return null;
  const isTest = action.mode === 'test';
  const submit = async () => {
    if (!password) {
      toast('请输入数据库密码', { tone: 'warn' });
      return;
    }
    setBusy(true);
    try {
      if (isTest) {
        const result = await testDataResource(action.resource.id, password);
        if (!result.ok) {
          toast(result.error || result.errorCode || '连接失败', { tone: 'error' });
          setBusy(false);
          return;
        }
        toast(`连接成功 · ${result.latencyMs || 0}ms` + (result.readOnlyTxSupported ? ' · 只读事务可用' : ' · 只读事务不可用'));
        onTested();
      } else {
        const workspace = await createWorkspace(action.resource.id, password);
        onConnected(action.resource, workspace);
      }
    } catch (e) {
      toast(e.message || (isTest ? '测试失败' : '连接失败'), { tone: 'error' });
      setBusy(false);
    }
  };
  return (
    <Dialog open={open} onClose={onClose} width={420}
      title={`${isTest ? '测试连接' : '连接数据库'} · ${action.resource.name}`}
      desc="密码仅保存在当前 Mooncell 登录会话的 Console 内存中；退出、过期或服务重启即清除"
      foot={<React.Fragment>
        <Btn variant="ghost" onClick={onClose} disabled={busy}>取消</Btn>
        <Btn variant="primary" icon={isTest ? 'activity' : 'check'} onClick={submit} disabled={busy}>
          {busy ? <Spinner size={12} /> : (isTest ? '测试' : '连接')}
        </Btn>
      </React.Fragment>}>
      <form onSubmit={(e) => { e.preventDefault(); submit(); }} autoComplete="off">
        <Field label="数据库密码">
          <input className="input" type="password" value={password} autoFocus
            onChange={(e) => setPassword(e.target.value)} autoComplete="off" data-1p-ignore="true" data-lpignore="true" />
        </Field>
      </form>
    </Dialog>
  );
}

export { DataResourcesPage };
