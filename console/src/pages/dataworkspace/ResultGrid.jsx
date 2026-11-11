// 结果表格：固定行高虚拟滚动、列宽拖拽、本页排序、单元格选择/复制、详情面板、就地编辑。
import React from 'react';
import { createPortal } from 'react-dom';
import { Btn, Spinner, toast, confirmDialog } from '../../components/primitives.jsx';
import { cellIsBinary, cellText, copyText, toTSV, toInsertSQL } from './utils.js';

const ROW_H = 28;
const OVERSCAN = 12;
const NUM_COL_W = 56;
const ACT_COL_W = 76;

function estimateWidths(columns, rows) {
  return columns.map((c, i) => {
    let w = String(c).length * 7.5 + 40;
    const n = Math.min(rows.length, 40);
    for (let r = 0; r < n; r += 1) {
      const t = cellText(rows[r][i]);
      w = Math.max(w, Math.min(t.length, 48) * 7 + 22);
    }
    return Math.round(Math.max(72, Math.min(360, w)));
  });
}

function compareCells(a, b) {
  if (a == null && b == null) return 0;
  if (a == null) return 1;
  if (b == null) return -1;
  const na = typeof a === 'number' ? a : (typeof a === 'string' && a.trim() !== '' && Number.isFinite(Number(a)) ? Number(a) : NaN);
  const nb = typeof b === 'number' ? b : (typeof b === 'string' && b.trim() !== '' && Number.isFinite(Number(b)) ? Number(b) : NaN);
  if (!Number.isNaN(na) && !Number.isNaN(nb)) return na - nb;
  return cellText(a).localeCompare(cellText(b), 'zh-CN', { numeric: true });
}

function prettyValue(v) {
  if (v == null) return { text: 'NULL', json: false };
  if (cellIsBinary(v)) return { text: `二进制 ${v.size ?? '?'} 字节\n\nbase64 预览（最多 256 字节）：\n${v.preview || ''}`, json: false };
  const s = String(v);
  const t = s.trim();
  if ((t.startsWith('{') && t.endsWith('}')) || (t.startsWith('[') && t.endsWith(']'))) {
    try { return { text: JSON.stringify(JSON.parse(t), null, 2), json: true }; } catch (e) { /* 非 JSON */ }
  }
  return { text: s, json: false };
}

function CellView({ v }) {
  if (v == null) return <span className="dr-null">NULL</span>;
  if (cellIsBinary(v)) return <span className="mono dr-binary">&lt;binary {v.size}B&gt;</span>;
  if (typeof v === 'boolean') return <span className="mono">{String(v)}</span>;
  return String(v);
}

function ResultGrid({
  result, canEdit, dbType, busy, onSaveEdits, onPendingChange,
}) {
  const columns = result.columns || [];
  const rows = result.rows || [];
  const editable = result.editable;
  const pkSet = React.useMemo(() => {
    const s = new Set();
    (editable?.primaryKeys || []).forEach((k) => s.add(String(k).toLowerCase()));
    return s;
  }, [editable]);

  const [widths, setWidths] = React.useState(() => estimateWidths(columns, rows));
  const [sort, setSort] = React.useState(null); // {col, dir}
  const [sel, setSel] = React.useState(null); // {r: 原始行下标, c: 列下标；c=-1 表示整行}
  const [detail, setDetail] = React.useState(false);
  const [menu, setMenu] = React.useState(null);
  const [scrollTop, setScrollTop] = React.useState(0);
  const [viewportH, setViewportH] = React.useState(400);
  const [editingRows, setEditingRows] = React.useState(() => new Set());
  const [deletedRows, setDeletedRows] = React.useState(() => new Set());
  const [drafts, setDrafts] = React.useState(() => new Map());
  const [focusCell, setFocusCell] = React.useState(null);
  const [saving, setSaving] = React.useState(false);
  const scrollRef = React.useRef(null);
  const rafRef = React.useRef(0);

  // 结果变化：重置所有本地状态
  React.useEffect(() => {
    setWidths(estimateWidths(columns, rows));
    setSort(null);
    setSel(null);
    setDetail(false);
    setEditingRows(new Set());
    setDeletedRows(new Set());
    setDrafts(new Map());
    if (scrollRef.current) scrollRef.current.scrollTop = 0;
    setScrollTop(0);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [result]);

  const pending = editingRows.size > 0 || deletedRows.size > 0;
  React.useEffect(() => { onPendingChange?.(pending); }, [pending, onPendingChange]);

  React.useEffect(() => {
    const el = scrollRef.current;
    if (!el) return undefined;
    const ro = new ResizeObserver(() => setViewportH(el.clientHeight || 400));
    ro.observe(el);
    setViewportH(el.clientHeight || 400);
    return () => ro.disconnect();
  }, []);

  React.useEffect(() => {
    if (!menu) return undefined;
    const close = () => setMenu(null);
    window.addEventListener('click', close);
    window.addEventListener('resize', close);
    window.addEventListener('blur', close);
    return () => {
      window.removeEventListener('click', close);
      window.removeEventListener('resize', close);
      window.removeEventListener('blur', close);
    };
  }, [menu]);

  const order = React.useMemo(() => {
    const idx = rows.map((_, i) => i);
    if (!sort) return idx;
    const dir = sort.dir === 'desc' ? -1 : 1;
    return idx.sort((a, b) => dir * compareCells(rows[a][sort.col], rows[b][sort.col]) || a - b);
  }, [rows, sort]);
  const displayPos = React.useMemo(() => {
    const m = new Map();
    order.forEach((ri, p) => m.set(ri, p));
    return m;
  }, [order]);

  const onScroll = (e) => {
    const top = e.currentTarget.scrollTop;
    cancelAnimationFrame(rafRef.current);
    rafRef.current = requestAnimationFrame(() => setScrollTop(top));
  };

  const total = order.length;
  const start = Math.max(0, Math.floor(scrollTop / ROW_H) - OVERSCAN);
  const end = Math.min(total, Math.ceil((scrollTop + viewportH) / ROW_H) + OVERSCAN);

  const rowValue = (ri) => (editingRows.has(ri) && drafts.has(ri) ? drafts.get(ri) : rows[ri]);

  // ---- 就地编辑 ----
  const beginEdit = (ri, ci = null) => {
    if (!canEdit) return;
    setEditingRows((prev) => new Set(prev).add(ri));
    setDrafts((prev) => {
      if (prev.has(ri)) return prev;
      const next = new Map(prev);
      next.set(ri, rows[ri].slice());
      return next;
    });
    if (ci != null) setFocusCell({ r: ri, c: ci });
  };
  const cancelRow = (ri) => {
    setEditingRows((prev) => { const n = new Set(prev); n.delete(ri); return n; });
    setDeletedRows((prev) => { const n = new Set(prev); n.delete(ri); return n; });
    setDrafts((prev) => { const n = new Map(prev); n.delete(ri); return n; });
  };
  const cancelAll = () => {
    setEditingRows(new Set());
    setDeletedRows(new Set());
    setDrafts(new Map());
  };
  const markDelete = (ri) => {
    if (!canEdit) return;
    if (rows[ri].some(cellIsBinary)) {
      toast('该行含二进制列，无法构造完整乐观删除条件；请用带条件的 SQL 删除', { tone: 'warn' });
      return;
    }
    beginEdit(ri);
    setDeletedRows((prev) => new Set(prev).add(ri));
  };
  const setDraftCell = (ri, ci, value) => {
    setDrafts((prev) => {
      const next = new Map(prev);
      const row = (next.get(ri) || rows[ri]).slice();
      row[ci] = value;
      next.set(ri, row);
      return next;
    });
  };

  const save = async () => {
    const pks = editable?.primaryKeys || [];
    const updates = [];
    const deletes = [];
    const touched = new Set([...editingRows, ...deletedRows]);
    for (const ri of touched) {
      const orig = rows[ri];
      if (!orig) continue;
      const keys = {};
      pks.forEach((pk) => {
        const ci = columns.findIndex((c) => String(c).toLowerCase() === String(pk).toLowerCase());
        if (ci >= 0) keys[pk] = orig[ci];
      });
      if (deletedRows.has(ri)) {
        const old = {};
        columns.forEach((col, ci) => {
          if (pkSet.has(String(col).toLowerCase()) || cellIsBinary(orig[ci])) return;
          old[col] = orig[ci] == null ? null : orig[ci];
        });
        deletes.push({ keys, old });
        continue;
      }
      const draft = drafts.get(ri) || orig;
      const set = {};
      const old = {};
      let changed = false;
      columns.forEach((col, ci) => {
        if (pkSet.has(String(col).toLowerCase()) || cellIsBinary(orig[ci]) || cellIsBinary(draft[ci])) return;
        const before = orig[ci];
        const after = draft[ci];
        const same = (before == null && after == null) || (before != null && after != null && String(before) === String(after));
        if (!same) {
          set[col] = after;
          old[col] = before == null ? null : before;
          changed = true;
        }
      });
      if (changed) updates.push({ keys, set, old });
    }
    if (!updates.length && !deletes.length) {
      toast('没有需要保存的变更', { tone: 'warn' });
      return;
    }
    const ok = await confirmDialog({
      title: '确认保存就地编辑',
      message: `将更新 ${updates.length} 行、删除 ${deletes.length} 行。自动提交模式下直接写入数据库，无法通过工作台回滚。`,
      confirmText: '保存',
      tone: 'danger',
    });
    if (!ok) return;
    setSaving(true);
    try {
      const done = await onSaveEdits({ updates, deletes });
      if (done) cancelAll();
    } finally {
      setSaving(false);
    }
  };

  // ---- 复制 / 菜单 ----
  const copyCell = async (ri, ci) => {
    const ok = await copyText(cellText(rowValue(ri)[ci]));
    toast(ok ? '已复制' : '复制失败，请手动选择文本', { tone: ok ? undefined : 'warn' });
  };
  const copyRows = async (ris, fmt) => {
    const data = ris.map((ri) => rowValue(ri));
    let text;
    if (fmt === 'json') {
      text = JSON.stringify(data.map((r) => Object.fromEntries(columns.map((c, i) => [c, cellIsBinary(r[i]) ? cellText(r[i]) : r[i]]))), null, 2);
    } else if (fmt === 'insert') {
      const table = editable?.table ? (editable.schema ? `${editable.schema}.${editable.table}` : editable.table) : 'table_name';
      text = toInsertSQL(table, columns, data, dbType);
    } else {
      text = toTSV(columns, data, fmt === 'tsv-header');
    }
    const ok = await copyText(text);
    toast(ok ? `已复制 ${ris.length} 行` : '复制失败', { tone: ok ? undefined : 'warn' });
  };

  const openMenu = (e, ri, ci) => {
    e.preventDefault();
    e.stopPropagation();
    setSel({ r: ri, c: ci });
    const items = [];
    if (ci >= 0) {
      items.push({ label: '复制值', action: () => copyCell(ri, ci) });
      items.push({ label: '查看详情', action: () => setDetail(true) });
    }
    items.push({ label: '复制行（TSV）', action: () => copyRows([ri], 'tsv') });
    items.push({ label: '复制行（JSON）', action: () => copyRows([ri], 'json') });
    items.push({ label: '复制为 INSERT', action: () => copyRows([ri], 'insert') });
    items.push({ label: '复制本页（含表头）', action: () => copyRows(order, 'tsv-header') });
    if (ci >= 0) items.push({ label: `复制列名「${columns[ci]}」`, action: async () => { await copyText(String(columns[ci])); toast('已复制'); } });
    if (canEdit) {
      items.push({ sep: true });
      if (ci >= 0 && !pkSet.has(String(columns[ci]).toLowerCase()) && !cellIsBinary(rows[ri][ci])) {
        items.push({ label: '编辑此单元格', action: () => beginEdit(ri, ci) });
        items.push({ label: '设为 NULL', action: () => { beginEdit(ri); setDraftCell(ri, ci, null); } });
      }
      items.push(deletedRows.has(ri)
        ? { label: '撤销删除', action: () => cancelRow(ri) }
        : { label: '删除此行', danger: true, action: () => markDelete(ri) });
    }
    const estH = 16 + items.length * 30;
    let x = e.clientX;
    let y = e.clientY;
    if (x + 220 > window.innerWidth - 8) x = Math.max(8, window.innerWidth - 228);
    if (y + estH > window.innerHeight - 8) y = Math.max(8, window.innerHeight - estH - 8);
    setMenu({ x, y, items });
  };

  // ---- 键盘 ----
  const ensureVisible = (ri) => {
    const el = scrollRef.current;
    if (!el) return;
    const p = displayPos.get(ri) ?? 0;
    const top = p * ROW_H;
    const headH = 30;
    if (top < el.scrollTop) el.scrollTop = top;
    else if (top + ROW_H > el.scrollTop + el.clientHeight - headH) el.scrollTop = top + ROW_H - el.clientHeight + headH;
  };
  const onKeyDown = (e) => {
    if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA') return;
    if (!sel || total === 0) return;
    const p = displayPos.get(sel.r) ?? 0;
    const move = (np, nc) => {
      const cp = Math.max(0, Math.min(total - 1, np));
      const ri = order[cp];
      setSel({ r: ri, c: Math.max(-1, Math.min(columns.length - 1, nc)) });
      ensureVisible(ri);
      e.preventDefault();
    };
    if (e.key === 'ArrowDown') move(p + 1, sel.c);
    else if (e.key === 'ArrowUp') move(p - 1, sel.c);
    else if (e.key === 'ArrowLeft') move(p, sel.c - 1);
    else if (e.key === 'ArrowRight') move(p, sel.c + 1);
    else if (e.key === 'PageDown') move(p + Math.floor(viewportH / ROW_H), sel.c);
    else if (e.key === 'PageUp') move(p - Math.floor(viewportH / ROW_H), sel.c);
    else if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'c') {
      e.preventDefault();
      if (sel.c < 0) copyRows([sel.r], 'tsv');
      else copyCell(sel.r, sel.c);
    } else if (e.key === 'Enter') {
      e.preventDefault();
      if (canEdit && sel.c >= 0 && !pkSet.has(String(columns[sel.c]).toLowerCase())) beginEdit(sel.r, sel.c);
      else setDetail(true);
    } else if (e.key === 'Escape') {
      setDetail(false);
    }
  };

  // ---- 列宽拖拽 ----
  const startResize = (ci, e) => {
    e.preventDefault();
    e.stopPropagation();
    const startX = e.clientX;
    const startW = widths[ci];
    const move = (ev) => {
      const w = Math.max(48, Math.min(1200, startW + ev.clientX - startX));
      setWidths((prev) => { const n = prev.slice(); n[ci] = w; return n; });
    };
    const up = () => {
      window.removeEventListener('mousemove', move);
      window.removeEventListener('mouseup', up);
    };
    window.addEventListener('mousemove', move);
    window.addEventListener('mouseup', up);
  };
  const toggleSort = (ci) => {
    if (pending) {
      toast('有未保存的编辑，暂不能排序', { tone: 'warn' });
      return;
    }
    setSort((s) => {
      if (!s || s.col !== ci) return { col: ci, dir: 'asc' };
      if (s.dir === 'asc') return { col: ci, dir: 'desc' };
      return null;
    });
  };

  const tableWidth = NUM_COL_W + (canEdit ? ACT_COL_W : 0) + widths.reduce((a, b) => a + b, 0);
  const offset = result.offset || 0;
  const detailValue = sel && sel.c >= 0 && rows[sel.r] ? prettyValue(rowValue(sel.r)[sel.c]) : null;

  return (
    <div className="dr-grid-wrap">
      {pending ? (
        <div className="dr-grid-editbar">
          <span>编辑中 {editingRows.size} 行{deletedRows.size ? ` · 删除 ${deletedRows.size} 行` : ''}</span>
          <div style={{ flex: 1 }} />
          <Btn size="sm" variant="ghost" disabled={saving} onClick={cancelAll}>取消全部</Btn>
          <Btn size="sm" variant="primary" disabled={saving || busy} onClick={save}>{saving ? <Spinner size={12} /> : '保存变更'}</Btn>
        </div>
      ) : null}
      <div className="dr-grid-body">
        <div className="dr-grid-scroll" ref={scrollRef} onScroll={onScroll} tabIndex={0} onKeyDown={onKeyDown}>
          <table className="dr-grid" style={{ width: tableWidth }}>
            <colgroup>
              <col style={{ width: NUM_COL_W }} />
              {canEdit ? <col style={{ width: ACT_COL_W }} /> : null}
              {widths.map((w, i) => <col key={i} style={{ width: w }} />)}
            </colgroup>
            <thead>
              <tr>
                <th className="dr-grid-num">#</th>
                {canEdit ? <th className="dr-grid-act">操作</th> : null}
                {columns.map((col, ci) => (
                  <th key={`${col}-${ci}`} title={`${col}${sort?.col === ci ? '（本页排序）' : '，点击按本页排序'}`}>
                    <button type="button" className="dr-grid-th" onClick={() => toggleSort(ci)}>
                      <span className="dr-grid-th-name">{col}</span>
                      {pkSet.has(String(col).toLowerCase()) ? <span className="dr-pk-mark">PK</span> : null}
                      {sort?.col === ci ? <span className="dr-sort">{sort.dir === 'asc' ? '▲' : '▼'}</span> : null}
                    </button>
                    <span className="dr-col-resize" onMouseDown={(e) => startResize(ci, e)} />
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {start > 0 ? <tr style={{ height: start * ROW_H }} aria-hidden="true"><td colSpan={columns.length + 2} /></tr> : null}
              {order.slice(start, end).map((ri, k) => {
                const p = start + k;
                const editing = editingRows.has(ri);
                const deleted = deletedRows.has(ri);
                const row = rowValue(ri);
                const rowSel = sel && sel.r === ri;
                return (
                  <tr key={ri}
                    className={[deleted ? 'dr-row-deleted' : '', editing ? 'dr-row-editing' : '', rowSel && sel.c < 0 ? 'is-sel' : ''].filter(Boolean).join(' ') || undefined}>
                    <td className="dr-grid-num" onClick={() => setSel({ r: ri, c: -1 })} onContextMenu={(e) => openMenu(e, ri, -1)}>
                      {offset + ri + 1}
                    </td>
                    {canEdit ? (
                      <td className="dr-grid-act">
                        {!editing ? (
                          <React.Fragment>
                            <button type="button" className="dr-row-act" disabled={busy || saving} onClick={() => beginEdit(ri)}>改</button>
                            <button type="button" className="dr-row-act dr-row-act-danger" disabled={busy || saving} onClick={() => markDelete(ri)}>删</button>
                          </React.Fragment>
                        ) : (
                          <button type="button" className="dr-row-act" disabled={saving} onClick={() => cancelRow(ri)}>{deleted ? '撤销' : '取消'}</button>
                        )}
                      </td>
                    ) : null}
                    {columns.map((col, ci) => {
                      const v = row[ci];
                      const isPK = pkSet.has(String(col).toLowerCase());
                      const binary = cellIsBinary(v) || cellIsBinary(rows[ri][ci]);
                      const cellSel = rowSel && sel.c === ci;
                      if (editing && !deleted && !isPK && !binary) {
                        return (
                          <td key={ci} className="dr-grid-editcell">
                            <input
                              className="dr-cell-input"
                              value={v == null ? '' : cellText(v)}
                              placeholder={v == null ? 'NULL' : ''}
                              autoFocus={focusCell && focusCell.r === ri && focusCell.c === ci}
                              onFocus={() => setSel({ r: ri, c: ci })}
                              onChange={(e) => setDraftCell(ri, ci, e.target.value)}
                              onKeyDown={(e) => { if (e.key === 'Escape') { e.currentTarget.blur(); scrollRef.current?.focus(); } }}
                            />
                            <button type="button" className="dr-cell-null-btn" title="设为 NULL" onClick={() => setDraftCell(ri, ci, null)}>N</button>
                          </td>
                        );
                      }
                      return (
                        <td key={ci}
                          className={cellSel ? 'is-sel' : undefined}
                          onMouseDown={() => setSel({ r: ri, c: ci })}
                          onDoubleClick={() => {
                            if (canEdit && !isPK && !binary) beginEdit(ri, ci);
                            else setDetail(true);
                          }}
                          onContextMenu={(e) => openMenu(e, ri, ci)}>
                          <CellView v={v} />
                        </td>
                      );
                    })}
                  </tr>
                );
              })}
              {end < total ? <tr style={{ height: (total - end) * ROW_H }} aria-hidden="true"><td colSpan={columns.length + 2} /></tr> : null}
            </tbody>
          </table>
          {total === 0 ? <div className="dr-grid-empty">查询成功，没有返回行</div> : null}
        </div>
        {detail && detailValue ? (
          <aside className="dr-cell-detail">
            <div className="dr-cell-detail-head">
              <strong className="mono" title={columns[sel.c]}>{columns[sel.c]}</strong>
              <span className="dr-muted">第 {offset + sel.r + 1} 行{detailValue.json ? ' · JSON' : ''}</span>
              <div style={{ flex: 1 }} />
              <Btn size="sm" variant="ghost" icon="copy" title="复制值" onClick={() => copyCell(sel.r, sel.c)} />
              <Btn size="sm" variant="ghost" icon="x" title="关闭 (Esc)" onClick={() => setDetail(false)} />
            </div>
            <pre className="dr-cell-detail-body">{detailValue.text}</pre>
          </aside>
        ) : null}
      </div>
      {menu ? createPortal(
        <div className="card dr-context-menu" style={{ left: menu.x, top: menu.y }}
          onClick={(e) => e.stopPropagation()} onContextMenu={(e) => e.preventDefault()}>
          {menu.items.map((it, i) => (it.sep ? <div key={`s${i}`} className="dr-menu-sep" /> : (
            <button key={it.label} type="button" className={it.danger ? 'is-danger' : undefined}
              onClick={() => { setMenu(null); it.action(); }}>{it.label}</button>
          )))}
        </div>, document.body,
      ) : null}
    </div>
  );
}

export default React.memo(ResultGrid);
