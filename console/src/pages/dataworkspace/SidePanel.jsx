// 工作台左侧面板：对象树（含列、搜索）/ 已保存 SQL / 执行历史。
import React from 'react';
import { createPortal } from 'react-dom';
import { Btn, Icon, Spinner, toast, confirmDialog } from '../../components/primitives.jsx';
import { metadataChildren, metadataStructure } from '../../lib/dataresource-api.js';
import { copyText } from './utils.js';

function kindLabelFor(kind, dbType) {
  switch (kind) {
    case 'schema': return dbType === 'mysql' ? '库' : '模式';
    case 'table': return '表';
    case 'view': return '视图';
    case 'matview': return '物化';
    case 'function': return '函数';
    case 'procedure': return '过程';
    case 'sequence': return '序列';
    case 'trigger': return '触发器';
    default: return '';
  }
}

const isRelation = (kind) => kind === 'table' || kind === 'view' || kind === 'matview';

/** 按方言在需要时给标识符加引号 */
export function quoteIdent(name, dbType) {
  const s = String(name);
  if (dbType === 'mysql') return /^[A-Za-z_][A-Za-z0-9_$]*$/.test(s) ? s : `\`${s.replace(/`/g, '``')}\``;
  if (dbType === 'dm') return /^[A-Z_][A-Z0-9_$#]*$/.test(s) ? s : `"${s.replace(/"/g, '""')}"`;
  return /^[a-z_][a-z0-9_$]*$/.test(s) ? s : `"${s.replace(/"/g, '""')}"`;
}

function timeAgo(ts) {
  const d = (Date.now() - ts) / 1000;
  if (d < 60) return '刚刚';
  if (d < 3600) return `${Math.floor(d / 60)} 分钟前`;
  if (d < 86400) return `${Math.floor(d / 3600)} 小时前`;
  const t = new Date(ts);
  return `${t.getMonth() + 1}-${t.getDate()} ${String(t.getHours()).padStart(2, '0')}:${String(t.getMinutes()).padStart(2, '0')}`;
}

function SidePanel({
  resource, completion, defaultSchema, nodeActions, onInsertText, onOpenSQL,
  saved, onDeleteSaved, history, onClearHistory, onBack, onCollapse, refreshKey,
}) {
  const [tab, setTab] = React.useState('objects');
  const [tree, setTree] = React.useState([]);
  const [expanded, setExpanded] = React.useState({});
  const [childrenMap, setChildrenMap] = React.useState({});
  const [columnsMap, setColumnsMap] = React.useState({});
  const [loading, setLoading] = React.useState(false);
  const [filter, setFilter] = React.useState('');
  const [selected, setSelected] = React.useState(null);
  const [menu, setMenu] = React.useState(null);
  const dbType = resource.dbType;

  const loadRoot = React.useCallback(async () => {
    setLoading(true);
    try {
      const nodes = await metadataChildren(resource.id, '');
      const list = Array.isArray(nodes) ? nodes : [];
      setTree(list);
      setChildrenMap({});
      setColumnsMap({});
      // 只有一个 schema 时自动展开，少点一次
      const auto = {};
      if (list.length === 1 && list[0].id) {
        const kids = await metadataChildren(resource.id, list[0].id).catch(() => []);
        setChildrenMap({ [list[0].id]: Array.isArray(kids) ? kids : [] });
        auto[list[0].id] = true;
      }
      setExpanded(auto);
    } catch (e) {
      toast(e.message || '加载元数据失败', { tone: 'error' });
    } finally {
      setLoading(false);
    }
  }, [resource.id]);

  React.useEffect(() => { loadRoot(); }, [loadRoot, refreshKey]);

  React.useEffect(() => {
    if (!menu) return undefined;
    const close = () => setMenu(null);
    window.addEventListener('click', close);
    window.addEventListener('resize', close);
    return () => {
      window.removeEventListener('click', close);
      window.removeEventListener('resize', close);
    };
  }, [menu]);

  const toggle = async (node) => {
    const id = node.id;
    if (expanded[id]) {
      setExpanded((cur) => ({ ...cur, [id]: false }));
      return;
    }
    try {
      if (isRelation(node.kind)) {
        if (!columnsMap[id]) {
          const st = await metadataStructure(resource.id, id);
          setColumnsMap((cur) => ({ ...cur, [id]: st.columns || [] }));
        }
      } else if (!childrenMap[id]) {
        const kids = await metadataChildren(resource.id, id);
        setChildrenMap((cur) => ({ ...cur, [id]: Array.isArray(kids) ? kids : [] }));
      }
      setExpanded((cur) => ({ ...cur, [id]: true }));
    } catch (e) {
      toast(e.message || '加载失败', { tone: 'error' });
    }
  };

  const qualified = (node) => {
    const name = quoteIdent(node.name, dbType);
    if (node.schema && defaultSchema && node.schema.toLowerCase() !== String(defaultSchema).toLowerCase()) {
      return `${quoteIdent(node.schema, dbType)}.${name}`;
    }
    return name;
  };

  const openMenu = (e, node) => {
    const base = nodeActions(node);
    if (!isRelation(node.kind) && base.length === 0) return;
    e.preventDefault();
    e.stopPropagation();
    const items = [
      ...base,
      { sep: true },
      { label: '插入名称到编辑器', action: () => onInsertText(qualified(node)) },
      { label: '复制名称', action: async () => { await copyText(qualified(node)); toast('已复制'); } },
    ];
    const estH = 36 + items.length * 30;
    let x = e.clientX;
    let y = e.clientY;
    if (x + 220 > window.innerWidth - 8) x = Math.max(8, window.innerWidth - 228);
    if (y + estH > window.innerHeight - 8) y = Math.max(8, window.innerHeight - estH - 8);
    setMenu({ x, y, node, items });
  };

  const renderNode = (node, depth = 0) => {
    const id = node.id || node.name;
    const rel = isRelation(node.kind);
    const open = !!expanded[id];
    const kids = rel ? null : childrenMap[id];
    const cols = rel ? columnsMap[id] : null;
    const kindLabel = kindLabelFor(node.kind, dbType);
    const expandable = rel || !['function', 'procedure', 'sequence', 'trigger'].includes(node.kind);
    return (
      <div key={id}>
        <div className={`dr-tree-node${selected === id ? ' is-sel' : ''}`} style={{ paddingLeft: 6 + depth * 13 }}
          title={rel ? '双击查看数据 · 右键更多操作' : undefined}
          onClick={() => { setSelected(id); if (!rel && expandable) toggle(node); }}
          onDoubleClick={() => rel && nodeActions(node)[0]?.action()}
          onContextMenu={(e) => { setSelected(id); openMenu(e, node); }}>
          {expandable ? (
            <span className="dr-tree-caret" onClick={(e) => { if (rel) { e.stopPropagation(); toggle(node); } }}>
              <Icon name={open ? 'chevronD' : 'chevronR'} size={12} />
            </span>
          ) : <span style={{ width: 12 }} />}
          <Icon name={rel ? 'box' : 'folder'} size={13} />
          <span className="dr-tree-label">{node.name}</span>
          {kindLabel ? <span className="dr-tree-kind">{kindLabel}</span> : null}
        </div>
        {open && kids ? kids.map((child) => renderNode(child, depth + 1)) : null}
        {open && kids && kids.length === 0 ? <div className="dr-tree-empty" style={{ paddingLeft: 26 + depth * 13 }}>空</div> : null}
        {open && cols ? cols.map((c) => (
          <div key={`${id}:${c.name}`} className="dr-tree-node dr-tree-col" style={{ paddingLeft: 26 + depth * 13 }}
            title={`${c.name} ${c.dataType}${c.isNullable ? '' : ' NOT NULL'}${c.comment ? ` — ${c.comment}` : ''}\n双击插入列名`}
            onDoubleClick={() => onInsertText(quoteIdent(c.name, dbType))}>
            <span className="dr-tree-label mono">{c.name}</span>
            <span className="dr-tree-kind">{c.dataType}</span>
          </div>
        )) : null}
      </div>
    );
  };

  const q = filter.trim().toLowerCase();
  const matches = React.useMemo(() => {
    if (!q) return [];
    const objs = completion?.objects || [];
    return objs.filter((o) => o.name.toLowerCase().includes(q) || `${o.schema}.${o.name}`.toLowerCase().includes(q)).slice(0, 300);
  }, [q, completion]);

  const savedFiltered = React.useMemo(() => {
    if (!q) return saved;
    return saved.filter((s) => String(s.name).toLowerCase().includes(q) || String(s.sqlText || '').toLowerCase().includes(q));
  }, [q, saved]);
  const historyFiltered = React.useMemo(() => {
    if (!q) return history;
    return history.filter((h) => h.sql.toLowerCase().includes(q));
  }, [q, history]);

  return (
    <aside className="card dr-tree-pane">
      <div className="dr-pane-head">
        <div className="dr-pane-head-meta" title={`${resource.host}:${resource.port}/${resource.databaseName || ''}`}>
          <Btn size="sm" variant="ghost" icon="chevronL" title="返回资源列表" onClick={onBack} />
          <span className="dr-pane-title">{resource.name}</span>
        </div>
        <div style={{ display: 'flex', gap: 2 }}>
          {tab === 'objects' ? <Btn size="sm" variant="ghost" icon="rotate" title="刷新对象" disabled={loading} onClick={loadRoot} /> : null}
          <Btn size="sm" variant="ghost" icon="chevronL" title="折叠侧栏" onClick={onCollapse} />
        </div>
      </div>
      <div className="dr-side-tabs" role="tablist">
        {[['objects', '对象'], ['saved', `已保存${saved.length ? ` ${saved.length}` : ''}`], ['history', '历史']].map(([k, label]) => (
          <button key={k} type="button" role="tab" aria-selected={tab === k} className={tab === k ? 'is-active' : undefined}
            onClick={() => setTab(k)}>{label}</button>
        ))}
      </div>
      <div className="dr-side-search">
        <Icon name="search" size={13} />
        <input value={filter} onChange={(e) => setFilter(e.target.value)}
          placeholder={tab === 'objects' ? '搜索表/视图' : tab === 'saved' ? '搜索已保存 SQL' : '搜索历史'} />
        {filter ? <button type="button" className="dr-side-search-x" onClick={() => setFilter('')}><Icon name="x" size={12} /></button> : null}
      </div>
      <div className="dr-tree-scroll">
        {tab === 'objects' ? (
          <React.Fragment>
            {loading ? <div className="dr-inline-status"><Spinner size={13} />加载中</div> : null}
            {!q && !loading && tree.length === 0 ? <div className="dr-inline-status">暂无元数据</div> : null}
            {!q ? tree.map((node) => renderNode(node)) : null}
            {q ? (
              <React.Fragment>
                {!completion ? <div className="dr-inline-status"><Spinner size={13} />正在加载对象清单</div> : null}
                {completion && matches.length === 0 ? <div className="dr-inline-status">无匹配对象</div> : null}
                {matches.map((o) => renderNode({ ...o }, 0))}
                {completion?.truncated ? <div className="dr-inline-status">对象过多，仅搜索前 5000 个</div> : null}
              </React.Fragment>
            ) : null}
          </React.Fragment>
        ) : null}
        {tab === 'saved' ? (
          savedFiltered.length === 0 ? <div className="dr-inline-status">{saved.length ? '无匹配' : '暂无保存的 SQL · 编辑器内 Ctrl+S 保存'}</div> : (
            savedFiltered.map((item) => (
              <div key={item.id} className="dr-side-item" title={item.sqlText}>
                <button type="button" className="dr-side-item-main" onClick={() => onOpenSQL(item.sqlText || '', item.name, item)}>
                  <strong>{item.name}</strong>
                  <span className="mono">{String(item.sqlText || '').split('\n')[0]}</span>
                </button>
                <Btn size="sm" variant="ghost" icon="trash" title="删除" onClick={async () => {
                  const ok = await confirmDialog({ title: '删除保存的 SQL', message: `确认删除「${item.name}」？`, tone: 'danger' });
                  if (ok) onDeleteSaved(item);
                }} />
              </div>
            ))
          )
        ) : null}
        {tab === 'history' ? (
          <React.Fragment>
            {historyFiltered.length === 0 ? <div className="dr-inline-status">{history.length ? '无匹配' : '暂无执行历史'}</div> : null}
            {historyFiltered.map((h, i) => (
              <div key={`${h.at}-${i}`} className="dr-side-item" title={h.sql}>
                <button type="button" className="dr-side-item-main" onClick={() => onOpenSQL(h.sql, null, null)}>
                  <span className="dr-hist-meta">
                    <span className={`dr-hist-dot ${h.ok ? 'ok' : 'err'}`} />
                    {timeAgo(h.at)} · {h.ms}ms{h.info ? ` · ${h.info}` : ''}
                  </span>
                  <span className="mono">{h.sql.split('\n')[0]}</span>
                </button>
                <Btn size="sm" variant="ghost" icon="copy" title="复制" onClick={async () => { await copyText(h.sql); toast('已复制'); }} />
              </div>
            ))}
            {history.length ? (
              <div style={{ padding: 8, textAlign: 'center' }}>
                <Btn size="sm" variant="ghost" onClick={onClearHistory}>清空历史</Btn>
              </div>
            ) : null}
          </React.Fragment>
        ) : null}
      </div>
      {menu ? createPortal(
        <div className="card dr-context-menu" style={{ left: menu.x, top: menu.y }}
          onClick={(e) => e.stopPropagation()} onContextMenu={(e) => e.preventDefault()}>
          <div className="dr-context-title">{menu.node.name}</div>
          {menu.items.map((it, i) => (it.sep ? <div key={`s${i}`} className="dr-menu-sep" /> : (
            <button key={it.label} type="button" onClick={() => { setMenu(null); it.action(); }}>{it.label}</button>
          )))}
        </div>, document.body,
      ) : null}
    </aside>
  );
}

export default React.memo(SidePanel);
