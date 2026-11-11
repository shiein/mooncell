// 数据资源工作台：多标签 SQL 编辑器 + 脚本执行 + 多结果页 + 对象/已保存/历史侧栏。
//
// 执行模型：后端坚持「一次请求一条语句」（安全检查、危险确认、审计按条进行），
// 前端用与后端共享规则的拆分器（lib/sql-split.js）把脚本拆成语句逐条提交。
import React from 'react';
import { createPortal } from 'react-dom';
import { format as formatSQL } from 'sql-formatter';
import { useMC } from '../lib/data.js';
import {
  Btn, Icon, Spinner, EmptyState, Dialog, toast, Badge, Field, confirmDialog, Checkbox, Switch,
} from '../components/primitives.jsx';
import {
  deleteWorkspace, executeSQL, cancelWorkspaceSQL, patchAutoCommit, applyRowEdits,
  commitWorkspace, rollbackWorkspace, metadataStructure, metadataDDL,
  sqlTemplate, listSavedSQL, createSavedSQL, updateSavedSQL, deleteSavedSQL,
  exportWorkspace, previewImport, selectImportSheet, executeImport, deleteImport,
  getResourceCapabilities, countWorkspaceSQL, resetSessionSettings, metadataCompletion,
  releaseWorkspaceOnUnload,
} from '../lib/dataresource-api.js';
import { splitStatements, splitDialectFor, looksLikeMissingSemicolon } from '../lib/sql-split.js';
import SqlEditor from './dataworkspace/SqlEditor.jsx';
import ResultGrid from './dataworkspace/ResultGrid.jsx';
import SidePanel, { quoteIdent } from './dataworkspace/SidePanel.jsx';
import {
  buildSnapshotCSVBlob, downloadBlob, exportFileName, friendlyWorkspaceError, txLabel,
  positionToLineCol, loadJSON, saveJSON, HISTORY_MAX, HISTORY_SQL_MAX, newTabId, copyText, toTSV,
} from './dataworkspace/utils.js';

const TREE_MIN = 200;
const TREE_MAX = 520;
const EDITOR_MIN = 120;
const EDITOR_MAX = 720;
/** 结果区分页：默认每页 100；加载全部上限 10000（与后端 MaxLimit 对齐） */
const PAGE_SIZE = 100;
const LOAD_ALL_MAX = 10000;

function dialectForFormat(dbType) {
  if (dbType === 'mysql') return 'mysql';
  if (dbType === 'dm') return 'plsql';
  return 'postgresql';
}

function useDarkTheme() {
  const read = () => document.documentElement.getAttribute('data-theme') === 'dark';
  const [dark, setDark] = React.useState(read);
  React.useEffect(() => {
    const mo = new MutationObserver(() => setDark(read()));
    mo.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
    return () => mo.disconnect();
  }, []);
  return dark;
}

/** 语句首个关键字（结果页标签用） */
function stmtLabel(sql) {
  const m = String(sql).replace(/^(\s|--[^\n]*\n|\/\*[\s\S]*?\*\/)+/, '').match(/^[(\s]*([A-Za-z]+)/);
  return m ? m[1].toUpperCase() : 'SQL';
}

const isMac = typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform || '');
const MOD = isMac ? '⌘' : 'Ctrl';

function DataWorkspacePage({ resource, initialWorkspace, onBack }) {
  const store = useMC();
  const user = store.user;
  const dark = useDarkTheme();
  const canWrite = resource.accessMode === 'write' || resource.accessMode === 'admin' || store.can('admin');
  const splitDialect = splitDialectFor(resource.dbType);
  const editorRef = React.useRef(null);

  const [wsId] = React.useState(initialWorkspace?.workspaceId || null);
  const [autoCommit, setAutoCommit] = React.useState(initialWorkspace?.autoCommit !== false);
  const [txState, setTxState] = React.useState(initialWorkspace?.txState || 'none');
  const [sessionSettings, setSessionSettings] = React.useState([]);
  const [caps, setCaps] = React.useState({ ddlSupported: true, importSupported: true });

  // ---- 查询标签（按用户 × 资源持久化到 localStorage） ----
  const [tabs, setTabs] = React.useState(() => {
    const saved = loadJSON('tabs', user, resource.id, null);
    if (saved && Array.isArray(saved.tabs) && saved.tabs.length) return saved.tabs;
    return [{ id: newTabId(), title: '查询 1', sql: '' }];
  });
  const [activeTabId, setActiveTabId] = React.useState(() => {
    const saved = loadJSON('tabs', user, resource.id, null);
    return saved?.active && saved.tabs?.some((t) => t.id === saved.active) ? saved.active : null;
  });
  const activeTab = tabs.find((t) => t.id === activeTabId) || tabs[0];
  const tabTextRef = React.useRef(null);
  if (!tabTextRef.current) tabTextRef.current = new Map(tabs.map((t) => [t.id, t.sql || '']));
  const [dirtyTabs, setDirtyTabs] = React.useState(() => new Set());
  const persistTimer = React.useRef(null);
  const tabsRef = React.useRef(tabs);
  tabsRef.current = tabs;
  const activeIdRef = React.useRef(activeTab.id);
  activeIdRef.current = activeTab.id;

  const persistTabs = React.useCallback(() => {
    clearTimeout(persistTimer.current);
    persistTimer.current = setTimeout(() => {
      const list = tabsRef.current.map((t) => ({ ...t, sql: tabTextRef.current.get(t.id) ?? t.sql ?? '' }));
      saveJSON('tabs', user, resource.id, { active: activeIdRef.current, tabs: list });
      const dirty = new Set(list.filter((t) => t.savedId && t.sql !== t.savedText).map((t) => t.id));
      setDirtyTabs((prev) => (prev.size === dirty.size && [...dirty].every((x) => prev.has(x)) ? prev : dirty));
    }, 400);
  }, [user, resource.id]);
  React.useEffect(() => { persistTabs(); }, [tabs, activeTab.id, persistTabs]);
  React.useEffect(() => () => clearTimeout(persistTimer.current), []);

  const onDocChange = React.useCallback((tabId, text) => {
    tabTextRef.current.set(tabId, text);
    persistTabs();
  }, [persistTabs]);

  const openInTab = React.useCallback((sqlText, { title, saved, reuseEmpty = true } = {}) => {
    const cur = activeIdRef.current;
    const curText = (tabTextRef.current.get(cur) || '').trim();
    const curTab = tabsRef.current.find((t) => t.id === cur);
    if (reuseEmpty && !curText && !curTab?.savedId) {
      tabTextRef.current.set(cur, sqlText);
      setTabs((list) => list.map((t) => (t.id === cur ? {
        ...t, title: title || t.title, savedId: saved?.id, savedText: saved ? sqlText : undefined,
      } : t)));
      editorRef.current?.setDoc(sqlText);
      return cur;
    }
    const id = newTabId();
    tabTextRef.current.set(id, sqlText);
    setTabs((list) => [...list, {
      id, title: title || `查询 ${list.length + 1}`, sql: sqlText, savedId: saved?.id, savedText: saved ? sqlText : undefined,
    }]);
    setActiveTabId(id);
    return id;
  }, []);

  const closeTab = async (id) => {
    const tab = tabs.find((t) => t.id === id);
    if (tab && dirtyTabs.has(id)) {
      const ok = await confirmDialog({ title: '关闭标签', message: `「${tab.title}」有未保存的修改，确认关闭？`, confirmText: '关闭', tone: 'danger' });
      if (!ok) return;
    }
    editorRef.current?.dropTab(id);
    tabTextRef.current.delete(id);
    if (tabs.length === 1) {
      const nid = newTabId();
      tabTextRef.current.set(nid, '');
      setTabs([{ id: nid, title: '查询 1', sql: '' }]);
      setActiveTabId(nid);
      return;
    }
    const idx = tabs.findIndex((t) => t.id === id);
    const next = tabs.filter((t) => t.id !== id);
    setTabs(next);
    if (activeTab.id === id) setActiveTabId(next[Math.max(0, idx - 1)].id);
  };

  const [renaming, setRenaming] = React.useState(null); // {id, value}
  const commitRename = () => {
    if (renaming && renaming.value.trim()) {
      setTabs((list) => list.map((t) => (t.id === renaming.id ? { ...t, title: renaming.value.trim().slice(0, 40) } : t)));
    }
    setRenaming(null);
  };

  // ---- 执行历史 ----
  const [history, setHistory] = React.useState(() => loadJSON('history', user, resource.id, []));
  const addHistory = React.useCallback((entry) => {
    setHistory((list) => {
      const sqlText = entry.sql.length > HISTORY_SQL_MAX ? entry.sql.slice(0, HISTORY_SQL_MAX) : entry.sql;
      const next = [{ ...entry, sql: sqlText }, ...list.filter((h) => h.sql !== sqlText)].slice(0, HISTORY_MAX);
      saveJSON('history', user, resource.id, next);
      return next;
    });
  }, [user, resource.id]);
  const clearHistory = async () => {
    const ok = await confirmDialog({ title: '清空执行历史', message: '历史只保存在本浏览器，清空后不可恢复。', tone: 'danger' });
    if (!ok) return;
    setHistory([]);
    saveJSON('history', user, resource.id, []);
  };

  // ---- 已保存 SQL ----
  const [saved, setSaved] = React.useState([]);
  const refreshSaved = React.useCallback(async () => {
    const items = await listSavedSQL(resource.id);
    setSaved(Array.isArray(items) ? items.map((i) => ({ ...i, sqlText: i.sqlText || i.sql_text || '' })) : []);
  }, [resource.id]);

  // ---- 补全：表清单 + 按需加载的列 ----
  const [completion, setCompletion] = React.useState(null);
  const colsCache = React.useRef(new Map());
  const colsPending = React.useRef(new Set());
  const [colsVersion, setColsVersion] = React.useState(0);
  const defaultSchema = React.useMemo(() => {
    const schemas = completion?.schemas || [];
    if (schemas.length === 1) return schemas[0];
    if (resource.defaultSchema) return resource.defaultSchema;
    if (resource.dbType === 'mysql') return resource.databaseName || schemas[0];
    if (resource.dbType === 'dm') return resource.username;
    return 'public';
  }, [completion, resource]);
  const schemaConfig = React.useMemo(() => {
    if (!completion) return null;
    const schema = {};
    for (const o of completion.objects) {
      const cols = colsCache.current.get(`${o.schema}\u0000${o.name}`);
      if (!schema[o.schema]) schema[o.schema] = {};
      schema[o.schema][o.name] = cols || [];
    }
    return { schema, defaultSchema };
    // colsVersion 变化时重建（列懒加载完成）
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [completion, defaultSchema, colsVersion]);

  const onTableRefs = React.useCallback(async (refs) => {
    if (!completion) return;
    const want = [];
    for (const ref of refs) {
      const parts = ref.split('.');
      const name = parts[parts.length - 1].toLowerCase();
      const sch = parts.length > 1 ? parts[0].toLowerCase() : null;
      const o = completion.objects.find((x) => x.name.toLowerCase() === name && (!sch || x.schema.toLowerCase() === sch));
      if (!o) continue;
      const key = `${o.schema}\u0000${o.name}`;
      if (colsCache.current.has(key) || colsPending.current.has(key)) continue;
      want.push({ o, key });
    }
    if (!want.length) return;
    for (const { o, key } of want.slice(0, 6)) {
      colsPending.current.add(key);
      try {
        // eslint-disable-next-line no-await-in-loop
        const st = await metadataStructure(resource.id, o.id);
        colsCache.current.set(key, (st.columns || []).map((c) => ({ label: c.name, type: 'property', detail: c.dataType })));
      } catch (e) {
        colsCache.current.set(key, []);
      } finally {
        colsPending.current.delete(key);
      }
    }
    setColsVersion((v) => v + 1);
  }, [completion, resource.id]);

  // ---- 结果 ----
  const [runs, setRuns] = React.useState([]);
  const runsRef = React.useRef(runs);
  runsRef.current = runs;
  const [activeRun, setActiveRun] = React.useState(0); // -1 = 消息总览
  const [running, setRunning] = React.useState(false);
  const [inflight, setInflight] = React.useState(0);
  const [continueOnError, setContinueOnError] = React.useState(false);
  const [gridPending, setGridPending] = React.useState(false);
  const stopRef = React.useRef(false);
  const busy = running || inflight > 0;

  const patchRun = React.useCallback((idx, patch) => {
    setRuns((list) => list.map((r, i) => (i === idx ? { ...r, ...patch } : r)));
  }, []);

  // ---- 危险语句确认（脚本内可选「全部确认」） ----
  const [danger, setDanger] = React.useState(null);
  const askDanger = (sqlText, message, multi) => new Promise((resolve) => setDanger({ sql: sqlText, message, multi, resolve }));
  const answerDanger = (choice) => {
    danger?.resolve(choice);
    setDanger(null);
  };

  React.useEffect(() => {
    if (!wsId) {
      toast('数据库连接已失效，请返回后重新输入密码', { tone: 'error' });
      return undefined;
    }
    let alive = true;
    getResourceCapabilities(resource.id).then((c) => {
      if (alive && c) setCaps({ ddlSupported: c.ddlSupported !== false, importSupported: c.importSupported !== false });
    }).catch(() => {});
    refreshSaved().catch(() => {});
    metadataCompletion(resource.id).then((d) => {
      if (alive) setCompletion({ objects: d.objects || [], schemas: d.schemas || [], truncated: !!d.truncated });
    }).catch(() => { if (alive) setCompletion({ objects: [], schemas: [], truncated: false }); });
    const onHide = () => releaseWorkspaceOnUnload(resource.id, wsId);
    window.addEventListener('pagehide', onHide);
    return () => {
      alive = false;
      window.removeEventListener('pagehide', onHide);
      stopRef.current = true;
      deleteWorkspace(resource.id, wsId).catch(() => {});
    };
  }, [resource.id, wsId, refreshSaved]);

  // ---- 执行 ----
  const applyServerState = (res) => {
    if (res?.txState) setTxState(res.txState);
    if (res && Array.isArray(res.sessionSettings)) setSessionSettings(res.sessionSettings);
    else if (res) setSessionSettings([]);
  };

  const markErrorInEditor = (entry, position) => {
    if (!position || entry.tabId !== activeIdRef.current) return;
    const lc = positionToLineCol(entry.sql, position);
    if (lc) editorRef.current?.markError(entry.from + lc.offset, entry.from + lc.offset + 1);
  };

  const runStatements = async (stmts) => {
    if (busy || !wsId || !stmts.length) return;
    stopRef.current = false;
    editorRef.current?.clearError();
    const tabId = activeIdRef.current;
    const entries = stmts.map((s, i) => ({
      key: `${Date.now()}-${i}`, sql: s.sql, from: s.from, to: s.to, tabId, status: 'pending',
    }));
    setRuns(entries);
    setActiveRun(0);
    setRunning(true);
    const final = entries.map((e) => ({ ...e }));
    let confirmAll = false;
    let firstError = -1;
    for (let i = 0; i < entries.length; i += 1) {
      if (stopRef.current) {
        for (let j = i; j < entries.length; j += 1) {
          final[j].status = 'skipped';
          patchRun(j, { status: 'skipped' });
        }
        break;
      }
      const entry = entries[i];
      patchRun(i, { status: 'running' });
      if (entries.length > 1) setActiveRun(i);
      let confirmed = confirmAll;
      const started = Date.now();
      // eslint-disable-next-line no-constant-condition
      while (true) {
        try {
          // eslint-disable-next-line no-await-in-loop
          const res = await executeSQL(resource.id, wsId, { sql: entry.sql, limit: PAGE_SIZE, offset: 0, confirmed });
          applyServerState(res);
          const patch = { status: 'ok', res, pageIndex: 0, total: null, expanded: false };
          Object.assign(final[i], patch);
          patchRun(i, patch);
          let info;
          if (res.columns) info = `${res.returnedRows}${res.hasMore ? '+' : ''} 行`;
          else if (res.statementType === 'SET') info = '会话设置';
          else info = `影响 ${res.affectedRows ?? 0} 行`;
          addHistory({ sql: entry.sql, at: Date.now(), ms: res.durationMs ?? Date.now() - started, ok: true, info });
          break;
        } catch (e) {
          if (e.txState) setTxState(e.txState);
          if (e.code === 'DANGEROUS_SQL' && !confirmed) {
            // eslint-disable-next-line no-await-in-loop
            const choice = await askDanger(entry.sql, e.message, entries.length > 1 && i < entries.length - 1);
            if (choice === 'all') { confirmAll = true; confirmed = true; continue; }
            if (choice === 'one') { confirmed = true; continue; }
            final[i].status = 'skipped';
            patchRun(i, { status: 'skipped', note: '已取消执行' });
            stopRef.current = true;
            break;
          }
          const canceled = e.code === 'QUERY_CANCELED';
          const error = { message: friendlyWorkspaceError(e), code: e.code, position: e.body?.position || 0 };
          const patch = { status: canceled ? 'canceled' : 'error', error };
          Object.assign(final[i], patch);
          patchRun(i, patch);
          addHistory({ sql: entry.sql, at: Date.now(), ms: Date.now() - started, ok: false, info: canceled ? '已取消' : (e.code || '失败') });
          if (firstError < 0 && !canceled) {
            firstError = i;
            markErrorInEditor(entry, error.position);
          }
          if (!continueOnError || canceled || e.code === 'WORKSPACE_CLOSED') stopRef.current = true;
          break;
        }
      }
    }
    setRunning(false);
    if (final.length === 1) {
      setActiveRun(0);
      return;
    }
    let pick = final.findIndex((r) => r.status === 'error');
    if (pick < 0) {
      for (let k = final.length - 1; k >= 0; k -= 1) {
        if (final[k].res?.columns) { pick = k; break; }
      }
    }
    setActiveRun(pick < 0 ? -1 : pick);
  };

  const runCurrent = () => {
    const target = editorRef.current?.getTarget();
    if (!target || !target.sql.trim()) {
      toast('没有可执行的 SQL：请把光标放在语句内，或选中要执行的文本', { tone: 'warn' });
      return;
    }
    const stmts = target.isSelection
      ? splitStatements(target.sql, splitDialect).map((s) => ({ ...s, from: s.from + target.from, to: s.to + target.from }))
      : [target];
    if (!stmts.length) {
      toast('选中内容里没有可执行的语句', { tone: 'warn' });
      return;
    }
    runStatements(stmts);
  };

  const runAll = () => {
    const stmts = splitStatements(editorRef.current?.getDoc() || '', splitDialect);
    if (!stmts.length) {
      toast('编辑器里没有可执行的语句', { tone: 'warn' });
      return;
    }
    runStatements(stmts);
  };

  const stopRun = () => {
    stopRef.current = true;
    // 只调服务端 cancel：等 execute 返回 QUERY_CANCELED 与真实 txState
    if (wsId) cancelWorkspaceSQL(resource.id, wsId).catch(() => {});
  };

  const withInflight = async (fn) => {
    setInflight((n) => n + 1);
    try { return await fn(); } finally { setInflight((n) => n - 1); }
  };

  const loadPage = (idx, pageIndex, limit = PAGE_SIZE, expanded = false) => withInflight(async () => {
    const entry = runsRef.current[idx];
    if (!entry) return false;
    patchRun(idx, { loading: true });
    try {
      const res = await executeSQL(resource.id, wsId, { sql: entry.sql, limit, offset: pageIndex * PAGE_SIZE, confirmed: false });
      applyServerState(res);
      patchRun(idx, { res, pageIndex, expanded, loading: false });
      if (expanded && res.hasMore) toast(`已加载前 ${LOAD_ALL_MAX} 行，更多数据请加条件过滤或用全量导出`, { tone: 'warn' });
      return true;
    } catch (e) {
      if (e.txState) setTxState(e.txState);
      patchRun(idx, { loading: false });
      toast(friendlyWorkspaceError(e), { tone: 'error' });
      return false;
    }
  });

  const countRows = (idx) => withInflight(async () => {
    const entry = runsRef.current[idx];
    if (!entry) return;
    patchRun(idx, { counting: true });
    try {
      const d = await countWorkspaceSQL(resource.id, wsId, entry.sql);
      if (d.txState) setTxState(d.txState);
      patchRun(idx, { total: d.total, counting: false });
    } catch (e) {
      if (e.txState) setTxState(e.txState);
      patchRun(idx, { counting: false });
      toast(friendlyWorkspaceError(e), { tone: 'error' });
    }
  });

  const saveGridEdits = async (idx, payload) => {
    const entry = runsRef.current[idx];
    if (!entry?.res?.editable) return false;
    try {
      const res = await applyRowEdits(resource.id, wsId, {
        schema: entry.res.editable.schema || '', table: entry.res.editable.table, ...payload,
      });
      toast(`已保存 · 更新 ${res.updated || 0} · 删除 ${res.deleted || 0}`);
      const ok = await loadPage(idx, entry.pageIndex || 0);
      if (!ok) toast('保存成功，但刷新结果失败，请重新查询', { tone: 'warn' });
      return true;
    } catch (e) {
      toast(e.message || '保存失败', { tone: 'error' });
      return false;
    }
  };
  const saveGridEditsRef = React.useRef(saveGridEdits);
  saveGridEditsRef.current = saveGridEdits;
  const activeRunRef = React.useRef(activeRun);
  activeRunRef.current = activeRun;
  const onSaveEditsStable = React.useCallback((payload) => saveGridEditsRef.current(activeRunRef.current, payload), []);

  // ---- 事务 ----
  const toggleAC = async () => {
    if (!wsId || busy) return;
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
  const doResetSession = async () => {
    try {
      await resetSessionSettings(resource.id, wsId);
      setSessionSettings([]);
      toast('会话设置已重置');
    } catch (e) {
      toast(e.message || '重置失败', { tone: 'error' });
    }
  };

  // ---- 格式化 / 保存 ----
  const doFormat = () => {
    const target = editorRef.current?.getTarget();
    if (!target || !target.sql.trim()) return;
    try {
      const formatted = formatSQL(target.sql, { language: dialectForFormat(resource.dbType), keywordCase: 'upper' });
      editorRef.current.replaceRange(target.from, target.to, formatted);
    } catch (e) {
      toast('格式化失败，已保留原文', { tone: 'warn' });
    }
  };

  const [saveOpen, setSaveOpen] = React.useState(false);
  const [saveName, setSaveName] = React.useState('');
  const quickSave = async () => {
    const tab = tabsRef.current.find((t) => t.id === activeIdRef.current);
    const text = editorRef.current?.getDoc() || '';
    if (!text.trim()) {
      toast('编辑器为空', { tone: 'warn' });
      return;
    }
    if (tab?.savedId) {
      try {
        await updateSavedSQL(resource.id, tab.savedId, { name: tab.title, sqlText: text });
        setTabs((list) => list.map((t) => (t.id === tab.id ? { ...t, savedText: text } : t)));
        toast(`已保存「${tab.title}」`);
        refreshSaved().catch(() => {});
      } catch (e) {
        toast(e.message || '保存失败', { tone: 'error' });
      }
      return;
    }
    setSaveName(tab && !/^查询 \d+$/.test(tab.title) ? tab.title : '');
    setSaveOpen(true);
  };
  const submitSave = async () => {
    const name = saveName.trim();
    const text = editorRef.current?.getDoc() || '';
    if (!name || !text.trim()) {
      toast('名称和 SQL 内容不能为空', { tone: 'warn' });
      return;
    }
    try {
      const created = await createSavedSQL(resource.id, { name, sqlText: text });
      const id = activeIdRef.current;
      setTabs((list) => list.map((t) => (t.id === id ? { ...t, title: name, savedId: created.id, savedText: text } : t)));
      setSaveOpen(false);
      toast('已保存');
      refreshSaved().catch(() => {});
    } catch (e) {
      toast(e.message || '保存失败', { tone: 'error' });
    }
  };
  const onDeleteSaved = React.useCallback(async (item) => {
    try {
      await deleteSavedSQL(resource.id, item.id);
      setTabs((list) => list.map((t) => (t.savedId === item.id ? { ...t, savedId: undefined, savedText: undefined } : t)));
      refreshSaved().catch(() => {});
    } catch (e) {
      toast(e.message || '删除失败', { tone: 'error' });
    }
  }, [resource.id, refreshSaved]);
  const onOpenSQL = React.useCallback((sqlText, name, item) => {
    if (item) {
      const existing = tabsRef.current.find((t) => t.savedId === item.id);
      if (existing) {
        setActiveTabId(existing.id);
        return;
      }
    }
    openInTab(sqlText, { title: name || undefined, saved: item || undefined });
  }, [openInTab]);

  const handlersRef = React.useRef({});
  handlersRef.current = { runCurrent, runAll, save: quickSave, format: doFormat };

  // ---- 对象操作 ----
  const pendingRunRef = React.useRef(null);
  React.useEffect(() => {
    // 「查看数据」新开标签后，等编辑器切到该标签再执行
    const p = pendingRunRef.current;
    if (p && p.tabId === activeTab.id) {
      pendingRunRef.current = null;
      runStatements([{ sql: p.sql, from: 0, to: p.sql.length }]);
    }
  });
  const viewData = async (node) => {
    try {
      const data = await sqlTemplate(resource.id, node.id, 'SELECT');
      const sqlText = (data.sql || data.template || '').trim();
      if (!sqlText) {
        toast('无法生成查询', { tone: 'error' });
        return;
      }
      const tabId = openInTab(sqlText, { title: node.name });
      pendingRunRef.current = { tabId, sql: sqlText };
    } catch (e) {
      toast(e.message || '查看数据失败', { tone: 'error' });
    }
  };
  const insertTemplate = async (node, operation) => {
    try {
      const data = await sqlTemplate(resource.id, node.id, operation);
      const text = data.sql || data.template;
      if (text) openInTab(text, { title: `${operation} ${node.name}` });
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
      openInTab(await fetchDDL(node), { title: `DDL ${node.name}` });
    } catch (e) {
      toast(e.message || '获取 DDL 失败', { tone: 'error' });
    }
  };
  const downloadDDL = async (node) => {
    try {
      downloadBlob(new Blob([await fetchDDL(node)], { type: 'text/sql;charset=utf-8' }), `${node.name}.sql`);
    } catch (e) {
      toast(e.message || '导出 DDL 失败', { tone: 'error' });
    }
  };

  const [structureState, setStructureState] = React.useState(null);
  const showStructure = async (node) => {
    setStructureState({ node, loading: true });
    try {
      const [structure, ddl] = await Promise.all([metadataStructure(resource.id, node.id), fetchDDL(node).catch(() => '')]);
      setStructureState({ node, structure, ddl, loading: false });
    } catch (e) {
      setStructureState(null);
      toast(e.message || '读取表结构失败', { tone: 'error' });
    }
  };

  const [importState, setImportState] = React.useState(null);
  const openImport = async (node) => {
    try {
      const structure = await metadataStructure(resource.id, node.id);
      setImportState({ node, structure, file: null, preview: null, mapping: [], busy: false });
    } catch (e) {
      toast(e.message || '读取目标表结构失败', { tone: 'error' });
    }
  };
  const closeImport = () => {
    if (importState?.preview?.importId) deleteImport(resource.id, importState.preview.importId).catch(() => {});
    setImportState(null);
  };
  const applyPreview = (preview, cur) => {
    const byLower = new Map((cur.structure?.columns || []).map((c) => [c.name.toLowerCase(), c.name]));
    return { ...cur, preview, mapping: (preview.columns || []).map((n) => byLower.get(String(n).toLowerCase()) || ''), busy: false };
  };
  const chooseImportFile = async (file) => {
    if (!file) return;
    setImportState((cur) => ({ ...cur, file, busy: true }));
    try {
      const preview = await previewImport(resource.id, file);
      setImportState((cur) => applyPreview(preview, cur));
    } catch (e) {
      setImportState((cur) => ({ ...cur, busy: false }));
      toast(e.message || '文件预览失败', { tone: 'error' });
    }
  };
  const changeImportSheet = async (sheet) => {
    if (!importState?.preview?.importId) return;
    setImportState((cur) => ({ ...cur, busy: true }));
    try {
      const preview = await selectImportSheet(resource.id, importState.preview.importId, sheet);
      setImportState((cur) => applyPreview(preview, cur));
    } catch (e) {
      setImportState((cur) => ({ ...cur, busy: false }));
      toast(e.message || '切换工作表失败', { tone: 'error' });
    }
  };
  const submitImport = async () => {
    if (!importState?.preview?.importId) return;
    setImportState((cur) => ({ ...cur, busy: true }));
    try {
      const data = await executeImport(resource.id, importState.preview.importId, {
        tableName: importState.node.name, schema: importState.node.schema || '', columnMapping: importState.mapping,
      });
      if (data.error) throw new Error(data.error);
      toast(`导入完成 · ${data.importedRows || 0} 行`);
      setImportState(null);
    } catch (e) {
      setImportState((cur) => ({ ...cur, busy: false }));
      toast(e.message || '导入失败', { tone: 'error' });
    }
  };

  const actionsRef = React.useRef({});
  actionsRef.current = { viewData, showStructure, showDDL, downloadDDL, insertTemplate, openImport };
  const nodeActions = React.useCallback((node) => {
    const leaf = node.kind === 'table' || node.kind === 'view' || node.kind === 'matview';
    if (!leaf) return [];
    const a = actionsRef.current;
    const table = node.kind === 'table';
    const actions = [{ label: '查看数据', action: () => a.viewData(node) }];
    if (table) actions.push({ label: '查看表结构', action: () => a.showStructure(node) });
    if (caps.ddlSupported) {
      actions.push({ label: '查看 DDL', action: () => a.showDDL(node) }, { label: '导出 DDL', action: () => a.downloadDDL(node) });
    }
    if (canWrite && table) {
      actions.push(
        { label: '生成 INSERT', action: () => a.insertTemplate(node, 'INSERT') },
        { label: '生成 UPDATE', action: () => a.insertTemplate(node, 'UPDATE') },
        { label: '生成 DELETE', action: () => a.insertTemplate(node, 'DELETE') },
      );
      if (caps.importSupported) actions.push({ label: '导入数据', action: () => a.openImport(node) });
    }
    return actions;
  }, [caps, canWrite]);

  // ---- 导出 ----
  const [exportState, setExportState] = React.useState(null);
  const current = activeRun >= 0 ? runs[activeRun] : null;
  const doExport = async () => {
    if (!exportState || !current?.res?.columns) return;
    try {
      if (exportState.scope === 'current') {
        if (exportState.format !== 'csv') {
          toast('当前页仅支持 CSV；完整结果请选「全部结果」', { tone: 'warn' });
          return;
        }
        downloadBlob(buildSnapshotCSVBlob(current.res.columns, current.res.rows || []), exportFileName(resource.name, 'csv'));
        setExportState(null);
        return;
      }
      setExportState((s) => ({ ...s, busy: true }));
      const blob = await exportWorkspace(resource.id, wsId, { sql: current.sql, format: exportState.format, scope: 'all' });
      downloadBlob(blob, exportFileName(resource.name, exportState.format));
      setExportState(null);
    } catch (e) {
      setExportState((s) => (s ? { ...s, busy: false } : s));
      toast(e.message || '导出失败', { tone: 'error' });
    }
  };

  // ---- 布局 ----
  const [treeCollapsed, setTreeCollapsed] = React.useState(false);
  const [treeWidth, setTreeWidth] = React.useState(() => Number(localStorage.getItem('mc_dr_tree_width')) || 280);
  const [editorHeight, setEditorHeight] = React.useState(() => Number(localStorage.getItem('mc_dr_editor_height')) || 260);
  const [fullscreen, setFullscreen] = React.useState(false);
  const [sessionOpen, setSessionOpen] = React.useState(false);
  const [keysOpen, setKeysOpen] = React.useState(false);

  React.useEffect(() => {
    try { localStorage.setItem('mc_dr_tree_width', String(Math.round(treeWidth))); } catch (e) { /* ignore */ }
  }, [treeWidth]);
  React.useEffect(() => {
    try { localStorage.setItem('mc_dr_editor_height', String(Math.round(editorHeight))); } catch (e) { /* ignore */ }
  }, [editorHeight]);

  React.useEffect(() => {
    if (!fullscreen) return undefined;
    const prev = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    document.documentElement.setAttribute('data-dr-fullscreen', '1');
    const onKey = (e) => {
      if (e.key === 'Escape' && !document.querySelector('.dialog-overlay, .dr-context-menu')) setFullscreen(false);
    };
    window.addEventListener('keydown', onKey);
    return () => {
      document.body.style.overflow = prev;
      document.documentElement.removeAttribute('data-dr-fullscreen');
      window.removeEventListener('keydown', onKey);
    };
  }, [fullscreen]);

  const beginResize = (type, event) => {
    event.preventDefault();
    const startX = event.clientX;
    const startY = event.clientY;
    const initialTree = treeWidth;
    const initialEditor = editorHeight;
    const move = (e) => {
      if (type === 'tree') setTreeWidth(Math.max(TREE_MIN, Math.min(TREE_MAX, initialTree + e.clientX - startX)));
      else setEditorHeight(Math.max(EDITOR_MIN, Math.min(EDITOR_MAX, initialEditor + e.clientY - startY)));
    };
    const up = () => {
      window.removeEventListener('mousemove', move);
      window.removeEventListener('mouseup', up);
      document.body.classList.remove('dr-resizing');
    };
    document.body.classList.add('dr-resizing');
    window.addEventListener('mousemove', move);
    window.addEventListener('mouseup', up);
  };

  const insertText = React.useCallback((text) => editorRef.current?.insertAtCursor(text), []);
  const collapseTree = React.useCallback(() => setTreeCollapsed(true), []);

  // ---- 渲染：结果区 ----
  const canGridEditFor = (entry) => !!(canWrite && autoCommit && !entry?.res?.passthrough
    && entry?.res?.editable?.primaryKeys?.length && entry?.res?.columns?.length);

  const activeEntry = activeRun >= 0 ? runs[activeRun] : null;
  // 结果对象引用稳定：只有该结果本身变化时 ResultGrid 才重渲染
  const gridResult = React.useMemo(() => (activeEntry?.res ? { ...activeEntry.res, offset: activeEntry.res.offset || 0 } : null),
    [activeEntry?.res]); // eslint-disable-line react-hooks/exhaustive-deps

  const renderResultBody = () => {
    if (!runs.length) {
      return (
        <EmptyState icon="terminal" title="结果区"
          desc={`${MOD}+Enter 执行光标所在语句或选中内容 · ${MOD}+Shift+Enter 执行全部（多条语句用 ; 分隔）`} />
      );
    }
    if (activeRun < 0) {
      return (
        <div className="dr-msg-list">
          <table className="table">
            <thead><tr><th style={{ width: 44 }}>#</th><th style={{ width: 84 }}>状态</th><th>语句</th><th style={{ width: 240 }}>结果</th><th style={{ width: 80 }}>耗时</th></tr></thead>
            <tbody>
              {runs.map((r, i) => (
                <tr key={r.key} className="dr-msg-row" onClick={() => setActiveRun(i)}>
                  <td>{i + 1}</td>
                  <td><RunStatus run={r} /></td>
                  <td className="mono dr-msg-sql">{r.sql.split('\n')[0]}</td>
                  <td>{runSummary(r)}</td>
                  <td>{r.res?.durationMs != null ? `${r.res.durationMs}ms` : '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      );
    }
    const entry = activeEntry;
    if (!entry) return null;
    if (entry.status === 'pending' || entry.status === 'running') {
      return <div className="dr-run-wait"><Spinner size={16} /> 执行中… <Btn size="sm" variant="ghost" icon="stop" onClick={stopRun}>停止</Btn></div>;
    }
    if (entry.status === 'skipped') {
      return <div className="dr-run-note">{entry.note || '未执行：前序语句失败或已停止'}</div>;
    }
    if (entry.status === 'error' || entry.status === 'canceled') {
      const lc = entry.error?.position ? positionToLineCol(entry.sql, entry.error.position) : null;
      if (lc) {
        // 换算成编辑器行号（语句起点之前的换行数 + 语句内行号）
        const docText = tabTextRef.current.get(entry.tabId) || '';
        const before = docText.slice(0, entry.from);
        if (lc.line === 1) lc.col += before.length - (before.lastIndexOf('\n') + 1); // 语句不在行首时补列偏移
        lc.line += (before.match(/\n/g) || []).length;
      }
      return (
        <div className={`dr-error-panel${entry.status === 'canceled' ? ' is-canceled' : ''}`}>
          <div className="dr-error-head">
            <Icon name="alert" size={15} />
            <strong>{entry.status === 'canceled' ? '已取消' : '执行失败'}</strong>
            {entry.error?.code ? <span className="code-chip">{entry.error.code}</span> : null}
            {lc ? (
              <Btn size="sm" variant="ghost" onClick={() => markErrorInEditor(entry, entry.error.position)}>
                定位到第 {lc.line} 行第 {lc.col} 列
              </Btn>
            ) : null}
            <div style={{ flex: 1 }} />
            <Btn size="sm" variant="ghost" icon="copy" onClick={async () => { await copyText(entry.error?.message || ''); toast('已复制'); }}>复制错误</Btn>
          </div>
          <pre className="dr-error-msg">{entry.error?.message}</pre>
          {looksLikeMissingSemicolon(entry.sql) ? (
            <div className="dr-error-hint">这段 SQL 中空行后又开始了新语句——如果是多条语句，请用 <code>;</code> 分隔后再执行。</div>
          ) : null}
          <details className="dr-error-sql"><summary>执行的 SQL</summary><pre className="mono">{entry.sql}</pre></details>
        </div>
      );
    }
    const res = entry.res;
    if (!res?.columns) {
      return (
        <div className="dr-run-note is-ok">
          <Icon name="check" size={15} />
          {res?.statementType === 'SET'
            ? '会话设置已记录：本工作台后续每条语句执行前都会自动带上该设置'
            : `${res?.statementType || ''} 执行成功 · 影响 ${res?.affectedRows ?? 0} 行 · ${res?.durationMs ?? 0}ms`}
        </div>
      );
    }
    return (
      <ResultGrid
        result={gridResult}
        canEdit={canGridEditFor(entry)}
        dbType={resource.dbType}
        busy={busy || !!entry.loading}
        onSaveEdits={onSaveEditsStable}
        onPendingChange={setGridPending}
      />
    );
  };

  const renderResultBar = () => {
    const entry = activeEntry;
    const res = entry?.status === 'ok' ? entry.res : null;
    const from = res ? (res.offset || 0) + 1 : 0;
    const to = res ? (res.offset || 0) + (res.returnedRows || 0) : 0;
    // 第一页就取完：总数已知，不需要统计与翻页
    const singlePage = !!res && !res.hasMore && !entry?.pageIndex && !entry?.expanded;
    const knownTotal = entry?.total != null ? entry.total : (singlePage ? res.returnedRows : null);
    return (
      <div className="dr-result-status">
        {runs.length > 1 ? (
          <div className="dr-run-tabs">
            <button type="button" className={activeRun < 0 ? 'is-active' : undefined} onClick={() => setActiveRun(-1)}>
              消息{runs.some((r) => r.status === 'error') ? <span className="dr-dot err" /> : null}
            </button>
            {runs.map((r, i) => (
              <button key={r.key} type="button" className={activeRun === i ? 'is-active' : undefined} onClick={() => setActiveRun(i)} title={r.sql}>
                <RunStatus run={r} compact />
                {i + 1}·{stmtLabel(r.sql)}
              </button>
            ))}
          </div>
        ) : null}
        {res?.columns ? (
          <React.Fragment>
            <span className="dr-result-meta">
              {res.durationMs}ms · {singlePage ? `共 ${res.returnedRows} 行` : `第 ${res.returnedRows ? from : 0}–${to} 行`}
              {!singlePage && knownTotal != null ? ` / 共 ${knownTotal} 行` : (res.hasMore && knownTotal == null ? ' · 还有更多' : '')}
              {!res.passthrough && canWrite && res.editable?.reason && !canGridEditFor(entry) ? ` · ${res.editable.reason}` : ''}
              {canWrite && !autoCommit && res.editable?.primaryKeys?.length ? ' · 关闭自动提交时不可就地编辑' : ''}
            </span>
            <div style={{ flex: 1 }} />
            {entry.loading ? <Spinner size={13} /> : null}
            {!res.passthrough && knownTotal == null ? (
              <Btn size="sm" variant="ghost" disabled={busy || entry.counting} onClick={() => countRows(activeRun)}
                title="执行 COUNT(*) 统计总行数">{entry.counting ? <Spinner size={12} /> : '统计总数'}</Btn>
            ) : null}
            {!gridPending && !singlePage ? (
              <div className="dr-result-pager">
                <Btn size="sm" variant="ghost" icon="chevronL" title="上一页" disabled={busy || entry.expanded || !entry.pageIndex}
                  onClick={() => loadPage(activeRun, entry.pageIndex - 1)} />
                <span className="dr-pager-info">
                  {entry.expanded
                    ? `全部 ${res.returnedRows} 行`
                    : `第 ${(entry.pageIndex || 0) + 1}${entry.total != null ? ` / ${Math.max(1, Math.ceil(entry.total / PAGE_SIZE))}` : ''} 页`}
                </span>
                <Btn size="sm" variant="ghost" icon="chevronR" title="下一页" disabled={busy || entry.expanded || !res.hasMore}
                  onClick={() => loadPage(activeRun, (entry.pageIndex || 0) + 1)} />
                {!entry.expanded ? (
                  <Btn size="sm" variant="ghost" disabled={busy || (!res.hasMore && !entry.pageIndex)}
                    title={`一次加载（最多 ${LOAD_ALL_MAX} 行）`} onClick={() => loadPage(activeRun, 0, LOAD_ALL_MAX, true)}>加载全部</Btn>
                ) : (
                  <Btn size="sm" variant="ghost" disabled={busy} onClick={() => loadPage(activeRun, 0)}>恢复分页</Btn>
                )}
              </div>
            ) : null}
            <Btn size="sm" variant="ghost" icon="copy" title="复制本页（TSV，可直接粘贴到 Excel）"
              onClick={async () => {
                const ok = await copyText(toTSV(res.columns, res.rows || []));
                toast(ok ? `已复制 ${res.returnedRows} 行` : '复制失败', { tone: ok ? undefined : 'warn' });
              }} />
            <Btn size="sm" variant="ghost" icon="download" title="导出" onClick={() => setExportState({ scope: 'current', format: 'csv' })} />
          </React.Fragment>
        ) : (
          <span className="dr-result-meta">{runs.length ? runSummaryLine(runs, running) : '结果'}</span>
        )}
      </div>
    );
  };

  const activeTabText = tabTextRef.current.get(activeTab.id) ?? activeTab.sql ?? '';

  const workspace = (
    <div className={`dr-workspace${fullscreen ? ' is-fullscreen' : ''}`}>
      <div className="dr-workspace-grid">
        {!treeCollapsed ? (
          <React.Fragment>
            <div className="dr-side-slot" style={{ width: treeWidth }}>
              <SidePanel
                resource={resource}
                completion={completion}
                defaultSchema={defaultSchema}
                nodeActions={nodeActions}
                onInsertText={insertText}
                onOpenSQL={onOpenSQL}
                saved={saved}
                onDeleteSaved={onDeleteSaved}
                history={history}
                onClearHistory={clearHistory}
                onBack={onBack}
                onCollapse={collapseTree}
              />
            </div>
            <div className="dr-resizer dr-resizer-x" onMouseDown={(e) => beginResize('tree', e)} />
          </React.Fragment>
        ) : (
          <div className="dr-tree-rail">
            <Btn size="sm" variant="outline" icon="chevronL" title="返回列表" onClick={onBack} />
            <Btn size="sm" variant="outline" icon="chevronR" title="展开侧栏" onClick={() => setTreeCollapsed(false)} />
          </div>
        )}

        <main className="dr-main-pane">
          <div className="card dr-toolbar">
            {treeCollapsed ? <span className="dr-toolbar-resource" title={resource.name}>{resource.name}</span> : null}
            <Btn size="sm" variant="primary" icon="play" disabled={busy || !wsId} onClick={runCurrent}
              title={`执行光标所在语句或选中内容（${MOD}+Enter）`}>执行</Btn>
            <Btn size="sm" variant="outline" icon="layers" disabled={busy || !wsId} onClick={runAll}
              title={`按分号拆分后逐条执行编辑器全部内容（${MOD}+Shift+Enter）`}>执行全部</Btn>
            <Btn size="sm" variant="ghost" icon="stop" disabled={!running && inflight === 0} onClick={stopRun} title="取消正在执行的语句并停止脚本">停止</Btn>
            <span className="dr-toolbar-check" title="执行全部时某条失败后是否继续执行后续语句">
              <Checkbox checked={continueOnError} onChange={setContinueOnError} label="遇错继续" />
            </span>
            <span className="dr-toolbar-sep" />
            <span className="dr-toolbar-check" title="关闭后，写操作在同一事务中累积，需手动提交或回滚">
              <Switch on={autoCommit} onChange={toggleAC} />
              <span>自动提交</span>
            </span>
            {!autoCommit ? (
              <React.Fragment>
                <Btn size="sm" variant="ghost" disabled={txState !== 'active'} onClick={doCommit}>提交</Btn>
                <Btn size="sm" variant="ghost" disabled={txState === 'none'} onClick={doRollback}>回滚</Btn>
              </React.Fragment>
            ) : null}
            <span className="dr-toolbar-sep" />
            <Btn size="sm" variant="ghost" onClick={doFormat} title="格式化当前语句或选中内容（Shift+Alt+F）">格式化</Btn>
            <Btn size="sm" variant="ghost" onClick={quickSave} title={`保存到「已保存」（${MOD}+S）`}>保存</Btn>
            <div style={{ flex: 1 }} />
            {sessionSettings.length ? (
              <div className="dr-session-wrap">
                <Btn size="sm" variant="outline" icon="settings" onClick={() => setSessionOpen((v) => !v)}
                  title="本工作台已记录的会话设置，每条语句执行前自动重放">会话设置 {sessionSettings.length}</Btn>
                {sessionOpen ? (
                  <div className="card dr-popover" onMouseLeave={() => setSessionOpen(false)}>
                    <div className="dr-popover-title">每条语句执行前自动重放</div>
                    {sessionSettings.map((s) => <div key={s} className="mono dr-popover-item">{s}</div>)}
                    <div style={{ display: 'flex', justifyContent: 'flex-end', marginTop: 6 }}>
                      <Btn size="sm" variant="outline" onClick={() => { setSessionOpen(false); doResetSession(); }}>全部重置</Btn>
                    </div>
                  </div>
                ) : null}
              </div>
            ) : null}
            {!autoCommit || txState !== 'none' ? (
              <Badge tone={txState === 'active' ? 'warn' : txState === 'failed' ? 'error' : 'info'}>{txLabel(txState)}</Badge>
            ) : null}
            {!canWrite ? <Badge tone="info">只读</Badge> : null}
            <div className="dr-session-wrap">
              <Btn size="sm" variant="ghost" icon="zap" title="快捷键" onClick={() => setKeysOpen((v) => !v)} />
              {keysOpen ? (
                <div className="card dr-popover dr-keys" onMouseLeave={() => setKeysOpen(false)}>
                  {[
                    [`${MOD}+Enter`, '执行当前语句 / 选中内容'],
                    [`${MOD}+Shift+Enter`, '执行全部'],
                    [`${MOD}+S`, '保存 SQL'],
                    ['Shift+Alt+F', '格式化'],
                    [`${MOD}+/`, '注释 / 取消注释'],
                    [`${MOD}+F`, '查找替换'],
                    ['Ctrl+Space', '补全'],
                    ['结果表 方向键', '移动选中单元格'],
                    [`结果表 ${MOD}+C`, '复制单元格 / 行'],
                    ['结果表 Enter', '编辑 / 查看详情'],
                  ].map(([k, d]) => <div key={k} className="dr-key-row"><kbd>{k}</kbd><span>{d}</span></div>)}
                  <div className="dr-popover-title" style={{ marginTop: 6 }}>多条语句用 ; 分隔；MySQL 支持 DELIMITER，达梦支持单独一行的 /</div>
                </div>
              ) : null}
            </div>
            <Btn size="sm" variant="ghost" icon={fullscreen ? 'x' : 'box'}
              title={fullscreen ? '退出全屏 (Esc)' : '全屏工作台'} onClick={() => setFullscreen((v) => !v)} />
          </div>

          <div className="card dr-editor-card" style={{ height: editorHeight }}>
            <div className="dr-editor-tabs">
              {tabs.map((t) => (
                <div key={t.id} className={`dr-editor-tab${t.id === activeTab.id ? ' is-active' : ''}`}
                  onClick={() => setActiveTabId(t.id)} onDoubleClick={() => setRenaming({ id: t.id, value: t.title })}
                  title={`${t.title}${t.savedId ? '（已保存）' : ''} · 双击重命名`}>
                  {t.savedId ? <Icon name="star" size={11} /> : null}
                  {renaming?.id === t.id ? (
                    <input className="dr-editor-tab-input" autoFocus value={renaming.value}
                      onChange={(e) => setRenaming({ id: t.id, value: e.target.value })}
                      onBlur={commitRename}
                      onKeyDown={(e) => { if (e.key === 'Enter') commitRename(); if (e.key === 'Escape') setRenaming(null); }} />
                  ) : <span className="dr-editor-tab-title">{t.title}</span>}
                  {dirtyTabs.has(t.id) ? <span className="dr-dot" title="有未保存的修改" /> : null}
                  <button type="button" className="dr-editor-tab-x" title="关闭"
                    onClick={(e) => { e.stopPropagation(); closeTab(t.id); }}><Icon name="x" size={11} /></button>
                </div>
              ))}
              <button type="button" className="dr-editor-tab-add" title="新建查询" onClick={() => openInTab('', { reuseEmpty: false })}>
                <Icon name="plus" size={13} />
              </button>
            </div>
            <div className="dr-editor-body">
              <SqlEditor
                ref={editorRef}
                tabId={activeTab.id}
                initialDoc={activeTabText}
                dbType={resource.dbType}
                dialect={splitDialect}
                dark={dark}
                schemaConfig={schemaConfig}
                handlersRef={handlersRef}
                onDocChange={onDocChange}
                onTableRefs={onTableRefs}
              />
            </div>
          </div>
          <div className="dr-resizer dr-resizer-y" onMouseDown={(e) => beginResize('editor', e)} />

          <div className="card dr-result-card">
            {renderResultBar()}
            <div className="dr-result-body">{renderResultBody()}</div>
          </div>
        </main>
      </div>

      <Dialog open={saveOpen} onClose={() => setSaveOpen(false)} width={420} title="保存 SQL"
        foot={<React.Fragment>
          <Btn variant="ghost" onClick={() => setSaveOpen(false)}>取消</Btn>
          <Btn variant="primary" onClick={submitSave}>保存</Btn>
        </React.Fragment>}>
        <form onSubmit={(e) => { e.preventDefault(); submitSave(); }}>
          <Field label="名称"><input className="input" autoFocus value={saveName} onChange={(e) => setSaveName(e.target.value)} /></Field>
        </form>
      </Dialog>

      <Dialog open={!!danger} onClose={() => answerDanger('stop')} width={540} title="确认危险操作"
        desc={danger?.message}
        foot={<React.Fragment>
          <Btn variant="ghost" onClick={() => answerDanger('stop')}>{danger?.multi ? '停止脚本' : '取消'}</Btn>
          {danger?.multi ? <Btn variant="outline" onClick={() => answerDanger('all')}>本次脚本全部确认</Btn> : null}
          <Btn variant="destructive" onClick={() => answerDanger('one')}>执行这条</Btn>
        </React.Fragment>}>
        <pre className="dr-ddl" style={{ maxHeight: 220 }}>{danger?.sql}</pre>
      </Dialog>

      <Dialog open={!!exportState} onClose={() => setExportState(null)} width={440} title="导出查询结果"
        desc="「全部结果」会在服务端重新执行当前结果对应的查询"
        foot={<React.Fragment>
          <Btn variant="ghost" onClick={() => setExportState(null)}>取消</Btn>
          <Btn variant="primary" icon="download" disabled={exportState?.busy} onClick={doExport}>
            {exportState?.busy ? <Spinner size={12} /> : '导出'}
          </Btn>
        </React.Fragment>}>
        {exportState ? (
          <div className="dr-dialog-grid">
            <Field label="范围">
              <select className="input" value={exportState.scope} onChange={(e) => setExportState((s) => ({ ...s, scope: e.target.value }))}>
                <option value="current">当前页 {current?.res?.returnedRows || 0} 行</option>
                <option value="all">全部结果</option>
              </select>
            </Field>
            <Field label="格式">
              <select className="input" value={exportState.format} onChange={(e) => setExportState((s) => ({ ...s, format: e.target.value }))}>
                <option value="csv">CSV</option>
                <option value="xlsx">XLSX</option>
              </select>
            </Field>
          </div>
        ) : null}
      </Dialog>

      <Dialog open={!!structureState} onClose={() => setStructureState(null)} width={860}
        title={`表结构 · ${structureState?.node?.name || ''}`}
        foot={<React.Fragment>
          {structureState?.ddl ? <Btn variant="outline" icon="download" onClick={() => downloadDDL(structureState.node)}>导出 DDL</Btn> : null}
          <Btn variant="ghost" onClick={() => setStructureState(null)}>关闭</Btn>
        </React.Fragment>}>
        {structureState?.loading ? <div className="dr-inline-status"><Spinner size={13} />加载中</div> : null}
        {structureState?.structure ? <StructureContent state={structureState} dbType={resource.dbType} onInsert={insertText} /> : null}
      </Dialog>

      <Dialog open={!!importState} onClose={closeImport} width={900}
        title={`导入数据 · ${importState?.node?.name || ''}`}
        desc="CSV/XLSX 整批事务导入，任意一行失败将全部回滚"
        foot={<React.Fragment>
          <Btn variant="ghost" disabled={importState?.busy} onClick={closeImport}>取消</Btn>
          <Btn variant="primary" icon="upload" disabled={!importState?.preview || importState?.busy}
            onClick={submitImport}>{importState?.busy ? <Spinner size={12} /> : '开始导入'}</Btn>
        </React.Fragment>}>
        {importState ? <ImportContent state={importState} setState={setImportState} onFile={chooseImportFile} onSheet={changeImportSheet} /> : null}
      </Dialog>
    </div>
  );

  // 全屏：portal 到 body，盖住侧栏/顶栏整个 Mooncell 壳层
  return (
    <React.Fragment>
      {fullscreen ? createPortal(workspace, document.body) : workspace}
      {fullscreen ? <div className="dr-workspace-placeholder" aria-hidden="true" /> : null}
    </React.Fragment>
  );
}

function RunStatus({ run, compact }) {
  const map = {
    pending: ['等待', 'muted'], running: ['执行中', 'run'], ok: ['成功', 'ok'], error: ['失败', 'err'], canceled: ['已取消', 'warn'], skipped: ['未执行', 'muted'],
  };
  const [label, tone] = map[run.status] || ['—', 'muted'];
  if (compact) return run.status === 'running' ? <Spinner size={10} /> : <span className={`dr-dot ${tone}`} title={label} />;
  return <span className={`dr-run-status ${tone}`}>{run.status === 'running' ? <Spinner size={10} /> : null}{label}</span>;
}

function runSummary(r) {
  if (r.status === 'ok') {
    if (r.res?.columns) return `${r.res.returnedRows}${r.res.hasMore ? '+' : ''} 行`;
    if (r.res?.statementType === 'SET') return '会话设置已记录';
    return `影响 ${r.res?.affectedRows ?? 0} 行`;
  }
  if (r.status === 'error' || r.status === 'canceled') return <span className="dr-msg-err" title={r.error?.message}>{r.error?.message}</span>;
  if (r.status === 'skipped') return r.note || '未执行';
  return '';
}

function runSummaryLine(runs, running) {
  const ok = runs.filter((r) => r.status === 'ok').length;
  const err = runs.filter((r) => r.status === 'error').length;
  const skip = runs.filter((r) => r.status === 'skipped' || r.status === 'canceled').length;
  if (running) return `执行中 ${Math.min(runs.length, ok + err + 1)} / ${runs.length}`;
  return `共 ${runs.length} 条 · 成功 ${ok}${err ? ` · 失败 ${err}` : ''}${skip ? ` · 未执行 ${skip}` : ''}`;
}

function StructureContent({ state, dbType, onInsert }) {
  const structure = state.structure || {};
  return (
    <div className="dr-structure">
      <h4>字段</h4>
      <div className="dr-table-scroll" style={{ maxHeight: 260 }}>
        <table className="table">
          <thead><tr><th>名称</th><th>类型</th><th>可空</th><th>默认值</th><th>注释</th></tr></thead>
          <tbody>{(structure.columns || []).map((column) => (
            <tr key={column.name} title="双击插入列名" onDoubleClick={() => onInsert(quoteIdent(column.name, dbType))}>
              <td className="mono">{column.name}</td><td>{column.dataType}</td>
              <td>{column.isNullable ? '是' : '否'}</td><td className="mono">{column.defaultValue || '—'}</td>
              <td>{column.comment || '—'}</td>
            </tr>
          ))}</tbody>
        </table>
      </div>
      <div className="dr-structure-split">
        <section><h4>约束</h4>{(structure.constraints || []).length === 0 ? <span>无</span> : (
          structure.constraints.map((c) => (
            <div key={c.name} className="dr-meta-row">
              <strong>{c.name}</strong>
              <span>{c.type} · {(c.columns || []).join(', ') || c.definition}</span>
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
                  onChange={(e) => setState((cur) => {
                    const mapping = [...cur.mapping];
                    mapping[index] = e.target.value;
                    return { ...cur, mapping };
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

export { DataWorkspacePage };
