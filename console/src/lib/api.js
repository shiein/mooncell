// Mooncell — 后端 API 客户端
// 登录与 Agent 数据走真实后端(Go + SQLite);其余业务页仍为静态 mock。
// 会话基于 httpOnly cookie(mc_sid),前端拿不到也无需拿到 token。

// 统一会话失效处理:任何请求拿到 401 → 通知 App 回登录页。
// 否则闲置过期后,后台轮询/操作只会局部报错,用户仍停在原页面,不会被引导重新登录。
let onUnauthorized = null;
function setUnauthorizedHandler(fn) { onUnauthorized = fn; }
const _rawFetch = window.fetch.bind(window);
window.fetch = async (...args) => {
  const r = await _rawFetch(...args);
  if (r && r.status === 401 && onUnauthorized) onUnauthorized();
  return r;
};

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

// ---------- Agent 自更新(按架构上传升级包 → 推送匹配的包到各 Agent)----------
async function listAgentBinaries() {
  try {
    const r = await fetch('/api/agent-binaries', { credentials: 'same-origin' });
    if (!r.ok) return [];
    return (await r.json()).binaries || [];
  } catch (e) { return []; }
}

async function uploadAgentBinary(file, arch, version) {
  const fd = new FormData();
  fd.append('binary', file); fd.append('arch', arch); fd.append('version', version);
  const r = await fetch('/api/agent-binary', { method: 'POST', body: fd, credentials: 'same-origin' });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || '上传失败');
  return d;
}

async function updateAgentNode(id) {
  const r = await fetch(`/api/agents/${encodeURIComponent(id)}/update`, { method: 'POST', credentials: 'same-origin' });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || '更新失败');
  return d;
}

// ---------- 文件柜(真实二进制存储)----------
async function uploadCabinetFile(file, anon) {
  const fd = new FormData();
  fd.append('file', file);
  // 匿名走免登录公开端点(需 cabinet.anon_upload=true,否则后端 403);否则走登录端点。
  const r = await fetch(anon ? '/api/pub/cabinet' : '/api/cabinet', { method: 'POST', body: fd, credentials: 'same-origin' });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || '上传失败');
  return d; // 后端落库后的条目元数据
}

async function removeCabinetFile(id) {
  const r = await fetch(`/api/cabinet/${encodeURIComponent(id)}`, { method: 'DELETE', credentials: 'same-origin' });
  if (!r.ok) { const d = await r.json().catch(() => ({})); throw new Error(d.error || '删除失败'); }
}

// 公开:文件柜上限(MB)+ 匿名开关;用于上传前客户端大小预检。失败返回 null。
async function getPubLimits() {
  try {
    const r = await fetch('/api/pub/limits', { credentials: 'same-origin' });
    if (!r.ok) return null;
    return await r.json();
  } catch (e) { return null; }
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

// 查应用真实运行态(systemd/pm2):{active,state,pid};失败返回 null。
async function getAppStatus(appId) {
  try {
    const r = await fetch(`/api/agent/apps/${encodeURIComponent(appId)}/status`, { credentials: 'same-origin' });
    if (!r.ok) return null;
    return await r.json();
  } catch (e) { return null; }
}

// 真机启停已托管进程(systemd/pm2):action=start|stop;返回启停后真实状态 {active,state,pid},失败返回 null。
async function setAppLifecycle(appId, action) {
  try {
    const r = await fetch(`/api/agent/apps/${encodeURIComponent(appId)}/lifecycle?action=${action}`, {
      method: 'POST', credentials: 'same-origin',
    });
    if (!r.ok) return null;
    return await r.json();
  } catch (e) { return null; }
}

// 新建应用前真实预检(路径可写/端口空闲/运行时可用);失败返回 null。
async function precheckApp(query) {
  try {
    const r = await fetch('/api/agent/precheck?' + query, { credentials: 'same-origin' });
    if (!r.ok) return null;
    return await r.json();
  } catch (e) { return null; }
}

// 拉某 Agent 的能力清单(?agent 路由到指定 Agent;空/default 用内置)。失败返回 null。
const getAgentCapabilities = (agentId) => agentGet(qa('/api/agent/capabilities', agentId));
const getAgentSystem = (agentId) => agentGet(qa('/api/agent/system', agentId));
const getAgentPing = (agentId) => agentGet(qa('/api/agent/ping', agentId));

// 拉某 Agent 近 minutes 分钟的资源水位历史(巡检留存),用于总览曲线初始化;失败/无数据返回 null。
async function getAgentMetrics(agentId, minutes = 60) {
  try {
    const r = await fetch(`/api/agents/${encodeURIComponent(agentId || 'default')}/metrics?minutes=${minutes}`, { credentials: 'same-origin' });
    if (!r.ok) return null;
    return (await r.json()).points || [];
  } catch (e) { return null; }
}

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

// 审计倒序分页:hydrate 只带最近一窗(auditHydrateLimit),更早记录经此端点「加载更多」。
// 返回 { items, total };后端不可达返回 null(调用方回退到 hydrate 已加载的 store.audit)。
async function listAuditPage(offset, limit) {
  try {
    const r = await fetch(`/api/audit?offset=${offset}&limit=${limit}`, { credentials: 'same-origin' });
    if (!r.ok) return null;
    return await r.json();
  } catch (e) { return null; }
}

// 镜像写:乐观更新已在前端完成,这里把结果落库(失败仅告警,不打断 UI)。
// 注:kind=app 不走此通用入口(后端 403);应用配置改用 saveAppConfig(带服务端校验)。
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

// 应用配置类型化写入:经服务端校验(类型/Runner/路径/范围/agentId)后落库。
// 返回 {ok} 或 {error};校验失败返回 error 供调用方按需提示。
async function saveAppConfig(app) {
  try {
    const r = await fetch(`/api/apps/${encodeURIComponent(app.id)}/config`, {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(app), credentials: 'same-origin',
    });
    if (!r.ok) { const d = await r.json().catch(() => ({})); return { error: d.error || ('保存失败 (' + r.status + ')') }; }
    return { ok: true };
  } catch (e) { return { error: 'Console 不可达: ' + (e.message || e) }; }
}

async function deleteEntity(kind, id) {
  try {
    await fetch(`/api/data/${kind}/${encodeURIComponent(id)}`, {
      method: 'DELETE',
      credentials: 'same-origin',
    });
  } catch (e) { console.error('[persist] delete', kind, e); }
}

// appDelete:服务端权威删除应用(DELETE /api/apps/{id})——后端先经 Agent 下线(停服 +
// 清理 unit/pm2/nohup),成功后才删元数据并审计。不能走通用 /api/data 删除(后端禁止删 app)。
async function appDelete(appId) {
  const r = await fetch(`/api/apps/${encodeURIComponent(appId)}`, { method: 'DELETE', credentials: 'same-origin' });
  if (!r.ok) { const d = await r.json().catch(() => ({})); throw new Error(d.error || ('删除失败 (' + r.status + ')')); }
  return r.json().catch(() => ({}));
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
// 制品 sha256 由 Console 服务端权威计算并下发给 Agent 强校验(保证 Console→Agent 完整性),
// 前端无需计算。
const CHUNK_SIZE = 8 * 1024 * 1024;        // 8MB 分块
const CHUNK_THRESHOLD = 16 * 1024 * 1024;  // 超过 16MB 走分块上传 + 断点续传
const CHUNK_RETRY = 3;                      // 单块失败重试次数(断点续传)

// uploadChunked 把大文件分块顺序传到 Console,失败按 nextIndex 续传;返回 uploadId(失败抛错)。
// onProgress(sent,total) 用于进度展示。
async function uploadChunked(file, onProgress) {
  const startR = await fetch('/api/upload/start', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ filename: file.name, size: file.size }), credentials: 'same-origin',
  });
  if (!startR.ok) { const d = await startR.json().catch(() => ({})); throw new Error(d.error || '上传初始化失败'); }
  const { uploadId } = await startR.json();
  const total = Math.ceil(file.size / CHUNK_SIZE);
  let index = 0;
  while (index < total) {
    const blob = file.slice(index * CHUNK_SIZE, Math.min((index + 1) * CHUNK_SIZE, file.size));
    let ok = false, lastErr = '';
    for (let attempt = 0; attempt < CHUNK_RETRY && !ok; attempt++) {
      try {
        const r = await fetch(`/api/upload/${uploadId}?index=${index}`, { method: 'PUT', body: blob, credentials: 'same-origin' });
        const d = await r.json().catch(() => ({}));
        if (r.ok) { ok = true; if (typeof d.nextIndex === 'number') index = d.nextIndex; }
        else if (r.status === 409 && typeof d.nextIndex === 'number') { index = d.nextIndex; ok = true; } // 续传:对齐服务端进度
        else lastErr = d.error || ('HTTP ' + r.status);
      } catch (e) { lastErr = e.message || String(e); }
    }
    if (!ok) throw new Error('分块上传失败(已重试): ' + lastErr);
    onProgress && onProgress(Math.min(index * CHUNK_SIZE, file.size), file.size);
  }
  return uploadId;
}

async function deployViaAgentStream(appId, version, releaseId, file, onEvent, onUpload) {
  try {
    const fd = new FormData();
    fd.append('version', version || '');
    fd.append('releaseId', releaseId || '');
    // 大制品:先分块上传(断点续传)到 Console,再用 uploadId 触发部署;小制品直接随表单上传。
    if (file && file.size > CHUNK_THRESHOLD) {
      const uploadId = await uploadChunked(file, onUpload);
      fd.append('uploadId', uploadId);
    } else {
      fd.append('artifact', file);
    }
    const r = await fetch(`/api/agent/apps/${encodeURIComponent(appId)}/deploy/stream`, {
      method: 'POST', body: fd, credentials: 'same-origin',
    });
    return await consumeSSE(r, onEvent, '部署失败');
  } catch (e) {
    return { error: '上传/部署失败: ' + (e.message || e) };
  }
}

// 列出某应用在 Agent 上的真实历史备份(新→旧);失败返回 null,调用方必须显式处理失败态。
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
// 返回 {ended}|{aborted}|{error}——error 时真实应用显示错误态+重试(不再回退模拟日志);仅演示类型用模拟。
async function streamAppLogs(appId, { tail = 200, signal, onLine, agentId, runner, path }) {
  let r;
  try {
    // path 指定时跟随该声明日志文件(Console 校验属于本应用 logPaths);否则跟随进程 journal/pm2。
    let url;
    if (path) {
      url = `/api/agent/apps/${encodeURIComponent(appId)}/logs/file/stream?path=${encodeURIComponent(path)}&tail=${tail}`;
    } else {
      url = `/api/agent/apps/${encodeURIComponent(appId)}/logs/stream?tail=${tail}`;
      if (runner === 'pm2') url += `&runner=pm2`;
    }
    if (agentId && agentId !== 'default') url += `&agent=${encodeURIComponent(agentId)}`;
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
  listAgentBinaries, uploadAgentBinary, updateAgentNode,
  uploadCabinetFile, removeCabinetFile, getPubLimits,
  getAgentCapabilities, getAgentSystem, getAgentPing, getAgentMetrics, precheckApp, getAppStatus, setAppLifecycle,
  hydrateData, listAuditPage, putEntity, saveAppConfig, deleteEntity, appDelete, setUnauthorizedHandler, deployViaAgentStream,
  listAgentBackups, restoreViaAgentStream, streamAppLogs,
};
