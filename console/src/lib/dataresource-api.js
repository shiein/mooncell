// 数据资源模块 API 客户端

async function jsonFetch(url, opts = {}) {
  const r = await fetch(url, { credentials: 'same-origin', ...opts });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) {
    const err = new Error(d.error || d.message || `HTTP ${r.status}`);
    err.code = d.code;
    err.status = r.status;
    err.body = d;
    throw err;
  }
  return d;
}

export async function listDrivers() {
  const d = await jsonFetch('/api/data-resources/drivers');
  return d.drivers || [];
}

export async function listDataResources() {
  const d = await jsonFetch('/api/data-resources');
  return d.resources || [];
}

export async function createDataResource(payload) {
  return jsonFetch('/api/data-resources', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  });
}

export async function updateDataResource(id, payload) {
  return jsonFetch(`/api/data-resources/${encodeURIComponent(id)}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  });
}

export async function deleteDataResource(id, name) {
  return jsonFetch(`/api/data-resources/${encodeURIComponent(id)}`, {
    method: 'DELETE',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name }),
  });
}

export async function testDataResource(id) {
  return jsonFetch(`/api/data-resources/${encodeURIComponent(id)}/test`, { method: 'POST' });
}

export async function testDataResourceConfig(payload) {
  return jsonFetch('/api/data-resources/test', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  });
}

/** 编辑草稿测试：表单配置 + 可选密码（空则服务端用已存密码） */
export async function testDataResourceDraft(id, payload) {
  return jsonFetch(`/api/data-resources/${encodeURIComponent(id)}/test-draft`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  });
}

export async function createWorkspace(resourceId) {
  return jsonFetch(`/api/data-resources/${encodeURIComponent(resourceId)}/workspaces`, { method: 'POST' });
}

export async function patchAutoCommit(resourceId, workspaceId, autoCommit) {
  return jsonFetch(
    `/api/data-resources/${encodeURIComponent(resourceId)}/workspaces/${encodeURIComponent(workspaceId)}/auto-commit`,
    {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ autoCommit }),
    },
  );
}

export async function executeSQL(resourceId, workspaceId, { sql, limit, offset, confirmed, signal }) {
  return jsonFetch(
    `/api/data-resources/${encodeURIComponent(resourceId)}/workspaces/${encodeURIComponent(workspaceId)}/execute`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sql, limit, offset: offset || 0, confirmed }),
      signal,
    },
  );
}

/** 结果区就地编辑：按主键批量 UPDATE/DELETE（仅单表且有主键） */
export async function applyRowEdits(resourceId, workspaceId, payload) {
  return jsonFetch(
    `/api/data-resources/${encodeURIComponent(resourceId)}/workspaces/${encodeURIComponent(workspaceId)}/row-edits`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    },
  );
}

export async function commitWorkspace(resourceId, workspaceId) {
  return jsonFetch(
    `/api/data-resources/${encodeURIComponent(resourceId)}/workspaces/${encodeURIComponent(workspaceId)}/commit`,
    { method: 'POST' },
  );
}

export async function rollbackWorkspace(resourceId, workspaceId) {
  return jsonFetch(
    `/api/data-resources/${encodeURIComponent(resourceId)}/workspaces/${encodeURIComponent(workspaceId)}/rollback`,
    { method: 'POST' },
  );
}

export async function deleteWorkspace(resourceId, workspaceId) {
  return jsonFetch(
    `/api/data-resources/${encodeURIComponent(resourceId)}/workspaces/${encodeURIComponent(workspaceId)}`,
    { method: 'DELETE' },
  );
}

export async function metadataChildren(resourceId, parentId = '') {
  const q = parentId ? `?parentId=${encodeURIComponent(parentId)}` : '';
  const d = await jsonFetch(`/api/data-resources/${encodeURIComponent(resourceId)}/metadata/children${q}`);
  return d.children || d.nodes || d || [];
}

export async function metadataStructure(resourceId, nodeId) {
  return jsonFetch(
    `/api/data-resources/${encodeURIComponent(resourceId)}/metadata/structure?nodeId=${encodeURIComponent(nodeId)}`,
  );
}

export async function metadataDDL(resourceId, nodeId) {
  return jsonFetch(
    `/api/data-resources/${encodeURIComponent(resourceId)}/metadata/ddl?nodeId=${encodeURIComponent(nodeId)}`,
  );
}

export async function sqlTemplate(resourceId, nodeId, operation) {
  return jsonFetch(`/api/data-resources/${encodeURIComponent(resourceId)}/metadata/sql-template`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ nodeId, operation }),
  });
}

export async function listSavedSQL(resourceId) {
  const d = await jsonFetch(`/api/data-resources/${encodeURIComponent(resourceId)}/saved-sql`);
  return d.savedSql || d.items || [];
}

export async function createSavedSQL(resourceId, { name, sqlText }) {
  return jsonFetch(`/api/data-resources/${encodeURIComponent(resourceId)}/saved-sql`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name, sqlText }),
  });
}

export async function updateSavedSQL(resourceId, sqlId, { name, sqlText }) {
  return jsonFetch(
    `/api/data-resources/${encodeURIComponent(resourceId)}/saved-sql/${encodeURIComponent(sqlId)}`,
    {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name, sqlText }),
    },
  );
}

export async function deleteSavedSQL(resourceId, sqlId) {
  return jsonFetch(
    `/api/data-resources/${encodeURIComponent(resourceId)}/saved-sql/${encodeURIComponent(sqlId)}`,
    { method: 'DELETE' },
  );
}

/** 导出：返回 blob（流式响应可能非 JSON） */
export async function exportWorkspace(resourceId, workspaceId, {
  sql, format, scope = 'all', columns, rows,
}) {
  const r = await fetch(
    `/api/data-resources/${encodeURIComponent(resourceId)}/workspaces/${encodeURIComponent(workspaceId)}/export`,
    {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        sql, format: format || 'csv', scope, columns: columns || [], rows: rows || [],
      }),
    },
  );
  if (!r.ok) {
    const d = await r.json().catch(() => ({}));
    throw new Error(d.error || `导出失败 HTTP ${r.status}`);
  }
  return r.blob();
}

export async function previewImport(resourceId, file) {
  const form = new FormData();
  form.append('file', file);
  return jsonFetch(`/api/data-resources/${encodeURIComponent(resourceId)}/imports/preview`, {
    method: 'POST',
    body: form,
  });
}

export async function selectImportSheet(resourceId, importId, sheet) {
  return jsonFetch(
    `/api/data-resources/${encodeURIComponent(resourceId)}/imports/${encodeURIComponent(importId)}`,
    {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sheet }),
    },
  );
}

export async function executeImport(resourceId, importId, payload) {
  return jsonFetch(
    `/api/data-resources/${encodeURIComponent(resourceId)}/imports/${encodeURIComponent(importId)}/execute`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    },
  );
}

export async function deleteImport(resourceId, importId) {
  return jsonFetch(
    `/api/data-resources/${encodeURIComponent(resourceId)}/imports/${encodeURIComponent(importId)}`,
    { method: 'DELETE' },
  );
}
