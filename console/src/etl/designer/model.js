// 设计器文档模型：定义 JSON（权威）⇄ React Flow 节点/连线。
import { clone, uid } from '../util.js';

export const NODE_W = 220;
export const GRID = 16;

export function nodePos(n) {
  const ui = n.ui || {};
  return { x: Number(ui.x) || 0, y: Number(ui.y) || 0 };
}

export function flowType(kind, n) {
  if (kind === 'workflow') return n.type === 'start' ? 'start' : 'action';
  return 'step';
}

export function toFlowNodes(doc, kind, prev) {
  const prevById = {};
  (prev || []).forEach((p) => { prevById[p.id] = p; });
  const nodes = (doc.nodes || []).map((n) => {
    const p = prevById[n.id];
    return {
      ...(p || {}),
      id: n.id,
      type: flowType(kind, n),
      position: nodePos(n),
      data: {},
    };
  });
  // 注释作为伪节点渲染（不进 doc.nodes，不参与编译/连线）
  (doc.notes || []).forEach((nt) => {
    const p = prevById[nt.id];
    nodes.push({ ...(p || {}), id: nt.id, type: 'note', position: { x: nt.x || 0, y: nt.y || 0 }, data: {} });
  });
  return nodes;
}

// 连线 → 画布边。转换：sourceHandle=输出端口，targetHandle=输入端口；作业：只有一个输入。
export function toFlowEdges(doc, kind, selectedEdge) {
  return (doc.hops || []).map((h) => ({
    id: h.id,
    source: h.from,
    target: h.to,
    sourceHandle: h.fromPort || (kind === 'workflow' ? 'success' : 'main'),
    targetHandle: kind === 'workflow' ? 'in' : (h.toPort || 'main'),
    type: 'etl',
    selected: h.id === selectedEdge,
    data: {},
  }));
}

export function uniqueName(base, nodes) {
  const names = new Set((nodes || []).map((n) => n.name));
  if (!names.has(base)) return base;
  for (let i = 2; i < 1000; i += 1) {
    const c = `${base} ${i}`;
    if (!names.has(c)) return c;
  }
  return `${base} ${uid('')}`;
}

export function newNode(meta, pos, nodes) {
  let config = {};
  try { config = meta.defaults ? clone(meta.defaults) : {}; } catch (e) { config = {}; }
  return {
    id: uid('n'),
    type: meta.id,
    name: uniqueName(meta.label, nodes),
    version: meta.version || 1,
    config,
    ui: { x: Math.round(pos.x / GRID) * GRID, y: Math.round(pos.y / GRID) * GRID },
  };
}

// 连接合法性（前端预判，后端编译为准）。
export function canConnect(kind, doc, conn, catalog) {
  if (!conn.source || !conn.target || conn.source === conn.target) return '不能连向自身';
  const target = (doc.nodes || []).find((n) => n.id === conn.target);
  if (!target) return '目标不存在';
  if (kind === 'workflow' && target.type === 'start') return '「开始」不能有入线';
  const dup = (doc.hops || []).some((h) => h.from === conn.source && (h.fromPort || '') === (conn.sourceHandle || '')
    && h.to === conn.target && (kind === 'workflow' || (h.toPort || 'main') === (conn.targetHandle || 'main')));
  if (dup) return '已存在相同连线';
  if (kind === 'pipeline') {
    const meta = catalog && catalog.meta('pipeline', target.type);
    const port = meta && (meta.inputs || []).find((p) => p.name === (conn.targetHandle || 'main'));
    if (port && !port.multi && (doc.hops || []).some((h) => h.to === conn.target && (h.toPort || 'main') === port.name)) {
      return `「${target.name}」的输入只能有一条连线（需要汇流请先接「空操作」）`;
    }
  }
  return '';
}

export function makeHop(kind, conn) {
  const h = { id: uid('h'), from: conn.source, fromPort: conn.sourceHandle || (kind === 'workflow' ? 'success' : 'main'), to: conn.target };
  if (kind === 'pipeline') h.toPort = conn.targetHandle || 'main';
  return h;
}

// 整理布局：按最长路径分层，层内按原纵向顺序排列。
export function autoLayout(doc) {
  const nodes = doc.nodes || [];
  const hops = (doc.hops || []).filter((h) => !h.disabled);
  const depth = {};
  nodes.forEach((n) => { depth[n.id] = 0; });
  for (let iter = 0; iter < nodes.length; iter += 1) {
    let changed = false;
    hops.forEach((h) => {
      if (depth[h.to] !== undefined && depth[h.from] !== undefined && depth[h.to] < depth[h.from] + 1) {
        depth[h.to] = depth[h.from] + 1; changed = true;
      }
    });
    if (!changed) break;
  }
  const layers = {};
  nodes.forEach((n) => { (layers[depth[n.id]] = layers[depth[n.id]] || []).push(n); });
  const pos = {};
  Object.entries(layers).forEach(([d, ns]) => {
    ns.sort((a, b) => nodePos(a).y - nodePos(b).y);
    const total = ns.length * 120;
    ns.forEach((n, i) => { pos[n.id] = { x: 80 + Number(d) * 300, y: 200 - total / 2 + i * 120 + 60 }; });
  });
  return { ...doc, nodes: nodes.map((n) => ({ ...n, ui: { ...(n.ui || {}), ...pos[n.id] } })) };
}

// 问题按节点归类。
export function problemsByNode(problems) {
  const out = {};
  (problems || []).forEach((p) => {
    if (!p.nodeId) return;
    const o = (out[p.nodeId] = out[p.nodeId] || { errors: 0, warnings: 0, list: [] });
    if (p.severity === 'error') o.errors += 1; else o.warnings += 1;
    o.list.push(p);
  });
  return out;
}

// 文档标准化：补齐缺省字段，避免旧/空文档在画布上出错。
export function normalizeDoc(doc, kind) {
  const d = doc ? clone(doc) : {};
  d.kind = kind;
  d.format = d.format || 1;
  d.params = d.params || [];
  d.nodes = d.nodes || [];
  d.hops = d.hops || [];
  d.settings = d.settings || {};
  d.notes = d.notes || [];
  d.nodes.forEach((n, i) => {
    if (!n.ui) n.ui = { x: 80 + (i % 4) * 300, y: 80 + Math.floor(i / 4) * 140 };
    if (n.config == null) n.config = {};
  });
  return d;
}
