// 运行记录列表：状态筛选、按定义筛选、翻页；运行中的条目自动刷新。
import React from 'react';
import { Icon, Btn, Seg, Spinner, Select } from '../../components/primitives.jsx';
import { etlApi } from '../api.js';
import { RunStatus } from '../components.jsx';
import { fmtAgo, fmtDateTime, fmtDuration, fmtCompact, runDuration, TRIGGER_LABEL } from '../util.js';

const STATUS_FILTERS = [
  { value: '', label: '全部' },
  { value: 'running,queued', label: '进行中' },
  { value: 'failed,interrupted', label: '失败' },
  { value: 'succeeded', label: '成功' },
];

export function RunsPage({ go }) {
  const [status, setStatus] = React.useState('');
  const [defId, setDefId] = React.useState('');
  const [defs, setDefs] = React.useState([]);
  const [runs, setRuns] = React.useState(null);
  const [more, setMore] = React.useState(false);
  const [err, setErr] = React.useState('');
  const load = React.useCallback((before) => {
    etlApi.runs({ status, definitionId: defId, before, limit: 50 }).then((l) => {
      setErr('');
      setMore(l.length === 50);
      setRuns((cur) => (before ? [...(cur || []), ...l] : l));
    }).catch((e) => setErr(e.message));
  }, [status, defId]);
  React.useEffect(() => { setRuns(null); load(); }, [load]);
  React.useEffect(() => { etlApi.definitions().then(setDefs).catch(() => {}); }, []);
  const hasLive = (runs || []).some((r) => r.status === 'running' || r.status === 'queued');
  React.useEffect(() => {
    if (!hasLive) return undefined;
    const t = setInterval(() => load(), 3000);
    return () => clearInterval(t);
  }, [hasLive, load]);

  return (
    <div>
      <div className="etl-toolbar">
        <Seg value={status} onChange={setStatus} options={STATUS_FILTERS} />
        <div style={{ width: 240 }}>
          <Select value={defId} onChange={setDefId} options={[{ value: '', label: '全部作业与转换' }, ...defs.map((d) => ({ value: d.id, label: `${d.kind === 'workflow' ? '作业' : '转换'} · ${d.name}` }))]} />
        </div>
        <span style={{ flex: 1 }}></span>
        <Btn variant="ghost" icon="refresh" onClick={() => load()}>刷新</Btn>
      </div>
      <div className="card" style={{ overflow: 'hidden' }}>
        {err ? <div className="etl-list-empty" style={{ color: 'var(--error)' }}>{err}</div> : null}
        {!runs && !err ? <div className="etl-list-empty" style={{ padding: 40 }}><Spinner /></div> : null}
        {runs && !runs.length ? (
          <div className="etl-empty">
            <div style={{ display: 'inline-flex', padding: 14, borderRadius: 14, background: 'var(--muted)', color: 'var(--muted-fg)' }}><Icon name="activity" size={22} /></div>
            <h3>暂无运行记录</h3>
            <p>在设计器中点击「运行」，或为作业添加调度后，运行记录会出现在这里。</p>
          </div>
        ) : null}
        {runs && runs.length ? (
          <table className="table etl-table">
            <thead>
              <tr>
                <th style={{ width: 110 }}>状态</th><th>作业 / 转换</th><th style={{ width: 90 }}>触发</th><th style={{ width: 170 }}>开始</th>
                <th style={{ width: 110 }}>耗时</th><th style={{ width: 100, textAlign: 'right' }}>读取</th><th style={{ width: 100, textAlign: 'right' }}>写入</th>
              </tr>
            </thead>
            <tbody>
              {runs.map((r) => (
                <tr key={r.id} className="etl-row" onClick={() => go.run(r.id)}>
                  <td><RunStatus status={r.status} /></td>
                  <td>
                    <div className="etl-name">
                      <span className="etl-kind-chip" data-kind={r.kind} style={{ width: 26, height: 26, borderRadius: 7 }}><Icon name={r.kind === 'workflow' ? 'workflow' : 'pipeline'} size={13} /></span>
                      <div style={{ minWidth: 0 }}>
                        <div className="etl-name-title" style={{ fontSize: 13 }}>{r.definitionName}<span className="etl-muted" style={{ fontWeight: 400, marginLeft: 6 }}>v{r.revision}</span></div>
                        {r.error ? <div className="etl-name-desc" style={{ color: 'var(--error)' }} title={r.error}>{r.error}</div> : null}
                      </div>
                    </div>
                  </td>
                  <td className="etl-muted">{TRIGGER_LABEL[r.trigger] || r.trigger}</td>
                  <td className="etl-muted" title={fmtDateTime(r.startedAt || r.queuedAt, true)}>{fmtDateTime(r.startedAt || r.queuedAt)}<div style={{ fontSize: 11 }}>{fmtAgo(r.startedAt || r.queuedAt)}</div></td>
                  <td className="etl-num">{r.startedAt ? fmtDuration(runDuration(r)) : '—'}</td>
                  <td className="etl-num" style={{ textAlign: 'right' }}>{fmtCompact(r.rowsRead)}</td>
                  <td className="etl-num" style={{ textAlign: 'right' }}>{fmtCompact(r.rowsWritten)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : null}
        {more ? <div style={{ padding: 10, textAlign: 'center' }}><Btn variant="ghost" size="sm" onClick={() => load(runs[runs.length - 1].queuedAt)}>加载更多</Btn></div> : null}
      </div>
    </div>
  );
}
