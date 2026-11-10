// 数据集成模块共享组件：菜单、表达式编辑器（函数目录）、SQL 编辑器、连接/表/字段选择器、列表编辑器。
import React from 'react';
import { createPortal } from 'react-dom';
import CodeMirror from '@uiw/react-codemirror';
import { EditorView, keymap, placeholder as cmPlaceholder } from '@codemirror/view';
import { autocompletion } from '@codemirror/autocomplete';
import { sql as sqlLang } from '@codemirror/lang-sql';
import { Icon, Btn, Badge, Select, Spinner } from '../components/primitives.jsx';
import { etlApi } from './api.js';
import { KIND_LABEL, KIND_OPTIONS, KIND_SHORT, RUN_STATUS } from './util.js';

// ---------------------------------------------------------------- 主题

export function useDark() {
  const get = () => document.documentElement.getAttribute('data-theme') === 'dark';
  const [dark, setDark] = React.useState(get);
  React.useEffect(() => {
    const mo = new MutationObserver(() => setDark(get()));
    mo.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
    return () => mo.disconnect();
  }, []);
  return dark;
}

// ---------------------------------------------------------------- 菜单 / 浮层

// usePopover：锚定按钮的浮层，自动避让视口边缘，点击外部或 Esc 关闭。
export function usePopover() {
  const [anchor, setAnchor] = React.useState(null);
  const open = (e) => setAnchor(e.currentTarget.getBoundingClientRect());
  const close = () => setAnchor(null);
  React.useEffect(() => {
    if (!anchor) return;
    const onKey = (e) => { if (e.key === 'Escape') close(); };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [anchor]);
  return { anchor, open, close, isOpen: !!anchor };
}

export function Popover({ anchor, onClose, width = 220, height, align = 'left', children, className = 'etl-menu' }) {
  const ref = React.useRef(null);
  React.useEffect(() => {
    if (!anchor) return;
    const onDown = (e) => { if (ref.current && !ref.current.contains(e.target)) onClose(); };
    const t = setTimeout(() => window.addEventListener('mousedown', onDown), 0);
    return () => { clearTimeout(t); window.removeEventListener('mousedown', onDown); };
  }, [anchor, onClose]);
  if (!anchor) return null;
  const vw = window.innerWidth, vh = window.innerHeight;
  let left = align === 'right' ? anchor.right - width : anchor.left;
  left = Math.max(8, Math.min(left, vw - width - 8));
  let top = anchor.bottom + 6;
  const h = height || 260;
  if (top + h > vh - 8) top = Math.max(8, anchor.top - h - 6);
  return createPortal(
    <div ref={ref} className={className} style={{ left, top, width }} role="menu">{children}</div>,
    document.body,
  );
}

export function MenuItem({ icon, label, sub, danger, onClick, disabled }) {
  return (
    <button type="button" role="menuitem" data-danger={danger ? 'true' : undefined} disabled={disabled} onClick={onClick}
      style={disabled ? { opacity: .5, cursor: 'not-allowed' } : undefined}>
      {icon ? <Icon name={icon} size={15} style={{ color: danger ? 'var(--error)' : 'var(--muted-fg)', flex: 'none' }} /> : null}
      <span style={{ minWidth: 0 }}>
        <div>{label}</div>
        {sub ? <div className="etl-menu-sub">{sub}</div> : null}
      </span>
    </button>
  );
}

// ---------------------------------------------------------------- 状态

export function RunStatus({ status, size }) {
  const m = RUN_STATUS[status] || { label: status, tone: 'default' };
  const live = status === 'running' || status === 'queued';
  return (
    <Badge tone={m.tone} style={size === 'lg' ? { height: 26, fontSize: 13, padding: '0 10px' } : undefined}>
      <span className={'dot' + (live ? ' pulse-dot' : '')}></span>{m.label}
    </Badge>
  );
}

export function KindTag({ kind }) {
  return <span className="etl-kind-tag" title={KIND_LABEL[kind] || kind}>{KIND_SHORT[kind] || '?'}</span>;
}

export function FieldChip({ field }) {
  return (
    <span className="etl-field-chip" title={`${field.name}：${KIND_LABEL[field.kind] || field.kind}${field.nativeType ? `（${field.nativeType}）` : ''}`}>
      <KindTag kind={field.kind} />
      <span style={{ overflow: 'hidden', textOverflow: 'ellipsis' }}>{field.name}</span>
    </span>
  );
}

// ---------------------------------------------------------------- 表达式编辑器

// 光标标记：catalog.insert 中的 "|" 是插入后光标位置。
function insertSnippet(view, snippet) {
  const text = snippet.replace('|', '');
  const caret = snippet.indexOf('|');
  const { from, to } = view.state.selection.main;
  view.dispatch({
    changes: { from, to, insert: text },
    selection: { anchor: from + (caret >= 0 ? caret : text.length) },
    scrollIntoView: true,
  });
  view.focus();
}

const IDENT_RE = /^[\p{L}_][\p{L}\p{N}_]*$/u;

function buildCompletions(fields, vars, functions) {
  return (ctx) => {
    const word = ctx.matchBefore(/[\p{L}\p{N}_]+/u);
    if (!word && !ctx.explicit) return null;
    const opts = [];
    (fields || []).forEach((f) => {
      const ok = IDENT_RE.test(f.name);
      opts.push({
        label: ok ? f.name : `field("${f.name}")`, type: 'variable', detail: KIND_LABEL[f.kind] || f.kind, boost: 3,
        info: f.nativeType ? `源类型 ${f.nativeType}` : undefined,
      });
    });
    Object.entries(vars || {}).forEach(([name, kind]) => {
      opts.push({ label: `var("${name}")`, displayLabel: name, type: 'constant', detail: `变量 · ${KIND_LABEL[kind] || kind}`, boost: 2 });
    });
    (functions || []).filter((f) => !f.operator).forEach((f) => {
      opts.push({
        label: f.name, type: 'function', detail: f.signature, info: f.description,
        apply: (view, c, from, to) => {
          view.dispatch({ changes: { from, to, insert: '' } });
          insertSnippet(view, f.insert);
        },
      });
    });
    return { from: word ? word.from : ctx.pos, options: opts, validFor: /^[\p{L}\p{N}_]*$/u };
  };
}

/**
 * ExpressionEditor：单行/多行表达式输入，带字段/变量/函数补全与可点选的函数目录。
 * props: value, onChange, fields（可用字段）, vars（变量 → 类型）, functions/categories（目录）, error, hint, placeholder, multiline
 */
export function ExpressionEditor({ value, onChange, fields, vars, catalog, error, hint, placeholder, multiline, autoFocus }) {
  const dark = useDark();
  const viewRef = React.useRef(null);
  const pop = usePopover();
  const functions = catalog ? catalog.functions : [];
  const ext = React.useMemo(() => [
    autocompletion({ override: [buildCompletions(fields, vars, functions)], icons: false }),
    EditorView.lineWrapping,
    cmPlaceholder(placeholder || '输入表达式，如 upper(trim(name))'),
    ...(multiline ? [] : [keymap.of([{ key: 'Enter', run: () => true }])]),
    EditorView.theme({ '&': { backgroundColor: 'transparent' }, '.cm-gutters': { display: 'none' } }),
  ], [fields, vars, functions, placeholder, multiline]);
  return (
    <div>
      <div className="etl-expr" data-invalid={error ? 'true' : undefined}>
        <CodeMirror value={value || ''} onChange={(v) => onChange(multiline ? v : v.replace(/\n/g, ' '))}
          theme={dark ? 'dark' : 'light'} basicSetup={{ lineNumbers: false, foldGutter: false, highlightActiveLine: false, autocompletion: false }}
          extensions={ext} autoFocus={autoFocus}
          minHeight={multiline ? '60px' : undefined}
          onCreateEditor={(v) => { viewRef.current = v; }} />
        <button type="button" className="etl-expr-fx" data-open={pop.isOpen ? 'true' : undefined} title="函数目录" onClick={pop.open}>ƒx</button>
      </div>
      {error ? <div className="etl-expr-msg" data-tone="error">{error}</div>
        : hint ? <div className="etl-expr-msg" data-tone="muted">{hint}</div> : null}
      <Popover anchor={pop.anchor} onClose={pop.close} width={600} height={420} align="right" className="etl-fncat">
        <FunctionCatalog catalog={catalog} fields={fields} vars={vars}
          onInsert={(snippet) => { if (viewRef.current) insertSnippet(viewRef.current, snippet); pop.close(); }} />
      </Popover>
    </div>
  );
}

function FunctionCatalog({ catalog, fields, vars, onInsert }) {
  const [cat, setCat] = React.useState('字段与变量');
  const [kw, setKw] = React.useState('');
  const fns = (catalog && catalog.functions) || [];
  const cats = (catalog && catalog.functionCategories) || [];
  const list = React.useMemo(() => {
    const k = kw.trim().toLowerCase();
    return fns.filter((f) => (k ? (f.name + f.description + f.signature).toLowerCase().includes(k) : f.category === cat));
  }, [fns, cat, kw]);
  const [sel, setSel] = React.useState(null);
  const cur = list.find((f) => f.name === sel) || list[0];
  const refItems = cat === '字段与变量' && !kw ? [
    ...(fields || []).map((f) => ({ key: 'f:' + f.name, label: f.name, kind: f.kind, insert: IDENT_RE.test(f.name) ? f.name : `field("${f.name}")`, from: '字段' })),
    ...Object.entries(vars || {}).map(([n, k]) => ({ key: 'v:' + n, label: n, kind: k, insert: `var("${n}")`, from: '变量' })),
  ] : [];
  return (
    <React.Fragment>
      <div className="etl-fncat-col" style={{ paddingTop: 6 }}>
        {cats.map((c) => (
          <button key={c} type="button" className="etl-fncat-cat" data-active={String(!kw && c === cat)} onClick={() => { setCat(c); setKw(''); setSel(null); }}>{c}</button>
        ))}
      </div>
      <div className="etl-fncat-col" style={{ display: 'flex', flexDirection: 'column' }}>
        <div className="etl-fncat-search">
          <input className="input input-sm" placeholder="搜索函数…" value={kw} onChange={(e) => { setKw(e.target.value); setSel(null); }} autoFocus />
        </div>
        <div style={{ flex: 1, overflowY: 'auto' }}>
          {refItems.map((r) => (
            <button key={r.key} type="button" className="etl-fncat-fn" onClick={() => onInsert(r.insert)} title={`插入${r.from} ${r.label}`}
              style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
              <KindTag kind={r.kind} /><span style={{ overflow: 'hidden', textOverflow: 'ellipsis' }}>{r.label}</span>
              <span style={{ marginLeft: 'auto', fontFamily: 'var(--font-ui)', fontSize: 11, color: 'var(--muted-fg)' }}>{r.from}</span>
            </button>
          ))}
          {list.map((f) => (
            <button key={f.name} type="button" className="etl-fncat-fn" data-active={String(cur && f.name === cur.name)}
              onClick={() => setSel(f.name)} onDoubleClick={() => onInsert(f.insert)}>{f.name}</button>
          ))}
          {!list.length && !refItems.length ? <div className="etl-list-empty">没有匹配的函数</div> : null}
        </div>
      </div>
      <div className="etl-fncat-col etl-fncat-detail">
        {cur ? (
          <React.Fragment>
            <div>
              <div className="etl-fncat-sig">{cur.signature}</div>
              <div style={{ fontSize: 11.5, color: 'var(--muted-fg)', marginTop: 4 }}>
                {cur.operator ? '运算符' : '返回'} {cur.returns && !cur.operator ? <b style={{ fontWeight: 600 }}>{KIND_LABEL[cur.returns] || (cur.returns === 'any' ? '随参数' : cur.returns)}</b> : null}
              </div>
            </div>
            <div className="etl-fncat-desc">{cur.description}</div>
            {(cur.examples || []).map((e, i) => (
              <div key={i} className="etl-fncat-ex"><div>{e.expr}</div><span>→ {e.result}</span></div>
            ))}
            <div style={{ marginTop: 'auto' }}>
              <Btn variant="primary" size="sm" icon="plus" onClick={() => onInsert(cur.insert)}>插入</Btn>
              <span style={{ fontSize: 11.5, color: 'var(--muted-fg)', marginLeft: 10 }}>双击函数名也可插入</span>
            </div>
          </React.Fragment>
        ) : <div className="etl-muted">选择一个函数查看释义与示例</div>}
      </div>
    </React.Fragment>
  );
}

// ---------------------------------------------------------------- SQL 编辑器

export function SqlEditor({ value, onChange, height = 160, vars }) {
  const dark = useDark();
  const ext = React.useMemo(() => [sqlLang(), EditorView.lineWrapping], []);
  const names = Object.keys(vars || {}).filter((n) => !['RUN_ID', 'RUN_START', 'TRIGGER'].includes(n));
  return (
    <div>
      <div className="etl-expr" style={{ overflow: 'hidden' }}>
        <CodeMirror value={value || ''} onChange={onChange} height={`${height}px`} theme={dark ? 'dark' : 'light'}
          basicSetup={{ lineNumbers: true, foldGutter: false, highlightActiveLine: false }} extensions={ext} />
      </div>
      <div className="etl-expr-msg" data-tone="muted">
        {'${变量} 作为绑定参数（安全，推荐）；#{变量} 只能替换表名/模式名'}
        {names.length ? <span>　可用：{names.map((n) => <code key={n} className="code-chip" style={{ marginLeft: 4 }}>{'${' + n + '}'}</code>)}</span> : null}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------- 选择器

let connCache = null;
export function useConnections() {
  const [list, setList] = React.useState(connCache);
  React.useEffect(() => {
    let alive = true;
    etlApi.connections().then((l) => { connCache = l; if (alive) setList(l); }).catch(() => { if (alive) setList([]); });
    return () => { alive = false; };
  }, []);
  return list;
}
export function forgetConnections() { connCache = null; }

export function ConnectionSelect({ value, onChange }) {
  const list = useConnections();
  if (!list) return <div className="input" style={{ display: 'flex', alignItems: 'center' }}><Spinner size={12} /></div>;
  const missing = value && !list.find((c) => c.id === value);
  const opts = [{ value: '', label: list.length ? '选择连接…' : '尚无连接，请先在「连接」中新建' },
    ...list.map((c) => ({ value: c.id, label: `${c.name} · ${dialectShort(c.dbType)}` })),
    ...(missing ? [{ value, label: '（连接已删除）' }] : [])];
  return <Select options={opts} value={value || ''} onChange={onChange} />;
}

export function dialectShort(t) {
  return { postgres: 'PostgreSQL', mysql: 'MySQL', dm: '达梦', kingbase: 'Kingbase', sqlite: 'SQLite' }[t] || t;
}

// TablePicker：schema + 表（来自连接元数据）；可手输。
export function TablePicker({ connection, schema, table, onChange }) {
  const [schemas, setSchemas] = React.useState(null);
  const [tables, setTables] = React.useState(null);
  const [err, setErr] = React.useState('');
  React.useEffect(() => {
    setSchemas(null); setErr('');
    if (!connection) return;
    etlApi.schemas(connection).then((d) => setSchemas(d)).catch((e) => setErr(e.message));
  }, [connection]);
  React.useEffect(() => {
    setTables(null);
    if (!connection) return;
    etlApi.tables(connection, schema).then((d) => setTables(d.tables || [])).catch((e) => setErr(e.message));
  }, [connection, schema]);
  if (!connection) return <div className="field-hint">先选择连接</div>;
  const schemaOpts = [{ value: '', label: schemas ? `默认（${schemas.default || '—'}）` : '加载中…' },
    ...((schemas && schemas.schemas) || []).map((s) => ({ value: s, label: s }))];
  return (
    <div className="etl-row2">
      <div>
        <label className="field-label">模式</label>
        <Select options={schemaOpts} value={schema || ''} onChange={(v) => onChange({ schema: v, table })} />
      </div>
      <div>
        <label className="field-label">表</label>
        <input className="input" list={`etl-tables-${connection}-${schema}`} value={table || ''} placeholder={tables ? `${tables.length} 张表` : '加载中…'}
          onChange={(e) => onChange({ schema, table: e.target.value })} />
        <datalist id={`etl-tables-${connection}-${schema}`}>
          {(tables || []).map((t) => <option key={t.name} value={t.name}>{t.kind === 'view' ? '视图' : '表'}</option>)}
        </datalist>
      </div>
      {err ? <div className="field-hint" style={{ color: 'var(--error)', gridColumn: '1 / -1' }}>{err}</div> : null}
    </div>
  );
}

export function useColumns(connection, schema, table) {
  const [state, setState] = React.useState({ cols: null, indexes: [], error: '' });
  React.useEffect(() => {
    if (!connection || !table) { setState({ cols: null, indexes: [], error: '' }); return; }
    let alive = true;
    const t = setTimeout(() => {
      etlApi.columns(connection, schema, table)
        .then((d) => { if (alive) setState({ cols: d.columns || [], indexes: d.indexes || [], error: '' }); })
        .catch((e) => { if (alive) setState({ cols: null, indexes: [], error: e.message }); });
    }, 300);
    return () => { alive = false; clearTimeout(t); };
  }, [connection, schema, table]);
  return state;
}

export function FieldSelect({ fields, value, onChange, placeholder, allowEmpty }) {
  const list = fields || [];
  const missing = value && !list.find((f) => f.name === value);
  const opts = [
    ...(allowEmpty || !value ? [{ value: '', label: placeholder || '选择字段…' }] : []),
    ...list.map((f) => ({ value: f.name, label: f.name })),
    ...(missing ? [{ value, label: `${value}（不存在）` }] : []),
  ];
  return <Select options={opts} value={value || ''} onChange={onChange} />;
}

export function KindSelect({ value, onChange, allowAuto, autoLabel }) {
  const opts = [...(allowAuto ? [{ value: '', label: autoLabel || '自动推导' }] : []), ...KIND_OPTIONS];
  return <Select options={opts} value={value || ''} onChange={onChange} />;
}

// ListEditor：等宽网格行编辑器（字段映射、赋值、常量字段等）。
export function ListEditor({ columns, rows, onChange, renderRow, empty, addLabel = '添加', onAdd, extraActions }) {
  const tpl = columns.map((c) => c.width || '1fr').join(' ') + ' 24px';
  const update = (i, patch) => onChange(rows.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  const remove = (i) => onChange(rows.filter((_, j) => j !== i));
  return (
    <div className="etl-list">
      <div className="etl-list-head" style={{ gridTemplateColumns: tpl }}>
        {columns.map((c) => <div key={c.label}>{c.label}</div>)}<div></div>
      </div>
      {rows.length === 0 ? <div className="etl-list-empty">{empty || '暂无'}</div> : null}
      {rows.map((r, i) => (
        <div key={i} className="etl-list-row" style={{ gridTemplateColumns: tpl }}>
          {renderRow(r, (patch) => update(i, patch), i)}
          <button type="button" className="etl-list-x" title="移除" onClick={() => remove(i)}><Icon name="x" size={13} /></button>
        </div>
      ))}
      <div className="etl-list-foot">
        <Btn variant="ghost" size="sm" icon="plus" onClick={onAdd}>{addLabel}</Btn>
        {extraActions}
      </div>
    </div>
  );
}
