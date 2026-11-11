// 步骤与作业项的配置表单。新增组件时：写一个表单组件并 registerForm(kind, type, Component)。
// 未注册的类型退化为 JSON 编辑（GenericForm），保证后端新增组件时前端至少可配置。
//
// 表单 props：{ cfg, set(patch), ctx }
//   ctx = { catalog, input（主输入字段）, inputs（端口 → 字段）, vars（变量 → 类型）, pathError(path) → 文案, pipelines }
import React from 'react';
import { Field, Switch, Seg, Btn, Select, Icon } from '../components/primitives.jsx';
import {
  ExpressionEditor, SqlEditor, ConnectionSelect, TablePicker, FieldSelect, KindSelect, ListEditor, useColumns, FieldChip,
  Popover, usePopover,
} from './components.jsx';
import { etlApi } from './api.js';
import { KIND_LABEL } from './util.js';

const registry = {};
export function registerForm(kind, type, Component) { registry[`${kind}:${type}`] = Component; }
export function formFor(kind, type) { return registry[`${kind}:${type}`] || GenericForm; }

function Inline({ label, sub, children }) {
  return (
    <div className="etl-inline">
      <div className="etl-inline-label"><span>{label}</span>{sub ? <small>{sub}</small> : null}</div>
      {children}
    </div>
  );
}

function Section({ title, hint, children, right }) {
  return (
    <div className="etl-insp-section">
      {title ? <div className="etl-insp-section-title">{title}{hint ? <span className="hint">{hint}</span> : null}<span style={{ flex: 1 }}></span>{right}</div> : null}
      {children}
    </div>
  );
}

const num = (v, d) => { const n = Number(v); return Number.isFinite(n) ? n : d; };

// ---------------------------------------------------------------- 通用（JSON）

function GenericForm({ cfg, set }) {
  const [text, setText] = React.useState(() => JSON.stringify(cfg || {}, null, 2));
  const [err, setErr] = React.useState('');
  return (
    <Section title="配置（JSON）" hint="此组件暂无专用表单">
      <textarea className="textarea mono" rows={14} value={text} style={{ fontSize: 12 }}
        onChange={(e) => {
          setText(e.target.value);
          try { const v = JSON.parse(e.target.value); setErr(''); set(v, true); } catch (x) { setErr('JSON 格式错误'); }
        }} />
      {err ? <div className="field-hint" style={{ color: 'var(--error)' }}>{err}</div> : null}
    </Section>
  );
}

// ---------------------------------------------------------------- 转换步骤

registerForm('pipeline', 'generate_rows', ({ cfg, set }) => (
  <React.Fragment>
    <div className="etl-row2">
      <Field label="行数"><input className="input" type="number" min="0" value={cfg.limit ?? 0} onChange={(e) => set({ limit: num(e.target.value, 0) })} /></Field>
      <Field label="序号字段" hint="留空则不生成"><input className="input" value={cfg.sequenceField || ''} placeholder="如 seq" onChange={(e) => set({ sequenceField: e.target.value })} /></Field>
    </div>
    <Section title="常量字段">
      <ListEditor
        columns={[{ label: '字段名' }, { label: '类型', width: '110px' }, { label: '值' }]}
        rows={cfg.fields || []} onChange={(fields) => set({ fields })}
        onAdd={() => set({ fields: [...(cfg.fields || []), { name: `field${(cfg.fields || []).length + 1}`, kind: 'string', value: '' }] })}
        empty="没有常量字段"
        renderRow={(r, up) => (
          <React.Fragment>
            <input className="input" value={r.name} onChange={(e) => up({ name: e.target.value })} />
            <KindSelect value={r.kind} onChange={(kind) => up({ kind })} />
            <input className="input" value={r.value ?? ''} placeholder="NULL" onChange={(e) => up({ value: e.target.value === '' ? null : e.target.value })} />
          </React.Fragment>
        )} />
    </Section>
  </React.Fragment>
));

registerForm('pipeline', 'table_input', ({ cfg, set, ctx }) => (
  <React.Fragment>
    <Field label="数据库连接"><ConnectionSelect value={cfg.connection} onChange={(connection) => set({ connection })} /></Field>
    <Section title="SQL" right={<TableToSelect connection={cfg.connection} onPick={(sql) => set({ sql })} />}>
      <SqlEditor value={cfg.sql} onChange={(sql) => set({ sql })} vars={ctx.vars} height={190} />
      {ctx.pathError('sql') ? <div className="field-hint" style={{ color: 'var(--error)' }}>{ctx.pathError('sql')}</div> : null}
    </Section>
    <Field label="最多读取行数" hint="0 = 不限制；用于抽样或调试">
      <input className="input" type="number" min="0" value={cfg.maxRows ?? 0} onChange={(e) => set({ maxRows: num(e.target.value, 0) })} />
    </Field>
  </React.Fragment>
));

// TableToSelect：选一张表，生成显式列清单的 SELECT（不依赖 SELECT *）。
function TableToSelect({ connection, onPick }) {
  const pop = usePopover();
  const [t, setT] = React.useState({ schema: '', table: '' });
  const { cols } = useColumns(pop.isOpen ? connection : null, t.schema, t.table);
  if (!connection) return null;
  return (
    <React.Fragment>
      <Btn variant="ghost" size="sm" icon="table" onClick={pop.open}>从表生成</Btn>
      <Popover anchor={pop.anchor} onClose={pop.close} width={340} height={200} align="right">
        <div style={{ padding: 10 }}>
          <TablePicker connection={connection} schema={t.schema} table={t.table} onChange={setT} />
          <div style={{ display: 'flex', justifyContent: 'flex-end', alignItems: 'center', gap: 8, marginTop: 12 }}>
            <span className="etl-muted" style={{ marginRight: 'auto' }}>{cols ? `${cols.length} 个字段` : ''}</span>
            <Btn variant="ghost" size="sm" onClick={pop.close}>取消</Btn>
            <Btn variant="primary" size="sm" disabled={!cols || !cols.length} onClick={() => {
              const qualified = t.schema ? `${t.schema}.${t.table}` : t.table;
              onPick(`SELECT ${cols.map((c) => c.name).join(',\n       ')}\nFROM ${qualified}`);
              pop.close();
            }}>生成 SELECT</Btn>
          </div>
        </div>
      </Popover>
    </React.Fragment>
  );
}

registerForm('pipeline', 'select_fields', ({ cfg, set, ctx }) => {
  const input = ctx.input || [];
  const rows = cfg.fields || [];
  return (
    <React.Fragment>
      <Section title="输出字段" hint="按列表顺序输出">
        <ListEditor
          columns={[{ label: '来源字段' }, { label: '改名为' }, { label: '类型', width: '104px' }]}
          rows={rows} onChange={(fields) => set({ fields })}
          onAdd={() => set({ fields: [...rows, { source: (input[0] || {}).name || '', name: '', kind: '' }] })}
          empty="未选择字段 —— 将原样输出全部字段"
          extraActions={<Btn variant="ghost" size="sm" onClick={() => set({ fields: input.map((f) => ({ source: f.name, name: '', kind: '' })) })}>添加全部输入字段</Btn>}
          renderRow={(r, up) => (
            <React.Fragment>
              <FieldSelect fields={input} value={r.source} onChange={(source) => up({ source })} />
              <input className="input" value={r.name || ''} placeholder="不改名" onChange={(e) => up({ name: e.target.value })} />
              <KindSelect value={r.kind} allowAuto autoLabel="不改" onChange={(kind) => up({ kind })} />
            </React.Fragment>
          )} />
      </Section>
      {rows.some((r) => r.kind === 'date' || r.kind === 'timestamp' || r.kind === 'string') ? (
        <Section title="格式" hint="文本与日期互转时使用，如 yyyy-MM-dd HH:mm:ss">
          {rows.map((r, i) => (r.kind === 'date' || r.kind === 'timestamp' || r.kind === 'string') ? (
            <div key={i} className="etl-inline">
              <span className="mono" style={{ fontSize: 12 }}>{r.name || r.source}</span>
              <input className="input input-sm" style={{ width: 200 }} value={r.format || ''} placeholder="自动识别"
                onChange={(e) => set({ fields: rows.map((x, j) => (j === i ? { ...x, format: e.target.value } : x)) })} />
            </div>
          ) : null)}
        </Section>
      ) : null}
      <Inline label="保留未列出的字段" sub="追加在末尾"><Switch on={!!cfg.keepOthers} onChange={(keepOthers) => set({ keepOthers })} /></Inline>
    </React.Fragment>
  );
});

registerForm('pipeline', 'calculator', ({ cfg, set, ctx }) => {
  const rows = cfg.fields || [];
  // 后面的表达式可以引用前面计算出的字段
  const available = (i) => {
    const base = [...(ctx.input || [])];
    rows.slice(0, i).forEach((r) => {
      if (r.name && !base.find((f) => f.name === r.name)) base.push({ name: r.name, kind: r.kind || 'string' });
    });
    return base;
  };
  return (
    <Section title="计算字段" hint="同名则覆盖原字段">
      {rows.map((r, i) => (
        <div key={i} className="card" style={{ padding: 12, display: 'flex', flexDirection: 'column', gap: 8, boxShadow: 'none' }}>
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 120px 24px', gap: 6, alignItems: 'center' }}>
            <input className="input input-sm" value={r.name} placeholder="字段名" onChange={(e) => set({ fields: rows.map((x, j) => (j === i ? { ...x, name: e.target.value } : x)) })} />
            <KindSelect value={r.kind} allowAuto onChange={(kind) => set({ fields: rows.map((x, j) => (j === i ? { ...x, kind } : x)) })} />
            <button type="button" className="etl-list-x" title="移除" onClick={() => set({ fields: rows.filter((_, j) => j !== i) })}><Icon name="x" size={13} /></button>
          </div>
          <ExpressionEditor value={r.expr} catalog={ctx.catalog} fields={available(i)} vars={ctx.vars}
            error={ctx.pathError(`fields[${i}].expr`) || ctx.pathError(`fields[${i}].kind`)}
            onChange={(expr) => set({ fields: rows.map((x, j) => (j === i ? { ...x, expr } : x)) })} />
        </div>
      ))}
      <Btn variant="outline" size="sm" icon="plus" onClick={() => set({ fields: [...rows, { name: `field${rows.length + 1}`, expr: '', kind: '' }] })}>添加计算字段</Btn>
    </Section>
  );
});

registerForm('pipeline', 'filter_rows', ({ cfg, set, ctx }) => (
  <Section title="条件" hint="为真走「是」，否则（含 NULL）走「否」">
    <ExpressionEditor value={cfg.condition} catalog={ctx.catalog} fields={ctx.input} vars={ctx.vars} multiline
      placeholder={'如 status == 1 and amount >= 100'} error={ctx.pathError('condition')}
      onChange={(condition) => set({ condition })} />
  </Section>
));

registerForm('pipeline', 'unique_hash', ({ cfg, set, ctx }) => {
  const keys = cfg.keys || [];
  const input = ctx.input || [];
  return (
    <Section title="去重字段" hint="不选 = 整行去重">
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
        {input.map((f) => {
          const on = keys.includes(f.name);
          return (
            <button key={f.name} type="button" onClick={() => set({ keys: on ? keys.filter((k) => k !== f.name) : [...keys, f.name] })}
              className="etl-field-chip" style={{ cursor: 'pointer', border: `1px solid ${on ? 'var(--primary)' : 'transparent'}`, background: on ? 'var(--primary-soft)' : 'var(--muted)', color: on ? 'var(--primary)' : undefined }}>
              {on ? <Icon name="check" size={11} /> : null}{f.name}
            </button>
          );
        })}
        {!input.length ? <span className="etl-muted">连接上游后可选择字段</span> : null}
      </div>
      <div className="field-hint">保留每组首次出现的行。不同键的数量受转换内存预算限制（设置 → 内存预算）。</div>
    </Section>
  );
});

registerForm('pipeline', 'unique_sorted', ({ cfg, set, ctx }) => {
  const keys = cfg.keys || [];
  const input = ctx.input || [];
  return (
    <Section title="去重字段" hint="不选 = 整行去重">
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
        {input.map((f) => {
          const on = keys.includes(f.name);
          return (
            <button key={f.name} type="button" onClick={() => set({ keys: on ? keys.filter((k) => k !== f.name) : [...keys, f.name] })}
              className="etl-field-chip" style={{ cursor: 'pointer', border: `1px solid ${on ? 'var(--primary)' : 'transparent'}`, background: on ? 'var(--primary-soft)' : 'var(--muted)', color: on ? 'var(--primary)' : undefined }}>
              {on ? <Icon name="check" size={11} /> : null}{f.name}
            </button>
          );
        })}
        {!input.length ? <span className="etl-muted">连接上游后可选择字段</span> : null}
      </div>
      <div className="field-hint">输入须已按去重字段排序（一般前置「排序记录」），只比较相邻行、保留每组首行。</div>
    </Section>
  );
});

registerForm('pipeline', 'sort_rows', ({ cfg, set, ctx }) => {
  const rows = cfg.fields || [];
  return (
    <Section title="排序字段" hint="按列表顺序依次比较；NULL 最小">
      <ListEditor
        columns={[{ label: '字段' }, { label: '方向', width: '92px' }]}
        rows={rows} onChange={(fields) => set({ fields })}
        onAdd={() => set({ fields: [...rows, { name: (ctx.input || [])[0]?.name || '', desc: false }] })}
        empty="至少一个排序字段"
        renderRow={(r, up) => (
          <React.Fragment>
            <FieldSelect fields={ctx.input} value={r.name} onChange={(name) => up({ name })} />
            <Select value={r.desc ? 'desc' : 'asc'} onChange={(v) => up({ desc: v === 'desc' })}
              options={[{ value: 'asc', label: '升序' }, { value: 'desc', label: '降序' }]} />
          </React.Fragment>
        )} />
      <div className="field-hint">超出内存预算（设置 → 内存预算）的部分溢写临时文件做外部归并。</div>
    </Section>
  );
});

registerForm('pipeline', 'value_mapper', ({ cfg, set, ctx }) => {
  const rows = cfg.mappings || [];
  return (
    <React.Fragment>
      <div className="etl-row2">
        <Field label="取值字段"><FieldSelect fields={ctx.input} value={cfg.field} onChange={(field) => set({ field })} /></Field>
        <Field label="写入字段" hint="留空 = 覆盖取值字段；输出为字符串"><input className="input" value={cfg.target || ''} placeholder={cfg.field || '同名字段'} onChange={(e) => set({ target: e.target.value })} /></Field>
      </div>
      <Field label="未匹配时" hint="留空 = 保留原值"><input className="input" value={cfg.default || ''} placeholder="保留原值" onChange={(e) => set({ default: e.target.value })} /></Field>
      <Section title="映射表" hint="按值的文本形式比较">
        <ListEditor
          columns={[{ label: '源值' }, { label: '目标值' }]}
          rows={rows} onChange={(mappings) => set({ mappings })}
          onAdd={() => set({ mappings: [...rows, { source: '', target: '' }] })}
          empty="没有映射 —— 值原样通过（未匹配策略仍生效）"
          renderRow={(r, up) => (
            <React.Fragment>
              <input className="input" value={r.source ?? ''} onChange={(e) => up({ source: e.target.value })} />
              <input className="input" value={r.target ?? ''} onChange={(e) => up({ target: e.target.value })} />
            </React.Fragment>
          )} />
      </Section>
    </React.Fragment>
  );
});

registerForm('pipeline', 'dummy', () => (
  <div className="etl-muted" style={{ lineHeight: 1.7 }}>原样透传，无需配置。可把多条字段布局一致的流汇合到这里，或作为调试时的观察点（选中后在底部「数据预览」查看）。</div>
));

// ---------------------------------------------------------------- 输出

function useTargetColumns(cfg) {
  return useColumns(cfg.connection, cfg.schema, cfg.table);
}

// 自动映射：流字段与目标列同名（不区分大小写）
function autoMap(input, cols) {
  const byLower = {};
  (cols || []).forEach((c) => { byLower[c.name.toLowerCase()] = c.name; });
  return (input || []).filter((f) => byLower[f.name.toLowerCase()]).map((f) => ({ stream: f.name, column: byLower[f.name.toLowerCase()] }));
}

function ColumnSelect({ cols, value, onChange }) {
  const opts = [{ value: '', label: '选择目标字段…' }, ...(cols || []).map((c) => ({ value: c.name, label: c.primaryKey ? `${c.name}（主键）` : c.name }))];
  if (value && !(cols || []).find((c) => c.name === value)) opts.push({ value, label: `${value}（不存在）` });
  return <Select options={opts} value={value || ''} onChange={onChange} />;
}

registerForm('pipeline', 'table_output', ({ cfg, set, ctx }) => {
  const { cols, error } = useTargetColumns(cfg);
  const rows = cfg.fields || [];
  return (
    <React.Fragment>
      <Field label="数据库连接"><ConnectionSelect value={cfg.connection} onChange={(connection) => set({ connection })} /></Field>
      <TablePicker connection={cfg.connection} schema={cfg.schema} table={cfg.table} onChange={(t) => set(t)} />
      {error ? <div className="field-hint" style={{ color: 'var(--error)' }}>{error}</div> : null}
      <div className="etl-row2">
        <Field label="提交记录数" hint="每个事务的行数"><input className="input" type="number" min="1" value={cfg.commitSize || 1000} onChange={(e) => set({ commitSize: num(e.target.value, 1000) })} /></Field>
        <div style={{ paddingTop: 22 }}><Inline label="开始前清空表" sub="全量重灌"><Switch on={!!cfg.truncate} onChange={(truncate) => set({ truncate })} /></Inline></div>
      </div>
      {cfg.truncate ? <div className="field-hint" style={{ color: 'var(--warn)' }}>清空与写入不在同一事务：中途失败时目标表只有部分数据，需重跑恢复（与 Kettle 一致）。</div> : null}
      <Section title="字段映射" hint="留空 = 按同名映射全部字段">
        <ListEditor
          columns={[{ label: '流字段' }, { label: '目标字段' }]}
          rows={rows} onChange={(fields) => set({ fields })}
          onAdd={() => set({ fields: [...rows, { stream: '', column: '' }] })}
          empty="按同名自动映射"
          extraActions={<Btn variant="ghost" size="sm" disabled={!cols} onClick={() => set({ fields: autoMap(ctx.input, cols) })}>按名称匹配</Btn>}
          renderRow={(r, up) => (
            <React.Fragment>
              <FieldSelect fields={ctx.input} value={r.stream} onChange={(stream) => up({ stream })} />
              <ColumnSelect cols={cols} value={r.column} onChange={(column) => up({ column })} />
            </React.Fragment>
          )} />
      </Section>
    </React.Fragment>
  );
});

registerForm('pipeline', 'insert_update', ({ cfg, set, ctx }) => {
  const { cols, indexes, error } = useTargetColumns(cfg);
  const keys = cfg.keys || [];
  const fields = cfg.fields || [];
  const keyCols = keys.map((k) => k.column.toLowerCase());
  const indexed = !keys.length || (indexes || []).some((ix) => keyCols.every((k, i) => (ix.columns[i] || '').toLowerCase() === k));
  return (
    <React.Fragment>
      <Field label="数据库连接"><ConnectionSelect value={cfg.connection} onChange={(connection) => set({ connection })} /></Field>
      <TablePicker connection={cfg.connection} schema={cfg.schema} table={cfg.table} onChange={(t) => set(t)} />
      {error ? <div className="field-hint" style={{ color: 'var(--error)' }}>{error}</div> : null}
      <Section title="查询关键字" hint="用于判断记录是否已存在">
        <ListEditor
          columns={[{ label: '流字段' }, { label: '表字段' }]}
          rows={keys} onChange={(k) => set({ keys: k })}
          onAdd={() => set({ keys: [...keys, { stream: '', column: '' }] })}
          empty="至少需要一个关键字"
          extraActions={cols && cols.some((c) => c.primaryKey) ? (
            <Btn variant="ghost" size="sm" icon="key" onClick={() => set({ keys: cols.filter((c) => c.primaryKey).map((c) => ({ stream: ((ctx.input || []).find((f) => f.name.toLowerCase() === c.name.toLowerCase()) || {}).name || '', column: c.name })) })}>使用主键</Btn>
          ) : null}
          renderRow={(r, up) => (
            <React.Fragment>
              <FieldSelect fields={ctx.input} value={r.stream} onChange={(stream) => up({ stream })} />
              <ColumnSelect cols={cols} value={r.column} onChange={(column) => up({ column })} />
            </React.Fragment>
          )} />
        {cols && keys.length && !indexed ? <div className="field-hint" style={{ color: 'var(--warn)' }}>关键字段上没有索引，大表查找会很慢。建议在目标表建立索引。</div> : null}
      </Section>
      <Section title="写入字段" hint="勾选「更新」的字段在记录已存在时被更新">
        <ListEditor
          columns={[{ label: '流字段' }, { label: '表字段' }, { label: '更新', width: '44px' }]}
          rows={fields} onChange={(f) => set({ fields: f })}
          onAdd={() => set({ fields: [...fields, { stream: '', column: '', update: true }] })}
          empty="只插入关键字段"
          extraActions={<Btn variant="ghost" size="sm" disabled={!cols} onClick={() => {
            const ks = new Set(keys.map((k) => k.column.toLowerCase()));
            set({ fields: autoMap(ctx.input, cols).filter((m) => !ks.has(m.column.toLowerCase())).map((m) => ({ ...m, update: true })) });
          }}>按名称匹配</Btn>}
          renderRow={(r, up) => (
            <React.Fragment>
              <FieldSelect fields={ctx.input} value={r.stream} onChange={(stream) => up({ stream })} />
              <ColumnSelect cols={cols} value={r.column} onChange={(column) => up({ column })} />
              <div style={{ display: 'flex', justifyContent: 'center' }}><Switch on={!!r.update} onChange={(update) => up({ update })} /></div>
            </React.Fragment>
          )} />
      </Section>
      <div className="etl-row2">
        <Field label="提交记录数"><input className="input" type="number" min="1" value={cfg.commitSize || 1000} onChange={(e) => set({ commitSize: num(e.target.value, 1000) })} /></Field>
        <Field label="关键字为空时">
          <Select value={cfg.nullKeys || 'error'} onChange={(nullKeys) => set({ nullKeys })} options={[{ value: 'error', label: '进入错误输出' }, { value: 'insert', label: '作为新记录插入' }]} />
        </Field>
      </div>
      <Inline label="值未变化时跳过更新" sub="大幅减少重复同步的写入"><Switch on={cfg.skipUnchanged !== false} onChange={(skipUnchanged) => set({ skipUnchanged })} /></Inline>
      <Inline label="只插入，不更新" sub="已存在的记录保持不变"><Switch on={!!cfg.noUpdate} onChange={(noUpdate) => set({ noUpdate })} /></Inline>
    </React.Fragment>
  );
});

function KeyedWriteForm({ cfg, set, ctx, withFields, keyHint, fieldsTitle, fieldsHint, fieldsEmpty }) {
  const { cols, indexes, error } = useTargetColumns(cfg);
  const keys = cfg.keys || [];
  const keyCols = keys.map((k) => (k.column || '').toLowerCase());
  const indexed = !keys.length || (indexes || []).some((ix) => keyCols.every((k, i) => (ix.columns[i] || '').toLowerCase() === k));
  return (
    <React.Fragment>
      <Field label="数据库连接"><ConnectionSelect value={cfg.connection} onChange={(connection) => set({ connection })} /></Field>
      <TablePicker connection={cfg.connection} schema={cfg.schema} table={cfg.table} onChange={(t) => set(t)} />
      {error ? <div className="field-hint" style={{ color: 'var(--error)' }}>{error}</div> : null}
      <Section title="查询关键字" hint={keyHint || '命中则处理，未命中的行进错误输出'}>
        <ListEditor
          columns={[{ label: '流字段' }, { label: '表字段' }]}
          rows={keys} onChange={(k) => set({ keys: k })}
          onAdd={() => set({ keys: [...keys, { stream: '', column: '' }] })}
          empty="至少需要一个关键字"
          extraActions={cols && cols.some((c) => c.primaryKey) ? (
            <Btn variant="ghost" size="sm" icon="key" onClick={() => set({ keys: cols.filter((c) => c.primaryKey).map((c) => ({ stream: ((ctx.input || []).find((f) => f.name.toLowerCase() === c.name.toLowerCase()) || {}).name || '', column: c.name })) })}>使用主键</Btn>
          ) : null}
          renderRow={(r, up) => (
            <React.Fragment>
              <FieldSelect fields={ctx.input} value={r.stream} onChange={(stream) => up({ stream })} />
              <ColumnSelect cols={cols} value={r.column} onChange={(column) => up({ column })} />
            </React.Fragment>
          )} />
        {cols && keys.length && !indexed ? <div className="field-hint" style={{ color: 'var(--warn)' }}>关键字段上没有索引，大表查找会很慢。建议在目标表建立索引。</div> : null}
      </Section>
      {withFields ? (
        <Section title={fieldsTitle || '更新字段'} hint={fieldsHint}>
          <ListEditor
            columns={[{ label: '流字段' }, { label: '表字段' }]}
            rows={cfg.fields || []} onChange={(fields) => set({ fields })}
            onAdd={() => set({ fields: [...(cfg.fields || []), { stream: '', column: '' }] })}
            empty={fieldsEmpty || '至少需要一个更新字段'}
            extraActions={<Btn variant="ghost" size="sm" disabled={!cols} onClick={() => {
              const ks = new Set(keys.map((k) => (k.column || '').toLowerCase()));
              set({ fields: autoMap(ctx.input, cols).filter((m) => !ks.has(m.column.toLowerCase())) });
            }}>按名称匹配</Btn>}
            renderRow={(r, up) => (
              <React.Fragment>
                <FieldSelect fields={ctx.input} value={r.stream} onChange={(stream) => up({ stream })} />
                <ColumnSelect cols={cols} value={r.column} onChange={(column) => up({ column })} />
              </React.Fragment>
            )} />
        </Section>
      ) : null}
      <Field label="提交记录数"><input className="input" type="number" min="1" value={cfg.commitSize || 1000} onChange={(e) => set({ commitSize: num(e.target.value, 1000) })} /></Field>
    </React.Fragment>
  );
}

registerForm('pipeline', 'update', (p) => <KeyedWriteForm {...p} withFields />);
registerForm('pipeline', 'delete', (p) => <KeyedWriteForm {...p} />);

registerForm('pipeline', 'merge_rows', ({ cfg, set, ctx }) => {
  const keys = cfg.keys || [];
  const values = cfg.values || [];
  const input = ctx.input || [];
  return (
    <React.Fragment>
      <Section title="合并键" hint="参照/对比两路输入都必须已按键升序排序（各前置「排序记录」）">
        <ListEditor
          columns={[{ label: '字段' }]}
          rows={keys.map((k) => ({ name: k }))}
          onChange={(rows) => set({ keys: rows.map((r) => r.name).filter(Boolean) })}
          onAdd={() => set({ keys: [...keys, (input.find((f) => !keys.includes(f.name)) || {}).name || ''] })}
          empty="至少一个合并键"
          renderRow={(r, up) => <FieldSelect fields={input} value={r.name} onChange={(name) => up({ name })} />} />
      </Section>
      <Section title="比较字段" hint="不选 = 全部非键字段">
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
          {input.map((f) => {
            const on = values.includes(f.name);
            return (
              <button key={f.name} type="button" onClick={() => set({ values: on ? values.filter((k) => k !== f.name) : [...values, f.name] })}
                className="etl-field-chip" style={{ cursor: 'pointer', border: `1px solid ${on ? 'var(--primary)' : 'transparent'}`, background: on ? 'var(--primary-soft)' : 'var(--muted)', color: on ? 'var(--primary)' : undefined }}>
                {on ? <Icon name="check" size={11} /> : null}{f.name}
              </button>
            );
          })}
          {!input.length ? <span className="etl-muted">连接上游后可选择字段</span> : null}
        </div>
      </Section>
      <Field label="标志字段" hint="键同、值异为 changed；只在对比流为 new；只在参照流为 deleted；全同为 identical">
        <input className="input" value={cfg.flagField || 'flag'} onChange={(e) => set({ flagField: e.target.value })} />
      </Field>
      <div className="field-hint">连线时：现有/旧数据接「参照」，新数据接「对比」。输出 = 对比流字段 + 标志字段，一般接「数据同步」把差异写入目标表。</div>
    </React.Fragment>
  );
});

registerForm('pipeline', 'sync_after_merge', ({ cfg, set, ctx }) => (
  <React.Fragment>
    <Field label="标志字段" hint="含 identical/changed/new/deleted 的字段（一般来自「合并记录」）">
      <input className="input" value={cfg.flagField || 'flag'} onChange={(e) => set({ flagField: e.target.value })} />
    </Field>
    <KeyedWriteForm cfg={cfg} set={set} ctx={ctx} withFields
      keyHint="new 按它插入、changed/deleted 按它定位行"
      fieldsTitle="写入字段" fieldsHint="插入/更新共用；键所在列自动并入插入"
      fieldsEmpty="留空 = 除键和标志字段外的全部字段" />
  </React.Fragment>
));

// ---------------------------------------------------------------- 查询

registerForm('pipeline', 'stream_lookup', ({ cfg, set, ctx }) => {
  const keys = cfg.keys || [];
  const rets = cfg.returns || [];
  const ref = (ctx.inputs && ctx.inputs.info) || [];
  return (
    <React.Fragment>
      <Section title="查询键" hint="主流字段 ↔ 参照流字段（等值）">
        <ListEditor
          columns={[{ label: '流字段' }, { label: '参照字段' }]}
          rows={keys} onChange={(keys) => set({ keys })}
          onAdd={() => set({ keys: [...keys, { stream: '', column: '' }] })}
          empty="至少一个查询键"
          renderRow={(r, up) => (
            <React.Fragment>
              <FieldSelect fields={ctx.input} value={r.stream} onChange={(stream) => up({ stream })} />
              <FieldSelect fields={ref} value={r.column} onChange={(column) => up({ column })} />
            </React.Fragment>
          )} />
      </Section>
      <Section title="返回字段" hint="参照流字段追加到输出；重复键保留首行">
        <ListEditor
          columns={[{ label: '参照字段' }, { label: '输出名', width: '110px' }, { label: '默认值', width: '90px' }]}
          rows={rets} onChange={(returns) => set({ returns })}
          onAdd={() => set({ returns: [...rets, { column: '', name: '', default: '' }] })}
          empty="至少一个返回字段"
          renderRow={(r, up) => (
            <React.Fragment>
              <FieldSelect fields={ref} value={r.column} onChange={(column) => up({ column })} />
              <input className="input" value={r.name || ''} placeholder={r.column || '同名列'} onChange={(e) => up({ name: e.target.value })} />
              <input className="input" value={r.default || ''} placeholder="NULL" onChange={(e) => up({ default: e.target.value })} />
            </React.Fragment>
          )} />
      </Section>
      <div className="field-hint">参照流整体装入内存（受内存预算限制），大数据量请改用「数据库查询」。</div>
    </React.Fragment>
  );
});

registerForm('pipeline', 'db_lookup', ({ cfg, set, ctx }) => {
  const { cols, indexes, error } = useTargetColumns(cfg);
  const keys = cfg.keys || [];
  const rets = cfg.returns || [];
  const keyCols = keys.map((k) => (k.column || '').toLowerCase());
  const indexed = !keys.length || (indexes || []).some((ix) => keyCols.every((k, i) => (ix.columns[i] || '').toLowerCase() === k));
  return (
    <React.Fragment>
      <Field label="数据库连接"><ConnectionSelect value={cfg.connection} onChange={(connection) => set({ connection })} /></Field>
      <TablePicker connection={cfg.connection} schema={cfg.schema} table={cfg.table} onChange={(t) => set(t)} />
      {error ? <div className="field-hint" style={{ color: 'var(--error)' }}>{error}</div> : null}
      <Section title="查询键" hint="流字段 = 表字段（等值）">
        <ListEditor
          columns={[{ label: '流字段' }, { label: '表字段' }]}
          rows={keys} onChange={(k) => set({ keys: k })}
          onAdd={() => set({ keys: [...keys, { stream: '', column: '' }] })}
          empty="至少一个查询键"
          renderRow={(r, up) => (
            <React.Fragment>
              <FieldSelect fields={ctx.input} value={r.stream} onChange={(stream) => up({ stream })} />
              <ColumnSelect cols={cols} value={r.column} onChange={(column) => up({ column })} />
            </React.Fragment>
          )} />
        {cols && keys.length && !indexed ? <div className="field-hint" style={{ color: 'var(--warn)' }}>关键字段上没有索引，逐行查询会很慢。</div> : null}
      </Section>
      <Section title="返回字段" hint="查到的列追加到输出；未命中填默认值">
        <ListEditor
          columns={[{ label: '表字段' }, { label: '输出名', width: '110px' }, { label: '默认值', width: '90px' }]}
          rows={rets} onChange={(returns) => set({ returns })}
          onAdd={() => set({ returns: [...rets, { column: '', name: '', default: '' }] })}
          empty="至少一个返回字段"
          extraActions={<Btn variant="ghost" size="sm" disabled={!cols} onClick={() => {
            const used = new Set((ctx.input || []).map((f) => f.name.toLowerCase()));
            set({ returns: (cols || []).filter((c) => !used.has(c.name.toLowerCase())).map((c) => ({ column: c.name, name: '', default: '' })) });
          }}>按名称匹配</Btn>}
          renderRow={(r, up) => (
            <React.Fragment>
              <ColumnSelect cols={cols} value={r.column} onChange={(column) => up({ column })} />
              <input className="input" value={r.name || ''} placeholder={r.column || '同名列'} onChange={(e) => up({ name: e.target.value })} />
              <input className="input" value={r.default || ''} placeholder="NULL" onChange={(e) => up({ default: e.target.value })} />
            </React.Fragment>
          )} />
      </Section>
      <div className="etl-row2">
        <Field label="多行命中排序" hint="留空 = 任意第一行"><input className="input" value={cfg.orderBy || ''} onChange={(e) => set({ orderBy: e.target.value })} /></Field>
        <Field label="选项">
          <label style={{ display: 'flex', gap: 6, alignItems: 'center', fontSize: 13 }}><input type="checkbox" checked={!!cfg.failOnMulti} onChange={(e) => set({ failOnMulti: e.target.checked })} />多行命中报错</label>
          <label style={{ display: 'flex', gap: 6, alignItems: 'center', fontSize: 13 }}><input type="checkbox" checked={!!cfg.noCache} onChange={(e) => set({ noCache: e.target.checked })} />关闭缓存（默认按键缓存结果）</label>
        </Field>
      </div>
    </React.Fragment>
  );
});

// ---------------------------------------------------------------- 文件 / 接口

const KIND_OPTIONS = ['string', 'integer', 'number', 'decimal', 'boolean', 'date', 'timestamp', 'binary'].map((k) => ({ value: k, label: k }));

function DeclaredFieldsEditor({ rows, onChange, withPath }) {
  const cols = withPath
    ? [{ label: '名称' }, { label: '路径', width: '120px' }, { label: '类型', width: '100px' }, { label: '格式', width: '90px' }]
    : [{ label: '名称' }, { label: '类型', width: '110px' }, { label: '格式', width: '110px' }];
  return (
    <ListEditor
      columns={cols}
      rows={rows} onChange={onChange}
      onAdd={() => onChange([...rows, { name: '', path: '', kind: 'string', format: '' }])}
      empty="声明文件的列（按位置对应）"
      renderRow={(r, up) => (
        <React.Fragment>
          <input className="input" value={r.name || ''} onChange={(e) => up({ name: e.target.value })} />
          {withPath ? <input className="input mono" value={r.path || ''} placeholder="a.b" onChange={(e) => up({ path: e.target.value })} /> : null}
          <Select options={KIND_OPTIONS} value={r.kind || 'string'} onChange={(kind) => up({ kind })} />
          <input className="input" value={r.format || ''} placeholder="yyyy-MM-dd" onChange={(e) => up({ format: e.target.value })} />
        </React.Fragment>
      )} />
  );
}

registerForm('pipeline', 'csv_input', ({ cfg, set }) => {
  const rows = cfg.fields || [];
  return (
    <React.Fragment>
      <Field label="文件路径" hint="服务器上的绝对路径"><input className="input mono" value={cfg.path || ''} onChange={(e) => set({ path: e.target.value })} /></Field>
      <div className="etl-row2">
        <Field label="分隔符"><input className="input" value={cfg.delimiter || ','} onChange={(e) => set({ delimiter: e.target.value })} /></Field>
        <Field label="编码"><Select options={[{ value: 'utf-8', label: 'UTF-8' }, { value: 'gbk', label: 'GBK' }]} value={cfg.encoding || 'utf-8'} onChange={(encoding) => set({ encoding })} /></Field>
      </div>
      <Field label="首行为表头"><input type="checkbox" checked={cfg.hasHeader !== false} onChange={(e) => set({ hasHeader: e.target.checked })} /></Field>
      <Section title="字段声明" hint="按位置对应文件列；转换失败的行进入错误输出">
        <DeclaredFieldsEditor rows={rows} onChange={(fields) => set({ fields })} />
      </Section>
    </React.Fragment>
  );
});

registerForm('pipeline', 'csv_output', ({ cfg, set }) => (
  <React.Fragment>
    <Field label="文件路径" hint="服务器上的绝对路径"><input className="input mono" value={cfg.path || ''} onChange={(e) => set({ path: e.target.value })} /></Field>
    <div className="etl-row2">
      <Field label="分隔符"><input className="input" value={cfg.delimiter || ','} onChange={(e) => set({ delimiter: e.target.value })} /></Field>
      <Field label="编码"><Select options={[{ value: 'utf-8', label: 'UTF-8' }, { value: 'gbk', label: 'GBK' }]} value={cfg.encoding || 'utf-8'} onChange={(encoding) => set({ encoding })} /></Field>
    </div>
    <Field label="选项">
      <label style={{ display: 'flex', gap: 6, alignItems: 'center', fontSize: 13 }}><input type="checkbox" checked={cfg.header !== false} onChange={(e) => set({ header: e.target.checked })} />写入表头</label>
      <label style={{ display: 'flex', gap: 6, alignItems: 'center', fontSize: 13 }}><input type="checkbox" checked={!!cfg.append} onChange={(e) => set({ append: e.target.checked })} />追加（不覆盖已有文件）</label>
    </Field>
  </React.Fragment>
));

registerForm('pipeline', 'http_input', ({ cfg, set, ctx }) => {
  const rows = cfg.fields || [];
  const pg = cfg.page || {};
  const setPg = (p) => set({ page: { ...pg, ...p } });
  return (
    <React.Fragment>
      <div className="etl-row2">
        <Field label="方法"><Select options={[{ value: 'GET', label: 'GET' }, { value: 'POST', label: 'POST' }]} value={cfg.method || 'GET'} onChange={(method) => set({ method })} /></Field>
        <Field label="超时（秒）"><input className="input" type="number" min="1" value={cfg.timeout || 60} onChange={(e) => set({ timeout: num(e.target.value, 60) })} /></Field>
      </div>
      <Field label="URL" hint="可用 ${变量}"><input className="input mono" value={cfg.url || ''} placeholder="https://…" onChange={(e) => set({ url: e.target.value })} /></Field>
      <Section title="请求头">
        <ListEditor
          columns={[{ label: '名称' }, { label: '值' }]}
          rows={Object.entries(cfg.headers || {}).map(([k, v]) => ({ k, v }))}
          onChange={(rs) => set({ headers: Object.fromEntries(rs.filter((r) => r.k).map((r) => [r.k, r.v])) })}
          onAdd={() => set({ headers: { ...(cfg.headers || {}), '': '' } })}
          empty="无请求头"
          renderRow={(r, up) => (
            <React.Fragment>
              <input className="input mono" value={r.k} placeholder="Authorization" onChange={(e) => up({ k: e.target.value })} />
              <input className="input mono" value={r.v} placeholder="Bearer ${token}" onChange={(e) => up({ v: e.target.value })} />
            </React.Fragment>
          )} />
      </Section>
      {cfg.method === 'POST' ? <Field label="正文（JSON）"><textarea className="input mono" rows={3} value={cfg.body || ''} onChange={(e) => set({ body: e.target.value })} /></Field> : null}
      <Field label="数据路径" hint="响应 JSON 中数组的点路径，如 data.rows；留空 = 根"><input className="input mono" value={cfg.dataPath || ''} onChange={(e) => set({ dataPath: e.target.value })} /></Field>
      <Section title="分页" hint="页码参数按页递增，返回不足页大小时停止；留空参数名 = 不分页">
        <div className="etl-row2">
          <Field label="页码参数"><input className="input mono" value={pg.param || ''} onChange={(e) => setPg({ param: e.target.value })} /></Field>
          <Field label="页大小参数"><input className="input mono" value={pg.sizeParam || ''} onChange={(e) => setPg({ sizeParam: e.target.value })} /></Field>
        </div>
        <div className="etl-row2">
          <Field label="页大小"><input className="input" type="number" min="1" value={pg.size || ''} onChange={(e) => setPg({ size: num(e.target.value, 0) })} /></Field>
          <Field label="最大页数"><input className="input" type="number" min="1" value={pg.maxPages || 1000} onChange={(e) => setPg({ maxPages: num(e.target.value, 1000) })} /></Field>
        </div>
      </Section>
      <Section title="字段" hint="点路径相对每个数组元素取值；失败的行进入错误输出">
        <DeclaredFieldsEditor rows={rows} onChange={(fields) => set({ fields })} withPath />
      </Section>
    </React.Fragment>
  );
});

// ---------------------------------------------------------------- 作业项

registerForm('workflow', 'start', ({ ctx }) => (
  <div className="etl-muted" style={{ lineHeight: 1.7 }}>
    作业从这里开始。定时运行请在模块的「调度」中为本作业添加调度规则{ctx.schedules != null ? `（当前 ${ctx.schedules} 个）` : ''}。
  </div>
));

// 请求头编辑器：map ↔ ListEditor 行。
function HeadersEditor({ map, onChange, title }) {
  return (
    <Section title={title || '请求头'}>
      <ListEditor
        columns={[{ label: '名称' }, { label: '值' }]}
        rows={Object.entries(map || {}).map(([k, v]) => ({ k, v }))}
        onChange={(rs) => onChange(Object.fromEntries(rs.filter((r) => r.k).map((r) => [r.k, r.v])))}
        onAdd={() => onChange({ ...(map || {}), '': '' })}
        empty="无请求头"
        renderRow={(r, up) => (
          <React.Fragment>
            <input className="input mono" value={r.k} placeholder="Authorization" onChange={(e) => up({ k: e.target.value })} />
            <input className="input mono" value={r.v} placeholder="值，可用 ${var}" onChange={(e) => up({ v: e.target.value })} />
          </React.Fragment>
        )} />
    </Section>
  );
}

// 引用定义（转换/作业）的参数映射子表单。
function DefParamsForm({ defId, map, ctx, onChange }) {
  const [params, setParams] = React.useState(null);
  React.useEffect(() => {
    setParams(null);
    if (!defId) return;
    etlApi.definition(defId).then((d) => setParams((d.doc && d.doc.params) || [])).catch(() => setParams([]));
  }, [defId]);
  return (
    <Section title="参数" hint="值为对作业变量求值的表达式，如 maxTs">
      {params == null ? <div className="etl-muted">读取参数…</div> : null}
      {params && !params.length ? <div className="etl-muted">该定义没有声明参数。</div> : null}
      {(params || []).map((p) => (
        <div key={p.name}>
          <label className="field-label">{p.name} <span className="etl-muted">· {KIND_LABEL[p.kind]}{p.required ? ' · 必填' : ''}</span></label>
          <ExpressionEditor value={map[p.name] || ''} catalog={ctx.catalog} fields={[]} vars={ctx.vars}
            placeholder={p.default ? `默认 ${p.default.value}` : '如 maxTs 或 addDays(today(), -1)'}
            error={ctx.pathError(`params.${p.name}`)}
            onChange={(v) => {
              const next = { ...map };
              if (v) next[p.name] = v; else delete next[p.name];
              onChange(next);
            }} />
        </div>
      ))}
    </Section>
  );
}

registerForm('workflow', 'pipeline', ({ cfg, set, ctx }) => {
  const list = ctx.pipelines || [];
  return (
    <React.Fragment>
      <Field label="转换">
        <Select value={cfg.pipeline || ''} onChange={(pipeline) => set({ pipeline, params: {} })}
          options={[{ value: '', label: list.length ? '选择转换…' : '尚无转换' }, ...list.map((p) => ({ value: p.id, label: p.name }))]} />
      </Field>
      {cfg.pipeline ? <DefParamsForm defId={cfg.pipeline} map={cfg.params || {}} ctx={ctx} onChange={(params) => set({ params })} /> : null}
    </React.Fragment>
  );
});

registerForm('workflow', 'workflow', ({ cfg, set, ctx }) => {
  const list = ctx.workflows || [];
  return (
    <React.Fragment>
      <Field label="作业" hint="嵌套上限 8 层；子作业节点状态归入本次运行">
        <Select value={cfg.workflow || ''} onChange={(workflow) => set({ workflow, params: {} })}
          options={[{ value: '', label: list.length ? '选择作业…' : '尚无其他作业' }, ...list.map((w) => ({ value: w.id, label: w.name }))]} />
      </Field>
      {cfg.workflow ? <DefParamsForm defId={cfg.workflow} map={cfg.params || {}} ctx={ctx} onChange={(params) => set({ params })} /> : null}
    </React.Fragment>
  );
});

registerForm('workflow', 'http', ({ cfg, set, ctx }) => {
  const heads = cfg.headers || {};
  const rows = cfg.extract || [];
  return (
    <React.Fragment>
      <div className="etl-row2">
        <Field label="方法">
          <Select value={cfg.method || 'GET'} onChange={(method) => set({ method })}
            options={['GET', 'POST', 'PUT', 'DELETE', 'PATCH', 'HEAD'].map((m) => ({ value: m, label: m }))} />
        </Field>
        <Field label="超时（秒）"><input className="input" type="number" min="1" value={cfg.timeout ?? 30} onChange={(e) => set({ timeout: num(e.target.value, 30) })} /></Field>
      </div>
      <Field label="URL" hint="可引用变量，如 ${baseUrl}/sync">
        <input className="input mono" value={cfg.url || ''} placeholder="https://…" onChange={(e) => set({ url: e.target.value })} />
        {ctx.pathError('url') ? <div className="field-hint" style={{ color: 'var(--error)' }}>{ctx.pathError('url')}</div> : null}
      </Field>
      <HeadersEditor map={heads} onChange={(headers) => set({ headers })} />
      {cfg.method && cfg.method !== 'GET' ? (
        <Section title="请求正文" hint="可引用变量">
          <textarea className="input mono" rows={4} value={cfg.body || ''} onChange={(e) => set({ body: e.target.value })} />
        </Section>
      ) : null}
      <div className="etl-row2">
        <Field label="状态码写入变量"><input className="input" value={cfg.statusVar || ''} placeholder="如 code" onChange={(e) => set({ statusVar: e.target.value })} /></Field>
        <Field label="响应写入变量" hint="可配 dataPath 取子树">
          <input className="input" value={cfg.resultVar || ''} placeholder="如 resp" onChange={(e) => set({ resultVar: e.target.value })} />
        </Field>
      </div>
      {cfg.resultVar ? (
        <Field label="数据路径" hint="响应 JSON 内的点路径，如 data.items；留空取整体">
          <input className="input" value={cfg.dataPath || ''} onChange={(e) => set({ dataPath: e.target.value })} />
        </Field>
      ) : null}
      <Section title="提取字段 → 变量" hint="按点路径从响应 JSON 取值并转换类型">
        <ListEditor
          columns={[{ label: '路径' }, { label: '变量名' }, { label: '类型', width: '104px' }]}
          rows={rows} onChange={(extract) => set({ extract })}
          onAdd={() => set({ extract: [...rows, { path: '', var: '', kind: 'string' }] })}
          empty="无需提取时留空"
          renderRow={(r, up) => (
            <React.Fragment>
              <input className="input" value={r.path} placeholder="如 data.total" onChange={(e) => up({ path: e.target.value })} />
              <input className="input" value={r.var} placeholder="变量" onChange={(e) => up({ var: e.target.value })} />
              <KindSelect value={r.kind} onChange={(kind) => up({ kind })} />
            </React.Fragment>
          )} />
      </Section>
    </React.Fragment>
  );
});

registerForm('workflow', 'notify', ({ cfg, set }) => (
  <React.Fragment>
    <Field label="Webhook 地址" hint="POST JSON 到此地址，可引用变量">
      <input className="input mono" value={cfg.url || ''} placeholder="https://…" onChange={(e) => set({ url: e.target.value })} />
    </Field>
    <Section title="正文模板" hint="可引用变量，如运行完成：${msg}">
      <textarea className="input mono" rows={5} value={cfg.template || ''} onChange={(e) => set({ template: e.target.value })} />
    </Section>
    <HeadersEditor map={cfg.headers || {}} title="额外请求头" onChange={(headers) => set({ headers })} />
    <div className="etl-row2">
      <Field label="凭据请求头" hint="如 Authorization"><input className="input mono" value={cfg.secretHeader || ''} onChange={(e) => set({ secretHeader: e.target.value })} /></Field>
      <Field label="凭据" hint={cfg.secretCipher ? '已加密保存；重新输入可更换；清空 = 删除' : '保存定义时加密存储'}>
        <input className="input" type="password" value={cfg.secret || ''} placeholder={cfg.secretCipher ? '••••••（保持不变）' : '如 Bearer xxx'}
          onChange={(e) => set({ secret: e.target.value })} />
      </Field>
    </div>
  </React.Fragment>
));

registerForm('workflow', 'sql', ({ cfg, set, ctx }) => (
  <React.Fragment>
    <Field label="数据库连接"><ConnectionSelect value={cfg.connection} onChange={(connection) => set({ connection })} /></Field>
    <Section title="SQL"><SqlEditor value={cfg.sql} onChange={(sql) => set({ sql })} vars={ctx.vars} height={170} /></Section>
    <Field label="影响行数写入变量" hint="可选，如 affected"><input className="input" value={cfg.affectedVar || ''} onChange={(e) => set({ affectedVar: e.target.value })} /></Field>
  </React.Fragment>
));

registerForm('workflow', 'sql_value', ({ cfg, set, ctx }) => {
  const rows = cfg.assign || [];
  return (
    <React.Fragment>
      <Field label="数据库连接"><ConnectionSelect value={cfg.connection} onChange={(connection) => set({ connection })} /></Field>
      <Section title="查询"><SqlEditor value={cfg.sql} onChange={(sql) => set({ sql })} vars={ctx.vars} height={130} /></Section>
      <Section title="结果 → 变量" hint="取第一行">
        <ListEditor
          columns={[{ label: '结果列' }, { label: '变量名' }, { label: '类型', width: '104px' }]}
          rows={rows} onChange={(assign) => set({ assign })}
          onAdd={() => set({ assign: [...rows, { column: '', var: '', kind: 'string' }] })}
          empty="添加至少一个赋值"
          renderRow={(r, up) => (
            <React.Fragment>
              <input className="input" value={r.column} placeholder="如 maxTs" onChange={(e) => up({ column: e.target.value, var: r.var || e.target.value })} />
              <input className="input" value={r.var} placeholder="变量" onChange={(e) => up({ var: e.target.value })} />
              <KindSelect value={r.kind} onChange={(kind) => up({ kind })} />
            </React.Fragment>
          )} />
      </Section>
      <Field label="查询无结果时">
        <Seg value={cfg.onEmpty || 'null'} onChange={(onEmpty) => set({ onEmpty })} options={[{ value: 'null', label: '变量置为 NULL' }, { value: 'fail', label: '作业项失败' }]} />
      </Field>
    </React.Fragment>
  );
});

registerForm('workflow', 'condition', ({ cfg, set, ctx }) => (
  <Section title="条件" hint="可直接引用变量名">
    <ExpressionEditor value={cfg.expr} catalog={ctx.catalog} vars={ctx.vars} multiline
      fields={Object.entries(ctx.vars || {}).map(([name, kind]) => ({ name, kind }))}
      placeholder="如 cnt > 1000" error={ctx.pathError('expr')} onChange={(expr) => set({ expr })} />
  </Section>
));

registerForm('workflow', 'set_vars', ({ cfg, set, ctx }) => {
  const rows = cfg.assign || [];
  const fields = Object.entries(ctx.vars || {}).map(([name, kind]) => ({ name, kind }));
  return (
    <Section title="赋值" hint="按顺序执行，后面的可引用前面的">
      {rows.map((r, i) => (
        <div key={i} className="card" style={{ padding: 12, display: 'flex', flexDirection: 'column', gap: 8, boxShadow: 'none' }}>
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 120px 24px', gap: 6 }}>
            <input className="input input-sm" value={r.var} placeholder="变量名" onChange={(e) => set({ assign: rows.map((x, j) => (j === i ? { ...x, var: e.target.value } : x)) })} />
            <KindSelect value={r.kind} onChange={(kind) => set({ assign: rows.map((x, j) => (j === i ? { ...x, kind } : x)) })} />
            <button type="button" className="etl-list-x" onClick={() => set({ assign: rows.filter((_, j) => j !== i) })}><Icon name="x" size={13} /></button>
          </div>
          <ExpressionEditor value={r.expr} catalog={ctx.catalog} fields={fields} vars={ctx.vars} placeholder="如 addDays(today(), -1)"
            error={ctx.pathError(`assign[${i}].expr`)} onChange={(expr) => set({ assign: rows.map((x, j) => (j === i ? { ...x, expr } : x)) })} />
        </div>
      ))}
      <Btn variant="outline" size="sm" icon="plus" onClick={() => set({ assign: [...rows, { var: '', kind: 'string', expr: '' }] })}>添加赋值</Btn>
    </Section>
  );
});

registerForm('workflow', 'wait', ({ cfg, set }) => (
  <Field label="等待秒数"><input className="input" type="number" min="0" value={cfg.seconds ?? 0} onChange={(e) => set({ seconds: num(e.target.value, 0) })} /></Field>
));

registerForm('workflow', 'success', () => <div className="etl-muted">标记该分支正常结束，无需配置。</div>);

registerForm('workflow', 'abort', ({ cfg, set }) => (
  <Field label="失败原因" hint="可引用变量，如 数量异常：${cnt}">
    <input className="input" value={cfg.message || ''} onChange={(e) => set({ message: e.target.value })} />
  </Field>
));

// ---------------------------------------------------------------- 作业项通用：超时与重试

export function RetryEditor({ node, onChange }) {
  const r = node.retry || {};
  return (
    <div className="etl-row2">
      <Field label="超时（秒）" hint="0 = 不限"><input className="input" type="number" min="0" value={node.timeoutSec || 0} onChange={(e) => onChange({ timeoutSec: num(e.target.value, 0) })} /></Field>
      <Field label="失败重试" hint={r.max ? `每次间隔 ${r.backoffSec || 0} 秒` : '不重试'}>
        <div style={{ display: 'flex', gap: 6 }}>
          <input className="input" type="number" min="0" max="10" value={r.max || 0} title="重试次数" onChange={(e) => onChange({ retry: { ...r, max: num(e.target.value, 0) } })} />
          <input className="input" type="number" min="0" value={r.backoffSec || 0} title="间隔秒数" onChange={(e) => onChange({ retry: { ...r, backoffSec: num(e.target.value, 0) } })} />
        </div>
      </Field>
    </div>
  );
}

export { FieldChip };
