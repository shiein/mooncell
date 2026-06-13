// Mooncell — 登录相关的后端 API 客户端
// 仅登录走真实后端(Hono + SQLite);其余页面为静态迁移,沿用 mock 数据。
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

export { login, logout, getSession };
