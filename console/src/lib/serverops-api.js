// 服务器运维 API 客户端。
// 密码仅在 createSession 调用时经内存传递，不得写入 URL / localStorage。

async function jsonFetch(url, opts = {}) {
  const r = await fetch(url, { credentials: 'same-origin', ...opts });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) {
    const err = new Error(d.message || d.error || `HTTP ${r.status}`);
    err.code = d.code;
    err.status = r.status;
    err.body = d;
    err.retryable = d.retryable;
    err.nextOffset = d.nextOffset;
    throw err;
  }
  return d;
}

export async function listServerResources() {
  const d = await jsonFetch('/api/server-resources');
  return { resources: d.resources || [], cleanupPending: d.cleanupPending || 0 };
}

export async function getServerResource(id) {
  return jsonFetch(`/api/server-resources/${encodeURIComponent(id)}`);
}

export async function createServerResource(payload) {
  return jsonFetch('/api/server-resources', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  });
}

export async function updateServerResource(id, payload) {
  return jsonFetch(`/api/server-resources/${encodeURIComponent(id)}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  });
}

export async function deleteServerResource(id) {
  return jsonFetch(`/api/server-resources/${encodeURIComponent(id)}`, {
    method: 'DELETE',
  });
}

export async function probeHostKey({ host, port }) {
  return jsonFetch('/api/server-resources/host-key/probe', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ host, port: Number(port) || 22 }),
  });
}

export async function confirmHostKey(id, { algorithm, sha256, updatedAt }) {
  return jsonFetch(`/api/server-resources/${encodeURIComponent(id)}/host-key/confirm`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ algorithm, sha256, updatedAt }),
  });
}

/**
 * 创建 SSH 会话。password 只在本函数栈内使用，调用方须在结束后清空 state。
 * SSH 认证失败返回 422，不会触发全局 401 退出。
 */
export async function createServerSession(resourceId, { password, cols, rows }) {
  return jsonFetch(`/api/server-resources/${encodeURIComponent(resourceId)}/sessions`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password, cols: cols || 120, rows: rows || 36 }),
  });
}

export async function deleteServerSession(resourceId, sessionId) {
  return jsonFetch(
    `/api/server-resources/${encodeURIComponent(resourceId)}/sessions/${encodeURIComponent(sessionId)}`,
    { method: 'DELETE' },
  );
}

export async function listServerFiles(resourceId, sessionId, path = '.') {
  const q = new URLSearchParams({ path: path || '.' });
  return jsonFetch(
    `/api/server-resources/${encodeURIComponent(resourceId)}/sessions/${encodeURIComponent(sessionId)}/files?${q}`,
  );
}

/** 下载 URL：用 <a> 导航，禁止 fetch+blob（避免百兆进内存）。 */
export function serverDownloadUrl(resourceId, sessionId, path) {
  const q = new URLSearchParams({ path });
  return `/api/server-resources/${encodeURIComponent(resourceId)}/sessions/${encodeURIComponent(sessionId)}/download?${q}`;
}

export async function initServerUpload(resourceId, sessionId, { directory, filename, size, overwrite }) {
  return jsonFetch(
    `/api/server-resources/${encodeURIComponent(resourceId)}/sessions/${encodeURIComponent(sessionId)}/uploads`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ directory, filename, size, overwrite: !!overwrite }),
    },
  );
}

export async function uploadServerChunk(resourceId, sessionId, transferId, offset, blob, sha256Hex) {
  const q = new URLSearchParams({ offset: String(offset) });
  const headers = { 'Content-Type': 'application/octet-stream' };
  if (sha256Hex) headers['X-Chunk-SHA256'] = sha256Hex;
  const r = await fetch(
    `/api/server-resources/${encodeURIComponent(resourceId)}/sessions/${encodeURIComponent(sessionId)}/uploads/${encodeURIComponent(transferId)}?${q}`,
    { method: 'PUT', credentials: 'same-origin', headers, body: blob },
  );
  const d = await r.json().catch(() => ({}));
  if (!r.ok) {
    const err = new Error(d.message || d.error || `HTTP ${r.status}`);
    err.code = d.code;
    err.status = r.status;
    err.nextOffset = d.nextOffset;
    throw err;
  }
  return d;
}

export async function completeServerUpload(resourceId, sessionId, transferId) {
  return jsonFetch(
    `/api/server-resources/${encodeURIComponent(resourceId)}/sessions/${encodeURIComponent(sessionId)}/uploads/${encodeURIComponent(transferId)}/complete`,
    { method: 'POST' },
  );
}

export async function cancelServerUpload(resourceId, sessionId, transferId) {
  return jsonFetch(
    `/api/server-resources/${encodeURIComponent(resourceId)}/sessions/${encodeURIComponent(sessionId)}/uploads/${encodeURIComponent(transferId)}`,
    { method: 'DELETE' },
  );
}

export async function getUploadStatus(resourceId, transferId) {
  return jsonFetch(
    `/api/server-resources/${encodeURIComponent(resourceId)}/uploads/${encodeURIComponent(transferId)}`,
  );
}

/** 列出当前用户在该资源上可续传的 uploading 任务。 */
export async function listActiveUploads(resourceId) {
  const d = await jsonFetch(`/api/server-resources/${encodeURIComponent(resourceId)}/uploads`);
  return d.transfers || [];
}

export async function resumeServerUpload(resourceId, sessionId, transferId, { localSize } = {}) {
  return jsonFetch(
    `/api/server-resources/${encodeURIComponent(resourceId)}/sessions/${encodeURIComponent(sessionId)}/uploads/${encodeURIComponent(transferId)}/resume`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ localSize: localSize || 0 }),
    },
  );
}

/** WebSocket 终端 URL（同源，cookie 自动携带）。 */
export function terminalWsUrl(resourceId, sessionId) {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${location.host}/api/server-resources/${encodeURIComponent(resourceId)}/sessions/${encodeURIComponent(sessionId)}/terminal`;
}

/** 计算 ArrayBuffer 的 SHA-256 hex（Web Crypto）。 */
export async function sha256Hex(buffer) {
  const digest = await crypto.subtle.digest('SHA-256', buffer);
  return Array.from(new Uint8Array(digest)).map((b) => b.toString(16).padStart(2, '0')).join('');
}
