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

// 返回当前登录用户名,未登录返回 null
async function getSession() {
  try {
    const r = await fetch('/api/session', { credentials: 'same-origin' });
    if (!r.ok) return null;
    const d = await r.json();
    return d.user || null;
  } catch (e) {
    return null;
  }
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

// 真实部署:multipart 上传 config(JSON)+ artifact(File)到 Agent(经 Console 代理)。
// 返回 {result, version, steps} 或 {error}。
async function deployViaAgent(appId, config, file) {
  try {
    const fd = new FormData();
    fd.append('config', JSON.stringify(config));
    fd.append('artifact', file);
    const r = await fetch(`/api/agent/apps/${encodeURIComponent(appId)}/deploy`, {
      method: 'POST', body: fd, credentials: 'same-origin',
    });
    const d = await r.json().catch(() => ({}));
    if (!r.ok) return { error: d.error || `部署失败 (${r.status})` };
    return d;
  } catch (e) {
    return { error: 'Agent 不可达: ' + (e.message || e) };
  }
}

// 真实部署(SSE 实时流):multipart 上传后,Agent 每完成一步推送 step 事件,结束推送 done。
// onEvent(type, data) 在每个事件到达时回调(type ∈ "step"|"done");返回最终结果 {result,version,steps} 或 {error}。
async function deployViaAgentStream(appId, config, file, onEvent) {
  try {
    const fd = new FormData();
    fd.append('config', JSON.stringify(config));
    fd.append('artifact', file);
    const r = await fetch(`/api/agent/apps/${encodeURIComponent(appId)}/deploy/stream`, {
      method: 'POST', body: fd, credentials: 'same-origin',
    });
    // 非 SSE(出错)→ 当作 JSON 错误处理
    if (!r.ok || !(r.headers.get('Content-Type') || '').includes('text/event-stream')) {
      const d = await r.json().catch(() => ({}));
      return { error: d.error || `部署失败 (${r.status})` };
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
    return done || { error: '部署流中断,未收到结果' };
  } catch (e) {
    return { error: 'Agent 不可达: ' + (e.message || e) };
  }
}

export {
  login, logout, getSession,
  getAgentCapabilities, getAgentSystem, getAgentPing,
  hydrateData, putEntity, deleteEntity, deployViaAgent, deployViaAgentStream,
};
