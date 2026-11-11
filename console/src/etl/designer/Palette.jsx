// 组件面板：按分类列出步骤/作业项；拖入画布或点击添加到视图中央。
import React from 'react';
import { Icon } from '../../components/primitives.jsx';
import { CATEGORY, matches } from '../util.js';

export const DND_TYPE = 'application/x-mooncell-etl';

export function Palette({ kind, catalog, onAdd, disabled }) {
  const [kw, setKw] = React.useState('');
  const [collapsed, setCollapsed] = React.useState({});
  const items = (kind === 'workflow' ? catalog.actions : catalog.steps) || [];
  const groups = React.useMemo(() => {
    const g = {};
    items.filter((m) => m.id !== 'start' && matches(m, kw)).forEach((m) => { (g[m.category] = g[m.category] || []).push(m); });
    const order = ['input', 'transform', 'flow', 'lookup', 'join', 'output', 'control', 'utility'];
    return order.filter((c) => g[c]).map((c) => ({
      cat: c, items: g[c].sort((a, b) => (a.maturity === 'planned') - (b.maturity === 'planned')),
    }));
  }, [items, kw]);
  return (
    <aside className="etl-palette" data-readonly={disabled ? '1' : undefined}>
      <div className="etl-palette-head">
        <div className="etl-search" style={{ width: '100%' }}>
          <Icon name="search" size={14} />
          <input className="input input-sm" placeholder={kind === 'workflow' ? '搜索作业项…' : '搜索组件，支持 Kettle 名称'} value={kw} onChange={(e) => setKw(e.target.value)} />
        </div>
      </div>
      <div className="etl-palette-scroll">
        {groups.map(({ cat, items: list }) => {
          const c = CATEGORY[cat] || CATEGORY.utility;
          const isCollapsed = collapsed[cat] && !kw;
          return (
            <div key={cat} className="etl-palette-group">
              <button type="button" className="etl-palette-group-title" onClick={() => setCollapsed((s) => ({ ...s, [cat]: !s[cat] }))}>
                <Icon name={isCollapsed ? 'chevronR' : 'chevronD'} size={12} style={{ color: 'var(--muted-fg)' }} />
                {kind === 'workflow' ? '作业项' : c.label}
                <span className="count">{list.length}</span>
              </button>
              {isCollapsed ? null : list.map((m) => {
                const planned = m.maturity === 'planned';
                return (
                  <button key={m.id} type="button" className="etl-palette-item" data-planned={String(planned)}
                    draggable={!planned && !disabled} title={disabled ? '只读授权：不可修改定义' : m.description}
                    onDragStart={(e) => { e.dataTransfer.setData(DND_TYPE, m.id); e.dataTransfer.effectAllowed = 'move'; }}
                    onClick={() => { if (!planned && !disabled) onAdd(m); }}>
                    <span className="etl-chip" style={{ background: c.soft, color: c.color }}><Icon name={m.icon || 'circle'} size={14} /></span>
                    <span style={{ minWidth: 0, flex: 1 }}>
                      <div className="etl-palette-label">{m.label}</div>
                      <div className="etl-palette-hint">{planned ? '规划中' : (m.keywords && m.keywords[0]) || ''}</div>
                    </span>
                  </button>
                );
              })}
            </div>
          );
        })}
        {!groups.length ? <div className="etl-list-empty">没有匹配的组件</div> : null}
      </div>
    </aside>
  );
}
