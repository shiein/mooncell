// 属性检查器：选中节点 → 组件配置；选中连线 → 连线属性；都未选 → 定义设置（参数、运行参数）。
import React from 'react';
import { Icon, Btn, Field, Switch, Seg, Select, Tabs } from '../../components/primitives.jsx';
import { formFor, RetryEditor } from '../forms.jsx';
import { FieldChip, KindSelect, ListEditor } from '../components.jsx';
import { CATEGORY, KIND_LABEL } from '../util.js';

function Problems({ list, onFocus }) {
  if (!list || !list.length) return null;
  return (
    <div className="etl-insp-problems">
      {list.map((p, i) => (
        <div key={i} className="etl-problem" data-sev={p.severity}>
          <Icon name="alert" size={13} />
          <button type="button" onClick={() => onFocus && onFocus(p)}>{p.message}</button>
        </div>
      ))}
    </div>
  );
}

function FieldList({ fields, empty }) {
  if (!fields || !fields.length) return <div className="etl-muted">{empty}</div>;
  return (
    <div className="etl-fields">
      {fields.map((f) => (
        <div key={f.name} className="etl-field-row">
          <span className="etl-kind-tag" style={{ textAlign: 'center' }}>{KIND_LABEL[f.kind] ? f.kind.slice(0, 4) : '?'}</span>
          <span style={{ minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={f.name}>
            {f.name}{f.nativeType ? <span className="etl-muted" style={{ marginLeft: 6, fontSize: 11 }}>{f.nativeType}</span> : null}
          </span>
          <span className="etl-muted" style={{ fontSize: 11.5 }}>{KIND_LABEL[f.kind] || f.kind}</span>
        </div>
      ))}
    </div>
  );
}

export function Inspector({ d }) {
  const { kind, catalog, selection } = d;
  if (selection.type === 'node') {
    const n = d.doc.nodes.find((x) => x.id === selection.id);
    if (n) return <NodeInspector d={d} node={n} />;
  }
  if (selection.type === 'edge') {
    const h = d.doc.hops.find((x) => x.id === selection.id);
    if (h) return <EdgeInspector d={d} hop={h} />;
  }
  return <DefinitionInspector d={d} />;
}

// RoBanner：只读授权提示（access=use）。
function RoBanner({ d }) {
  if (!d.readOnly) return null;
  return (
    <div className="etl-insp-ro" role="note">
      <Icon name="key" size={12} /> 只读授权：可查看与运行，修改需要编辑授权
    </div>
  );
}

function NodeInspector({ d, node }) {
  const { kind, catalog } = d;
  const meta = catalog.meta(kind, node.type) || { label: node.type, description: '' };
  const c = CATEGORY[meta.category] || CATEGORY.utility;
  const [tab, setTab] = React.useState('config');
  React.useEffect(() => { setTab('config'); }, [node.id]);
  const Form = formFor(kind, node.type);
  const probs = (d.problems[node.id] && d.problems[node.id].list) || [];
  const shape = d.nodeSchemas[node.id] || {};
  const input = (shape.inputs && shape.inputs.main) || [];
  const ctx = {
    catalog,
    input,
    inputs: shape.inputs || {},
    vars: d.vars,
    pipelines: d.pipelines,
    workflows: (d.workflows || []).filter((w) => w.id !== (d.meta && d.meta.id)), // 排除自身，防直接自引用
    schedules: d.scheduleCount,
    pathError: (path) => {
      const p = probs.find((x) => x.path === path && x.severity === 'error') || probs.find((x) => x.path && x.path.startsWith(path) && x.severity === 'error');
      return p ? p.message : '';
    },
  };
  const set = (patch, replace) => d.updateNode(node.id, (n) => ({ ...n, config: replace ? patch : { ...n.config, ...patch } }));
  const tabs = kind === 'pipeline'
    ? [{ id: 'config', label: '配置' }, { id: 'fields', label: '字段', count: (shape.outputs && shape.outputs.main ? shape.outputs.main.length : undefined) }]
    : [{ id: 'config', label: '配置' }, ...(node.type !== 'start' ? [{ id: 'advanced', label: '超时与重试' }] : [])];
  return (
    <aside className="etl-inspector">
      <div className="etl-insp-head">
        <span className="etl-chip" style={{ background: c.soft, color: c.color, width: 34, height: 34, borderRadius: 9 }}><Icon name={meta.icon || 'circle'} size={17} /></span>
        <div style={{ minWidth: 0, flex: 1 }}>
          <input className="etl-insp-name" value={node.name} readOnly={d.readOnly} onChange={(e) => d.updateNode(node.id, (n) => ({ ...n, name: e.target.value }))} aria-label="名称" />
          <div className="etl-insp-desc">{meta.label} · {meta.description}</div>
        </div>
        {!d.readOnly ? <Btn variant="ghost" size="sm" icon="trash" title="删除（Delete）" onClick={() => d.removeNodes([node.id])}></Btn> : null}
      </div>
      <RoBanner d={d} />
      <div className="etl-insp-tabs"><Tabs tabs={tabs} active={tab} onChange={setTab} /></div>
      {kind === 'pipeline' ? (
        <div className="etl-inline" style={{ padding: '7px 14px', borderBottom: '1px solid var(--border)' }}>
          <div className="etl-inline-label">
            <span>并行副本</span>
            <small>跑 {Math.max(1, node.copies || 1)} 份实例；默认轮询分行、合计只收一份输入，「复制」则每份收全量（下游会写多倍行数）。源步骤与全量状态步骤不适用</small>
          </div>
          <input type="number" className="input" style={{ width: 64 }} min={1} max={8} disabled={d.readOnly}
            value={node.copies || 1}
            onChange={(e) => {
              const v = Math.max(1, Math.min(8, parseInt(e.target.value, 10) || 1));
              d.updateNode(node.id, (n) => ({ ...n, copies: v > 1 ? v : undefined }));
            }} />
          {(node.copies || 1) > 1 ? (
            <Seg value={node.copiesDistribution || 'distribute'} disabled={d.readOnly}
              onChange={(v) => d.updateNode(node.id, (n) => ({ ...n, copiesDistribution: v === 'distribute' ? undefined : v }))}
              options={[{ value: 'distribute', label: '分发' }, { value: 'copy', label: '复制' }]} />
          ) : null}
        </div>
      ) : null}
      <div className="etl-insp-body" key={node.id + tab}>
        {tab === 'config' ? <Form cfg={node.config || {}} set={set} ctx={ctx} node={node} /> : null}
        {tab === 'advanced' ? (
          <RetryEditor node={node} onChange={(patch) => d.updateNode(node.id, (n) => ({ ...n, ...patch }))} />
        ) : null}
        {tab === 'fields' ? (
          <React.Fragment>
            <div className="etl-insp-section">
              <div className="etl-insp-section-title">输入字段<span className="hint">{input.length ? `${input.length} 个` : ''}</span></div>
              <FieldList fields={input} empty={meta.inputs && meta.inputs.length ? '尚未连接上游，或上游有错误' : '源步骤没有输入'} />
            </div>
            {Object.entries(shape.outputs || {}).map(([port, fields]) => (
              <div key={port} className="etl-insp-section">
                <div className="etl-insp-section-title">输出 · {portLabel(meta, port)}<span className="hint">{fields.length} 个</span></div>
                <FieldList fields={fields} empty="无" />
              </div>
            ))}
            {!shape.outputs ? <div className="etl-muted">字段在校验后显示（编辑停顿片刻会自动校验）。</div> : null}
          </React.Fragment>
        ) : null}
      </div>
      <Problems list={probs} />
    </aside>
  );
}

function portLabel(meta, port) {
  const p = (meta.outputs || []).find((x) => x.name === port);
  if (p) return p.label;
  if (port === 'error') return '错误';
  return port;
}

function EdgeInspector({ d, hop }) {
  const { kind, catalog, doc } = d;
  const from = doc.nodes.find((n) => n.id === hop.from);
  const to = doc.nodes.find((n) => n.id === hop.to);
  const fromMeta = from && catalog.meta(kind, from.type);
  const fields = d.hopFields[hop.id];
  const siblings = doc.hops.filter((h) => h.from === hop.from && (h.fromPort || 'main') === (hop.fromPort || 'main') && !h.disabled);
  const dist = (from && from.distribution && from.distribution[hop.fromPort || 'main']) || 'copy';
  const portOpts = kind === 'workflow'
    ? [...((fromMeta && fromMeta.outputs) || []).map((p) => ({ value: p.name, label: p.label })), { value: 'always', label: '无条件（成功或失败都继续）' }]
    : [];
  return (
    <aside className="etl-inspector">
      <div className="etl-insp-head">
        <span className="etl-chip" style={{ background: 'var(--muted)', color: 'var(--fg-secondary)', width: 34, height: 34, borderRadius: 9 }}><Icon name="arrowR" size={16} /></span>
        <div style={{ minWidth: 0, flex: 1 }}>
          <div className="etl-insp-title">连线</div>
          <div className="etl-insp-desc">{from ? from.name : '?'} → {to ? to.name : '?'}</div>
        </div>
        {!d.readOnly ? <Btn variant="ghost" size="sm" icon="trash" title="删除连线" onClick={() => d.removeHops([hop.id])}></Btn> : null}
      </div>
      <RoBanner d={d} />
      <div className="etl-insp-body">
        {kind === 'workflow' ? (
          <Field label="触发条件" hint="上游作业项结束于该结果时，沿此连线继续">
            <Select options={portOpts} value={hop.fromPort || 'success'} onChange={(fromPort) => d.updateHop(hop.id, { fromPort })} />
          </Field>
        ) : (
          <React.Fragment>
            <div className="etl-inline">
              <div className="etl-inline-label"><span>输出端口</span><small>{portLabel(fromMeta || {}, hop.fromPort || 'main')}</small></div>
            </div>
            {siblings.length > 1 ? (
              <Field label="多条连线时的分发方式" hint={dist === 'copy' ? '每条下游都收到全部记录' : '记录轮流分给各条下游（用于并行）'}>
                <Seg value={dist} onChange={(v) => d.updateNode(from.id, (n) => ({ ...n, distribution: { ...(n.distribution || {}), [hop.fromPort || 'main']: v } }))}
                  options={[{ value: 'copy', label: '复制' }, { value: 'distribute', label: '分发' }]} />
              </Field>
            ) : null}
            <div className="etl-insp-section">
              <div className="etl-insp-section-title">流经字段<span className="hint">{fields ? `${fields.length} 个` : ''}</span></div>
              {fields ? <div style={{ display: 'flex', flexWrap: 'wrap', gap: 5 }}>{fields.map((f) => <FieldChip key={f.name} field={f} />)}</div>
                : <div className="etl-muted">上游尚未产出字段（检查上游配置）。</div>}
            </div>
          </React.Fragment>
        )}
        <div className="etl-inline">
          <div className="etl-inline-label"><span>停用此连线</span><small>停用后编译与运行都忽略它</small></div>
          <Switch on={!!hop.disabled} onChange={(disabled) => d.updateHop(hop.id, { disabled })} />
        </div>
      </div>
    </aside>
  );
}

function DefinitionInspector({ d }) {
  const { kind, doc } = d;
  const s = doc.settings || {};
  const setSettings = (patch) => d.updateDoc((x) => ({ ...x, settings: { ...(x.settings || {}), ...patch } }));
  const params = doc.params || [];
  const globalProblems = (d.allProblems || []).filter((p) => !p.nodeId);
  return (
    <aside className="etl-inspector">
      <div className="etl-insp-head">
        <span className="etl-kind-chip" data-kind={kind} style={{ width: 34, height: 34, borderRadius: 9 }}><Icon name={kind === 'workflow' ? 'workflow' : 'pipeline'} size={17} /></span>
        <div style={{ minWidth: 0, flex: 1 }}>
          <div className="etl-insp-title">{kind === 'workflow' ? '作业设置' : '转换设置'}</div>
          <div className="etl-insp-desc">选中画布上的组件或连线可编辑其属性。</div>
        </div>
      </div>
      <RoBanner d={d} />
      <div className="etl-insp-body">
        <Field label="描述">
          <textarea className="textarea" rows={2} value={d.meta.description || ''} placeholder="这个定义做什么、数据从哪来到哪去"
            onChange={(e) => d.setMeta({ description: e.target.value })} />
        </Field>
        <Field label="文件夹" hint="工程列表按文件夹分组/筛选">
          <input className="input" value={d.meta.folder || ''} placeholder="留空为未分组"
            onChange={(e) => d.setMeta({ folder: e.target.value })} />
        </Field>
        <div className="etl-insp-section">
          <div className="etl-insp-section-title">参数<span className="hint">{kind === 'workflow' ? '运行或调度时可覆盖' : '由作业传入，或手动运行时填写'}</span></div>
          <ListEditor
            columns={[{ label: '名称' }, { label: '类型', width: '100px' }, { label: '默认值' }]}
            rows={params.map((p) => ({ ...p, def: p.default && p.default.value != null ? String(p.default.value) : '' }))}
            onChange={(rows) => d.updateDoc((x) => ({
              ...x,
              params: rows.map((r) => ({ name: r.name, kind: r.kind, required: r.required, description: r.description, default: r.def !== '' ? { kind: r.kind, value: r.def } : undefined })),
            }))}
            onAdd={() => d.updateDoc((x) => ({ ...x, params: [...(x.params || []), { name: `p${(x.params || []).length + 1}`, kind: 'string' }] }))}
            empty="没有参数"
            renderRow={(r, up) => (
              <React.Fragment>
                <input className="input mono" value={r.name} onChange={(e) => up({ name: e.target.value })} />
                <KindSelect value={r.kind} onChange={(k) => up({ kind: k })} />
                <input className="input" value={r.def} placeholder="无" onChange={(e) => up({ def: e.target.value })} />
              </React.Fragment>
            )} />
        </div>
        <div className="etl-insp-section">
          <div className="etl-insp-section-title">运行</div>
          {kind === 'pipeline' ? (
            <React.Fragment>
              <div className="etl-row2">
                <Field label="批大小" hint="每批行数"><input className="input" type="number" min="1" value={s.batchSize || 1000} onChange={(e) => setSettings({ batchSize: Number(e.target.value) || 1000 })} /></Field>
                <Field label="连线缓冲" hint="每条连线的批数"><input className="input" type="number" min="1" value={s.channelBatches || 8} onChange={(e) => setSettings({ channelBatches: Number(e.target.value) || 8 })} /></Field>
              </div>
              <Field label="内存预算（MB）" hint="去重、排序等有状态步骤的上限"><input className="input" type="number" min="16" value={s.memoryBudgetMB || 512} onChange={(e) => setSettings({ memoryBudgetMB: Number(e.target.value) || 512 })} /></Field>
              <div className="etl-inline">
                <div className="etl-inline-label">
                  <span>源端下推</span>
                  <small>把「表输入 → 过滤/字段选择」折叠为源库 SQL，默认关闭。预览不下推；开启后只有能证明与行内求值等价的条件才会下推（字符串比较、函数等一律不下推）；可空字段上的大小比较只在源端预筛，过滤步骤保留在行内复核，NULL 行仍按原样报错</small>
                </div>
                <Switch on={!!s.pushdown} disabled={d.readOnly} onChange={(v) => setSettings({ pushdown: v || undefined })} />
              </div>
            </React.Fragment>
          ) : (
            <Field label="最大并行作业项" hint="同时就绪的分支并发执行"><input className="input" type="number" min="1" value={s.maxParallel || 4} onChange={(e) => setSettings({ maxParallel: Number(e.target.value) || 4 })} /></Field>
          )}
          <Field label="日志级别">
            <Select value={s.logLevel || 'info'} onChange={(logLevel) => setSettings({ logLevel })}
              options={[{ value: 'error', label: '错误' }, { value: 'warn', label: '警告' }, { value: 'info', label: '基本' }, { value: 'detail', label: '详细' }, { value: 'debug', label: '调试' }]} />
          </Field>
        </div>
      </div>
      <Problems list={globalProblems} />
    </aside>
  );
}
