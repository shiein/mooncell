// 数据资源列表：admin CRUD + 普通用户进入工作台
import React from 'react';
import { useMC, fmtTime } from '../lib/data.js';
import { Btn, Field, Icon, Badge, Spinner, EmptyState, Dialog, toast } from '../components/primitives.jsx';
import { PageHead } from '../components/Shell.jsx';
import { useAsync } from '../lib/async.js';
import {
  listDataResources, listDrivers, createDataResource, updateDataResource,
  deleteDataResource, testDataResource,
} from '../lib/dataresource-api.js';

const DB_TYPE_LABEL = {
  pgx: 'PostgreSQL',
  mysql: 'MySQL',
  dm: '达梦 DM8',
  kingbase: 'KingbaseES',
  opengauss: 'Vastbase G100（当前构建不可用）',
};

function modeLabel(m) {
  if (m === 'admin') return '管理员';
  if (m === 'write') return '读写';
  if (m === 'read') return '只读';
  return m || '—';
}

function DataResourcesPage({ onOpenWorkspace }) {
  const store = useMC();
  const isAdmin = store.can('admin');
  const { data: resources, error, loading, retry } = useAsync(listDataResources, []);
  const [drivers, setDrivers] = React.useState([]);
  const [edit, setEdit] = React.useState(null); // null | {} create | resource edit
  const [delRes, setDelRes] = React.useState(null);

  React.useEffect(() => {
    listDrivers().then(setDrivers).catch(() => setDrivers([]));
  }, []);

  const onTest = async (r) => {
    try {
      const res = await testDataResource(r.id);
      if (res.ok) {
        toast(`连接成功 · ${res.latencyMs || 0}ms` + (res.readOnlyTxSupported ? ' · 只读事务可用' : ' · 只读事务不可用'));
      } else {
        toast(res.error || res.errorCode || '连接失败', { tone: 'error' });
      }
      retry();
    } catch (e) {
      toast(e.message || '测试失败', { tone: 'error' });
    }
  };

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
              <th>数据库</th>
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
                <td className="mono" style={{ fontSize: 12.5 }}>{r.databaseName}</td>
                <td><Badge tone={r.accessMode === 'read' ? 'info' : 'success'}>{modeLabel(r.accessMode)}</Badge></td>
                <td style={{ fontSize: 12.5, color: 'var(--muted-fg)' }}>{r.lastTestInfo || '未测试'}</td>
                <td>
                  <div style={{ display: 'flex', gap: 4, justifyContent: 'flex-end' }}>
                    <Btn size="sm" variant="primary" onClick={() => onOpenWorkspace(r)}>进入</Btn>
                    {isAdmin ? (
                      <React.Fragment>
                        <Btn size="sm" variant="ghost" icon="activity" title="测试连接" onClick={() => onTest(r)} />
                        <Btn size="sm" variant="ghost" icon="settings" title="编辑" onClick={() => setEdit(r)} />
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
    </div>
  );
}

function ResourceDialog({ open, resource, drivers, onClose, onSaved }) {
  const isEdit = !!(resource && resource.id);
  const [form, setForm] = React.useState({});
  const [busy, setBusy] = React.useState(false);
  React.useEffect(() => {
    if (!open) return;
    if (isEdit) {
      setForm({
        name: resource.name || '',
        dbType: resource.dbType || 'pgx',
        host: resource.host || '',
        port: resource.port || 5432,
        databaseName: resource.databaseName || '',
        defaultSchema: resource.defaultSchema || '',
        username: resource.username || '',
        password: '',
        sslMode: resource.sslMode || 'disable',
      });
    } else {
      setForm({
        name: '', dbType: 'pgx', host: '127.0.0.1', port: 5432,
        databaseName: '', defaultSchema: 'public', username: '', password: '', sslMode: 'disable',
      });
    }
    setBusy(false);
  }, [open, resource, isEdit]);

  const set = (k, v) => setForm((f) => ({ ...f, [k]: v }));

  const submit = async () => {
    if (!form.name?.trim() || !form.host?.trim() || !form.databaseName?.trim() || !form.username?.trim()) {
      toast('请填写名称、主机、数据库与用户名', { tone: 'warn' });
      return;
    }
    if (!isEdit && !form.password) {
      toast('新建时密码不能为空', { tone: 'warn' });
      return;
    }
    setBusy(true);
    try {
      const payload = {
        name: form.name.trim(),
        dbType: form.dbType,
        host: form.host.trim(),
        port: Number(form.port) || 5432,
        databaseName: form.databaseName.trim(),
        defaultSchema: (form.defaultSchema || '').trim(),
        username: form.username.trim(),
        sslMode: form.sslMode || 'disable',
      };
      if (form.password) payload.password = form.password;
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
    : ['pgx', 'mysql', 'dm', 'kingbase'].map((id) => ({ id, label: DB_TYPE_LABEL[id] }));

  return (
    <Dialog open={open} onClose={onClose} width={520}
      title={isEdit ? `编辑 · ${resource.name}` : '新建数据资源'}
      desc="连接配置不开放任意 DSN；保存后需测试连接才可授予只读权限"
      foot={<React.Fragment>
        <Btn variant="ghost" onClick={onClose}>取消</Btn>
        <Btn variant="primary" icon="check" disabled={busy} onClick={submit}>{busy ? <Spinner size={12} /> : '保存'}</Btn>
      </React.Fragment>}>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        <Field label="名称"><input className="input" value={form.name || ''} onChange={(e) => set('name', e.target.value)} /></Field>
        <Field label="数据库类型">
          <select className="input" value={form.dbType || 'pgx'} onChange={(e) => set('dbType', e.target.value)}>
            {opts.map((d) => (
              <option key={d.id} value={d.id}>{d.label}{d.experimental ? '（实验性）' : ''}</option>
            ))}
          </select>
        </Field>
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 100px', gap: 10 }}>
          <Field label="主机"><input className="input" value={form.host || ''} onChange={(e) => set('host', e.target.value)} /></Field>
          <Field label="端口"><input className="input" type="number" value={form.port || ''} onChange={(e) => set('port', e.target.value)} /></Field>
        </div>
        <Field label="数据库名"><input className="input" value={form.databaseName || ''} onChange={(e) => set('databaseName', e.target.value)} /></Field>
        <Field label="默认 Schema" hint="MySQL 可留空；PG 默认 public">
          <input className="input" value={form.defaultSchema || ''} onChange={(e) => set('defaultSchema', e.target.value)} />
        </Field>
        <Field label="用户名"><input className="input" value={form.username || ''} onChange={(e) => set('username', e.target.value)} autoComplete="off" /></Field>
        <Field label={isEdit ? '密码（留空保留原密码）' : '密码'}>
          <input className="input" type="password" value={form.password || ''} onChange={(e) => set('password', e.target.value)} autoComplete="new-password" />
        </Field>
        <Field label="SSL">
          <select className="input" value={form.sslMode || 'disable'} onChange={(e) => set('sslMode', e.target.value)}>
            <option value="disable">disable</option>
            <option value="require">require</option>
          </select>
        </Field>
      </div>
    </Dialog>
  );
}

function DeleteDialog({ open, resource, onClose, onDeleted }) {
  const [name, setName] = React.useState('');
  const [busy, setBusy] = React.useState(false);
  React.useEffect(() => { if (open) { setName(''); setBusy(false); } }, [open]);
  if (!resource) return null;
  const submit = async () => {
    if (name.trim() !== resource.name) {
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

export { DataResourcesPage };
