// 数据资源工作台：元数据树 + SQL 编辑器 + 结果/结构/导入导出。
import React from 'react';
import { createPortal } from 'react-dom';
import CodeMirror from '@uiw/react-codemirror';
import { sql } from '@codemirror/lang-sql';
import { format as formatSQL } from 'sql-formatter';
import { useMC } from '../lib/data.js';
import {
  Btn, Icon, Spinner, EmptyState, Dialog, toast, Badge, Field, confirmDialog,
} from '../components/primitives.jsx';
import {
  createWorkspace, deleteWorkspace, executeSQL, patchAutoCommit, applyRowEdits,
  commitWorkspace, rollbackWorkspace, metadataChildren, metadataStructure, metadataDDL,
  sqlTemplate, listSavedSQL, createSavedSQL, updateSavedSQL, deleteSavedSQL,
  exportWorkspace, previewImport, selectImportSheet, executeImport, deleteImport,
} from '../lib/dataresource-api.js';

const TREE_MIN = 180;
const TREE_MAX = 480;
const EDITOR_MIN = 120;
const EDITOR_MAX = 560;

/** schema 节点：MySQL 下是 database（库）；PG/Kingbase/DM 是 schema/owner（模式） */
function kindLabelFor(kind, dbType) {
  if (kind === 'schema') {
    return dbType === 'mysql' ? '库' : '模式';
  }
  if (kind === 'table') return '表';
  if (kind === 'view') return '视图';
  if (kind === 'matview') return '物化';
  if (kind === 'function') return '函数';
  if (kind === 'procedure') return '过程';
  if (kind === 'sequence') return '序列';
  if (kind === 'trigger') return '触发器';
  return '';
}

function cellIsBinary(v) {
  return v != null && typeof v === 'object' && v.type === 'binary';
}

function cellToEditString(v) {
  if (v == null) return '';
  if (cellIsBinary(v)) return '';
  return String(v);
}

/** 单元格哨兵：显式 NULL（与空字符串区分） */
const CELL_NULL = { __null: true };

function isCellNull(v) {
  return v === null || (v && typeof v === 'object' && v.__null);
}

function isTreeLeaf(kind) {
  return kind === 'table' || kind === 'view' || kind === 'matview';
}

function dialectFor(dbType) {
  if (dbType === 'mysql') return 'mysql';
  if (dbType === 'dm') return 'plsql';
  return 'postgresql';
}

function cellDisplay(v) {
  if (v == null) return <span style={{ color: 'var(--muted-fg)' }}>NULL</span>;
  if (typeof v === 'object' && v.type === 'binary') {
    return <span className="mono" style={{ fontSize: 11 }}>&lt;binary {v.size}B&gt;</span>;
  }
  return String(v);
}

function findStatementRange(source, cursor) {
  const ranges = [];
  let start = 0;
  let state = 'normal';
  let dollarTag = '';
  for (let i = 0; i < source.length; i += 1) {
    const c = source[i];
    const next = source[i + 1];
    if (state === 'line') {
      if (c === '\n') state = 'normal';
      continue;
    }
    if (state === 'block') {
      if (c === '*' && next === '/') { state = 'normal'; i += 1; }
      continue;
    }
    if (state === 'single') {
      if (c === "'" && next === "'") { i += 1; continue; }
      if (c === "'") state = 'normal';
      continue;
    }
    if (state === 'double') {
      if (c === '"' && next === '"') { i += 1; continue; }
      if (c === '"') state = 'normal';
      continue;
    }
    if (state === 'backtick') {
      if (c === '`' && next === '`') { i += 1; continue; }
      if (c === '`') state = 'normal';
      continue;
    }
    if (state === 'dollar') {
      if (source.startsWith(dollarTag, i)) {
        i += dollarTag.length - 1;
        state = 'normal';
      }
      continue;
    }
    if (c === '-' && next === '-') { state = 'line'; i += 1; continue; }
    if (c === '/' && next === '*') { state = 'block'; i += 1; continue; }
    if (c === "'") { state = 'single'; continue; }
    if (c === '"') { state = 'double'; continue; }
    if (c === '`') { state = 'backtick'; continue; }
    if (c === '$') {
      const match = source.slice(i).match(/^(\$\$|\$[A-Za-z_][A-Za-z0-9_]*\$)/);
      if (match) {
        dollarTag = match[1];
        state = 'dollar';
        i += dollarTag.length - 1;
        continue;
      }
    }
    if (c === ';') {
      ranges.push({ from: start, to: i + 1 });
      start = i + 1;
    }
  }
  if (start < source.length) ranges.push({ from: start, to: source.length });
  const selected = ranges.find((r) => cursor >= r.from && cursor <= r.to) || ranges[ranges.length - 1];
  if (!selected) return { from: 0, to: source.length, sql: source.trim() };
  let from = selected.from;
  let to = selected.to;
  while (from < to && /\s/.test(source[from])) from += 1;
  while (to > from && /\s/.test(source[to - 1])) to -= 1;
  return { from, to, sql: source.slice(from, to) };
}

function downloadBlob(blob, filename) {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement('a');
  anchor.href = url;
  anchor.download = filename;
  anchor.click();
  setTimeout(() => URL.revokeObjectURL(url), 0);
}

function DataWorkspacePage({ resource, onBack }) {
  const store = useMC();
  const dark = document.documentElement.getAttribute('data-theme') === 'dark';
  const canWrite = resource.accessMode === 'write' || resource.accessMode === 'admin' || store.can('admin');
  const editorRef = React.useRef(null);
  const runControllerRef = React.useRef(null);

  const [wsId, setWsId] = React.useState(null);
  const [autoCommit, setAutoCommit] = React.useState(true);
  const [txState, setTxState] = React.useState('none');
  const [sqlText, setSqlText] = React.useState('SELECT 1');
  const [busy, setBusy] = React.useState(false);
  const [result, setResult] = React.useState(null);
  const [msg, setMsg] = React.useState('');
  // 结果区就地编辑（仅单表 + 主键 + 写权限）
  const [editMode, setEditMode] = React.useState(false);
  const [draftRows, setDraftRows] = React.useState([]); // 与 result.rows 对齐的可编辑副本
  const [deletedRows, setDeletedRows] = React.useState(() => new Set());
  const [editSaving, setEditSaving] = React.useState(false);
  const lastSQLRef = React.useRef('');
  const [tree, setTree] = React.useState([]);
  const [expanded, setExpanded] = React.useState({});
  const [childrenMap, setChildrenMap] = React.useState({});
  const [treeLoading, setTreeLoading] = React.useState(false);
  const [saved, setSaved] = React.useState([]);
  const [activeSaved, setActiveSaved] = React.useState(null);
  const [saveOpen, setSaveOpen] = React.useState(false);
  const [saveName, setSaveName] = React.useState('');
  const [loadOpen, setLoadOpen] = React.useState(false);
  const [contextMenu, setContextMenu] = React.useState(null);
  const [structureState, setStructureState] = React.useState(null);
  const [importState, setImportState] = React.useState(null);
  const [exportState, setExportState] = React.useState(null);
  const [treeCollapsed, setTreeCollapsed] = React.useState(false);
  const [treeWidth, setTreeWidth] = React.useState(() => Number(localStorage.getItem('mc_dr_tree_width')) || 280);
  const [editorHeight, setEditorHeight] = React.useState(() => Number(localStorage.getItem('mc_dr_editor_height')) || 220);
  const [fullscreen, setFullscreen] = React.useState(false);

  React.useEffect(() => {
    if (!fullscreen) return undefined;
    const prev = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    // 全屏时给 body 打标，便于全局样式屏蔽侧栏/顶栏层叠干扰
    document.documentElement.setAttribute('data-dr-fullscreen', '1');
    const onKey = (e) => {
      if (e.key === 'Escape') {
        if (contextMenu) {
          setContextMenu(null);
          return;
        }
        setFullscreen(false);
      }
    };
    window.addEventListener('keydown', onKey);
    return () => {
      document.body.style.overflow = prev;
      document.documentElement.removeAttribute('data-dr-fullscreen');
      window.removeEventListener('keydown', onKey);
    };
  }, [fullscreen, contextMenu]);

  const refreshSaved = React.useCallback(async () => {
    const items = await listSavedSQL(resource.id);
    setSaved(Array.isArray(items) ? items : []);
  }, [resource.id]);

  const refreshRoot = React.useCallback(async () => {
    setTreeLoading(true);
    try {
      const nodes = await metadataChildren(resource.id, '');
      setTree(Array.isArray(nodes) ? nodes : []);
      setExpanded({});
      setChildrenMap({});
    } catch (e) {
      toast(e.message || '加载元数据失败', { tone: 'error' });
    } finally {
      setTreeLoading(false);
    }
  }, [resource.id]);

  React.useEffect(() => {
    let alive = true;
    let createdWorkspace = '';
    createWorkspace(resource.id).then((d) => {
      createdWorkspace = d.workspaceId;
      if (!alive) {
        deleteWorkspace(resource.id, createdWorkspace).catch(() => {});
        return;
      }
      setWsId(createdWorkspace);
      setAutoCommit(!!d.autoCommit);
      setTxState(d.txState || 'none');
    }).catch((e) => toast(e.message || '创建工作台失败', { tone: 'error' }));
    refreshRoot();
    refreshSaved().catch(() => {});
    return () => {
      alive = false;
      runControllerRef.current?.abort();
      if (createdWorkspace) deleteWorkspace(resource.id, createdWorkspace).catch(() => {});
    };
  }, [resource.id, refreshRoot, refreshSaved]);

  React.useEffect(() => {
    const close = () => setContextMenu(null);
    window.addEventListener('click', close);
    window.addEventListener('resize', close);
    return () => {
      window.removeEventListener('click', close);
      window.removeEventListener('resize', close);
    };
  }, []);

  const editorTarget = React.useCallback(() => {
    const view = editorRef.current;
    if (!view) return { from: 0, to: sqlText.length, sql: sqlText.trim() };
    const selection = view.state.selection.main;
    if (!selection.empty) {
      return {
        from: selection.from,
        to: selection.to,
        sql: view.state.sliceDoc(selection.from, selection.to).trim(),
      };
    }
    return findStatementRange(view.state.doc.toString(), selection.head);
  }, [sqlText]);

  const resetEditState = React.useCallback((res) => {
    setEditMode(false);
    setDeletedRows(new Set());
    if (res?.rows) {
      setDraftRows(res.rows.map((row) => row.map((cell) => (cellIsBinary(cell) ? cell : cell))));
    } else {
      setDraftRows([]);
    }
  }, []);

  const runSQL = async (confirmed = false, fixedSQL = '') => {
    const target = fixedSQL || editorTarget().sql;
    if (!wsId || !target) return;
    const controller = new AbortController();
    runControllerRef.current = controller;
    setBusy(true);
    setMsg('');
    try {
      const res = await executeSQL(resource.id, wsId, {
        sql: target, limit: 100, confirmed, signal: controller.signal,
      });
      lastSQLRef.current = target;
      setResult(res);
      resetEditState(res);
      setTxState(res.txState || 'none');
      if (res.statementType && res.statementType !== 'SELECT') {
        setMsg(`${res.statementType} · 影响 ${res.affectedRows ?? 0} 行 · ${res.durationMs}ms`);
      } else {
        let extra = '';
        if (canWrite && res.editable) {
          if (res.editable.primaryKeys?.length) {
            extra = ' · 可就地编辑';
          } else if (res.editable.reason) {
            extra = ' · 不可就地编辑';
          }
        }
        setMsg(`返回 ${res.returnedRows ?? 0} 行`
          + (res.totalStatus === 'available' ? ` / 共 ${res.total}` : '')
          + ` · ${res.durationMs}ms` + extra);
      }
    } catch (e) {
      if (e.name === 'AbortError') {
        setMsg('执行已取消');
        return;
      }
      if (e.code === 'DANGEROUS_SQL' && !confirmed) {
        const ok = await confirmDialog({
          title: '确认危险 SQL',
          message: e.message || '该操作可能影响大量数据，确认继续执行？',
          confirmText: '确认执行',
          tone: 'danger',
          icon: 'alert',
        });
        if (ok) {
          setBusy(false);
          return runSQL(true, target);
        }
        return;
      }
      setResult(null);
      setMsg(e.message || '执行失败');
      toast(e.message || '执行失败', { tone: 'error' });
    } finally {
      if (runControllerRef.current === controller) {
        runControllerRef.current = null;
        setBusy(false);
      }
    }
  };

  const toggleAC = async () => {
    if (!wsId) return;
    try {
      const d = await patchAutoCommit(resource.id, wsId, !autoCommit);
      setAutoCommit(!!d.autoCommit);
      setTxState(d.txState || 'none');
    } catch (e) {
      toast(e.message || '切换失败', { tone: 'error' });
    }
  };

  const doCommit = async () => {
    try {
      const d = await commitWorkspace(resource.id, wsId);
      setTxState(d.txState || 'none');
      toast('已提交');
    } catch (e) {
      toast(e.message || '提交失败', { tone: 'error' });
    }
  };

  const doRollback = async () => {
    try {
      const d = await rollbackWorkspace(resource.id, wsId);
      setTxState(d.txState || 'none');
      toast('已回滚');
    } catch (e) {
      toast(e.message || '回滚失败', { tone: 'error' });
    }
  };

  const canGridEdit = !!(
    canWrite && autoCommit && result?.editable?.primaryKeys?.length && result?.columns?.length
  );
  const pkSet = React.useMemo(() => {
    const s = new Set();
    (result?.editable?.primaryKeys || []).forEach((k) => s.add(String(k).toLowerCase()));
    return s;
  }, [result]);

  const beginEdit = () => {
    if (!canGridEdit) return;
    setDraftRows((result.rows || []).map((row) => row.slice()));
    setDeletedRows(new Set());
    setEditMode(true);
  };

  const cancelEdit = () => {
    resetEditState(result);
  };

  const setCellValue = (rowIndex, colIndex, value) => {
    setDraftRows((rows) => {
      const next = rows.map((r) => r.slice());
      if (!next[rowIndex]) return rows;
      // 保留空字符串，不自动变 NULL
      next[rowIndex][colIndex] = value;
      return next;
    });
  };

  const setCellNull = (rowIndex, colIndex) => {
    setDraftRows((rows) => {
      const next = rows.map((r) => r.slice());
      if (!next[rowIndex]) return rows;
      next[rowIndex][colIndex] = null;
      return next;
    });
  };

  const toggleDeleteRow = (rowIndex) => {
    setDeletedRows((prev) => {
      const next = new Set(prev);
      if (next.has(rowIndex)) next.delete(rowIndex);
      else next.add(rowIndex);
      return next;
    });
  };

  const saveEdits = async () => {
    if (!wsId || !result?.editable || !canGridEdit) return;
    const cols = result.columns || [];
    const pks = result.editable.primaryKeys || [];
    const orig = result.rows || [];
    const updates = [];
    const deletes = [];

    for (let ri = 0; ri < orig.length; ri += 1) {
      const keys = {};
      pks.forEach((pk) => {
        const ci = cols.findIndex((c) => String(c).toLowerCase() === String(pk).toLowerCase());
        if (ci >= 0) keys[pk] = orig[ri][ci];
      });
      if (deletedRows.has(ri)) {
        deletes.push({ keys });
        continue;
      }
      const set = {};
      const old = {};
      let changed = false;
      cols.forEach((col, ci) => {
        if (pkSet.has(String(col).toLowerCase())) return;
        if (cellIsBinary(orig[ri][ci])) return;
        const before = orig[ri][ci];
        let after = draftRows[ri]?.[ci];
        if (cellIsBinary(after)) return;
        // 比较：null 与 '' 不同
        const same = (before == null && after == null)
          || (before != null && after != null && String(before) === String(after));
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
    {
      const ok = await confirmDialog({
        title: '确认保存就地编辑',
        message: `将更新 ${updates.length} 行、删除 ${deletes.length} 行。此操作在自动提交模式下直接写入数据库，无法通过工作台回滚。`,
        confirmText: '保存',
        tone: 'danger',
      });
      if (!ok) return;
    }

    setEditSaving(true);
    try {
      const res = await applyRowEdits(resource.id, wsId, {
        schema: result.editable.schema || '',
        table: result.editable.table,
        updates,
        deletes,
      });
      toast(`已保存 · 更新 ${res.updated || 0} · 删除 ${res.deleted || 0}`);
      setEditMode(false);
      // 重新执行原查询刷新
      if (lastSQLRef.current) {
        await runSQL(false, lastSQLRef.current);
      }
    } catch (e) {
      toast(e.message || '保存失败', { tone: 'error' });
    } finally {
      setEditSaving(false);
    }
  };

  const doFormat = () => {
    const target = editorTarget();
    if (!target.sql) return;
    try {
      const formatted = formatSQL(target.sql, { language: dialectFor(resource.dbType) });
      const view = editorRef.current;
      if (view) {
        view.dispatch({
          changes: { from: target.from, to: target.to, insert: formatted },
          selection: { anchor: target.from + formatted.length },
        });
      } else {
        setSqlText(formatted);
      }
    } catch (e) {
      toast('格式化失败，已保留原文', { tone: 'warn' });
    }
  };

  const loadChildren = async (node, force = false) => {
    const id = node.id || '';
    if (!force && childrenMap[id]) {
      setExpanded((current) => ({ ...current, [id]: !current[id] }));
      return;
    }
    try {
      const children = await metadataChildren(resource.id, id);
      setChildrenMap((current) => ({ ...current, [id]: Array.isArray(children) ? children : [] }));
      setExpanded((current) => ({ ...current, [id]: true }));
    } catch (e) {
      toast(e.message || '加载元数据失败', { tone: 'error' });
    }
  };

  const insertTemplate = async (node, operation) => {
    try {
      const data = await sqlTemplate(resource.id, node.id, operation);
      if (data.sql || data.template) {
        setSqlText(data.sql || data.template);
        setActiveSaved(null);
      }
    } catch (e) {
      toast(e.message || '生成模板失败', { tone: 'error' });
    }
  };

  const fetchDDL = async (node) => {
    const data = await metadataDDL(resource.id, node.id);
    return data.ddl || data.SQL || '';
  };

  const showDDL = async (node) => {
    try {
      setSqlText(await fetchDDL(node));
      setActiveSaved(null);
    } catch (e) {
      toast(e.message || '获取 DDL 失败', { tone: 'error' });
    }
  };

  const downloadDDL = async (node) => {
    try {
      const ddl = await fetchDDL(node);
      downloadBlob(new Blob([ddl], { type: 'text/sql;charset=utf-8' }), `${node.name}.sql`);
    } catch (e) {
      toast(e.message || '导出 DDL 失败', { tone: 'error' });
    }
  };

  const showStructure = async (node) => {
    setStructureState({ node, loading: true });
    try {
      const [structure, ddl] = await Promise.all([
        metadataStructure(resource.id, node.id),
        fetchDDL(node).catch(() => ''),
      ]);
      setStructureState({ node, structure, ddl, loading: false });
    } catch (e) {
      setStructureState(null);
      toast(e.message || '读取表结构失败', { tone: 'error' });
    }
  };

  const openImport = async (node) => {
    try {
      const structure = await metadataStructure(resource.id, node.id);
      setImportState({
        node, structure, file: null, preview: null, mapping: [], busy: false,
      });
    } catch (e) {
      toast(e.message || '读取目标表结构失败', { tone: 'error' });
    }
  };

  const closeImport = () => {
    if (importState?.preview?.importId) {
      deleteImport(resource.id, importState.preview.importId).catch(() => {});
    }
    setImportState(null);
  };

  const applyPreview = (preview, current) => {
    const targetNames = (current.structure?.columns || []).map((column) => column.name);
    const byLower = new Map(targetNames.map((name) => [name.toLowerCase(), name]));
    return {
      ...current,
      preview,
      mapping: (preview.columns || []).map((name) => byLower.get(String(name).toLowerCase()) || ''),
      busy: false,
    };
  };

  const chooseImportFile = async (file) => {
    if (!file) return;
    setImportState((current) => ({ ...current, file, busy: true }));
    try {
      const preview = await previewImport(resource.id, file);
      setImportState((current) => applyPreview(preview, current));
    } catch (e) {
      setImportState((current) => ({ ...current, busy: false }));
      toast(e.message || '文件预览失败', { tone: 'error' });
    }
  };

  const changeImportSheet = async (sheet) => {
    if (!importState?.preview?.importId) return;
    setImportState((current) => ({ ...current, busy: true }));
    try {
      const preview = await selectImportSheet(resource.id, importState.preview.importId, sheet);
      setImportState((current) => applyPreview(preview, current));
    } catch (e) {
      setImportState((current) => ({ ...current, busy: false }));
      toast(e.message || '切换工作表失败', { tone: 'error' });
    }
  };

  const submitImport = async () => {
    if (!importState?.preview?.importId) return;
    setImportState((current) => ({ ...current, busy: true }));
    try {
      const data = await executeImport(resource.id, importState.preview.importId, {
        tableName: importState.node.name,
        schema: importState.node.schema || '',
        columnMapping: importState.mapping,
      });
      if (data.error) {
        throw new Error(data.error);
      }
      toast(`导入完成 · ${data.importedRows || 0} 行`);
      setImportState(null);
    } catch (e) {
      setImportState((current) => ({ ...current, busy: false }));
      toast(e.message || '导入失败', { tone: 'error' });
    }
  };

  const saveSQL = async (asNew) => {
    const name = saveName.trim();
    if (!name || !sqlText.trim()) {
      toast('名称和 SQL 内容不能为空', { tone: 'warn' });
      return;
    }
    try {
      if (!asNew && activeSaved) {
        await updateSavedSQL(resource.id, activeSaved.id, { name, sqlText });
        setActiveSaved({ ...activeSaved, name, sqlText });
        toast('已更新');
      } else {
        const created = await createSavedSQL(resource.id, { name, sqlText });
        setActiveSaved(created);
        toast('已保存');
      }
      setSaveOpen(false);
      await refreshSaved();
    } catch (e) {
      toast(e.message || '保存失败', { tone: 'error' });
    }
  };

  const doExport = async () => {
    if (!wsId || !exportState) return;
    try {
      const current = exportState.scope === 'current';
      const blob = await exportWorkspace(resource.id, wsId, {
        sql: current ? editorTarget().sql : '',
        format: exportState.format,
        scope: exportState.scope,
        columns: current ? (result?.columns || []) : [],
        rows: current ? (result?.rows || []) : [],
      });
      downloadBlob(blob, `export.${exportState.format}`);
      setExportState(null);
    } catch (e) {
      toast(e.message || '导出失败', { tone: 'error' });
    }
  };

  const beginResize = (type, event) => {
    event.preventDefault();
    const startX = event.clientX;
    const startY = event.clientY;
    const initialTree = treeWidth;
    const initialEditor = editorHeight;
    const move = (e) => {
      if (type === 'tree') {
        setTreeWidth(Math.max(TREE_MIN, Math.min(TREE_MAX, initialTree + e.clientX - startX)));
      } else {
        setEditorHeight(Math.max(EDITOR_MIN, Math.min(EDITOR_MAX, initialEditor + e.clientY - startY)));
      }
    };
    const up = () => {
      window.removeEventListener('mousemove', move);
      window.removeEventListener('mouseup', up);
    };
    window.addEventListener('mousemove', move);
    window.addEventListener('mouseup', up);
  };

  React.useEffect(() => {
    localStorage.setItem('mc_dr_tree_width', String(Math.round(treeWidth)));
  }, [treeWidth]);
  React.useEffect(() => {
    localStorage.setItem('mc_dr_editor_height', String(Math.round(editorHeight)));
  }, [editorHeight]);

  const contextActions = (node) => {
    const table = node.kind === 'table';
    const leaf = isTreeLeaf(node.kind);
    if (!leaf) return [];
    const actions = [
      { label: '生成 SELECT', action: () => insertTemplate(node, 'SELECT') },
      ...(table ? [{ label: '查看表结构', action: () => showStructure(node) }] : []),
      { label: '查看 DDL', action: () => showDDL(node) },
      { label: '导出 DDL', action: () => downloadDDL(node) },
    ];
    if (canWrite && table) {
      actions.push(
        { label: '生成 INSERT', action: () => insertTemplate(node, 'INSERT') },
        { label: '生成 UPDATE', action: () => insertTemplate(node, 'UPDATE') },
        { label: '生成 DELETE', action: () => insertTemplate(node, 'DELETE') },
        { label: '导入数据', action: () => openImport(node) },
      );
    }
    return actions;
  };

  const renderNode = (node, depth = 0) => {
    const id = node.id || node.name;
    const kids = childrenMap[id];
    const isOpen = expanded[id];
    const leaf = isTreeLeaf(node.kind);
    const kindLabel = kindLabelFor(node.kind, resource.dbType);
    return (
      <div key={id}>
        <div className="dr-tree-node" style={{ paddingLeft: 8 + depth * 13 }}
          onClick={() => (leaf ? null : loadChildren(node))}
          onDoubleClick={() => leaf && insertTemplate(node, 'SELECT')}
          onContextMenu={(event) => {
            const actions = contextActions(node);
            if (actions.length === 0) return;
            event.preventDefault();
            event.stopPropagation();
            // clientX/Y 是视口坐标；菜单 portal 到 body 后用 fixed，避免 .content 祖先 transform 导致漂移
            const pad = 8;
            const estW = 220;
            const estH = 36 + actions.length * 34;
            let x = event.clientX;
            let y = event.clientY;
            if (x + estW > window.innerWidth - pad) x = Math.max(pad, window.innerWidth - estW - pad);
            if (y + estH > window.innerHeight - pad) y = Math.max(pad, window.innerHeight - estH - pad);
            setContextMenu({ x, y, node, actions });
          }}>
          {!leaf ? <Icon name={isOpen ? 'chevronD' : 'chevronR'} size={12} /> : <span style={{ width: 12 }} />}
          <Icon name={leaf ? 'box' : 'folder'} size={13} />
          <span className="dr-tree-label">{node.name}</span>
          {kindLabel ? <span className="dr-tree-kind">{kindLabel}</span> : null}
        </div>
        {isOpen && kids ? kids.map((child) => renderNode(child, depth + 1)) : null}
      </div>
    );
  };

  const contextMenuNode = contextMenu
    ? createPortal(
      <div
        className="card dr-context-menu"
        style={{ left: contextMenu.x, top: contextMenu.y }}
        onClick={(e) => e.stopPropagation()}
        onContextMenu={(e) => { e.preventDefault(); e.stopPropagation(); }}
      >
        <div className="dr-context-title">{contextMenu.node.name}</div>
        {contextMenu.actions.map((item) => (
          <button key={item.label} type="button" onClick={() => {
            setContextMenu(null);
            item.action();
          }}>{item.label}</button>
        ))}
      </div>,
      document.body,
    )
    : null;

  const workspace = (
    <div className={`dr-workspace${fullscreen ? ' is-fullscreen' : ''}`}>
      <div className="dr-workspace-grid">
        {!treeCollapsed ? (
          <React.Fragment>
            <aside className="card dr-tree-pane" style={{ width: treeWidth }}>
              <div className="dr-pane-head">
                <div className="dr-pane-head-meta" title={`${resource.host}:${resource.port}/${resource.databaseName}`}>
                  <Btn size="sm" variant="ghost" icon="chevronL" title="返回列表" onClick={onBack} />
                  <span className="dr-pane-title">{resource.name}</span>
                </div>
                <div style={{ display: 'flex', gap: 2 }}>
                  <Btn size="sm" variant="ghost" icon="rotate" title="刷新" disabled={treeLoading} onClick={refreshRoot} />
                  <Btn size="sm" variant="ghost" icon="chevronL" title="折叠元数据" onClick={() => setTreeCollapsed(true)} />
                </div>
              </div>
              <div className="dr-tree-scroll">
                {treeLoading ? <div className="dr-inline-status"><Spinner size={13} />加载中</div> : null}
                {!treeLoading && tree.length === 0 ? <div className="dr-inline-status">暂无元数据</div> : null}
                {tree.map((node) => renderNode(node))}
              </div>
            </aside>
            <div className="dr-resizer dr-resizer-x" onMouseDown={(e) => beginResize('tree', e)} />
          </React.Fragment>
        ) : (
          <div className="dr-tree-rail">
            <Btn size="sm" variant="outline" icon="chevronL" title="返回列表" onClick={onBack} />
            <Btn size="sm" variant="outline" icon="chevronR" title="展开元数据" onClick={() => setTreeCollapsed(false)} />
          </div>
        )}

        <main className="dr-main-pane">
          <div className="card dr-toolbar">
            {treeCollapsed ? (
              <React.Fragment>
                <span className="dr-toolbar-resource" title={resource.name}>{resource.name}</span>
                <span className="dr-toolbar-sep" />
              </React.Fragment>
            ) : null}
            <Btn size="sm" variant="primary" icon="play" disabled={busy || !wsId} onClick={() => runSQL(false)}>执行</Btn>
            <Btn size="sm" variant="ghost" icon="stop" disabled={!busy}
              onClick={() => runControllerRef.current?.abort()}>取消</Btn>
            <Btn size="sm" variant="ghost" disabled={busy} onClick={toggleAC}>自动提交: {autoCommit ? '开' : '关'}</Btn>
            <Btn size="sm" variant="ghost" disabled={autoCommit || txState !== 'active'} onClick={doCommit}>提交</Btn>
            <Btn size="sm" variant="ghost" disabled={autoCommit || txState === 'none'} onClick={doRollback}>回滚</Btn>
            <span className="dr-toolbar-sep" />
            <Btn size="sm" variant="ghost" onClick={doFormat}>格式化</Btn>
            <Btn size="sm" variant="ghost" icon="download" disabled={!result?.columns}
              onClick={() => setExportState({ scope: 'current', format: 'csv' })}>导出</Btn>
            <Btn size="sm" variant="ghost" onClick={() => {
              setSaveName(activeSaved?.name || '');
              setSaveOpen(true);
            }}>保存 SQL</Btn>
            <Btn size="sm" variant="ghost" onClick={() => setLoadOpen(true)}>加载 SQL</Btn>
            <div style={{ flex: 1 }} />
            <Badge tone={txState === 'active' ? 'warn' : txState === 'failed' ? 'error' : 'info'}>事务 {txState}</Badge>
            {!canWrite ? <Badge tone="info">只读</Badge> : null}
            <Btn size="sm" variant="ghost" icon={fullscreen ? 'x' : 'layers'}
              title={fullscreen ? '退出全屏 (Esc)' : '全屏工作台'}
              onClick={() => setFullscreen((v) => !v)}>
              {fullscreen ? '退出全屏' : '全屏'}
            </Btn>
          </div>

          <div className="card dr-editor-card" style={{ flexBasis: editorHeight }}>
            <CodeMirror value={sqlText} height={`${editorHeight}px`} theme={dark ? 'dark' : 'light'}
              extensions={[sql()]} onCreateEditor={(view) => { editorRef.current = view; }}
              onChange={(value) => setSqlText(value)}
              basicSetup={{ lineNumbers: true, foldGutter: true }} />
          </div>
          <div className="dr-resizer dr-resizer-y" onMouseDown={(e) => beginResize('editor', e)} />

          <div className="card dr-result-card">
            <div className="dr-result-status">
              <span style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                {msg || '结果'}
                {canWrite && result?.editable?.reason ? (
                  <span style={{ marginLeft: 8, color: 'var(--warn)' }} title={result.editable.reason}>
                    · {result.editable.reason}
                  </span>
                ) : null}
              </span>
              {busy ? <Spinner size={13} /> : null}
              {canWrite && !autoCommit && result?.editable?.primaryKeys?.length ? (
                <span style={{ fontSize: 11, color: 'var(--muted-fg)' }} title="关闭自动提交时不可就地编辑">就地编辑已禁用</span>
              ) : null}
              {canGridEdit && !editMode ? (
                <Btn size="sm" variant="outline" disabled={busy} onClick={beginEdit}>编辑结果</Btn>
              ) : null}
              {editMode ? (
                <React.Fragment>
                  <Btn size="sm" variant="ghost" disabled={editSaving} onClick={cancelEdit}>取消</Btn>
                  <Btn size="sm" variant="primary" disabled={editSaving} onClick={saveEdits}>
                    {editSaving ? <Spinner size={12} /> : '保存变更'}
                  </Btn>
                </React.Fragment>
              ) : null}
            </div>
            {result?.columns ? (
              <div className="dr-table-scroll">
                <table className="table">
                  <thead>
                    <tr>
                      {editMode ? <th style={{ width: 44 }}>删除</th> : null}
                      {result.columns.map((column) => (
                        <th key={column}>
                          {column}
                          {pkSet.has(String(column).toLowerCase()) ? (
                            <span className="dr-pk-mark" title="主键"> PK</span>
                          ) : null}
                        </th>
                      ))}
                    </tr>
                  </thead>
                  <tbody>
                    {(editMode ? draftRows : (result.rows || [])).map((row, rowIndex) => {
                      const markedDel = editMode && deletedRows.has(rowIndex);
                      return (
                        <tr key={rowIndex} className={markedDel ? 'dr-row-deleted' : undefined}>
                          {editMode ? (
                            <td>
                              <input type="checkbox" checked={!!markedDel}
                                onChange={() => toggleDeleteRow(rowIndex)}
                                title="标记删除" />
                            </td>
                          ) : null}
                          {row.map((cell, cellIndex) => {
                            const col = result.columns[cellIndex];
                            const isPK = pkSet.has(String(col).toLowerCase());
                            const binary = cellIsBinary(cell) || cellIsBinary(result.rows?.[rowIndex]?.[cellIndex]);
                            if (editMode && !markedDel && !isPK && !binary) {
                              const isNull = cell == null;
                              return (
                                <td key={cellIndex} style={{ fontSize: 12.5, padding: '2px 4px' }}>
                                  <div className="dr-cell-edit">
                                    <input
                                      className="input dr-cell-input"
                                      value={isNull ? '' : cellToEditString(cell)}
                                      placeholder={isNull ? 'NULL' : ''}
                                      onChange={(e) => setCellValue(rowIndex, cellIndex, e.target.value)}
                                    />
                                    <button type="button" className="dr-cell-null-btn" title="设为 NULL"
                                      onClick={() => setCellNull(rowIndex, cellIndex)}>N</button>
                                  </div>
                                </td>
                              );
                            }
                            return (
                              <td key={cellIndex} style={{ fontSize: 12.5, opacity: markedDel ? 0.45 : 1 }}>
                                {cellDisplay(editMode ? (result.rows?.[rowIndex]?.[cellIndex] ?? cell) : cell)}
                              </td>
                            );
                          })}
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            ) : <EmptyState icon="terminal" title="结果区" desc="选中 SQL 或将光标放在语句内，然后执行" />}
          </div>
        </main>
      </div>

      <Dialog open={saveOpen} onClose={() => setSaveOpen(false)} width={420}
        title={activeSaved ? '更新保存的 SQL' : '保存 SQL'}
        foot={<React.Fragment>
          <Btn variant="ghost" onClick={() => setSaveOpen(false)}>取消</Btn>
          {activeSaved ? <Btn variant="outline" onClick={() => saveSQL(true)}>另存为</Btn> : null}
          <Btn variant="primary" onClick={() => saveSQL(false)}>{activeSaved ? '更新' : '保存'}</Btn>
        </React.Fragment>}>
        <Field label="名称"><input className="input" value={saveName} onChange={(e) => setSaveName(e.target.value)} /></Field>
      </Dialog>

      <Dialog open={loadOpen} onClose={() => setLoadOpen(false)} width={560} title="加载已保存 SQL"
        foot={<Btn variant="ghost" onClick={() => setLoadOpen(false)}>关闭</Btn>}>
        {saved.length === 0 ? <div className="dr-inline-status">暂无保存的 SQL</div> : (
          <div className="dr-saved-list">
            {saved.map((item) => (
              <div key={item.id} className="dr-saved-row">
                <button type="button" className="dr-saved-main" onClick={() => {
                  setSqlText(item.sqlText || item.sql_text || '');
                  setActiveSaved(item);
                  setLoadOpen(false);
                }}>
                  <strong>{item.name}</strong>
                  <span>{String(item.sqlText || item.sql_text || '').split('\n')[0]}</span>
                </button>
                <Btn size="sm" variant="ghost" icon="trash" title="删除" onClick={async () => {
                  const ok = await confirmDialog({ title: '删除保存的 SQL', message: `确认删除「${item.name}」？`, tone: 'danger' });
                  if (!ok) return;
                  try {
                    await deleteSavedSQL(resource.id, item.id);
                    if (activeSaved?.id === item.id) setActiveSaved(null);
                    await refreshSaved();
                  } catch (e) {
                    toast(e.message || '删除失败', { tone: 'error' });
                  }
                }} />
              </div>
            ))}
          </div>
        )}
      </Dialog>

      <Dialog open={!!exportState} onClose={() => setExportState(null)} width={420} title="导出查询结果"
        desc="全量导出会重新执行最近一次成功的查询"
        foot={<React.Fragment>
          <Btn variant="ghost" onClick={() => setExportState(null)}>取消</Btn>
          <Btn variant="primary" icon="download" onClick={doExport}>导出</Btn>
        </React.Fragment>}>
        {exportState ? <div className="dr-dialog-grid">
          <Field label="范围">
            <select className="input" value={exportState.scope}
              onChange={(e) => setExportState((current) => ({ ...current, scope: e.target.value }))}>
              <option value="current">当前 {result?.returnedRows || 0} 行</option>
              <option value="all">全部结果</option>
            </select>
          </Field>
          <Field label="格式">
            <select className="input" value={exportState.format}
              onChange={(e) => setExportState((current) => ({ ...current, format: e.target.value }))}>
              <option value="csv">CSV</option>
              <option value="xlsx">XLSX</option>
            </select>
          </Field>
        </div> : null}
      </Dialog>

      <Dialog open={!!structureState} onClose={() => setStructureState(null)} width={860}
        title={`表结构 · ${structureState?.node?.name || ''}`}
        foot={<React.Fragment>
          {structureState?.ddl ? <Btn variant="outline" icon="download"
            onClick={() => downloadDDL(structureState.node)}>导出 DDL</Btn> : null}
          <Btn variant="ghost" onClick={() => setStructureState(null)}>关闭</Btn>
        </React.Fragment>}>
        {structureState?.loading ? <div className="dr-inline-status"><Spinner size={13} />加载中</div> : null}
        {structureState?.structure ? <StructureContent state={structureState} /> : null}
      </Dialog>

      <Dialog open={!!importState} onClose={closeImport} width={900}
        title={`导入数据 · ${importState?.node?.name || ''}`}
        desc="CSV/XLSX 整批事务导入，任意一行失败将全部回滚"
        foot={<React.Fragment>
          <Btn variant="ghost" disabled={importState?.busy} onClick={closeImport}>取消</Btn>
          <Btn variant="primary" icon="upload" disabled={!importState?.preview || importState?.busy}
            onClick={submitImport}>{importState?.busy ? <Spinner size={12} /> : '开始导入'}</Btn>
        </React.Fragment>}>
        {importState ? <ImportContent state={importState} setState={setImportState}
          onFile={chooseImportFile} onSheet={changeImportSheet} /> : null}
      </Dialog>
    </div>
  );

  // 全屏：portal 到 body，盖住侧栏/顶栏整个 Mooncell 壳层（避免 .content-inner transform 把 fixed 困在内容区）
  return (
    <React.Fragment>
      {fullscreen ? createPortal(workspace, document.body) : workspace}
      {contextMenuNode}
      {/* 全屏时原内容区占位，避免路由区塌陷闪动 */}
      {fullscreen ? <div className="dr-workspace-placeholder" aria-hidden="true" /> : null}
    </React.Fragment>
  );
}

function StructureContent({ state }) {
  const structure = state.structure || {};
  return (
    <div className="dr-structure">
      <h4>字段</h4>
      <div className="dr-table-scroll" style={{ maxHeight: 260 }}>
        <table className="table">
          <thead><tr><th>名称</th><th>类型</th><th>可空</th><th>默认值</th><th>注释</th></tr></thead>
          <tbody>{(structure.columns || []).map((column) => (
            <tr key={column.name}>
              <td className="mono">{column.name}</td><td>{column.dataType}</td>
              <td>{column.isNullable ? '是' : '否'}</td><td className="mono">{column.defaultValue || '—'}</td>
              <td>{column.comment || '—'}</td>
            </tr>
          ))}</tbody>
        </table>
      </div>
      <div className="dr-structure-split">
        <section><h4>约束</h4>{(structure.constraints || []).length === 0 ? <span>无</span> : (
          structure.constraints.map((constraint) => (
            <div key={constraint.name} className="dr-meta-row">
              <strong>{constraint.name}</strong>
              <span>{constraint.type} · {(constraint.columns || []).join(', ') || constraint.definition}</span>
            </div>
          ))
        )}</section>
        <section><h4>索引</h4>{(structure.indexes || []).length === 0 ? <span>无</span> : (
          structure.indexes.map((index) => (
            <div key={index.name} className="dr-meta-row">
              <strong>{index.name}</strong>
              <span>{index.unique ? 'UNIQUE · ' : ''}{(index.columns || []).join(', ') || index.definition}</span>
            </div>
          ))
        )}</section>
      </div>
      {state.ddl ? <React.Fragment><h4>DDL</h4><pre className="dr-ddl">{state.ddl}</pre></React.Fragment> : null}
    </div>
  );
}

function ImportContent({ state, setState, onFile, onSheet }) {
  const targetColumns = state.structure?.columns || [];
  return (
    <div className="dr-import">
      <Field label="文件" hint="仅支持 CSV、XLSX，大小限制由 Console 配置决定">
        <input className="input" type="file" accept=".csv,.xlsx"
          disabled={state.busy || !!state.preview} onChange={(e) => onFile(e.target.files?.[0])} />
      </Field>
      {state.preview?.sheets?.length > 1 ? (
        <Field label="工作表">
          <select className="input" value={state.preview.sheet || state.preview.sheets[0]}
            disabled={state.busy} onChange={(e) => onSheet(e.target.value)}>
            {state.preview.sheets.map((sheet) => <option key={sheet} value={sheet}>{sheet}</option>)}
          </select>
        </Field>
      ) : null}
      {state.preview ? (
        <React.Fragment>
          <h4>字段映射</h4>
          <div className="dr-mapping-grid">
            {(state.preview.columns || []).map((source, index) => (
              <div key={`${source}-${index}`} className="dr-mapping-row">
                <span className="mono">{source || `第 ${index + 1} 列`}</span>
                <Icon name="chevronR" size={12} />
                <select className="input" value={state.mapping[index] || ''} disabled={state.busy}
                  onChange={(e) => setState((current) => {
                    const mapping = [...current.mapping];
                    mapping[index] = e.target.value;
                    return { ...current, mapping };
                  })}>
                  <option value="">跳过</option>
                  {targetColumns.map((column) => <option key={column.name} value={column.name}>{column.name}</option>)}
                </select>
              </div>
            ))}
          </div>
          <h4>预览</h4>
          <div className="dr-table-scroll" style={{ maxHeight: 230 }}>
            <table className="table">
              <tbody>{(state.preview.preview || []).map((row, rowIndex) => (
                <tr key={rowIndex}>{row.map((cell, index) => (
                  rowIndex === 0 ? <th key={index}>{cell}</th> : <td key={index}>{cell}</td>
                ))}</tr>
              ))}</tbody>
            </table>
          </div>
        </React.Fragment>
      ) : null}
    </div>
  );
}

export { DataWorkspacePage, findStatementRange };
