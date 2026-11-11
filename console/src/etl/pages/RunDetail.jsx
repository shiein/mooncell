// 运行详情：概要、作业时间线（甘特）、步骤度量（Kettle 列）、日志（级别/节点过滤、搜索高亮、实时跟随）。
import React from 'react';
import { Icon, Btn, Spinner, Seg, Select, Badge, CopyChip, Dialog, toast, confirmDialog } from '../../components/primitives.jsx';
import { etlApi, subscribeRun } from '../api.js';
import { RunStatus } from '../components.jsx';
import { NODE_STATUS, TRIGGER_LABEL, fmtDateTime, fmtDuration, fmtNum, fmtRate, fmtClock, runDuration } from '../util.js';

export function RunDetailPage({ runId, go }) {
  const [data, setData] = React.useState(null);
  const [err, setErr] = React.useState('');
  const [logs, setLogs] = React.useState([]);
  const [focus, setFocus] = React.useState(null); // 作业中选中的「运行转换」作业项
  const [sample, setSample] = React.useState(null); // { node, sample } 拒绝行采样弹窗
  const lastSeq = React.useRef(0);

  const loadDetail = React.useCallback(() => etlApi.runDetail(runId).then((d) => { setData(d); setErr(''); return d; }).catch((e) => setErr(e.message)), [runId]);
  const loadLogs = React.useCallback(async () => {
    const l = await etlApi.runLogs(runId, { after: lastSeq.current, limit: 5000 });
    if (l.length) {
      lastSeq.current = l[l.length - 1].seq;
      setLogs((cur) => [...cur, ...l]);
    }
  }, [runId]);

  React.useEffect(() => {
    setData(null); setLogs([]); lastSeq.current = 0;
    let unsub = null;
    loadDetail().then((d) => {
      loadLogs();
      if (d && d.live) {
        unsub = subscribeRun(runId, {
          node: (n) => setData((cur) => cur && ({ ...cur, nodes: upsertNode(cur.nodes, n) })),
          metrics: ({ parent, metrics }) => setData((cur) => cur && ({ ...cur, nodes: cur.nodes.map((n) => (n.parentNode === parent && metrics[n.nodeId] ? { ...n, metrics: metrics[n.nodeId] } : n)) })),
          status: (r) => setData((cur) => cur && ({ ...cur, run: { ...cur.run, ...r } })),
          log: (e) => {
            if (e.seq > lastSeq.current) { lastSeq.current = e.seq; setLogs((cur) => [...cur.slice(-20000), e]); }
          },
          end: () => { setTimeout(() => { loadDetail(); loadLogs(); }, 300); },
        });
      }
    });
    return () => { if (unsub) unsub(); };
  }, [runId, loadDetail, loadLogs]);

  if (err) return <div className="card etl-list-empty" style={{ padding: 40 }}>{err} <Btn variant="ghost" size="sm" onClick={() => go.back('runs')}>返回</Btn></div>;
  if (!data) return <div className="etl-list-empty" style={{ padding: 60 }}><Spinner /></div>;
  const { run, nodes } = data;
  const top = nodes.filter((n) => !n.parentNode);
  const children = (parent) => nodes.filter((n) => n.parentNode === parent);
  const live = run.status === 'running' || run.status === 'queued';
  const canResume = run.kind === 'workflow' && ['failed', 'interrupted', 'canceled'].includes(run.status);

  const cancel = async () => {
    const ok = await confirmDialog({ title: '取消运行', message: '正在写入的事务会回滚，已提交的批次保留。', confirmText: '取消运行', tone: 'danger' });
    if (!ok) return;
    try { await etlApi.cancelRun(run.id); toast('已请求取消'); } catch (e) { toast(e.message, { tone: 'error' }); }
  };
  const resume = async () => {
    try { const r = await etlApi.resumeRun(run.id); toast('已从失败处继续'); go.run(r.id); } catch (e) { toast(e.message, { tone: 'error' }); }
  };
  const rerun = async () => {
    try { const r = await etlApi.run(run.definitionId, paramsOf(run)); toast('已重新运行'); go.run(r.id); } catch (e) { toast(e.message, { tone: 'error' }); }
  };

  // 在设计器中定位节点：父链向上找到顶层祖先，再沿「作业→转换/子作业」引用链
  // 向下解析出节点所属定义；被引定义已删除时报错不跳转。
  const locate = async (n) => {
    try {
      const chain = [];
      let cur = n;
      let guard = 0;
      while (cur.parentNode && guard++ < 32) {
        const p = nodes.find((x) => x.nodeId === cur.parentNode);
        if (!p) break;
        chain.unshift(p);
        cur = p;
      }
      let defId = run.definitionId;
      for (const anc of chain) {
        const def = await etlApi.definition(defId);
        const doc = def.doc || {};
        const node = (doc.nodes || []).find((x) => x.id === anc.nodeId);
        const next = node && node.config && (node.config.pipeline || node.config.workflow);
        if (!next) throw new Error(`节点「${anc.nodeName || anc.nodeId}」未引用定义`);
        defId = next;
      }
      go.designer(defId, n.nodeId);
    } catch (e) {
      toast(`定位失败：${e.message}`, { tone: 'error' });
    }
  };

  return (
    <div>
      <div className="etl-run-head">
        <Btn variant="ghost" size="sm" icon="chevronL" onClick={() => go.back('runs')}>运行记录</Btn>
        <div style={{ flex: 1, minWidth: 0 }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
            <span className="etl-kind-chip" data-kind={run.kind}><Icon name={run.kind === 'workflow' ? 'workflow' : 'pipeline'} size={15} /></span>
            <h2 style={{ fontSize: 19, letterSpacing: '-0.015em' }}>{run.definitionName}</h2>
            <RunStatus status={run.status} size="lg" />
          </div>
          <div style={{ display: 'flex', gap: 14, marginTop: 6, fontSize: 12.5, color: 'var(--muted-fg)', alignItems: 'center', flexWrap: 'wrap' }}>
            <CopyChip text={run.id} />
            <span>版本 v{run.revision}</span>
            <span>{TRIGGER_LABEL[run.trigger] || run.trigger} · {run.triggeredBy || '—'}</span>
            {run.resumeOf ? <button type="button" className="link-btn" onClick={() => go.run(run.resumeOf)}>续跑自 {run.resumeOf.slice(0, 12)}…</button> : null}
          </div>
        </div>
        <div style={{ display: 'flex', gap: 8 }}>
          <Btn variant="ghost" icon="pencil" onClick={() => go.designer(run.definitionId)}>打开设计器</Btn>
          {live ? <Btn variant="destructive-outline" icon="stop" onClick={cancel}>取消</Btn> : null}
          {canResume ? <Btn variant="outline" icon="rotate" onClick={resume}>从失败处继续</Btn> : null}
          {!live ? <Btn variant="primary" icon="play" onClick={rerun}>再次运行</Btn> : null}
        </div>
      </div>

      {run.error ? (
        <div className="card" style={{ padding: '12px 16px', marginBottom: 14, borderColor: 'var(--error)', background: 'var(--error-soft)', color: 'var(--error)', display: 'flex', gap: 10, fontSize: 13 }}>
          <Icon name="alert" size={16} style={{ flex: 'none', marginTop: 1 }} />
          <span style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{run.error}</span>
        </div>
      ) : null}

      <div className="card etl-run-meta">
        <div><label>开始</label><strong>{fmtDateTime(run.startedAt || run.queuedAt, true)}</strong></div>
        <div><label>结束</label><strong>{run.finishedAt ? fmtDateTime(run.finishedAt, true) : live ? '进行中…' : '—'}</strong></div>
        <div><label>耗时</label><strong>{run.startedAt ? fmtDuration(runDuration(run)) : '排队中'}</strong></div>
        <div><label>读取行</label><strong>{fmtNum(run.rowsRead || sum(nodes, 'input'))}</strong></div>
        <div><label>写入行</label><strong>{fmtNum(run.rowsWritten || sum(nodes, 'output') + sum(nodes, 'updated'))}</strong></div>
        <div><label>日志</label><strong>{fmtNum(run.logCount || logs.length)}{run.logDropped ? <span style={{ fontSize: 11.5, color: 'var(--warn)', fontWeight: 500 }}> · 截断 {fmtNum(run.logDropped)}</span> : null}</strong></div>
      </div>

      {run.kind === 'workflow' ? (
        <div className="card" style={{ marginBottom: 14, overflow: 'hidden' }}>
          <div style={{ padding: '12px 14px', borderBottom: '1px solid var(--border)', fontWeight: 600, fontSize: 13.5 }}>作业项时间线</div>
          <Gantt nodes={top} run={run} onPick={(n) => setFocus(focus === n.nodeId ? null : n.nodeId)} focus={focus} hasChildren={(id) => children(id).length > 0} locate={locate} />
          {focus && children(focus).length ? (
            <div style={{ borderTop: '1px solid var(--border)' }}>
              <div style={{ padding: '10px 14px', fontSize: 12.5, fontWeight: 600, background: 'var(--bg)' }}>「{(top.find((n) => n.nodeId === focus) || {}).nodeName}」中的步骤</div>
              <StepMetrics nodes={children(focus)} locate={locate} onSample={(n) => setSample({ node: n, sample: n.samples })} />
            </div>
          ) : null}
        </div>
      ) : (
        <div className="card" style={{ marginBottom: 14, overflow: 'hidden' }}>
          <div style={{ padding: '12px 14px', borderBottom: '1px solid var(--border)', fontWeight: 600, fontSize: 13.5 }}>步骤度量</div>
          <StepMetrics nodes={top} locate={locate} onSample={(n) => setSample({ node: n, sample: n.samples })} />
        </div>
      )}

      <LogPanel logs={logs} nodes={nodes} live={live} />
      {sample ? <SampleDialog node={sample.node} sample={sample.sample} onClose={() => setSample(null)} /> : null}
    </div>
  );
}

function upsertNode(list, n) {
  const i = list.findIndex((x) => x.nodeId === n.nodeId && (x.parentNode || '') === (n.parentNode || ''));
  if (i < 0) return [...list, n];
  const next = list.slice();
  next[i] = { ...list[i], ...n };
  return next;
}

function sum(nodes, k) { return nodes.reduce((a, n) => a + ((n.metrics && n.metrics[k]) || 0), 0); }

function paramsOf(run) {
  try { return typeof run.params === 'string' ? JSON.parse(run.params) : (run.params || {}); } catch (e) { return {}; }
}

function Gantt({ nodes, run, onPick, focus, hasChildren, locate }) {
  const start = run.startedAt || run.queuedAt;
  const end = run.finishedAt || Date.now();
  const span = Math.max(1, end - start);
  const ordered = nodes.slice().sort((a, b) => (a.startedAt || Infinity) - (b.startedAt || Infinity));
  return (
    <div className="etl-gantt">
      {ordered.map((n) => {
        const st = NODE_STATUS[n.status] || { label: n.status, tone: 'default' };
        const s = n.startedAt ? ((n.startedAt - start) / span) * 100 : 0;
        const w = n.startedAt ? (((n.finishedAt || Date.now()) - n.startedAt) / span) * 100 : 0;
        const clickable = hasChildren(n.nodeId);
        return (
          <div key={n.nodeId} className="etl-gantt-row" data-clickable={String(clickable)} onClick={() => clickable && onPick(n)}
            style={focus === n.nodeId ? { background: 'var(--primary-soft)' } : undefined}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
              {clickable ? <Icon name={focus === n.nodeId ? 'chevronD' : 'chevronR'} size={12} style={{ color: 'var(--muted-fg)' }} /> : <span style={{ width: 12 }}></span>}
              <span style={{ fontWeight: 600, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{n.nodeName || n.nodeId}</span>
              <Badge tone={st.tone}>{st.label}</Badge>
              {n.status === 'failed' ? (
                <button type="button" className="link-btn" style={{ fontSize: 11.5, display: 'inline-flex', alignItems: 'center', gap: 3 }}
                  title="在设计器中定位该节点" onClick={(e) => { e.stopPropagation(); locate && locate(n); }}>
                  <Icon name="locate" size={12} />定位
                </button>
              ) : null}
              {n.attempt > 1 ? <span className="etl-muted">×{n.attempt}</span> : null}
            </div>
            <div className="etl-gantt-track" title={n.error || ''}>
              {n.startedAt ? <div className="etl-gantt-bar" data-status={n.status} style={{ left: `${s}%`, width: `${Math.max(w, 0.4)}%` }}></div> : null}
            </div>
            <div className="etl-num etl-muted" style={{ textAlign: 'right' }}>
              {n.startedAt && n.finishedAt ? fmtDuration(n.finishedAt - n.startedAt) : n.status === 'running' ? '运行中' : '—'}
            </div>
          </div>
        );
      })}
    </div>
  );
}

function StepMetrics({ nodes, locate, onSample }) {
  const cols = [['read', '读'], ['written', '写'], ['input', '输入'], ['output', '输出'], ['updated', '更新'], ['rejected', '拒绝'], ['errors', '错误']];
  return (
    <div style={{ overflowX: 'auto' }}>
      <table className="table" style={{ fontSize: 12.5 }}>
        <thead>
          <tr>
            <th>步骤</th><th style={{ width: 90 }}>状态</th>
            {cols.map(([, l]) => <th key={l} style={{ textAlign: 'right', width: 80 }}>{l}</th>)}
            <th style={{ textAlign: 'right', width: 90 }}>耗时</th><th style={{ textAlign: 'right', width: 100 }}>速度</th>
          </tr>
        </thead>
        <tbody>
          {nodes.map((n) => {
            const m = n.metrics || {};
            const st = NODE_STATUS[n.status] || { label: n.status, tone: 'default' };
            return (
              <tr key={n.nodeId}>
                <td>
                  <div style={{ fontWeight: 600, display: 'flex', alignItems: 'center', gap: 6 }}>
                    {n.nodeName || n.nodeId}
                    {n.status === 'failed' ? (
                      <button type="button" className="link-btn" style={{ fontSize: 11.5, display: 'inline-flex', alignItems: 'center', gap: 3 }}
                        title="在设计器中定位该节点" onClick={() => locate && locate(n)}>
                        <Icon name="locate" size={12} />定位
                      </button>
                    ) : null}
                  </div>
                  {n.error ? <div style={{ color: 'var(--error)', fontSize: 11.5, maxWidth: 360, whiteSpace: 'normal' }}>{n.error}</div> : <div className="etl-muted" style={{ fontSize: 11 }}>{n.nodeType}</div>}
                </td>
                <td><Badge tone={st.tone}>{st.label}</Badge></td>
                {cols.map(([k]) => (
                  <td key={k} className="etl-num" style={{ textAlign: 'right', color: (k === 'rejected' || k === 'errors') && m[k] ? 'var(--error)' : m[k] ? undefined : 'var(--muted-fg)' }}>
                    {k === 'rejected' && n.samples && n.samples.rows && n.samples.rows.length ? (
                      <button type="button" className="link-btn etl-num" title="查看拒绝行样本" onClick={() => onSample(n)}>{fmtNum(m[k] || 0)}</button>
                    ) : fmtNum(m[k] || 0)}
                  </td>
                ))}
                <td className="etl-num" style={{ textAlign: 'right' }}>{fmtDuration(m.durationMs)}</td>
                <td className="etl-num" style={{ textAlign: 'right' }}>{fmtRate(m.rowsPerSec) || '—'}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

const LEVEL_ORDER = { error: 1, warn: 2, info: 3, detail: 4, debug: 5 };

function LogPanel({ logs, nodes, live }) {
  const [level, setLevel] = React.useState('detail');
  const [node, setNode] = React.useState('');
  const [kw, setKw] = React.useState('');
  const [follow, setFollow] = React.useState(true);
  const ref = React.useRef(null);
  const names = React.useMemo(() => {
    const m = {};
    nodes.forEach((n) => { m[n.parentNode ? `${n.parentNode}/${n.nodeId}` : n.nodeId] = n.nodeName; });
    return m;
  }, [nodes]);
  const shown = logs.filter((l) => (LEVEL_ORDER[l.level] || 3) <= LEVEL_ORDER[level] && (!node || l.nodeId === node || (l.nodeId || '').startsWith(node + '/'))
    && (!kw || l.message.toLowerCase().includes(kw.toLowerCase())));
  React.useEffect(() => {
    if (follow && ref.current) ref.current.scrollTop = ref.current.scrollHeight;
  }, [shown.length, follow]);
  const nodeOpts = [{ value: '', label: '全部节点' }, ...nodes.filter((n) => !n.parentNode).map((n) => ({ value: n.nodeId, label: n.nodeName || n.nodeId }))];
  const hl = (msg) => {
    if (!kw) return msg;
    const i = msg.toLowerCase().indexOf(kw.toLowerCase());
    if (i < 0) return msg;
    return <React.Fragment>{msg.slice(0, i)}<mark>{msg.slice(i, i + kw.length)}</mark>{msg.slice(i + kw.length)}</React.Fragment>;
  };
  const download = () => {
    const text = logs.map((l) => `${new Date(l.ts).toISOString()} ${l.level.padEnd(6)} ${l.nodeId ? `[${names[l.nodeId] || l.nodeId}] ` : ''}${l.message}`).join('\n');
    const a = document.createElement('a');
    a.href = URL.createObjectURL(new Blob([text], { type: 'text/plain;charset=utf-8' }));
    a.download = 'etl-run.log';
    a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
  };
  return (
    <div className="card" style={{ overflow: 'hidden' }}>
      <div className="etl-log-tools">
        <span style={{ fontWeight: 600, fontSize: 13.5, marginRight: 6 }}>日志</span>
        {live ? <Badge tone="info" dot>实时</Badge> : null}
        <Seg value={level} onChange={setLevel} options={[{ value: 'error', label: '错误' }, { value: 'warn', label: '警告' }, { value: 'info', label: '基本' }, { value: 'detail', label: '详细' }, { value: 'debug', label: '调试' }]} />
        <div style={{ width: 180 }}><Select value={node} onChange={setNode} options={nodeOpts} /></div>
        <div className="etl-search" style={{ width: 200 }}>
          <Icon name="search" size={14} />
          <input className="input input-sm" placeholder="搜索日志" value={kw} onChange={(e) => setKw(e.target.value)} />
        </div>
        <span style={{ flex: 1 }}></span>
        <span className="etl-muted">{fmtNum(shown.length)} / {fmtNum(logs.length)} 条</span>
        <Btn variant={follow ? 'outline' : 'ghost'} size="sm" icon="arrowR" style={{ transform: 'none' }} onClick={() => setFollow(!follow)}>{follow ? '跟随中' : '跟随'}</Btn>
        <Btn variant="ghost" size="sm" icon="download" onClick={download}>下载</Btn>
      </div>
      <div ref={ref} className="console etl-log" onScroll={(e) => {
        const el = e.currentTarget;
        const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
        if (!atBottom && follow) setFollow(false);
      }}>
        {shown.map((l) => (
          <span key={l.seq} className="ln">
            <span className="ts">{fmtClock(l.ts)} </span>
            <span className={`lv-${l.level === 'detail' ? 'DEBUG' : l.level.toUpperCase()}`}>{l.level.toUpperCase().padEnd(6)}</span>
            {l.nodeId ? <span className="node">[{names[l.nodeId] || l.nodeId}] </span> : null}
            <span className={l.level === 'error' ? 'msg-error' : undefined}>{hl(l.message)}</span>
          </span>
        ))}
        {!shown.length ? <span className="ts">{logs.length ? '没有符合条件的日志' : live ? '等待日志…' : '没有日志'}</span> : null}
      </div>
    </div>
  );
}

// 拒绝行采样：error 端口前 N 条样本行（运行记录随节点保存）。
function SampleDialog({ node, sample, onClose }) {
  const fields = sample.fields || [];
  const rows = sample.rows || [];
  const cell = (v) => {
    if (v === null || v === undefined) return <span className="etl-null">null</span>;
    if (typeof v === 'object') return JSON.stringify(v);
    return String(v);
  };
  return (
    <Dialog open onClose={onClose} width={860} title={`拒绝行采样 · ${node.nodeName || node.nodeId}`}
      desc={sample.total > rows.length ? `共 ${fmtNum(sample.total)} 条被拒绝，仅采样前 ${rows.length} 条` : `共 ${rows.length} 条被拒绝`}>
      <div style={{ overflow: 'auto', maxHeight: '60vh' }}>
        <table className="table etl-preview-table" style={{ fontSize: 12.5 }}>
          <thead>
            <tr>
              <th style={{ width: 44 }}>#</th>
              {fields.map((f, i) => <th key={i}>{f}</th>)}
            </tr>
          </thead>
          <tbody>
            {rows.map((r, i) => (
              <tr key={i}>
                <td className="etl-muted">{i + 1}</td>
                {(r || []).map((v, j) => <td key={j} title={typeof v === 'object' && v !== null ? JSON.stringify(v) : v}>{cell(v)}</td>)}
              </tr>
            ))}
          </tbody>
        </table>
        {!rows.length ? <div className="etl-list-empty">没有采样行</div> : null}
      </div>
    </Dialog>
  );
}
