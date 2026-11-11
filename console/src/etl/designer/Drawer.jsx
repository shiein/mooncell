// 底部抽屉：校验问题 · 数据预览 · 本次运行（实时度量 + 日志）。
import React from 'react';
import { Icon, Btn, Tabs, Spinner, Select, Badge } from '../../components/primitives.jsx';
import { RunStatus } from '../components.jsx';
import { KIND_LABEL, NODE_STATUS, fmtNum, fmtRate, fmtDuration, fmtClock } from '../util.js';

export function Drawer({ d }) {
  const { drawer, setDrawer } = d;
  const errs = (d.allProblems || []).filter((p) => p.severity === 'error').length;
  const warns = (d.allProblems || []).length - errs;
  const tabs = [
    { id: 'problems', label: '问题', count: (d.allProblems || []).length || undefined },
    ...(d.kind === 'pipeline' && !d.readOnly ? [{ id: 'preview', label: '数据预览' }] : []),
    { id: 'run', label: d.live.runId ? '本次运行' : '运行' },
  ];
  return (
    <div className="etl-drawer" style={{ height: drawer.open ? 280 : 38 }}>
      <div className="etl-drawer-bar">
        <Tabs tabs={tabs} active={drawer.open ? drawer.tab : ''} onChange={(tab) => setDrawer({ open: !(drawer.open && drawer.tab === tab), tab })} />
        <span style={{ fontSize: 12, color: 'var(--muted-fg)', marginLeft: 6, display: 'inline-flex', gap: 10 }}>
          {errs ? <span style={{ color: 'var(--error)' }}>● {errs} 个错误</span> : null}
          {warns ? <span style={{ color: 'var(--warn)' }}>● {warns} 个提示</span> : null}
          {!errs && !warns && d.validated ? <span style={{ color: 'var(--success)' }}>● 校验通过</span> : null}
          {d.validating ? <span><Spinner size={10} /> 校验中</span> : null}
        </span>
        <span style={{ flex: 1 }}></span>
        {d.live.runId ? <RunStatus status={d.live.run ? d.live.run.status : 'queued'} /> : null}
        <Btn variant="ghost" size="sm" icon={drawer.open ? 'chevronD' : 'chevronR'} title={drawer.open ? '收起' : '展开'}
          onClick={() => setDrawer({ ...drawer, open: !drawer.open })}></Btn>
      </div>
      {drawer.open ? (
        <div className="etl-drawer-body">
          {drawer.tab === 'problems' ? <ProblemsTab d={d} /> : null}
          {drawer.tab === 'preview' ? <PreviewTab d={d} /> : null}
          {drawer.tab === 'run' ? <RunTab d={d} /> : null}
        </div>
      ) : null}
    </div>
  );
}

function ProblemsTab({ d }) {
  const list = d.allProblems || [];
  if (!list.length) {
    return <div className="etl-list-empty" style={{ padding: 32 }}>{d.validated ? '没有发现问题，可以运行。' : '编辑后会自动校验。'}</div>;
  }
  const nameOf = (id) => (d.doc.nodes.find((n) => n.id === id) || {}).name;
  return (
    <table className="table" style={{ fontSize: 12.5 }}>
      <tbody>
        {list.map((p, i) => (
          <tr key={i} style={{ cursor: p.nodeId ? 'pointer' : 'default' }} onClick={() => p.nodeId && d.focusNode(p.nodeId)}>
            <td style={{ width: 28, paddingRight: 0 }}><Icon name="alert" size={14} style={{ color: p.severity === 'error' ? 'var(--error)' : 'var(--warn)' }} /></td>
            <td style={{ width: 180, fontWeight: 600 }}>{p.nodeId ? nameOf(p.nodeId) || p.nodeId : '定义'}</td>
            <td>{p.message}</td>
            <td style={{ width: 120 }} className="etl-muted mono">{p.code}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function PreviewTab({ d }) {
  const sel = d.selection.type === 'node' ? d.doc.nodes.find((n) => n.id === d.selection.id) : null;
  const [limit, setLimit] = React.useState('100');
  const [port, setPort] = React.useState('main');
  const p = d.preview;
  React.useEffect(() => {
    if (p.result && !p.result.ports[port]) setPort(Object.keys(p.result.ports)[0] || 'main');
  }, [p.result, port]);
  if (!sel) return <div className="etl-list-empty" style={{ padding: 32 }}>在画布上选中一个步骤，查看流经它的数据（只运行该步骤及其上游，输出步骤不会写库）。</div>;
  const res = p.nodeId === sel.id ? p.result : null;
  const data = res && res.ports[port];
  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '8px 12px', borderBottom: '1px solid var(--border)', flex: 'none' }}>
        <span style={{ fontWeight: 600, fontSize: 12.5 }}>{sel.name}</span>
        {res ? Object.keys(res.ports).map((k) => (
          <button key={k} type="button" className="etl-cron-preset" data-active={String(k === port)} onClick={() => setPort(k)}>
            {k === 'main' ? '输出' : k === 'error' ? '错误' : k === 'true' ? '是' : k === 'false' ? '否' : k} · {res.ports[k].rows ? res.ports[k].rows.length : 0}
          </button>
        )) : null}
        <span style={{ flex: 1 }}></span>
        <span className="etl-muted">最多</span>
        <div style={{ width: 90 }}><Select value={limit} onChange={setLimit} options={['20', '100', '500', '1000']} /></div>
        <Btn variant="outline" size="sm" icon={p.loading ? undefined : 'eye'} disabled={p.loading} onClick={() => d.runPreview(sel.id, Number(limit))}>
          {p.loading ? <Spinner size={12} /> : null}{p.loading ? '预览中' : '预览'}
        </Btn>
      </div>
      {(d.doc.params || []).length ? (
        <div style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '5px 12px', borderBottom: '1px solid var(--border)', flex: 'none', overflowX: 'auto' }}>
          <span className="etl-muted" style={{ fontSize: 12, flex: 'none' }}>参数</span>
          {d.doc.params.map((prm) => (
            <label key={prm.name} style={{ display: 'inline-flex', alignItems: 'center', gap: 5, flex: 'none', fontSize: 12 }}
              title={`${KIND_LABEL[prm.kind] || prm.kind}${prm.description ? ' · ' + prm.description : ''}`}>
              <span className="mono" style={{ color: 'var(--fg)' }}>{prm.name}{prm.required ? '*' : ''}</span>
              <input className="input mono" style={{ width: 130, fontSize: 12, padding: '3px 8px' }}
                placeholder={prm.default && prm.default.value != null ? `默认 ${prm.default.value}` : KIND_LABEL[prm.kind] || ''}
                value={d.previewVals[prm.name] || ''}
                onChange={(e) => d.setPreviewVals((s) => ({ ...s, [prm.name]: e.target.value }))} />
            </label>
          ))}
          <span className="etl-muted" style={{ fontSize: 11.5, flex: 'none' }}>留空使用默认值</span>
        </div>
      ) : null}
      {p.error && p.nodeId === sel.id ? <div style={{ padding: 12, color: 'var(--error)', fontSize: 12.5 }}>{p.error}</div> : null}
      {res && res.error ? <div style={{ padding: '8px 12px', color: 'var(--error)', fontSize: 12.5 }}>{res.error}</div> : null}
      {data ? (
        <div style={{ flex: 1, overflow: 'auto' }}>
          <table className="table etl-preview-table">
            <thead>
              <tr>
                <th style={{ width: 44 }}>#</th>
                {(data.fields || []).map((f) => <th key={f.name} title={KIND_LABEL[f.kind]}>{f.name} <span className="etl-kind-tag">{f.kind}</span></th>)}
              </tr>
            </thead>
            <tbody>
              {(data.rows || []).map((r, i) => (
                <tr key={i}>
                  <td className="etl-muted">{i + 1}</td>
                  {(r || []).map((v, j) => <td key={j} title={v}>{(data.nulls[i] || [])[j] ? <span className="etl-null">null</span> : v}</td>)}
                </tr>
              ))}
            </tbody>
          </table>
          {!data.rows || !data.rows.length ? <div className="etl-list-empty">没有数据</div> : null}
          {data.truncated ? <div className="etl-list-empty">已截断为前 {data.rows.length} 行</div> : null}
        </div>
      ) : !p.loading && !(p.error && p.nodeId === sel.id) ? <div className="etl-list-empty" style={{ padding: 28 }}>点击「预览」运行到该步骤为止</div> : null}
    </div>
  );
}

function RunTab({ d }) {
  const { live } = d;
  if (!live.runId) {
    return <div className="etl-list-empty" style={{ padding: 32 }}>点击右上角「运行」后，这里实时显示每个{d.kind === 'workflow' ? '作业项' : '步骤'}的状态、行数与日志。</div>;
  }
  const nodes = d.doc.nodes.map((n) => ({ n, s: live.nodes[n.id] || {} }));
  return (
    <div style={{ display: 'grid', gridTemplateColumns: 'minmax(0, 1.2fr) minmax(0, 1fr)', height: '100%' }}>
      <div style={{ overflow: 'auto', borderRight: '1px solid var(--border)' }}>
        <table className="table" style={{ fontSize: 12 }}>
          <thead>
            <tr>
              <th>{d.kind === 'workflow' ? '作业项' : '步骤'}</th><th>状态</th>
              {d.kind === 'pipeline' ? <React.Fragment><th style={{ textAlign: 'right' }}>读</th><th style={{ textAlign: 'right' }}>写</th><th style={{ textAlign: 'right' }}>输出/更新</th><th style={{ textAlign: 'right' }}>拒绝</th><th style={{ textAlign: 'right' }}>速度</th></React.Fragment>
                : <th>结果</th>}
            </tr>
          </thead>
          <tbody>
            {nodes.map(({ n, s }) => {
              const m = s.metrics || {};
              const st = NODE_STATUS[s.status] || { label: s.status || '—', tone: 'default' };
              return (
                <tr key={n.id} style={{ cursor: 'pointer', whiteSpace: 'nowrap' }} onClick={() => d.focusNode(n.id)}>
                  <td style={{ fontWeight: 600, maxWidth: 200, overflow: 'hidden', textOverflow: 'ellipsis' }} title={n.name}>{n.name}</td>
                  <td><Badge tone={st.tone}>{st.label}</Badge></td>
                  {d.kind === 'pipeline' ? (
                    <React.Fragment>
                      <td className="etl-num" style={{ textAlign: 'right' }}>{fmtNum(Math.max(m.read || 0, m.input || 0))}</td>
                      <td className="etl-num" style={{ textAlign: 'right' }}>{fmtNum(m.written || 0)}</td>
                      <td className="etl-num" style={{ textAlign: 'right' }}>{fmtNum((m.output || 0))}{m.updated ? ` / ${fmtNum(m.updated)}` : ''}</td>
                      <td className="etl-num" style={{ textAlign: 'right', color: m.rejected ? 'var(--error)' : undefined }}>{fmtNum(m.rejected || 0)}</td>
                      <td className="etl-num" style={{ textAlign: 'right' }}>{s.status === 'running' ? fmtRate(m.rowsPerSec) : fmtDuration(m.durationMs)}</td>
                    </React.Fragment>
                  ) : <td className="etl-muted" style={{ maxWidth: 260, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{s.error || s.message || ''}</td>}
                </tr>
              );
            })}
          </tbody>
        </table>
        <div style={{ padding: '8px 12px' }}>
          <Btn variant="ghost" size="sm" icon="arrowR" onClick={() => d.openRun(live.runId)}>打开运行详情</Btn>
        </div>
      </div>
      <div className="console" style={{ fontSize: 11.5 }}>
        {(live.logs || []).slice(-400).map((l) => (
          <span key={l.seq} className="ln">
            <span className="ts">{fmtClock(l.ts)} </span>
            <span className={`lv-${(l.level || 'info').toUpperCase() === 'DETAIL' ? 'DEBUG' : (l.level || 'info').toUpperCase()}`}>{(l.level || '').padEnd(6)}</span>
            {l.nodeId ? <span style={{ color: '#9A9378' }}>[{(d.doc.nodes.find((n) => n.id === l.nodeId) || {}).name || l.nodeId}] </span> : null}
            {l.message}
          </span>
        ))}
        {!(live.logs || []).length ? <span className="ts">等待日志…</span> : null}
      </div>
    </div>
  );
}
