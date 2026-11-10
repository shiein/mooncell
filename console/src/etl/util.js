// 数据集成模块的展示工具：状态、分类配色、数字/时间格式化、值构造。
import React from 'react';
import { loadCatalog } from './api.js';

export const RUN_STATUS = {
  queued: { label: '排队中', tone: 'default' },
  running: { label: '运行中', tone: 'info' },
  succeeded: { label: '成功', tone: 'success' },
  failed: { label: '失败', tone: 'error' },
  canceled: { label: '已取消', tone: 'warn' },
  interrupted: { label: '已中断', tone: 'warn' },
};

export const NODE_STATUS = {
  pending: { label: '等待', tone: 'default' },
  running: { label: '运行中', tone: 'info' },
  succeeded: { label: '成功', tone: 'success' },
  failed: { label: '失败', tone: 'error' },
  skipped: { label: '跳过', tone: 'default' },
  canceled: { label: '已取消', tone: 'warn' },
  reused: { label: '复用', tone: 'purple' },
};

export const TRIGGER_LABEL = { manual: '手动', schedule: '调度', resume: '续跑', preview: '预览' };

// 分类 → 语义色（只用全局令牌）
export const CATEGORY = {
  input: { label: '输入', color: 'var(--info)', soft: 'var(--info-soft)' },
  transform: { label: '转换', color: 'var(--purple)', soft: 'var(--purple-soft)' },
  flow: { label: '流程', color: 'var(--warn)', soft: 'var(--warn-soft)' },
  lookup: { label: '查询', color: 'var(--cyan)', soft: 'var(--cyan-soft)' },
  join: { label: '连接', color: 'var(--cyan)', soft: 'var(--cyan-soft)' },
  output: { label: '输出', color: 'var(--success)', soft: 'var(--success-soft)' },
  control: { label: '作业项', color: 'var(--primary)', soft: 'var(--primary-soft)' },
  utility: { label: '工具', color: 'var(--fg-secondary)', soft: 'var(--muted)' },
};

export const PORT_TONE = {
  success: 'var(--success)', error: 'var(--error)', warn: 'var(--warn)', info: 'var(--info)',
};

export const KIND_LABEL = {
  string: '字符串', integer: '整数', number: '浮点数', decimal: '精确小数',
  boolean: '布尔', date: '日期', timestamp: '日期时间', binary: '二进制',
};
export const KIND_OPTIONS = Object.entries(KIND_LABEL).map(([value, label]) => ({ value, label }));
export const KIND_SHORT = { string: 'Abc', integer: '123', number: '1.2', decimal: '0.00', boolean: 'T/F', date: 'D', timestamp: 'DT', binary: '01' };

const nf = new Intl.NumberFormat('zh-CN');
export const fmtNum = (n) => (n == null || Number.isNaN(n) ? '—' : nf.format(Math.round(n)));

export function fmtCompact(n) {
  if (n == null) return '—';
  const a = Math.abs(n);
  if (a >= 1e8) return (n / 1e8).toFixed(a >= 1e9 ? 0 : 1) + ' 亿';
  if (a >= 1e4) return (n / 1e4).toFixed(a >= 1e5 ? 0 : 1) + ' 万';
  return nf.format(Math.round(n));
}

export function fmtRate(perSec) {
  if (!perSec) return '';
  if (perSec >= 1e4) return (perSec / 1e4).toFixed(1) + ' 万/秒';
  return nf.format(Math.round(perSec)) + '/秒';
}

export function fmtDuration(ms) {
  if (ms == null || ms < 0) return '—';
  if (ms < 1000) return `${ms} ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(s < 10 ? 1 : 0)} 秒`;
  const m = Math.floor(s / 60);
  const rs = Math.round(s % 60);
  if (m < 60) return `${m} 分 ${rs} 秒`;
  const h = Math.floor(m / 60);
  return `${h} 小时 ${m % 60} 分`;
}

const pad = (n) => String(n).padStart(2, '0');
export function fmtDateTime(ms, withSec) {
  if (!ms) return '—';
  const d = new Date(ms);
  const base = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
  return withSec ? `${base}:${pad(d.getSeconds())}` : base;
}

export function fmtClock(ms) {
  if (!ms) return '';
  const d = new Date(ms);
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}.${String(d.getMilliseconds()).padStart(3, '0')}`;
}

export function fmtAgo(ms) {
  if (!ms) return '—';
  const diff = Date.now() - ms;
  const future = diff < 0;
  const a = Math.abs(diff) / 1000;
  let t;
  if (a < 45) t = '刚刚';
  else if (a < 3600) t = `${Math.round(a / 60)} 分钟`;
  else if (a < 86400) t = `${Math.round(a / 3600)} 小时`;
  else if (a < 86400 * 30) t = `${Math.round(a / 86400)} 天`;
  else return fmtDateTime(ms);
  if (t === '刚刚') return future ? '即将' : t;
  return future ? `${t}后` : `${t}前`;
}

export function runDuration(run) {
  if (!run || !run.startedAt) return null;
  return (run.finishedAt || Date.now()) - run.startedAt;
}

// 类型化变量值：前端统一用 { kind, value } 形态，与后端 core.Value JSON 对齐。
export function makeValue(kind, text) {
  if (text === '' || text == null) return { kind, value: null };
  if (kind === 'integer' || kind === 'decimal' || kind === 'number') return { kind, value: String(text) };
  if (kind === 'boolean') return { kind, value: text === true || text === 'true' };
  return { kind, value: String(text) };
}

export function uid(prefix) {
  return `${prefix}${Date.now().toString(36).slice(-4)}${Math.random().toString(36).slice(2, 6)}`;
}

export const clone = (v) => JSON.parse(JSON.stringify(v));

// useCatalog：组件目录（步骤、作业项、函数、方言、类型）
export function useCatalog() {
  const [cat, setCat] = React.useState(null);
  const [error, setError] = React.useState(null);
  React.useEffect(() => {
    let alive = true;
    loadCatalog().then((c) => { if (alive) setCat(indexCatalog(c)); }).catch((e) => { if (alive) setError(e); });
    return () => { alive = false; };
  }, []);
  return { catalog: cat, error };
}

function indexCatalog(c) {
  const byId = {};
  (c.steps || []).forEach((m) => { byId[`pipeline:${m.id}`] = m; });
  (c.actions || []).forEach((m) => { byId[`workflow:${m.id}`] = m; });
  return { ...c, meta: (kind, type) => byId[`${kind}:${type}`] };
}

// 关键字过滤（名称、ID、Kettle 原名）
export function matches(meta, kw) {
  if (!kw) return true;
  const k = kw.trim().toLowerCase();
  return [meta.label, meta.id, meta.description, ...(meta.keywords || [])].some((s) => String(s || '').toLowerCase().includes(k));
}
