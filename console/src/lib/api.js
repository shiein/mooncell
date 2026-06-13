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

export { login, logout, getSession, getAgentCapabilities, getAgentSystem, getAgentPing };
