// 数据集成模块 API 客户端（/api/etl/*）。401 由全局 fetch 包装统一回登录页。

async function req(method, url, body) {
  const opts = { method, credentials: 'same-origin', headers: {} };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const r = await fetch(url, opts);
  const d = await r.json().catch(() => ({}));
  if (!r.ok) {
    const err = new Error(d.error || `HTTP ${r.status}`);
    err.status = r.status;
    err.code = d.code;
    err.body = d;
    throw err;
  }
  return d;
}

const q = (params) => {
  const s = new URLSearchParams();
  Object.entries(params || {}).forEach(([k, v]) => { if (v !== undefined && v !== null && v !== '') s.set(k, v); });
  const str = s.toString();
  return str ? `?${str}` : '';
};
const enc = encodeURIComponent;

export const etlApi = {
  overview: () => req('GET', '/api/etl/overview'),
  catalog: () => req('GET', '/api/etl/catalog'),

  connections: () => req('GET', '/api/etl/connections').then((d) => d.connections || []),
  saveConnection: (c) => (c.id ? req('PUT', `/api/etl/connections/${enc(c.id)}`, c) : req('POST', '/api/etl/connections', c)),
  deleteConnection: (id, force) => req('DELETE', `/api/etl/connections/${enc(id)}${force ? '?force=1' : ''}`),
  testConnection: (id) => req('POST', `/api/etl/connections/${enc(id)}/test`),
  testDraft: (c) => req('POST', '/api/etl/connections/test', c),
  schemas: (id) => req('GET', `/api/etl/connections/${enc(id)}/schemas`),
  tables: (id, schema) => req('GET', `/api/etl/connections/${enc(id)}/tables${q({ schema })}`),
  columns: (id, schema, table) => req('GET', `/api/etl/connections/${enc(id)}/columns${q({ schema, table })}`),

  definitions: (kind) => req('GET', `/api/etl/definitions${q({ kind })}`).then((d) => d.definitions || []),
  definition: (id) => req('GET', `/api/etl/definitions/${enc(id)}`),
  createDefinition: (d) => req('POST', '/api/etl/definitions', d),
  updateDefinition: (id, d) => req('PUT', `/api/etl/definitions/${enc(id)}`, d),
  deleteDefinition: (id, force) => req('DELETE', `/api/etl/definitions/${enc(id)}${force ? '?force=1' : ''}`),
  duplicate: (id) => req('POST', `/api/etl/definitions/${enc(id)}/duplicate`),
  versions: (id) => req('GET', `/api/etl/definitions/${enc(id)}/versions`).then((d) => d.versions || []),
  version: (id, rev) => req('GET', `/api/etl/definitions/${enc(id)}/versions/${rev}`),
  restoreVersion: (id, rev) => req('POST', `/api/etl/definitions/${enc(id)}/versions/${rev}/restore`),
  validate: (kind, doc) => req('POST', '/api/etl/definitions/validate', { kind, doc }),
  preview: (doc, nodeId, limit, params) => req('POST', '/api/etl/pipelines/preview', { doc, nodeId, limit, params }),
  run: (id, params) => req('POST', `/api/etl/definitions/${enc(id)}/run`, { params: params || {} }),

  runs: (filter) => req('GET', `/api/etl/runs${q(filter)}`).then((d) => d.runs || []),
  runDetail: (id) => req('GET', `/api/etl/runs/${enc(id)}`),
  runLogs: (id, opts) => req('GET', `/api/etl/runs/${enc(id)}/logs${q(opts)}`).then((d) => d.logs || []),
  cancelRun: (id) => req('POST', `/api/etl/runs/${enc(id)}/cancel`),
  resumeRun: (id) => req('POST', `/api/etl/runs/${enc(id)}/resume`),

  schedules: (targetId) => req('GET', `/api/etl/schedules${q({ targetId })}`).then((d) => d.schedules || []),
  saveSchedule: (s) => (s.id ? req('PUT', `/api/etl/schedules/${enc(s.id)}`, s) : req('POST', '/api/etl/schedules', s)),
  deleteSchedule: (id) => req('DELETE', `/api/etl/schedules/${enc(id)}`),
  previewCron: (cron, tz) => req('GET', `/api/etl/schedules/preview${q({ cron, tz })}`),
};

// subscribeRun 订阅运行事件（SSE）。返回取消函数。handlers: { status, node, metrics, log, end }
export function subscribeRun(runId, handlers) {
  const es = new EventSource(`/api/etl/runs/${enc(runId)}/events`, { withCredentials: true });
  ['status', 'node', 'metrics', 'log'].forEach((t) => {
    es.addEventListener(t, (e) => {
      try { handlers[t] && handlers[t](JSON.parse(e.data)); } catch (err) { /* 忽略坏帧 */ }
    });
  });
  es.addEventListener('end', () => { es.close(); handlers.end && handlers.end(); });
  es.onerror = () => {
    // 连接断开（如 Console 重启）：关闭并交给调用方用 REST 兜底刷新
    if (es.readyState === EventSource.CLOSED) handlers.end && handlers.end();
  };
  return () => es.close();
}

// 目录只需加载一次，模块内共享。
let catalogPromise = null;
export function loadCatalog(force) {
  if (!catalogPromise || force) {
    catalogPromise = etlApi.catalog().catch((e) => { catalogPromise = null; throw e; });
  }
  return catalogPromise;
}
