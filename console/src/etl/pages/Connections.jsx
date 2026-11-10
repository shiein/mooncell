// 连接：数据集成专用的数据库连接（密码加密保存，供调度无人值守使用）。与「数据资源」完全独立。
import React from 'react';
import { Icon, Btn, Spinner, Dialog, Field, Select, Badge, toast, confirmDialog } from '../../components/primitives.jsx';
import { etlApi, loadCatalog } from '../api.js';
import { forgetConnections } from '../components.jsx';
import { fmtAgo } from '../util.js';

const DIALECT_STYLE = {
  postgres: { short: 'PG', bg: 'var(--info-soft)', fg: 'var(--info)' },
  kingbase: { short: 'KB', bg: 'var(--cyan-soft)', fg: 'var(--cyan)' },
  mysql: { short: 'My', bg: 'var(--warn-soft)', fg: 'var(--warn)' },
  dm: { short: 'DM', bg: 'var(--primary-soft)', fg: 'var(--primary)' },
};

export function ConnectionsPage() {
  const [list, setList] = React.useState(null);
  const [editing, setEditing] = React.useState(null);
  const [testing, setTesting] = React.useState({});
  const load = React.useCallback(() => { forgetConnections(); etlApi.connections().then(setList).catch(() => setList([])); }, []);
  React.useEffect(() => { load(); }, [load]);

  const test = async (c) => {
    setTesting((t) => ({ ...t, [c.id]: true }));
    try {
      const r = await etlApi.testConnection(c.id);
      if (r.ok) toast(`连接成功 · ${r.latencyMs}ms`); else toast(r.error || '连接失败', { tone: 'error' });
      load();
    } finally { setTesting((t) => ({ ...t, [c.id]: false })); }
  };
  const del = async (c) => {
    const ok = await confirmDialog({ title: `删除连接「${c.name}」`, tone: 'danger', confirmText: '删除', message: '删除后引用它的步骤将无法运行。' });
    if (!ok) return;
    try { await etlApi.deleteConnection(c.id); }
    catch (e) {
      if (e.code !== 'in_use') { toast(e.message, { tone: 'error' }); return; }
      const force = await confirmDialog({ title: '连接仍被引用', tone: 'danger', confirmText: '仍然删除', message: e.message });
      if (!force) return;
      await etlApi.deleteConnection(c.id, true);
    }
    toast('已删除', { icon: 'trash' });
    load();
  };

  return (
    <div>
      <div className="etl-toolbar">
        <span className="etl-muted"><Icon name="key" size={13} style={{ verticalAlign: -2, marginRight: 5 }} />密码以独立密钥加密保存在 Console 数据库中，请将密钥文件与数据库一起备份。</span>
        <span style={{ flex: 1 }}></span>
        <Btn variant="primary" icon="plus" onClick={() => setEditing({})}>新建连接</Btn>
      </div>
      {!list ? <div className="etl-list-empty" style={{ padding: 40 }}><Spinner /></div> : null}
      {list && !list.length ? (
        <div className="card etl-empty">
          <div style={{ display: 'inline-flex', padding: 14, borderRadius: 14, background: 'var(--info-soft)', color: 'var(--info)' }}><Icon name="database" size={22} /></div>
          <h3>还没有连接</h3>
          <p>支持 PostgreSQL、MySQL、达梦 DM8、KingbaseES。连接只用于数据集成，与「数据资源」互不影响。</p>
          <Btn variant="outline" icon="plus" style={{ marginTop: 14 }} onClick={() => setEditing({})}>新建连接</Btn>
        </div>
      ) : null}
      {list && list.length ? (
        <div className="etl-conn-grid">
          {list.map((c) => {
            const ds = DIALECT_STYLE[c.dbType] || { short: c.dbType.slice(0, 2), bg: 'var(--muted)', fg: 'var(--fg-secondary)' };
            return (
              <div key={c.id} className="card etl-conn">
                <div className="etl-conn-top">
                  <span className="etl-dialect" style={{ background: ds.bg, color: ds.fg }}>{ds.short}</span>
                  <div style={{ minWidth: 0, flex: 1 }}>
                    <div className="etl-conn-name">{c.name}</div>
                    <div className="etl-conn-addr">{c.username}@{c.host}:{c.port}{c.database ? `/${c.database}` : ''}</div>
                  </div>
                </div>
                {c.description ? <div className="etl-muted" style={{ lineHeight: 1.55 }}>{c.description}</div> : null}
                <div className="etl-conn-foot">
                  {c.lastTestAt ? (
                    <Badge tone={c.lastTestOk ? 'success' : 'error'} dot>{c.lastTestOk ? '可用' : '失败'}</Badge>
                  ) : <Badge>未测试</Badge>}
                  <span className="etl-muted" style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={c.lastTestMsg}>
                    {c.lastTestAt ? `${fmtAgo(c.lastTestAt)} · ${c.lastTestMsg}` : c.hasPassword ? '' : '未设置密码'}
                  </span>
                  <Btn variant="ghost" size="sm" icon={testing[c.id] ? undefined : 'activity'} title="测试" disabled={testing[c.id]} onClick={() => test(c)}>{testing[c.id] ? <Spinner size={11} /> : null}</Btn>
                  <Btn variant="ghost" size="sm" icon="pencil" title="编辑" onClick={() => setEditing(c)}></Btn>
                  <Btn variant="ghost" size="sm" icon="trash" title="删除" onClick={() => del(c)}></Btn>
                </div>
              </div>
            );
          })}
        </div>
      ) : null}
      {editing ? <ConnectionDialog initial={editing} onClose={() => setEditing(null)} onSaved={() => { setEditing(null); load(); }} /> : null}
    </div>
  );
}

function ConnectionDialog({ initial, onClose, onSaved }) {
  const isEdit = !!initial.id;
  const [dialects, setDialects] = React.useState([]);
  const [f, setF] = React.useState(() => ({
    dbType: 'postgres', host: '127.0.0.1', port: 5432, database: '', defaultSchema: '', username: '', sslMode: 'disable',
    options: { timeZone: '', maxOpenConns: 8, connectTimeoutSec: 10 }, description: '', name: '', ...initial, password: '',
  }));
  const [busy, setBusy] = React.useState('');
  const set = (patch) => setF((x) => ({ ...x, ...patch }));
  React.useEffect(() => { loadCatalog().then((c) => setDialects(c.dialects || [])).catch(() => {}); }, []);
  const isDM = f.dbType === 'dm';
  const payload = () => ({ ...f, port: Number(f.port), password: f.password ? f.password : undefined,
    options: { ...f.options, maxOpenConns: Number(f.options.maxOpenConns) || 0, connectTimeoutSec: Number(f.options.connectTimeoutSec) || 0 } });
  const test = async () => {
    setBusy('test');
    try {
      const r = await etlApi.testDraft(payload());
      if (r.ok) toast(`连接成功 · ${r.latencyMs}ms · ${(r.version || '').slice(0, 60)}`); else toast(r.error || '连接失败', { tone: 'error' });
    } catch (e) { toast(e.message, { tone: 'error' }); } finally { setBusy(''); }
  };
  const save = async () => {
    if (!isEdit && !f.password) {
      const ok = await confirmDialog({ title: '未填写密码', message: '没有密码的连接只能用于免密认证的数据库。确定继续？', confirmText: '继续保存' });
      if (!ok) return;
    }
    setBusy('save');
    try { await etlApi.saveConnection(payload()); toast(isEdit ? '连接已更新' : '连接已创建'); onSaved(); } catch (e) { toast(e.message, { tone: 'error' }); setBusy(''); }
  };
  return (
    <Dialog open onClose={onClose} width={560} title={isEdit ? `编辑连接 · ${initial.name}` : '新建连接'} desc="密码加密保存，供定时任务无人值守使用；界面永不回显"
      foot={<React.Fragment>
        <Btn variant="ghost" onClick={onClose}>取消</Btn>
        <Btn variant="outline" icon="activity" disabled={!!busy} onClick={test}>{busy === 'test' ? <Spinner size={12} /> : null}测试连接</Btn>
        <Btn variant="primary" icon="check" disabled={!!busy} onClick={save}>{busy === 'save' ? <Spinner size={12} /> : null}保存</Btn>
      </React.Fragment>}>
      <form autoComplete="off" onSubmit={(e) => { e.preventDefault(); save(); }} style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        <div className="etl-row2">
          <Field label="名称"><input className="input" value={f.name} onChange={(e) => set({ name: e.target.value })} placeholder="如 业务库 A" autoFocus /></Field>
          <Field label="数据库类型">
            <Select value={f.dbType} onChange={(dbType) => {
              const d = dialects.find((x) => x.id === dbType);
              set({ dbType, port: d ? d.defaultPort : f.port });
            }} options={(dialects.length ? dialects : [{ id: 'postgres', label: 'PostgreSQL' }]).map((d) => ({ value: d.id, label: d.label }))} />
          </Field>
        </div>
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 110px', gap: 10 }}>
          <Field label="主机"><input className="input" value={f.host} onChange={(e) => set({ host: e.target.value })} /></Field>
          <Field label="端口"><input className="input" type="number" value={f.port} onChange={(e) => set({ port: e.target.value })} /></Field>
        </div>
        <div className="etl-row2">
          {isDM ? null : <Field label="数据库"><input className="input" value={f.database} onChange={(e) => set({ database: e.target.value })} /></Field>}
          <Field label={isDM ? '模式（用户模式）' : '默认模式'} hint={isDM ? '留空则使用用户名' : f.dbType === 'mysql' ? '通常与数据库相同，可留空' : '留空为 public'}>
            <input className="input" value={f.defaultSchema} onChange={(e) => set({ defaultSchema: e.target.value })} />
          </Field>
        </div>
        <div className="etl-row2">
          <Field label="用户名"><input className="input" value={f.username} onChange={(e) => set({ username: e.target.value })} autoComplete="off" /></Field>
          <Field label="密码" hint={isEdit ? (initial.hasPassword ? '留空表示不修改' : '当前未设置') : undefined}>
            <input className="input" type="password" value={f.password} onChange={(e) => set({ password: e.target.value })} autoComplete="new-password" placeholder={isEdit && initial.hasPassword ? '••••••••' : ''} />
          </Field>
        </div>
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: 10 }}>
          <Field label="SSL">
            <Select value={f.sslMode || 'disable'} onChange={(sslMode) => set({ sslMode })} options={[{ value: 'disable', label: '不加密' }, { value: 'require', label: '要求加密' }]} />
          </Field>
          <Field label="时区" hint="解释无时区的时间">
            <Select value={f.options.timeZone || ''} onChange={(timeZone) => set({ options: { ...f.options, timeZone } })}
              options={['', 'Asia/Shanghai', 'UTC'].map((z) => ({ value: z, label: z || 'Console 本地' }))} />
          </Field>
          <Field label="最大连接数"><input className="input" type="number" min="1" value={f.options.maxOpenConns} onChange={(e) => set({ options: { ...f.options, maxOpenConns: e.target.value } })} /></Field>
        </div>
        <Field label="描述（可选）"><input className="input" value={f.description} onChange={(e) => set({ description: e.target.value })} /></Field>
      </form>
    </Dialog>
  );
}
