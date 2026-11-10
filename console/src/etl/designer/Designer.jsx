// 设计器：沉浸式全窗编辑器。定义 JSON 是唯一权威；React Flow 节点只承载位置/选中/测量。
import React from 'react';
import { createPortal } from 'react-dom';
import {
  ReactFlow, ReactFlowProvider, Background, MiniMap, MarkerType, applyNodeChanges, useReactFlow,
} from '@xyflow/react';
import '@xyflow/react/dist/base.css';
import { Icon, Btn, Spinner, Dialog, Field, toast, confirmDialog } from '../../components/primitives.jsx';
import { etlApi, subscribeRun } from '../api.js';
import { useCatalog, clone, fmtAgo, fmtDateTime, KIND_LABEL, makeValue } from '../util.js';
import {
  normalizeDoc, toFlowNodes, toFlowEdges, newNode, canConnect, makeHop, autoLayout, problemsByNode, NODE_W, GRID,
} from './model.js';
import { DesignerContext, nodeTypes, edgeTypes } from './nodes.jsx';
import { Palette, DND_TYPE } from './Palette.jsx';
import { Inspector } from './Inspector.jsx';
import { Drawer } from './Drawer.jsx';

const HISTORY_LIMIT = 80;
const isTyping = (el) => el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT' || el.isContentEditable || (el.closest && el.closest('.cm-editor')));

// 沉浸式全窗：portal 到 body，避开内容区入场动画 transform 对 fixed 定位的影响；
// 同时锁住背景滚动。z-index 低于 Dialog/Toast，弹窗仍可盖在设计器之上。
export function DesignerPage(props) {
  React.useEffect(() => {
    const prev = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    return () => { document.body.style.overflow = prev; };
  }, []);
  return createPortal(
    <ReactFlowProvider>
      <Designer {...props} />
    </ReactFlowProvider>,
    document.body,
  );
}

function Designer({ defId, onBack, onOpenRun }) {
  const { catalog, error: catErr } = useCatalog();
  const rf = useReactFlow();
  const [meta, setMetaState] = React.useState(null); // { id, kind, name, description, folder, revision, updatedAt }
  const [doc, setDocState] = React.useState(null);
  const [rfNodes, setRfNodes] = React.useState([]);
  const [dirty, setDirty] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  const [loadErr, setLoadErr] = React.useState('');
  const [selection, setSelection] = React.useState({ type: 'none' });
  const [validation, setValidation] = React.useState({ problems: [], hops: {}, nodes: {}, vars: {} });
  const [validating, setValidating] = React.useState(false);
  const [validated, setValidated] = React.useState(false);
  const [drawer, setDrawer] = React.useState({ open: false, tab: 'problems' });
  const [preview, setPreview] = React.useState({ nodeId: null, loading: false, result: null, error: '' });
  const [live, setLive] = React.useState({ runId: null, run: null, nodes: {}, logs: [] });
  const [pipelines, setPipelines] = React.useState([]);
  const [scheduleCount, setScheduleCount] = React.useState(null);
  const [runDialog, setRunDialog] = React.useState(false);
  const [versionsOpen, setVersionsOpen] = React.useState(false);
  const history = React.useRef({ past: [], future: [], last: 0 });
  const unsubRef = React.useRef(null);
  const kind = meta ? meta.kind : 'pipeline';

  // ---- 加载
  const load = React.useCallback(async () => {
    try {
      const d = await etlApi.definition(defId);
      const nd = normalizeDoc(d.doc, d.kind);
      setMetaState({ id: d.id, kind: d.kind, name: d.name, description: d.description, folder: d.folder, revision: d.revision, updatedAt: d.updatedAt, updatedBy: d.updatedBy });
      setDocState(nd);
      setRfNodes(toFlowNodes(nd, d.kind));
      setDirty(false);
      history.current = { past: [], future: [], last: 0 };
      if (d.kind === 'workflow') etlApi.definitions('pipeline').then(setPipelines).catch(() => {});
      etlApi.schedules(d.id).then((l) => setScheduleCount(l.length)).catch(() => {});
    } catch (e) {
      setLoadErr(e.message || '加载失败');
    }
  }, [defId]);
  React.useEffect(() => { load(); }, [load]);
  React.useEffect(() => () => { if (unsubRef.current) unsubRef.current(); }, []);

  // ---- 文档变更（带撤销历史；连续输入 600ms 内合并为一步）
  const updateDoc = React.useCallback((fn, opts) => {
    setDocState((prev) => {
      if (!prev) return prev;
      const next = fn(prev);
      if (next === prev) return prev;
      const h = history.current;
      const now = Date.now();
      if (!(opts && opts.noHistory) && (now - h.last > 600 || !h.past.length)) {
        h.past.push(prev);
        if (h.past.length > HISTORY_LIMIT) h.past.shift();
      }
      h.last = now;
      h.future = [];
      return next;
    });
    setDirty(true);
  }, []);
  const setMeta = (patch) => { setMetaState((m) => ({ ...m, ...patch })); setDirty(true); };
  const updateNode = React.useCallback((id, fn) => updateDoc((d) => ({ ...d, nodes: d.nodes.map((n) => (n.id === id ? fn(n) : n)) })), [updateDoc]);
  const updateHop = React.useCallback((id, patch) => updateDoc((d) => ({ ...d, hops: d.hops.map((h) => (h.id === id ? { ...h, ...patch } : h)) })), [updateDoc]);

  const removeNodes = React.useCallback((ids) => {
    const set = new Set(ids);
    if (kind === 'workflow') {
      const start = doc && doc.nodes.find((n) => n.type === 'start');
      if (start && set.has(start.id)) { toast('「开始」不能删除', { tone: 'warn' }); set.delete(start.id); }
    }
    if (!set.size) return;
    updateDoc((d) => ({ ...d, nodes: d.nodes.filter((n) => !set.has(n.id)), hops: d.hops.filter((h) => !set.has(h.from) && !set.has(h.to)) }));
    setRfNodes((nds) => nds.filter((n) => !set.has(n.id)));
    setSelection({ type: 'none' });
  }, [doc, kind, updateDoc]);

  const removeHops = React.useCallback((ids) => {
    const set = new Set(ids);
    updateDoc((d) => ({ ...d, hops: d.hops.filter((h) => !set.has(h.id)) }));
    setSelection((s) => (s.type === 'edge' && set.has(s.id) ? { type: 'none' } : s));
  }, [updateDoc]);

  const undo = () => {
    const h = history.current;
    if (!h.past.length || !doc) return;
    const prev = h.past.pop();
    h.future.push(doc);
    h.last = 0;
    setDocState(prev);
    setRfNodes((nds) => toFlowNodes(prev, kind, nds));
    setDirty(true);
  };
  const redo = () => {
    const h = history.current;
    if (!h.future.length || !doc) return;
    const next = h.future.pop();
    h.past.push(doc);
    h.last = 0;
    setDocState(next);
    setRfNodes((nds) => toFlowNodes(next, kind, nds));
    setDirty(true);
  };

  // ---- 自动校验（防抖）
  const seq = React.useRef(0);
  React.useEffect(() => {
    if (!doc || !meta) return undefined;
    const my = ++seq.current;
    setValidating(true);
    const t = setTimeout(() => {
      etlApi.validate(kind, doc).then((v) => {
        if (my !== seq.current) return;
        setValidation({ problems: v.problems || [], hops: v.hops || {}, nodes: v.nodes || {}, vars: v.vars || {} });
        setValidated(true);
      }).catch((e) => {
        if (my !== seq.current) return;
        setValidation((x) => ({ ...x, problems: [{ severity: 'error', code: 'validate', message: e.message }] }));
      }).finally(() => { if (my === seq.current) setValidating(false); });
    }, 550);
    return () => clearTimeout(t);
  }, [doc, kind, meta && meta.id]);

  // ---- 保存
  const save = React.useCallback(async (opts) => {
    if (!doc || !meta) return null;
    setSaving(true);
    try {
      const d = await etlApi.updateDefinition(meta.id, { name: meta.name, description: meta.description, folder: meta.folder, doc, revision: meta.revision });
      setMetaState((m) => ({ ...m, revision: d.revision, updatedAt: d.updatedAt, updatedBy: d.updatedBy }));
      setDirty(false);
      if (!(opts && opts.quiet)) toast(`已保存 · 版本 ${d.revision}`);
      return d;
    } catch (e) {
      if (e.status === 409) {
        const ok = await confirmDialog({
          title: '内容已被他人修改', tone: 'danger', confirmText: '覆盖保存', cancelText: '放弃我的修改并重新加载',
          message: '服务器上已有更新的版本。\n\n· 覆盖保存：以你当前的画布为准，生成新版本（对方的修改仍可在「历史版本」中找回）。\n· 重新加载：丢弃你未保存的修改。',
        });
        if (ok) {
          const cur = await etlApi.definition(meta.id);
          const d = await etlApi.updateDefinition(meta.id, { name: meta.name, description: meta.description, folder: meta.folder, doc, revision: cur.revision });
          setMetaState((m) => ({ ...m, revision: d.revision, updatedAt: d.updatedAt }));
          setDirty(false);
          toast(`已覆盖保存 · 版本 ${d.revision}`);
          return d;
        }
        await load();
        return null;
      }
      toast(e.message || '保存失败', { tone: 'error' });
      return null;
    } finally {
      setSaving(false);
    }
  }, [doc, meta, load]);

  // ---- 运行
  const startRun = async (params) => {
    if (dirty) {
      const saved = await save({ quiet: true });
      if (!saved) return;
    }
    const errs = validation.problems.filter((p) => p.severity === 'error');
    if (errs.length) {
      const ok = await confirmDialog({ title: '存在错误', message: `校验发现 ${errs.length} 个错误，运行很可能失败。\n\n${errs.slice(0, 3).map((p) => '· ' + p.message).join('\n')}`, confirmText: '仍然运行' });
      if (!ok) return;
    }
    try {
      const run = await etlApi.run(meta.id, params);
      if (unsubRef.current) unsubRef.current();
      setLive({ runId: run.id, run, nodes: {}, logs: [] });
      setDrawer({ open: true, tab: 'run' });
      unsubRef.current = subscribeRun(run.id, {
        status: (r) => setLive((l) => (l.runId === run.id ? { ...l, run: r } : l)),
        node: (n) => {
          if (n.parentNode) return; // 作业中转换的内部步骤：在运行详情里查看
          setLive((l) => (l.runId === run.id ? { ...l, nodes: { ...l.nodes, [n.nodeId]: { ...(l.nodes[n.nodeId] || {}), ...n } } } : l));
        },
        metrics: ({ parent, metrics }) => {
          if (parent) return;
          setLive((l) => {
            if (l.runId !== run.id) return l;
            const nodes = { ...l.nodes };
            Object.entries(metrics || {}).forEach(([id, m]) => { nodes[id] = { ...(nodes[id] || {}), metrics: m }; });
            return { ...l, nodes };
          });
        },
        log: (e) => setLive((l) => (l.runId === run.id ? { ...l, logs: [...l.logs.slice(-1500), e] } : l)),
        end: () => {
          etlApi.runDetail(run.id).then((det) => {
            setLive((l) => {
              if (l.runId !== run.id) return l;
              const nodes = { ...l.nodes };
              (det.nodes || []).filter((n) => !n.parentNode).forEach((n) => { nodes[n.nodeId] = { ...(nodes[n.nodeId] || {}), ...n }; });
              return { ...l, run: det.run, nodes };
            });
            const st = det.run.status;
            toast(st === 'succeeded' ? '运行成功' : `运行${st === 'failed' ? '失败' : '结束'}：${det.run.error || st}`, { tone: st === 'succeeded' ? undefined : 'error' });
          }).catch(() => {});
        },
      });
    } catch (e) {
      toast(e.message || '启动失败', { tone: 'error' });
    }
  };
  const onRunClick = () => {
    if (doc.params && doc.params.length) setRunDialog(true);
    else startRun({});
  };
  const running = live.run && (live.run.status === 'running' || live.run.status === 'queued');
  const stopRun = async () => {
    try { await etlApi.cancelRun(live.runId); toast('已请求取消'); } catch (e) { toast(e.message, { tone: 'error' }); }
  };

  // ---- 预览
  const runPreview = async (nodeId, limit) => {
    setPreview({ nodeId, loading: true, result: null, error: '' });
    try {
      const res = await etlApi.preview(doc, nodeId, limit, defaultParams(doc));
      setPreview({ nodeId, loading: false, result: res, error: '' });
    } catch (e) {
      setPreview({ nodeId, loading: false, result: null, error: e.message });
    }
  };

  // ---- 画布事件
  const onNodesChange = React.useCallback((changes) => {
    const removed = changes.filter((c) => c.type === 'remove').map((c) => c.id);
    const rest = changes.filter((c) => c.type !== 'remove');
    if (rest.length) setRfNodes((nds) => applyNodeChanges(rest, nds));
    if (removed.length) removeNodes(removed);
    changes.forEach((c) => {
      if (c.type === 'select') {
        if (c.selected) setSelection({ type: 'node', id: c.id });
        else setSelection((s) => (s.type === 'node' && s.id === c.id ? { type: 'none' } : s));
      }
    });
  }, [removeNodes]);

  const onNodeDragStop = React.useCallback((_, __, dragged) => {
    const pos = {};
    (dragged || []).forEach((n) => { pos[n.id] = n.position; });
    updateDoc((d) => ({ ...d, nodes: d.nodes.map((n) => (pos[n.id] ? { ...n, ui: { ...(n.ui || {}), x: Math.round(pos[n.id].x), y: Math.round(pos[n.id].y) } } : n)) }));
  }, [updateDoc]);

  const onEdgesChange = React.useCallback((changes) => {
    const removed = changes.filter((c) => c.type === 'remove').map((c) => c.id);
    if (removed.length) removeHops(removed);
    changes.forEach((c) => {
      if (c.type === 'select') {
        if (c.selected) setSelection({ type: 'edge', id: c.id });
        else setSelection((s) => (s.type === 'edge' && s.id === c.id ? { type: 'none' } : s));
      }
    });
  }, [removeHops]);

  const onConnect = React.useCallback((conn) => {
    const err = canConnect(kind, doc, conn, catalog);
    if (err) { toast(err, { tone: 'warn' }); return; }
    updateDoc((d) => ({ ...d, hops: [...d.hops, makeHop(kind, conn)] }));
  }, [kind, doc, catalog, updateDoc]);

  const isValidConnection = React.useCallback((conn) => !canConnect(kind, doc, conn, catalog), [kind, doc, catalog]);

  const addNode = React.useCallback((m, screenPos) => {
    let pos;
    if (screenPos) pos = rf.screenToFlowPosition(screenPos);
    else {
      const el = document.querySelector('.etl-canvas');
      const r = el ? el.getBoundingClientRect() : { left: 400, top: 200, width: 600, height: 400 };
      pos = rf.screenToFlowPosition({ x: r.left + r.width / 2, y: r.top + r.height / 2 });
      pos = { x: pos.x + ((doc.nodes.length % 5) * GRID * 2), y: pos.y + ((doc.nodes.length % 5) * GRID * 2) };
    }
    pos = { x: pos.x - NODE_W / 2, y: pos.y - 28 };
    const n = newNode(m, pos, doc.nodes);
    updateDoc((d) => ({ ...d, nodes: [...d.nodes, n] }));
    setRfNodes((nds) => [...nds.map((x) => ({ ...x, selected: false })), { id: n.id, type: kind === 'workflow' ? 'action' : 'step', position: n.ui, data: {}, selected: true }]);
    setSelection({ type: 'node', id: n.id });
  }, [rf, doc, kind, updateDoc]);

  const onDrop = (e) => {
    e.preventDefault();
    const id = e.dataTransfer.getData(DND_TYPE);
    const m = id && catalog.meta(kind, id);
    if (m) addNode(m, { x: e.clientX, y: e.clientY });
  };

  const focusNode = (id) => {
    setRfNodes((nds) => nds.map((n) => ({ ...n, selected: n.id === id })));
    setSelection({ type: 'node', id });
    const n = rf.getNode(id);
    if (n) rf.setCenter(n.position.x + NODE_W / 2, n.position.y + 30, { zoom: Math.max(rf.getZoom(), 0.9), duration: 300 });
  };

  const tidy = () => {
    const next = autoLayout(doc);
    updateDoc(() => next);
    setRfNodes((nds) => toFlowNodes(next, kind, nds));
    setTimeout(() => rf.fitView({ padding: 0.25, duration: 300, maxZoom: 1 }), 30);
  };

  const duplicateSelected = () => {
    if (selection.type !== 'node') return;
    const src = doc.nodes.find((n) => n.id === selection.id);
    if (!src || src.type === 'start') return;
    const m = catalog.meta(kind, src.type);
    const n = { ...newNode(m || { id: src.type, label: src.name }, { x: src.ui.x + 40, y: src.ui.y + 40 }, doc.nodes), config: clone(src.config), name: `${src.name} 副本` };
    updateDoc((d) => ({ ...d, nodes: [...d.nodes, n] }));
    setRfNodes((nds) => [...nds.map((x) => ({ ...x, selected: false })), { id: n.id, type: kind === 'workflow' ? 'action' : 'step', position: n.ui, data: {}, selected: true }]);
    setSelection({ type: 'node', id: n.id });
  };

  // ---- 快捷键
  const keyRef = React.useRef({});
  keyRef.current = { save, undo, redo, duplicateSelected };
  React.useEffect(() => {
    const onKey = (e) => {
      const mod = e.metaKey || e.ctrlKey;
      if (!mod) return;
      const k = e.key.toLowerCase();
      if (k === 's') { e.preventDefault(); keyRef.current.save(); return; }
      if (isTyping(e.target)) return;
      if (k === 'z' && !e.shiftKey) { e.preventDefault(); keyRef.current.undo(); }
      else if ((k === 'z' && e.shiftKey) || k === 'y') { e.preventDefault(); keyRef.current.redo(); }
      else if (k === 'd') { e.preventDefault(); keyRef.current.duplicateSelected(); }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);
  React.useEffect(() => {
    if (!dirty) return undefined;
    const fn = (e) => { e.preventDefault(); e.returnValue = ''; };
    window.addEventListener('beforeunload', fn);
    return () => window.removeEventListener('beforeunload', fn);
  }, [dirty]);

  const back = async () => {
    if (dirty) {
      const ok = await confirmDialog({ title: '有未保存的修改', message: '离开后未保存的修改将丢失。', confirmText: '放弃修改并离开', tone: 'danger' });
      if (!ok) return;
    }
    onBack();
  };

  // ---- 派生数据
  const problems = React.useMemo(() => problemsByNode(validation.problems), [validation.problems]);
  const nodeById = React.useMemo(() => {
    const m = {};
    (doc ? doc.nodes : []).forEach((n) => { m[n.id] = n; });
    return m;
  }, [doc]);
  const hopById = React.useMemo(() => {
    const m = {};
    (doc ? doc.hops : []).forEach((h) => { m[h.id] = h; });
    return m;
  }, [doc]);
  const edges = React.useMemo(() => (doc ? toFlowEdges(doc, kind, selection.type === 'edge' ? selection.id : null).map((e) => ({
    ...e, markerEnd: { type: MarkerType.ArrowClosed, width: 16, height: 16, color: 'var(--border-strong)' },
  })) : []), [doc, kind, selection]);

  if (loadErr) {
    return (
      <div className="etl-designer" style={{ display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
        <div style={{ textAlign: 'center' }}>
          <div style={{ fontWeight: 600, marginBottom: 8 }}>无法打开：{loadErr}</div>
          <Btn variant="outline" icon="chevronL" onClick={onBack}>返回</Btn>
        </div>
      </div>
    );
  }
  if (!doc || !meta || !catalog) {
    return (
      <div className="etl-designer" style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', color: 'var(--muted-fg)', gap: 8 }}>
        {catErr ? `组件目录加载失败：${catErr.message}` : <React.Fragment><Spinner /> 正在打开设计器…</React.Fragment>}
      </div>
    );
  }

  const errCount = validation.problems.filter((p) => p.severity === 'error').length;
  const warnCount = validation.problems.length - errCount;
  const inCount = {};
  doc.hops.forEach((h) => { if (!h.disabled) inCount[h.to] = (inCount[h.to] || 0) + 1; });
  const ctx = {
    kind, catalog, nodeById, hopById, problems, live, hopFields: validation.hops, inCount,
    scheduleCount,
  };
  const d = {
    kind, catalog, doc, meta, setMeta, selection, updateDoc, updateNode, updateHop, removeNodes, removeHops,
    problems, allProblems: validation.problems, nodeSchemas: validation.nodes, hopFields: validation.hops, vars: validation.vars,
    pipelines, scheduleCount, drawer, setDrawer, preview, runPreview, live, focusNode, validating, validated,
    openRun: onOpenRun,
  };
  const saveState = saving ? 'saving' : dirty ? 'dirty' : 'saved';
  const isEmpty = kind === 'pipeline' ? doc.nodes.length === 0 : doc.nodes.length <= 1;

  return (
    <DesignerContext.Provider value={ctx}>
      <div className="etl-designer" data-screen-label="数据集成设计器">
        <header className="etl-dz-top">
          <Btn variant="ghost" size="sm" icon="chevronL" onClick={back} title="返回数据集成">数据集成</Btn>
          <span className="etl-dz-sep"></span>
          <div className="etl-dz-title">
            <span className="etl-kind-chip" data-kind={kind} style={{ width: 26, height: 26, borderRadius: 7 }}><Icon name={kind === 'workflow' ? 'workflow' : 'pipeline'} size={14} /></span>
            <span style={{ fontSize: 12, color: 'var(--muted-fg)' }}>{kind === 'workflow' ? '作业' : '转换'}</span>
            <input className="etl-dz-name" value={meta.name} style={{ width: `calc(${Math.min(26, Math.max(4, meta.name.length * 1.05))}em + 14px)` }} onChange={(e) => setMeta({ name: e.target.value })} aria-label="名称" />
            <span className="etl-save-state" data-state={saveState} title={meta.updatedAt ? `最后保存 ${fmtDateTime(meta.updatedAt, true)} · ${meta.updatedBy || ''}` : ''}>
              <span className="dot"></span>
              {saving ? '保存中…' : dirty ? '未保存' : `已保存 · v${meta.revision}`}
            </span>
          </div>
          <span style={{ flex: 1 }}></span>
          <button type="button" className="etl-save-state" style={{ cursor: 'pointer', padding: '4px 8px', borderRadius: 6 }}
            onClick={() => setDrawer({ open: true, tab: 'problems' })} title="查看校验问题">
            {validating ? <Spinner size={11} /> : null}
            {errCount ? <span style={{ color: 'var(--error)' }}>{errCount} 个错误</span> : null}
            {warnCount ? <span style={{ color: 'var(--warn)' }}>{warnCount} 个提示</span> : null}
            {!errCount && !warnCount && validated && !validating ? <span style={{ color: 'var(--success)' }}>校验通过</span> : null}
          </button>
          <span className="etl-dz-sep"></span>
          <Btn variant="ghost" size="sm" icon="undo" title="撤销（⌘Z）" disabled={!history.current.past.length} onClick={undo}></Btn>
          <Btn variant="ghost" size="sm" icon="redo" title="重做（⇧⌘Z）" disabled={!history.current.future.length} onClick={redo}></Btn>
          <Btn variant="ghost" size="sm" icon="layout" title="整理布局" onClick={tidy}></Btn>
          <Btn variant="ghost" size="sm" icon="history" title="历史版本" onClick={() => setVersionsOpen(true)}></Btn>
          <span className="etl-dz-sep"></span>
          {running ? (
            <Btn variant="destructive-outline" size="sm" icon="stop" onClick={stopRun}>停止</Btn>
          ) : (
            <Btn variant="outline" size="sm" icon="play" onClick={onRunClick}>运行</Btn>
          )}
          <Btn variant="primary" size="sm" icon="save" disabled={saving || !dirty} onClick={() => save()}>保存</Btn>
        </header>

        <div className="etl-dz-body">
          <Palette kind={kind} catalog={catalog} onAdd={(m) => addNode(m)} />
          <div className="etl-dz-center">
            <div className="etl-canvas" onDragOver={(e) => { e.preventDefault(); e.dataTransfer.dropEffect = 'move'; }} onDrop={onDrop}>
              <ReactFlow
                nodes={rfNodes} edges={edges} nodeTypes={nodeTypes} edgeTypes={edgeTypes}
                onNodesChange={onNodesChange} onEdgesChange={onEdgesChange} onConnect={onConnect}
                isValidConnection={isValidConnection} onNodeDragStop={onNodeDragStop}
                onPaneClick={() => setSelection({ type: 'none' })}
                snapToGrid snapGrid={[GRID, GRID]} fitView fitViewOptions={{ padding: 0.12, maxZoom: 1, minZoom: 0.6 }}
                minZoom={0.2} maxZoom={2} deleteKeyCode={['Backspace', 'Delete']} multiSelectionKeyCode={['Meta', 'Shift']}
                selectionOnDrag={false} panOnScroll zoomOnScroll={false} zoomActivationKeyCode={['Meta', 'Control']}
                proOptions={{ hideAttribution: true }} connectionRadius={28}
              >
                <Background gap={GRID} size={1.2} color="var(--border-strong)" />
                {drawer.open ? null : <MiniMap pannable zoomable position="bottom-right" style={{ width: 150, height: 96 }} nodeBorderRadius={6}
                  bgColor="var(--card)" maskColor="rgba(120, 110, 90, 0.10)" nodeColor="var(--border-strong)" />}
              </ReactFlow>
              <div className="etl-canvas-tools">
                <Btn variant="ghost" size="sm" icon="plus" title="放大" onClick={() => rf.zoomIn({ duration: 150 })}></Btn>
                <Btn variant="ghost" size="sm" icon="minus" title="缩小" onClick={() => rf.zoomOut({ duration: 150 })}></Btn>
                <Btn variant="ghost" size="sm" icon="maximize" title="适应画布" onClick={() => rf.fitView({ padding: 0.25, duration: 250, maxZoom: 1 })}></Btn>
                {live.runId && !running ? <Btn variant="ghost" size="sm" icon="x" title="清除运行状态" onClick={() => { if (unsubRef.current) unsubRef.current(); setLive({ runId: null, run: null, nodes: {}, logs: [] }); }}>清除运行状态</Btn> : null}
              </div>
              {isEmpty ? (
                <div className="etl-canvas-empty">
                  <div className="etl-canvas-empty-card">
                    <Icon name={kind === 'workflow' ? 'workflow' : 'pipeline'} size={22} />
                    <strong>{kind === 'workflow' ? '从「开始」编排作业' : '从左侧拖入第一个组件'}</strong>
                    <span>{kind === 'workflow'
                      ? '拖入「SQL 取值」「条件」「运行转换」等作业项，从节点右侧的圆点拖出连线。'
                      : '通常从「表输入」开始，经过「表达式」「过滤记录」等步骤，最后写入「表输出」或「插入 / 更新」。'}</span>
                  </div>
                </div>
              ) : null}
            </div>
            <Drawer d={d} />
          </div>
          <Inspector d={d} />
        </div>
      </div>
      {runDialog ? <RunParamsDialog doc={doc} onClose={() => setRunDialog(false)} onRun={(p) => { setRunDialog(false); startRun(p); }} /> : null}
      {versionsOpen ? <VersionsDialog meta={meta} dirty={dirty} onClose={() => setVersionsOpen(false)} onRestored={() => { setVersionsOpen(false); load(); }} /> : null}
    </DesignerContext.Provider>
  );
}

function defaultParams(doc) {
  const out = {};
  (doc.params || []).forEach((p) => { if (p.default) out[p.name] = p.default; });
  return out;
}

function RunParamsDialog({ doc, onClose, onRun }) {
  const [vals, setVals] = React.useState(() => {
    const v = {};
    (doc.params || []).forEach((p) => { v[p.name] = p.default && p.default.value != null ? String(p.default.value) : ''; });
    return v;
  });
  const submit = () => {
    const params = {};
    (doc.params || []).forEach((p) => { if (vals[p.name] !== '') params[p.name] = makeValue(p.kind, vals[p.name]); });
    onRun(params);
  };
  return (
    <Dialog open onClose={onClose} title="运行参数" desc="留空则使用默认值（无默认值时为 NULL）" width={480}
      foot={<React.Fragment><Btn variant="ghost" onClick={onClose}>取消</Btn><Btn variant="primary" icon="play" onClick={submit}>运行</Btn></React.Fragment>}>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        {(doc.params || []).map((p) => (
          <Field key={p.name} label={`${p.name} · ${KIND_LABEL[p.kind]}`} hint={p.kind === 'timestamp' ? '如 2026-11-01 00:00:00' : p.description}>
            <input className="input mono" value={vals[p.name]} onChange={(e) => setVals((s) => ({ ...s, [p.name]: e.target.value }))} />
          </Field>
        ))}
      </div>
    </Dialog>
  );
}

function VersionsDialog({ meta, dirty, onClose, onRestored }) {
  const [list, setList] = React.useState(null);
  React.useEffect(() => { etlApi.versions(meta.id).then(setList).catch(() => setList([])); }, [meta.id]);
  const restore = async (rev) => {
    const ok = await confirmDialog({
      title: `恢复到版本 ${rev}`, confirmText: '恢复',
      message: `将以版本 ${rev} 的内容生成一个新版本（当前版本仍保留在历史中）。${dirty ? '\n\n注意：你有未保存的修改，恢复后将被丢弃。' : ''}`,
    });
    if (!ok) return;
    try { await etlApi.restoreVersion(meta.id, rev); toast(`已恢复到版本 ${rev}`); onRestored(); } catch (e) { toast(e.message, { tone: 'error' }); }
  };
  return (
    <Dialog open onClose={onClose} title="历史版本" desc="每次保存生成一个版本，保留最近 30 个" width={520}>
      {list == null ? <div style={{ padding: 20, textAlign: 'center' }}><Spinner /></div> : null}
      {list && !list.length ? <div className="etl-list-empty">暂无历史版本</div> : null}
      {list && list.length ? (
        <table className="table" style={{ fontSize: 13 }}>
          <tbody>
            {list.map((v) => (
              <tr key={v.revision}>
                <td style={{ width: 70 }} className="mono">v{v.revision}</td>
                <td>{fmtDateTime(v.savedAt, true)}<span className="etl-muted" style={{ marginLeft: 8 }}>{fmtAgo(v.savedAt)}</span></td>
                <td className="etl-muted">{v.savedBy}</td>
                <td style={{ textAlign: 'right' }}>
                  {v.revision === meta.revision ? <span className="etl-muted">当前</span>
                    : <Btn variant="ghost" size="sm" icon="rotate" onClick={() => restore(v.revision)}>恢复</Btn>}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
    </Dialog>
  );
}
