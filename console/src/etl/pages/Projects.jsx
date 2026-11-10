// 工程：作业与转换列表、概览指标、新建/复制/删除/运行。
import React from 'react';
import { Icon, Btn, Seg, Spinner, Dialog, Field, toast, confirmDialog } from '../../components/primitives.jsx';
import { etlApi } from '../api.js';
import { RunStatus, Popover, usePopover, MenuItem } from '../components.jsx';
import { fmtAgo, fmtCompact, fmtDateTime } from '../util.js';

function useOverview() {
  const [ov, setOv] = React.useState(null);
  const load = React.useCallback(() => { etlApi.overview().then(setOv).catch(() => setOv(null)); }, []);
  React.useEffect(() => { load(); const t = setInterval(load, 15000); return () => clearInterval(t); }, [load]);
  return ov;
}

function Stats({ ov }) {
  const r = (ov && ov.runs24h) || {};
  const total = Object.values(r).reduce((a, b) => a + b, 0);
  const items = [
    { label: '作业', icon: 'workflow', value: ov ? ov.workflows : '—', sub: '控制流 · 可调度' },
    { label: '转换', icon: 'pipeline', value: ov ? ov.pipelines : '—', sub: '数据流 · 步骤链' },
    { label: '启用的调度', icon: 'calendar', value: ov ? ov.schedulesEnabled : '—', sub: ov && ov.upcoming && ov.upcoming[0] ? `下次 ${fmtAgo(ov.upcoming.map((s) => s.nextFireAt).filter(Boolean).sort()[0])}` : '暂无' },
    {
      label: '近 24 小时运行', icon: 'activity', value: ov ? total : '—',
      sub: ov ? (<span><span style={{ color: 'var(--success)' }}>{r.succeeded || 0} 成功</span>{r.failed ? <span style={{ color: 'var(--error)' }}> · {r.failed} 失败</span> : null}{ov.active && ov.active.length ? <span style={{ color: 'var(--info)' }}> · {ov.active.length} 运行中</span> : null}</span>) : '',
    },
    { label: '近 24 小时写入', icon: 'database', value: ov ? fmtCompact(ov.rowsWritten24h) : '—', sub: '插入 + 更新的行' },
  ];
  return (
    <div className="etl-stats">
      {items.map((s) => (
        <div key={s.label} className="card etl-stat">
          <div className="etl-stat-label"><Icon name={s.icon} size={13} />{s.label}</div>
          <div className="etl-stat-value">{s.value}</div>
          <div className="etl-stat-sub">{s.sub}</div>
        </div>
      ))}
    </div>
  );
}

export function ProjectsPage({ go }) {
  const ov = useOverview();
  const [list, setList] = React.useState(null);
  const [err, setErr] = React.useState('');
  const [filter, setFilter] = React.useState('all');
  const [kw, setKw] = React.useState('');
  const [creating, setCreating] = React.useState(null); // 'workflow' | 'pipeline'
  const newMenu = usePopover();
  const load = React.useCallback(() => {
    etlApi.definitions().then((l) => { setList(l); setErr(''); }).catch((e) => setErr(e.message));
  }, []);
  React.useEffect(() => { load(); }, [load]);

  const shown = (list || []).filter((d) => (filter === 'all' || d.kind === filter)
    && (!kw || (d.name + d.description + d.folder).toLowerCase().includes(kw.toLowerCase())));

  const run = async (d, e) => {
    e.stopPropagation();
    try {
      const r = await etlApi.run(d.id, {});
      toast(`已启动「${d.name}」`, { icon: 'play' });
      go.run(r.id);
    } catch (x) { toast(x.message, { tone: 'error' }); }
  };

  return (
    <div>
      <Stats ov={ov} />
      <div className="etl-toolbar">
        <Seg value={filter} onChange={setFilter} options={[{ value: 'all', label: '全部' }, { value: 'workflow', label: '作业' }, { value: 'pipeline', label: '转换' }]} />
        <div className="etl-search">
          <Icon name="search" size={14} />
          <input className="input" placeholder="搜索名称或描述" value={kw} onChange={(e) => setKw(e.target.value)} />
        </div>
        <span style={{ flex: 1 }}></span>
        <Btn variant="primary" icon="plus" onClick={newMenu.open}>新建<Icon name="chevronD" size={13} /></Btn>
        <Popover anchor={newMenu.anchor} onClose={newMenu.close} width={260} align="right">
          <MenuItem icon="workflow" label="新建作业" sub="编排执行顺序、条件分支与调度" onClick={() => { newMenu.close(); setCreating('workflow'); }} />
          <MenuItem icon="pipeline" label="新建转换" sub="在步骤之间流动与变换数据" onClick={() => { newMenu.close(); setCreating('pipeline'); }} />
        </Popover>
      </div>

      <div className="card" style={{ overflow: 'hidden' }}>
        {err ? <div className="etl-list-empty" style={{ color: 'var(--error)' }}>{err} <Btn variant="ghost" size="sm" onClick={load}>重试</Btn></div> : null}
        {!list && !err ? <div className="etl-list-empty" style={{ padding: 40 }}><Spinner /></div> : null}
        {list && !list.length ? <Empty onCreate={setCreating} /> : null}
        {list && list.length ? (
          <table className="table etl-table">
            <thead>
              <tr>
                <th>名称</th><th style={{ width: 90 }}>组件</th><th style={{ width: 170 }}>最近运行</th><th style={{ width: 150 }}>下次调度</th><th style={{ width: 150 }}>修改</th><th style={{ width: 120 }}></th>
              </tr>
            </thead>
            <tbody>
              {shown.map((d) => (
                <tr key={d.id} className="etl-row" onClick={() => go.designer(d.id)}>
                  <td>
                    <div className="etl-name">
                      <span className="etl-kind-chip" data-kind={d.kind}><Icon name={d.kind === 'workflow' ? 'workflow' : 'pipeline'} size={15} /></span>
                      <div style={{ minWidth: 0 }}>
                        <div className="etl-name-title">{d.name}</div>
                        <div className="etl-name-desc">{d.kind === 'workflow' ? '作业' : '转换'}{d.description ? ` · ${d.description}` : ''}</div>
                      </div>
                    </div>
                  </td>
                  <td className="etl-muted">{d.kind === 'workflow' ? Math.max(0, d.nodeCount - 1) : d.nodeCount} 个</td>
                  <td>
                    {d.lastRunId ? (
                      <button type="button" style={{ display: 'flex', alignItems: 'center', gap: 8, cursor: 'pointer' }} onClick={(e) => { e.stopPropagation(); go.run(d.lastRunId); }}>
                        <RunStatus status={d.lastRunStatus} /><span className="etl-muted">{fmtAgo(d.lastRunAt)}</span>
                      </button>
                    ) : <span className="etl-muted">从未运行</span>}
                  </td>
                  <td className="etl-muted" title={d.nextFireAt ? fmtDateTime(d.nextFireAt) : ''}>
                    {d.nextFireAt ? <span><Icon name="clock" size={12} style={{ verticalAlign: -2, marginRight: 4 }} />{fmtAgo(d.nextFireAt)}</span> : d.scheduleCount ? '调度已停用' : '—'}
                  </td>
                  <td className="etl-muted" title={fmtDateTime(d.updatedAt, true)}>{fmtAgo(d.updatedAt)} · {d.updatedBy}</td>
                  <td>
                    <div className="etl-actions" onClick={(e) => e.stopPropagation()}>
                      <Btn variant="ghost" size="sm" icon="play" title="运行" onClick={(e) => run(d, e)}></Btn>
                      <Btn variant="ghost" size="sm" icon="pencil" title="编辑" onClick={() => go.designer(d.id)}></Btn>
                      <RowMenu d={d} onChanged={load} />
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : null}
        {list && list.length && !shown.length ? <div className="etl-list-empty">没有匹配的项</div> : null}
      </div>
      {creating ? <CreateDialog kind={creating} onClose={() => setCreating(null)} onCreated={(d) => { setCreating(null); go.designer(d.id); }} /> : null}
    </div>
  );
}

function RowMenu({ d, onChanged }) {
  const pop = usePopover();
  const del = async () => {
    pop.close();
    const ok = await confirmDialog({ title: `删除「${d.name}」`, tone: 'danger', confirmText: '删除', message: '定义、历史版本与调度将被删除；运行记录保留。此操作不可撤销。' });
    if (!ok) return;
    try {
      await etlApi.deleteDefinition(d.id);
    } catch (e) {
      if (e.code === 'in_use') {
        const force = await confirmDialog({ title: '仍被引用', tone: 'danger', confirmText: '仍然删除', message: `${e.message}\n\n删除后这些作业运行到该步骤时会失败。` });
        if (!force) return;
        await etlApi.deleteDefinition(d.id, true);
      } else { toast(e.message, { tone: 'error' }); return; }
    }
    toast('已删除', { icon: 'trash' });
    onChanged();
  };
  return (
    <React.Fragment>
      <Btn variant="ghost" size="sm" icon="more" title="更多" onClick={pop.open}></Btn>
      <Popover anchor={pop.anchor} onClose={pop.close} width={180} align="right">
        <MenuItem icon="copy" label="复制" onClick={async () => { pop.close(); try { await etlApi.duplicate(d.id); toast('已复制'); onChanged(); } catch (e) { toast(e.message, { tone: 'error' }); } }} />
        <div className="etl-menu-sep"></div>
        <MenuItem icon="trash" label="删除" danger onClick={del} />
      </Popover>
    </React.Fragment>
  );
}

function Empty({ onCreate }) {
  return (
    <div className="etl-empty">
      <div style={{ display: 'inline-flex', padding: 14, borderRadius: 14, background: 'var(--primary-soft)', color: 'var(--primary)' }}><Icon name="flow" size={24} /></div>
      <h3>还没有作业或转换</h3>
      <p>典型用法：建一个转换描述「从哪读、怎么变、写到哪」，再建一个作业在合适的条件下调度它。</p>
      <div className="etl-concepts">
        <div className="etl-concept">
          <div className="etl-concept-title"><span className="etl-kind-chip" data-kind="pipeline" style={{ width: 26, height: 26 }}><Icon name="pipeline" size={14} /></span>转换</div>
          <p className="etl-muted">数据流。记录在步骤间流动：表输入 → 表达式 → 过滤 → 插入/更新。适合单表同步、清洗、合并。</p>
          <Btn variant="outline" size="sm" icon="plus" style={{ marginTop: 10 }} onClick={() => onCreate('pipeline')}>新建转换</Btn>
        </div>
        <div className="etl-concept">
          <div className="etl-concept-title"><span className="etl-kind-chip" data-kind="workflow" style={{ width: 26, height: 26 }}><Icon name="workflow" size={14} /></span>作业</div>
          <p className="etl-muted">控制流。按结果分支执行：取最大时间 → 判断增量是否足够 → 运行转换 → 失败时处理。可定时调度。</p>
          <Btn variant="outline" size="sm" icon="plus" style={{ marginTop: 10 }} onClick={() => onCreate('workflow')}>新建作业</Btn>
        </div>
      </div>
    </div>
  );
}

function CreateDialog({ kind, onClose, onCreated }) {
  const [name, setName] = React.useState('');
  const [desc, setDesc] = React.useState('');
  const [busy, setBusy] = React.useState(false);
  const submit = async () => {
    if (!name.trim()) { toast('请填写名称', { tone: 'warn' }); return; }
    setBusy(true);
    try { onCreated(await etlApi.createDefinition({ kind, name: name.trim(), description: desc })); } catch (e) { toast(e.message, { tone: 'error' }); setBusy(false); }
  };
  return (
    <Dialog open onClose={onClose} width={480} title={kind === 'workflow' ? '新建作业' : '新建转换'}
      desc={kind === 'workflow' ? '作业编排执行顺序与条件分支，可被调度' : '转换描述数据从哪里读、如何变换、写到哪里'}
      foot={<React.Fragment><Btn variant="ghost" onClick={onClose}>取消</Btn><Btn variant="primary" icon="check" disabled={busy} onClick={submit}>{busy ? <Spinner size={12} /> : null}创建并打开</Btn></React.Fragment>}>
      <form onSubmit={(e) => { e.preventDefault(); submit(); }} style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        <Field label="名称"><input className="input" autoFocus value={name} onChange={(e) => setName(e.target.value)} placeholder={kind === 'workflow' ? '如 订单增量同步作业' : '如 订单 B→A 增量'} /></Field>
        <Field label="描述（可选）"><textarea className="textarea" rows={2} value={desc} onChange={(e) => setDesc(e.target.value)} /></Field>
      </form>
    </Dialog>
  );
}
