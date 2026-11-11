// 画布节点与连线渲染。节点只持有 id，内容从 DesignerContext 读取（文档为唯一权威）。
import React from 'react';
import { Handle, Position, BaseEdge, EdgeLabelRenderer, getBezierPath } from '@xyflow/react';
import { Icon, Spinner } from '../../components/primitives.jsx';
import { CATEGORY, PORT_TONE, NODE_STATUS, fmtCompact, fmtRate, fmtDuration } from '../util.js';

export const DesignerContext = React.createContext(null);

const PORT_EDGE = {
  success: { color: 'var(--success)' }, true: { color: 'var(--success)', label: '是' },
  false: { color: 'var(--warn)', label: '否' }, failure: { color: 'var(--error)', label: '失败', dash: true },
  always: { color: 'var(--muted-fg)', label: '无条件', dash: true }, error: { color: 'var(--error)', label: '错误行', dash: true },
};

function chipStyle(meta) {
  const c = CATEGORY[(meta && meta.category) || 'utility'] || CATEGORY.utility;
  return { background: c.soft, color: c.color };
}

function StatusIcon({ status }) {
  if (status === 'running') return <Spinner size={12} style={{ color: 'var(--info)' }} />;
  if (status === 'succeeded' || status === 'reused') return <Icon name="check" size={13} style={{ color: 'var(--success)' }} />;
  if (status === 'failed') return <Icon name="x" size={13} style={{ color: 'var(--error)' }} />;
  if (status === 'skipped') return <Icon name="minus" size={13} style={{ color: 'var(--muted-fg)' }} />;
  return null;
}

function ProblemBadge({ p }) {
  if (!p || (!p.errors && !p.warnings)) return null;
  const tone = p.errors ? 'error' : 'warning';
  return <span className="etl-node-badge" data-tone={tone} title={p.list.map((x) => x.message).join('\n')}>{p.errors || p.warnings}</span>;
}

function spread(i, n) { return `${((i + 1) / (n + 1)) * 100}%`; }

// 转换步骤节点
export function StepNode({ id, selected }) {
  const ctx = React.useContext(DesignerContext);
  const n = ctx.nodeById[id];
  if (!n) return null;
  const meta = ctx.catalog.meta('pipeline', n.type) || { label: n.type, inputs: [], outputs: [{ name: 'main', label: '输出' }] };
  const live = ctx.live.nodes[id];
  const m = live && live.metrics;
  const outs = meta.outputs || [];
  const showLabels = outs.length > 1 || (outs[0] && outs[0].name !== 'main');
  const rows = m ? Math.max(m.read || 0, m.input || 0) : 0;
  return (
    <div className="etl-node" data-selected={String(!!selected)} data-status={live ? live.status : undefined}
      style={{ minHeight: outs.length > 2 ? outs.length * 16 + 12 : undefined }}>
      <ProblemBadge p={ctx.problems[id]} />
      {(meta.inputs || []).map((p, i, arr) => (
        <Handle key={p.name} type="target" position={Position.Left} id={p.name} style={{ top: spread(i, arr.length) }} title={p.label} />
      ))}
      <div className="etl-node-main">
        <span className="etl-chip" style={chipStyle(meta)}><Icon name={meta.icon || 'circle'} size={15} /></span>
        <div className="etl-node-text">
          <div className="etl-node-name" title={n.name}>{n.name}</div>
          <div className="etl-node-type">{meta.label}</div>
        </div>
        {live ? <StatusIcon status={live.status} /> : null}
      </div>
      {m ? (
        <div className="etl-node-foot">
          <span>{fmtCompact(rows)} 行</span>
          {m.output || m.updated ? <span className="ok">↓{fmtCompact((m.output || 0) + (m.updated || 0))}</span> : null}
          {m.rejected ? <span className="bad">✕{fmtCompact(m.rejected)}</span> : null}
          <span style={{ marginLeft: 'auto' }}>{live.status === 'running' ? fmtRate(m.rowsPerSec) : fmtDuration(m.durationMs)}</span>
        </div>
      ) : null}
      {outs.map((p, i) => (
        <React.Fragment key={p.name}>
          <Handle type="source" position={Position.Right} id={p.name} style={{ top: spread(i, outs.length) }} data-tone={p.tone} title={p.label} />
          {showLabels ? <span className="etl-port-label" style={{ top: spread(i, outs.length), color: PORT_TONE[p.tone] || 'var(--muted-fg)' }}>{p.label}</span> : null}
        </React.Fragment>
      ))}
      {meta.errorPort ? (
        <Handle type="source" position={Position.Bottom} id="error" data-tone="error" className="etl-handle-error" title="错误输出：把处理失败的行拖到这里连出去" style={{ left: '82%' }} />
      ) : null}
    </div>
  );
}

// 作业项节点
export function ActionNode({ id, selected }) {
  const ctx = React.useContext(DesignerContext);
  const n = ctx.nodeById[id];
  if (!n) return null;
  const meta = ctx.catalog.meta('workflow', n.type) || { label: n.type, inputs: [{ name: 'main' }], outputs: [] };
  const live = ctx.live.nodes[id];
  const outs = meta.outputs || [];
  const st = live && NODE_STATUS[live.status];
  // 「成功 / 失败」由连线颜色即可区分，只有条件类端口（是 / 否 / 出错）才标注文字
  const labeled = outs.some((p) => p.name !== 'success' && p.name !== 'failure');
  return (
    <div className="etl-node" style={{ width: 208, minHeight: outs.length > 2 ? outs.length * 16 + 12 : undefined }}
      data-selected={String(!!selected)} data-status={live ? live.status : undefined}>
      <ProblemBadge p={ctx.problems[id]} />
      {(meta.inputs || []).length ? <Handle type="target" position={Position.Left} id="in" /> : null}
      <div className="etl-node-main">
        <span className="etl-chip" style={chipStyle(meta)}><Icon name={meta.icon || 'circle'} size={15} /></span>
        <div className="etl-node-text">
          <div className="etl-node-name" title={n.name}>{n.name}</div>
          <div className="etl-node-type">{meta.label}{n.retry && n.retry.max ? ` · 重试 ${n.retry.max}` : ''}</div>
        </div>
        {live ? <StatusIcon status={live.status} /> : null}
      </div>
      {live && live.status !== 'pending' ? (
        <div className="etl-node-foot" style={{ fontFamily: 'var(--font-ui)' }}>
          <span style={{ color: st && st.tone === 'error' ? 'var(--error)' : undefined }}>{st ? st.label : live.status}</span>
          {live.attempt > 1 ? <span>第 {live.attempt} 次</span> : null}
          <span style={{ marginLeft: 'auto', fontFamily: 'var(--font-mono)' }}>{live.finishedAt && live.startedAt ? fmtDuration(live.finishedAt - live.startedAt) : ''}</span>
        </div>
      ) : null}
      {outs.map((p, i) => (
        <React.Fragment key={p.name}>
          <Handle type="source" position={Position.Right} id={p.name} style={{ top: spread(i, outs.length) }} data-tone={p.tone} title={p.label} />
          {labeled ? <span className="etl-port-label" style={{ top: spread(i, outs.length), color: PORT_TONE[p.tone] || 'var(--muted-fg)' }}>{p.label}</span> : null}
        </React.Fragment>
      ))}
    </div>
  );
}

// 开始节点（胶囊）
export function StartNode({ id, selected }) {
  const ctx = React.useContext(DesignerContext);
  const n = ctx.nodeById[id];
  if (!n) return null;
  const live = ctx.live.nodes[id];
  return (
    <div className="etl-node etl-node-start" data-selected={String(!!selected)}>
      <div className="etl-node-main" style={{ padding: '9px 18px 9px 12px' }}>
        <Icon name="play" size={13} />
        <div className="etl-node-text">
          <div className="etl-node-name">{n.name}</div>
          <div className="etl-node-type">{ctx.scheduleCount ? `${ctx.scheduleCount} 个调度` : '手动 / 调度触发'}</div>
        </div>
        {live && live.status === 'succeeded' ? <Icon name="check" size={13} /> : null}
      </div>
      <Handle type="source" position={Position.Right} id="success" />
    </div>
  );
}

// 注释便签（doc.notes，伪节点：可拖动/选中/删除，不参与编译）
export function NoteNode({ id, selected }) {
  const ctx = React.useContext(DesignerContext);
  const nt = ctx.noteById[id];
  if (!nt) return null;
  const rows = Math.min(12, Math.max(2, String(nt.text || '').split('\n').length));
  return (
    <div className="etl-note" data-selected={String(!!selected)} style={{ width: nt.w || 200 }}>
      <div className="etl-note-head" title="拖动移动；右键删除">
        <Icon name="fileText" size={11} />
        <button type="button" className="etl-note-x nodrag" title="删除注释" onClick={() => ctx.removeNote(id)}><Icon name="x" size={11} /></button>
      </div>
      <textarea className="etl-note-text nodrag" value={nt.text || ''} placeholder="注释…" rows={rows} spellCheck={false}
        onChange={(e) => ctx.updateNote(id, { text: e.target.value })} />
    </div>
  );
}

// 连线：转换显示字段数 / 运行中行数；作业按端口着色并标注非成功分支。
export function EtlEdge(props) {
  const { id, sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition, source, target, sourceHandleId, selected } = props;
  const ctx = React.useContext(DesignerContext);
  const [path, lx, ly] = getBezierPath({ sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition, curvature: 0.32 });
  const port = sourceHandleId || 'main';
  const style = PORT_EDGE[port];
  const isWF = ctx.kind === 'workflow';
  const hop = ctx.hopById[id];
  let label = null;
  let liveLabel = false;
  const srcLive = ctx.live.nodes[source];
  const dstLive = ctx.live.nodes[target];
  if (!isWF && srcLive && srcLive.metrics) {
    // 流经本连线的行数：目标只有这一条入线时取目标读入数（精确，适用于多端口分流）；否则退化为源写出数
    const exact = dstLive && dstLive.metrics && ctx.inCount[target] === 1;
    const n = exact ? dstLive.metrics.read : srcLive.metrics.written;
    label = `${fmtCompact(n || 0)} 行`;
    liveLabel = srcLive.status === 'running';
  } else if (!isWF && ctx.hopFields[id]) {
    label = `${ctx.hopFields[id].length} 字段`;
  }
  if (style && style.label && (isWF || port === 'error' || port === 'true' || port === 'false')) {
    label = label ? `${style.label} · ${label}` : style.label;
  }
  const color = selected ? undefined : (style && (isWF || port === 'error') ? style.color : undefined);
  const dashed = (style && style.dash) || (hop && hop.disabled);
  return (
    <React.Fragment>
      <BaseEdge id={id} path={path} className={liveLabel ? 'etl-edge-live' : undefined}
        style={{ stroke: color, strokeDasharray: dashed ? '5 4' : undefined, opacity: hop && hop.disabled ? 0.45 : 1 }}
        markerEnd={props.markerEnd} interactionWidth={18} />
      {label ? (
        <EdgeLabelRenderer>
          <div className="etl-edge-label nodrag nopan" data-live={String(liveLabel)}
            style={{ transform: `translate(-50%, -50%) translate(${lx}px, ${ly}px)`, color: !selected && style && (isWF || port === 'error') ? style.color : undefined }}>
            {label}
          </div>
        </EdgeLabelRenderer>
      ) : null}
    </React.Fragment>
  );
}

export const nodeTypes = { step: StepNode, action: ActionNode, start: StartNode, note: NoteNode };
export const edgeTypes = { etl: EtlEdge };
