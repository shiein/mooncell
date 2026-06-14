// Mooncell — 后端 API 客户端
// 登录与 Agent 数据走真实后端(Go + SQLite);其余业务页仍为静态 mock。
// 会话基于 httpOnly cookie(mc_sid),前端拿不到也无需拿到 token。

async function login(username, password) {
  const r = await fetch('/api/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
    credentials: 'same-origin',
  });
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || '登录失败');
  return data; // { user }
}

async function logout() {
  try {
    await fetch('/api/logout', { method: 'POST', credentials: 'same-origin' });
  } catch (e) { /* 忽略网络错误,前端无论如何清空会话 */ }
}

// 返回 { user, role },未登录返回 null
async function getSession() {
  try {
    const r = await fetch('/api/session', { credentials: 'same-origin' });
    if (!r.ok) return null;
    const d = await r.json();
    return d.user ? { user: d.user, role: d.role || 'viewer' } : null;
  } catch (e) {
    return null;
  }
}

// ---------- 用户管理(仅 admin)----------
async function listUsers() {
  try {
    const r = await fetch('/api/users', { credentials: 'same-origin' });
    if (!r.ok) return null;
    return (await r.json()).users || [];
  } catch (e) { return null; }
}

async function createUser(payload) {
  const r = await fetch('/api/users', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload), credentials: 'same-origin',
  });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || '创建失败');
  return d;
}

async function deleteUser(username) {
  const r = await fetch(`/api/users/${encodeURIComponent(username)}`, {
    method: 'DELETE', credentials: 'same-origin',
  });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || '删除失败');
  return d;
}

// ---------- 多 Agent 管理 ----------
// qa 给路径追加 ?agent=<id>(default/空不加,走配置内置 Agent)。
function qa(path, agentId) {
  return agentId && agentId !== 'default' ? `${path}?agent=${encodeURIComponent(agentId)}` : path;
}

async function listAgentNodes() {
  try {
    const r = await fetch('/api/agents', { credentials: 'same-origin' });
    if (!r.ok) return null;
    return (await r.json()).agents || [];
  } catch (e) { return null; }
}

async function addAgentNode(payload) {
  const r = await fetch('/api/agents', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload), credentials: 'same-origin',
  });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || '注册失败');
  return d;
}

async function removeAgentNode(id) {
  const r = await fetch(`/api/agents/${encodeURIComponent(id)}`, { method: 'DELETE', credentials: 'same-origin' });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || '删除失败');
  return d;
}

async function pingAgentNode(id) {
  try {
    const r = await fetch(`/api/agents/${encodeURIComponent(id)}/ping`, { credentials: 'same-origin' });
    return r.ok ? await r.json() : null;
  } catch (e) { return null; }
}

// ---------- 文件柜(真实二进制存储)----------
async function uploadCabinetFile(file, anon) {
  const fd = new FormData();
  fd.append('file', file);
  if (anon) fd.append('anon', 'true');
  const r = await fetch('/api/cabinet', { method: 'POST', body: fd, credentials: 'same-origin' });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || '上传失败');
  return d; // 后端落库后的条目元数据
}

async function removeCabinetFile(id) {
  const r = await fetch(`/api/cabinet/${encodeURIComponent(id)}`, { method: 'DELETE', credentials: 'same-origin' });
  if (!r.ok) { const d = await r.json().catch(() => ({})); throw new Error(d.error || '删除失败'); }
}

// ---------- Agent(经 Console 代理)----------
// 任一失败都返回 null,调用方据此回退到 mock 并把 Agent 显示为离线。
async function agentGet(path) {
  try {
    const r = await fetch(path, { credentials: 'same-origin' });
    if (!r.ok) return null;
    return await r.json();
  } catch (e) {
    return null;
  }
}

const getAgentCapabilities = () => agentGet('/api/agent/capabilities');
const getAgentSystem = () => agentGet('/api/agent/system');
const getAgentPing = () => agentGet('/api/agent/ping');

// ---------- 业务数据持久化(SQLite 文档存储)----------
// hydrateData:首启用 seed 种子初始化,始终返回库中当前全部数据;失败返回 null(前端回退 mock)。
async function hydrateData(seed) {
  try {
    const r = await fetch('/api/data', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(seed),
      credentials: 'same-origin',
    });
    if (!r.ok) return null;
    return await r.json(); // { apps, releases, backups, cabinet, audit }
  } catch (e) {
    return null;
  }
}

// 镜像写:乐观更新已在前端完成,这里把结果落库(失败仅告警,不打断 UI)。
async function putEntity(kind, obj) {
  try {
    await fetch(`/api/data/${kind}/${encodeURIComponent(obj.id)}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(obj),
      credentials: 'same-origin',
    });
  } catch (e) { console.error('[persist] put', kind, e); }
}

async function deleteEntity(kind, id) {
  try {
    await fetch(`/api/data/${kind}/${encodeURIComponent(id)}`, {
      method: 'DELETE',
      credentials: 'same-origin',
    });
  } catch (e) { console.error('[persist] delete', kind, e); }
}

// consumeSSE 消费一个 text/event-stream 响应:按 \n\n 分帧,解析 event/data,
// 每帧回调 onEvent(type, data);返回最终 done 事件数据。部署与还原共用。
async function consumeSSE(r, onEvent, errLabel) {
  if (!r.ok || !(r.headers.get('Content-Type') || '').includes('text/event-stream')) {
    const d = await r.json().catch(() => ({}));
    return { error: d.error || `${errLabel} (${r.status})` };
  }
  const reader = r.body.getReader();
  const decoder = new TextDecoder();
  let buf = '';
  let done = null;
  for (;;) {
    const { value, done: finished } = await reader.read();
    if (finished) break;
    buf += decoder.decode(value, { stream: true });
    let sep;
    while ((sep = buf.indexOf('\n\n')) >= 0) {
      const frame = buf.slice(0, sep);
      buf = buf.slice(sep + 2);
      let ev = 'message', data = '';
      for (const line of frame.split('\n')) {
        if (line.startsWith('event:')) ev = line.slice(6).trim();
        else if (line.startsWith('data:')) data += line.slice(5).trim();
      }
      if (!data) continue;
      let parsed;
      try { parsed = JSON.parse(data); } catch { continue; }
      onEvent && onEvent(ev, parsed);
      if (ev === 'done') done = parsed;
    }
  }
  return done || { error: '流中断,未收到结果' };
}

// 真实部署(SSE 实时流):前端只提交 制品 + version + releaseId;Agent 配置由 Console 据已存应用配置
// 服务端生成(前端不再组装,杜绝配置注入),目标 Agent 也据应用 agentId 服务端路由。
// releaseId 提供幂等(同 id 已成功不重复部署)。onEvent(type,data) 回调;返回 {result,version,steps} 或 {error}。
// sha256Hex 用 Web Crypto 计算文件 sha256(供 Agent 端强校验制品完整性)。
// 非安全上下文(http 非 localhost)下 crypto.subtle 不可用时返回空,Agent 端跳过校验。
async function sha256Hex(file) {
  try {
    if (!(window.crypto && crypto.subtle)) return '';
    const buf = await file.arrayBuffer();
    const hash = await crypto.subtle.digest('SHA-256', buf);
    return [...new Uint8Array(hash)].map((b) => b.toString(16).padStart(2, '0')).join('');
  } catch (e) { return ''; }
}

async function deployViaAgentStream(appId, version, releaseId, file, onEvent) {
  try {
    const fd = new FormData();
    fd.append('version', version || '');
    fd.append('releaseId', releaseId || '');
    fd.append('sha256', await sha256Hex(file));
    fd.append('artifact', file);
    const r = await fetch(`/api/agent/apps/${encodeURIComponent(appId)}/deploy/stream`, {
      method: 'POST', body: fd, credentials: 'same-origin',
    });
    return await consumeSSE(r, onEvent, '部署失败');
  } catch (e) {
    return { error: 'Agent 不可达: ' + (e.message || e) };
  }
}

// 列出某应用在 Agent 上的真实历史备份(新→旧);失败返回 null,调用方回退 mock。
async function listAgentBackups(appId, agentId) {
  try {
    const r = await fetch(qa(`/api/agent/apps/${encodeURIComponent(appId)}/backups`, agentId), { credentials: 'same-origin' });
    if (!r.ok) return null;
    const d = await r.json();
    return Array.isArray(d.backups) ? d.backups : null;
  } catch (e) {
    return null;
  }
}

// 真实还原(SSE 实时流):前端只提交 backup(时间戳目录名)+ version + releaseId;
// Agent 配置由 Console 服务端据已存应用配置生成。onEvent 回调;返回 {result,version,steps} 或 {error}。
async function restoreViaAgentStream(appId, version, backup, releaseId, onEvent) {
  try {
    const r = await fetch(`/api/agent/apps/${encodeURIComponent(appId)}/restore/stream`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ backup, version, releaseId }),
      credentials: 'same-origin',
    });
    return await consumeSSE(r, onEvent, '还原失败');
  } catch (e) {
    return { error: 'Agent 不可达: ' + (e.message || e) };
  }
}

// 订阅应用运行时日志(Agent journal SSE):先收最近 tail 行再实时跟随。
// onLine({ts,level,text}) 每行回调;signal 用于暂停/离开时中断。
// 返回 {ended}|{aborted}|{error}——error 时调用方回退到模拟日志。
async function streamAppLogs(appId, { tail = 200, signal, onLine, agentId, runner }) {
  let r;
  try {
    let url = `/api/agent/apps/${encodeURIComponent(appId)}/logs/stream?tail=${tail}`;
    if (agentId && agentId !== 'default') url += `&agent=${encodeURIComponent(agentId)}`;
    if (runner === 'pm2') url += `&runner=pm2`;
    r = await fetch(url, { credentials: 'same-origin', signal });
  } catch (e) {
    return e.name === 'AbortError' ? { aborted: true } : { error: 'Agent 不可达: ' + (e.message || e) };
  }
  if (!r.ok || !(r.headers.get('Content-Type') || '').includes('text/event-stream')) {
    const d = await r.json().catch(() => ({}));
    return { error: d.error || `日志流不可用 (${r.status})` };
  }
  const reader = r.body.getReader();
  const decoder = new TextDecoder();
  let buf = '';
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let sep;
      while ((sep = buf.indexOf('\n\n')) >= 0) {
        const frame = buf.slice(0, sep);
        buf = buf.slice(sep + 2);
        let ev = 'message', data = '';
        for (const line of frame.split('\n')) {
          if (line.startsWith('event:')) ev = line.slice(6).trim();
          else if (line.startsWith('data:')) data += line.slice(5).trim();
        }
        if (ev === 'line' && data) {
          try { onLine && onLine(JSON.parse(data)); } catch { /* 跳过坏帧 */ }
        }
      }
    }
  } catch (e) {
    return e.name === 'AbortError' ? { aborted: true } : { error: e.message || String(e) };
  }
  return { ended: true };
}

export {
  login, logout, getSession,
  listUsers, createUser, deleteUser,
  listAgentNodes, addAgentNode, removeAgentNode, pingAgentNode,
  uploadCabinetFile, removeCabinetFile,
  getAgentCapabilities, getAgentSystem, getAgentPing,
  hydrateData, putEntity, deleteEntity, deployViaAgentStream,
  listAgentBackups, restoreViaAgentStream, streamAppLogs,
};
