// 调度：cron 规则、启停、重叠/错过策略、参数覆盖；编辑时实时显示中文释义与未来 5 次触发。
import React from 'react';
import { Icon, Btn, Switch, Spinner, Dialog, Field, Select, Seg, toast, confirmDialog } from '../../components/primitives.jsx';
import { etlApi } from '../api.js';
import { fmtAgo, fmtDateTime, KIND_LABEL, makeValue } from '../util.js';

const PRESETS = [
  { cron: '*/15 * * * *', label: '每 15 分钟' },
  { cron: '0 * * * *', label: '每小时' },
  { cron: '0 2 * * *', label: '每天 02:00' },
  { cron: '0 9 * * 1-5', label: '工作日 09:00' },
  { cron: '0 3 * * 0', label: '每周日 03:00' },
  { cron: '0 1 1 * *', label: '每月 1 日 01:00' },
];

const OVERLAP = { skip: '跳过本次', queue: '排队等待', parallel: '并行运行' };

export function SchedulesPage({ go }) {
  const [list, setList] = React.useState(null);
  const [editing, setEditing] = React.useState(null);
  const load = React.useCallback(() => { etlApi.schedules().then(setList).catch(() => setList([])); }, []);
  React.useEffect(() => { load(); }, [load]);

  const toggle = async (s, enabled) => {
    try { await etlApi.saveSchedule({ ...s, enabled }); load(); toast(enabled ? '调度已启用' : '调度已停用'); } catch (e) { toast(e.message, { tone: 'error' }); }
  };
  const del = async (s) => {
    const ok = await confirmDialog({ title: '删除调度', tone: 'danger', confirmText: '删除', message: `删除「${s.name || s.cron}」后不再自动运行「${s.targetName}」。` });
    if (!ok) return;
    try { await etlApi.deleteSchedule(s.id); load(); } catch (e) { toast(e.message, { tone: 'error' }); }
  };

  return (
    <div>
      <div className="etl-toolbar">
        <span className="etl-muted">调度在 Console 进程内运行；Console 停机期间错过的触发按「错过策略」处理。</span>
        <span style={{ flex: 1 }}></span>
        <Btn variant="primary" icon="plus" onClick={() => setEditing({})}>新建调度</Btn>
      </div>
      <div className="card" style={{ overflow: 'hidden' }}>
        {!list ? <div className="etl-list-empty" style={{ padding: 40 }}><Spinner /></div> : null}
        {list && !list.length ? (
          <div className="etl-empty">
            <div style={{ display: 'inline-flex', padding: 14, borderRadius: 14, background: 'var(--muted)', color: 'var(--muted-fg)' }}><Icon name="calendar" size={22} /></div>
            <h3>还没有调度</h3>
            <p>为作业（或转换）添加 cron 规则，按时自动运行。</p>
            <Btn variant="outline" icon="plus" style={{ marginTop: 14 }} onClick={() => setEditing({})}>新建调度</Btn>
          </div>
        ) : null}
        {list && list.length ? (
          <table className="table etl-table">
            <thead>
              <tr><th style={{ width: 60 }}>启用</th><th>调度</th><th>目标</th><th style={{ width: 180 }}>下次触发</th><th style={{ width: 150 }}>上次触发</th><th style={{ width: 110 }}>重叠时</th><th style={{ width: 90 }}></th></tr>
            </thead>
            <tbody>
              {list.map((s) => (
                <tr key={s.id}>
                  <td><Switch on={s.enabled} onChange={(v) => toggle(s, v)} /></td>
                  <td>
                    <div style={{ fontWeight: 600 }}>{s.description}</div>
                    <div className="etl-muted"><code className="code-chip">{s.cron}</code>{s.name ? ` · ${s.name}` : ''}{s.timezone ? ` · ${s.timezone}` : ''}</div>
                  </td>
                  <td>
                    <button type="button" className="link-btn" style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }} onClick={() => go.designer(s.targetId)}>
                      <Icon name={s.targetKind === 'workflow' ? 'workflow' : 'pipeline'} size={13} />{s.targetName || '（已删除）'}
                    </button>
                  </td>
                  <td>{s.enabled && s.nextFireAt ? <span>{fmtDateTime(s.nextFireAt)}<div className="etl-muted" style={{ fontSize: 11 }}>{fmtAgo(s.nextFireAt)}</div></span> : <span className="etl-muted">—</span>}</td>
                  <td className="etl-muted">{s.lastFireAt ? fmtAgo(s.lastFireAt) : '从未'}</td>
                  <td className="etl-muted">{OVERLAP[s.overlap] || s.overlap}</td>
                  <td>
                    <div className="etl-actions">
                      <Btn variant="ghost" size="sm" icon="pencil" title="编辑" onClick={() => setEditing(s)}></Btn>
                      <Btn variant="ghost" size="sm" icon="trash" title="删除" onClick={() => del(s)}></Btn>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : null}
      </div>
      {editing ? <ScheduleDialog initial={editing} onClose={() => setEditing(null)} onSaved={() => { setEditing(null); load(); }} /> : null}
    </div>
  );
}

function ScheduleDialog({ initial, onClose, onSaved }) {
  const [s, setS] = React.useState(() => ({ cron: '0 2 * * *', timezone: '', enabled: true, overlap: 'skip', misfire: 'skip', name: '', params: {}, ...initial }));
  const [defs, setDefs] = React.useState([]);
  const [target, setTarget] = React.useState(null);
  const [prev, setPrev] = React.useState(null);
  const [busy, setBusy] = React.useState(false);
  const set = (patch) => setS((x) => ({ ...x, ...patch }));
  React.useEffect(() => { etlApi.definitions().then(setDefs).catch(() => {}); }, []);
  React.useEffect(() => {
    if (!s.targetId) { setTarget(null); return; }
    etlApi.definition(s.targetId).then(setTarget).catch(() => setTarget(null));
  }, [s.targetId]);
  React.useEffect(() => {
    const t = setTimeout(() => { etlApi.previewCron(s.cron, s.timezone).then(setPrev).catch(() => setPrev(null)); }, 250);
    return () => clearTimeout(t);
  }, [s.cron, s.timezone]);
  const params = (target && target.doc && target.doc.params) || [];
  const save = async () => {
    if (!s.targetId) { toast('请选择要调度的作业或转换', { tone: 'warn' }); return; }
    if (prev && !prev.valid) { toast(prev.error, { tone: 'warn' }); return; }
    setBusy(true);
    try { await etlApi.saveSchedule(s); toast('调度已保存'); onSaved(); } catch (e) { toast(e.message, { tone: 'error' }); setBusy(false); }
  };
  const tzs = ['', 'Asia/Shanghai', 'UTC', 'Asia/Hong_Kong', 'Asia/Tokyo', 'Europe/London', 'America/New_York'];
  return (
    <Dialog open onClose={onClose} width={600} title={initial.id ? '编辑调度' : '新建调度'}
      foot={<React.Fragment><Btn variant="ghost" onClick={onClose}>取消</Btn><Btn variant="primary" icon="check" disabled={busy} onClick={save}>{busy ? <Spinner size={12} /> : null}保存</Btn></React.Fragment>}>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
        <div className="etl-row2">
          <Field label="调度目标">
            <Select value={s.targetId || ''} onChange={(targetId) => set({ targetId, params: {} })}
              options={[{ value: '', label: '选择作业或转换…' }, ...defs.map((d) => ({ value: d.id, label: `${d.kind === 'workflow' ? '作业' : '转换'} · ${d.name}` }))]} />
          </Field>
          <Field label="名称（可选）"><input className="input" value={s.name} onChange={(e) => set({ name: e.target.value })} placeholder="如 夜间全量" /></Field>
        </div>
        <Field label="执行时间">
          <div className="etl-cron-presets" style={{ marginBottom: 8 }}>
            {PRESETS.map((p) => <button key={p.cron} type="button" className="etl-cron-preset" data-active={String(p.cron === s.cron)} onClick={() => set({ cron: p.cron })}>{p.label}</button>)}
          </div>
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 190px', gap: 10 }}>
            <input className="input mono" value={s.cron} onChange={(e) => set({ cron: e.target.value })} placeholder="分 时 日 月 周" />
            <Select value={s.timezone} onChange={(timezone) => set({ timezone })} options={tzs.map((z) => ({ value: z, label: z || 'Console 本地时区' }))} />
          </div>
          <div className="field-hint">5 段 cron（分 时 日 月 周），也支持 6 段（含秒）与 @daily、@every 30m。</div>
        </Field>
        <div className="etl-cron-box">
          {prev == null ? <Spinner size={12} /> : prev.valid ? (
            <React.Fragment>
              <div className="etl-cron-desc">{prev.description}</div>
              <div className="etl-cron-next">{(prev.next || []).map((t) => <span key={t}>{fmtDateTime(t)}</span>)}</div>
            </React.Fragment>
          ) : <div style={{ color: 'var(--error)', fontSize: 13 }}>{prev.error}</div>}
        </div>
        <div className="etl-row2">
          <Field label="上次还没结束时" hint={s.overlap === 'queue' ? '等上次结束后再运行（最多排队 1 个）' : s.overlap === 'parallel' ? '同时运行，注意目标表并发写入' : '本次不运行，记录在日志中'}>
            <Seg value={s.overlap} onChange={(overlap) => set({ overlap })} options={Object.entries(OVERLAP).map(([value, label]) => ({ value, label }))} />
          </Field>
          <Field label="Console 停机错过时" hint={s.misfire === 'run_once' ? '启动后补跑一次' : '直接等下一次'}>
            <Seg value={s.misfire} onChange={(misfire) => set({ misfire })} options={[{ value: 'skip', label: '跳过' }, { value: 'run_once', label: '补跑一次' }]} />
          </Field>
        </div>
        {params.length ? (
          <Field label="参数覆盖" hint="留空则使用定义中的默认值">
            <div className="etl-list">
              {params.map((p) => (
                <div key={p.name} className="etl-list-row" style={{ gridTemplateColumns: '150px 1fr' }}>
                  <span className="mono" style={{ fontSize: 12 }}>{p.name} <span className="etl-muted">{KIND_LABEL[p.kind]}</span></span>
                  <input className="input" value={(s.params[p.name] && s.params[p.name].value) ?? ''} placeholder={p.default ? `默认 ${p.default.value}` : '无默认'}
                    onChange={(e) => {
                      const next = { ...s.params };
                      if (e.target.value === '') delete next[p.name]; else next[p.name] = makeValue(p.kind, e.target.value);
                      set({ params: next });
                    }} />
                </div>
              ))}
            </div>
          </Field>
        ) : null}
        <div className="etl-inline"><span>启用</span><Switch on={s.enabled} onChange={(enabled) => set({ enabled })} /></div>
      </div>
    </Dialog>
  );
}
