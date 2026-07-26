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

export async function resumeServerUpload(resourceId, sessionId, transferId, { localSize, prefixChunks } = {}) {
  return jsonFetch(
    `/api/server-resources/${encodeURIComponent(resourceId)}/sessions/${encodeURIComponent(sessionId)}/uploads/${encodeURIComponent(transferId)}/resume`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ localSize: localSize || 0, prefixChunks: prefixChunks || [] }),
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
  // 普通 HTTP 内网页面通常没有 crypto.subtle；此时使用同结果的本地实现，
  // 不把 HTTPS 作为服务器运维功能的强制前置条件。
  if (typeof crypto !== 'undefined' && crypto.subtle) {
    try {
      const digest = await crypto.subtle.digest('SHA-256', buffer);
      return Array.from(new Uint8Array(digest)).map((b) => b.toString(16).padStart(2, '0')).join('');
    } catch (_) {
      // 部分旧浏览器会暴露 subtle 但在非安全上下文拒绝调用，继续使用本地实现。
    }
  }
  return sha256HexFallback(new Uint8Array(buffer));
}

const SHA256_K = [
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
];

export function sha256HexFallback(input) {
  const bitLength = input.length * 8;
  const paddedLength = Math.ceil((input.length + 9) / 64) * 64;
  const padded = new Uint8Array(paddedLength);
  padded.set(input);
  padded[input.length] = 0x80;
  const view = new DataView(padded.buffer);
  const high = Math.floor(bitLength / 0x100000000);
  const low = bitLength >>> 0;
  view.setUint32(paddedLength - 8, high);
  view.setUint32(paddedLength - 4, low);

  const h = new Uint32Array([
    0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
    0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
  ]);
  const w = new Uint32Array(64);
  const rotr = (x, n) => (x >>> n) | (x << (32 - n));

  for (let offset = 0; offset < paddedLength; offset += 64) {
    for (let i = 0; i < 16; i++) w[i] = view.getUint32(offset + i * 4);
    for (let i = 16; i < 64; i++) {
      const s0 = rotr(w[i - 15], 7) ^ rotr(w[i - 15], 18) ^ (w[i - 15] >>> 3);
      const s1 = rotr(w[i - 2], 17) ^ rotr(w[i - 2], 19) ^ (w[i - 2] >>> 10);
      w[i] = (w[i - 16] + s0 + w[i - 7] + s1) >>> 0;
    }
    let [a, b, c, d, e, f, g, hh] = h;
    for (let i = 0; i < 64; i++) {
      const s1 = rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25);
      const ch = (e & f) ^ (~e & g);
      const t1 = (hh + s1 + ch + SHA256_K[i] + w[i]) >>> 0;
      const s0 = rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22);
      const maj = (a & b) ^ (a & c) ^ (b & c);
      const t2 = (s0 + maj) >>> 0;
      hh = g;
      g = f;
      f = e;
      e = (d + t1) >>> 0;
      d = c;
      c = b;
      b = a;
      a = (t1 + t2) >>> 0;
    }
    h[0] = (h[0] + a) >>> 0;
    h[1] = (h[1] + b) >>> 0;
    h[2] = (h[2] + c) >>> 0;
    h[3] = (h[3] + d) >>> 0;
    h[4] = (h[4] + e) >>> 0;
    h[5] = (h[5] + f) >>> 0;
    h[6] = (h[6] + g) >>> 0;
    h[7] = (h[7] + hh) >>> 0;
  }
  return Array.from(h).map((n) => n.toString(16).padStart(8, '0')).join('');
}
