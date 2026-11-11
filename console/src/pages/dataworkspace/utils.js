// 数据资源工作台：纯函数与本地存储工具（无 React 依赖）。

/** 公式注入防护：与后端 csvFormulaSafe 保持一致。 */
export function csvFormulaSafe(s) {
  if (s == null || s === '') return '';
  const str = String(s);
  const c = str[0];
  if (['=', '@', '\t', '\r', '\n', '＝', '＋', '－', '＠'].includes(c)) return `'${str}`;
  if (c === '+' || c === '-') {
    if (Number.isFinite(Number(str))) return str;
    return `'${str}`;
  }
  return str;
}

export function cellIsBinary(v) {
  return v != null && typeof v === 'object' && v.type === 'binary';
}

/** 单元格 → 纯文本（复制/导出/详情） */
export function cellText(v) {
  if (v == null) return '';
  if (cellIsBinary(v)) return v.size != null ? `<binary ${v.size} bytes>` : '<binary>';
  return String(v);
}

/** 当前结果快照 → CSV Blob（客户端生成） */
export function buildSnapshotCSVBlob(columns, rows) {
  const esc = (v) => {
    const s = csvFormulaSafe(cellText(v));
    if (/[",\n\r]/.test(s)) return `"${s.replace(/"/g, '""')}"`;
    return s;
  };
  const lines = [columns.map(esc).join(',')];
  for (const row of rows || []) lines.push(columns.map((_, i) => esc(row[i])).join(','));
  return new Blob([new Uint8Array([0xEF, 0xBB, 0xBF]), `${lines.join('\n')}\n`], { type: 'text/csv;charset=utf-8' });
}

/** TSV（粘贴到 Excel/表格软件） */
export function toTSV(columns, rows, withHeader = true) {
  const esc = (v) => cellText(v).replace(/\t/g, ' ').replace(/\r?\n/g, ' ');
  const lines = [];
  if (withHeader) lines.push(columns.map(esc).join('\t'));
  for (const row of rows) lines.push(columns.map((_, i) => esc(row[i])).join('\t'));
  return lines.join('\n');
}

/** 行 → INSERT 语句（引用符按方言） */
export function toInsertSQL(table, columns, rows, dbType) {
  const q = (name) => (dbType === 'mysql' ? `\`${String(name).replace(/`/g, '``')}\`` : `"${String(name).replace(/"/g, '""')}"`);
  const lit = (v) => {
    if (v == null) return 'NULL';
    if (cellIsBinary(v)) return 'NULL /* binary */';
    if (typeof v === 'number' || typeof v === 'boolean') return String(v);
    return `'${String(v).replace(/'/g, "''")}'`;
  };
  const cols = columns.map(q).join(', ');
  return rows.map((row) => `INSERT INTO ${table} (${cols}) VALUES (${columns.map((_, i) => lit(row[i])).join(', ')});`).join('\n');
}

export function downloadBlob(blob, filename) {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement('a');
  anchor.href = url;
  anchor.download = filename;
  anchor.click();
  setTimeout(() => URL.revokeObjectURL(url), 0);
}

/** 资源名 + 时间戳的导出文件名（去掉文件系统不安全字符） */
export function exportFileName(resourceName, ext) {
  const safe = String(resourceName || 'export').replace(/[\\/:*?"<>|\s]+/g, '_').slice(0, 40);
  const d = new Date();
  const p = (n) => String(n).padStart(2, '0');
  return `${safe}_${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}_${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}.${ext}`;
}

export async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch (e) {
    // 非安全上下文（http 内网）下 clipboard API 不可用：退回 execCommand
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand('copy'); } catch (err) { ok = false; }
    ta.remove();
    return ok;
  }
}

/** 将后端稳定错误码转为可操作的提示。 */
export function friendlyWorkspaceError(e) {
  const code = e && e.code;
  const fallback = (e && e.message) || '执行失败';
  switch (code) {
    case 'WORKSPACE_CLOSED': return e.message || '工作台已关闭或失效，请重新打开资源';
    case 'RESOURCE_BUSY':
    case 'TX_ACTIVE':
    case 'IMPORT_ACTIVE': return e.message || '资源正被占用（事务/导入/配置变更），请稍后再试';
    case 'CONFIG_CHANGED': return e.message || '资源配置已变更，请刷新后重试';
    case 'AUTO_COMMIT_REQUIRED': return e.message || '请先开启自动提交';
    case 'DATA_RESOURCE_READ_ONLY': return e.message || '当前为只读授权，无法执行写操作';
    case 'QUERY_CANCELED': return e.message || '查询已取消或超时';
    case 'DUPLICATE_COLUMN': return e.message || '查询存在重复列名，请为列加别名';
    case 'REQUEST_BODY_TOO_LARGE': return e.message || '请求体过大，请缩小结果集或改用全量导出';
    default: return fallback;
  }
}

/** 事务状态中文 */
export function txLabel(state) {
  if (state === 'active') return '事务进行中';
  if (state === 'failed') return '事务失败，需回滚';
  return '无事务';
}

/** 1 起的字符序号 → { line, col }（基于语句文本） */
export function positionToLineCol(text, pos) {
  if (!pos || pos < 1) return null;
  const chars = Array.from(text);
  const before = chars.slice(0, pos - 1).join('');
  const lines = before.split('\n');
  return { line: lines.length, col: lines[lines.length - 1].length + 1, offset: before.length };
}

// ---- 本地存储（每用户 × 资源）；任何读写失败都退化为内存态 ----

function storageKey(kind, user, resourceId) {
  return `mc_dr_${kind}:${user || '-'}:${resourceId}`;
}

export function loadJSON(kind, user, resourceId, fallback) {
  try {
    const raw = localStorage.getItem(storageKey(kind, user, resourceId));
    if (!raw) return fallback;
    const v = JSON.parse(raw);
    return v == null ? fallback : v;
  } catch (e) {
    return fallback;
  }
}

export function saveJSON(kind, user, resourceId, value) {
  try {
    localStorage.setItem(storageKey(kind, user, resourceId), JSON.stringify(value));
  } catch (e) { /* 配额满或隐私模式：忽略 */ }
}

export const HISTORY_MAX = 200;
export const HISTORY_SQL_MAX = 20000; // 单条历史 SQL 最多保存的字符数

export function newTabId() {
  return `t${Date.now().toString(36)}${Math.random().toString(36).slice(2, 6)}`;
}
