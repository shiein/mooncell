import { test, expect } from '@playwright/test';

// 数据集成定义级授权（P3.16）：use=只读+运行 / edit=可写 / 无授权=入口隐藏+403。
const ADMIN = { user: 'admin', pass: '1qaz@WSX' };
const usernameInput = (page) => page.locator('input[autocomplete="username"]');

async function loginAs(page, user, pass) {
  await page.goto('/');
  await usernameInput(page).fill(user);
  await page.locator('input[type="password"]').fill(pass);
  await page.getByRole('button', { name: '登录' }).click();
  await expect(usernameInput(page)).toHaveCount(0, { timeout: 10000 });
}

async function makePipeline(page, name) {
  const doc = {
    format: 1, kind: 'pipeline', params: [],
    nodes: [
      { id: 'g', type: 'generate_rows', name: '生成', version: 1, config: { limit: 2 }, ui: { x: 96, y: 128 } },
      { id: 'd', type: 'dummy', name: '空', version: 1, config: {}, ui: { x: 448, y: 128 } },
    ],
    hops: [{ id: 'h', from: 'g', fromPort: 'main', to: 'd', toPort: 'main' }],
  };
  const resp = await page.request.post('/api/etl/definitions', { data: { kind: 'pipeline', name, doc } });
  expect(resp.ok()).toBeTruthy();
  return await resp.json();
}

async function setEtlGrants(page, username, grants) {
  const resp = await page.request.put(`/api/users/${encodeURIComponent(username)}`, { data: { etlGrants: grants } });
  expect(resp.ok()).toBeTruthy();
}

test('定义级授权：use 只读可运行、edit 可写、无授权隐藏且 403', async ({ page, browser }) => {
  await loginAs(page, ADMIN.user, ADMIN.pass);
  const tag = `G${Date.now() % 100000}`;
  const p1 = await makePipeline(page, `${tag}-授权目标`);
  const p2 = await makePipeline(page, `${tag}-未授权`);
  const p3 = await makePipeline(page, `${tag}-编辑目标`);
  const uname = `u${Date.now() % 100000}`;
  const pw = 'Test@12345';

  // ---- admin 创建用户：初始仅 use(p1)；edit(p3) 在 use-only 断言后再授 ----
  let resp = await page.request.post('/api/users', {
    data: {
      username: uname, password: pw, appIds: [],
      etlGrants: [{ definitionId: p1.id, accessMode: 'use' }],
    },
  });
  expect(resp.ok()).toBeTruthy();

  // ---- 无授权用户：导航隐藏 + API 403 ----
  const uname0 = `z${Date.now() % 100000}`;
  resp = await page.request.post('/api/users', { data: { username: uname0, password: pw, appIds: [] } });
  expect(resp.ok()).toBeTruthy();
  const ctx0 = await browser.newContext();
  const pg0 = await ctx0.newPage();
  await loginAs(pg0, uname0, pw);
  const sess0 = await (await pg0.request.get('/api/session')).json();
  expect(sess0.features.etl).toBeFalsy();
  await expect(pg0.getByRole('navigation').getByRole('button', { name: '数据集成' })).toHaveCount(0);
  expect((await pg0.request.get('/api/etl/definitions')).status()).toBe(403);
  await ctx0.close();

  // ---- 授权用户会话 ----
  const ctx = await browser.newContext();
  const up = await ctx.newPage();
  await loginAs(up, uname, pw);
  const sess = await (await up.request.get('/api/session')).json();
  expect(sess.features.etl).toBeTruthy();
  await up.getByRole('navigation').getByRole('button', { name: '数据集成' }).click();

  // 列表只显示授权的 use 定义（edit 尚未授）
  await expect(up.locator('.etl-row', { hasText: `${tag}-授权目标` })).toBeVisible();
  await expect(up.locator('.etl-row', { hasText: `${tag}-编辑目标` })).toHaveCount(0);
  await expect(up.locator('.etl-row', { hasText: `${tag}-未授权` })).toHaveCount(0);
  // 非管理员无新建/导入入口
  await expect(up.getByRole('button', { name: '导入', exact: true })).toHaveCount(0);
  await expect(up.getByRole('button', { name: '新建', exact: true })).toHaveCount(0);

  // use 定义：设计器只读——徽章可见、保存禁用、名称输入只读
  await up.locator('.etl-row', { hasText: `${tag}-授权目标` }).click();
  await expect(up.getByText('只读', { exact: true })).toBeVisible();
  await expect(up.locator('.etl-dz-top button:has-text("保存")')).toBeDisabled();
  await expect(up.locator('.etl-dz-name')).toHaveAttribute('readonly', '');
  // 只读下节点仍可查看（选中后检查器显示只读提示）
  await up.locator('.react-flow__node').first().click();
  await expect(up.locator('.etl-insp-ro')).toBeVisible();

  // API 层：use 的写操作全部 403；edit 定义可保存；无授权定义 403
  expect((await up.request.put(`/api/etl/definitions/${p1.id}`, { data: { revision: p1.revision } })).status()).toBe(403);
  expect((await up.request.delete(`/api/etl/definitions/${p1.id}`)).status()).toBe(403);
  expect((await up.request.post(`/api/etl/definitions/${p1.id}/duplicate`)).status()).toBe(403);
  expect((await up.request.get(`/api/etl/definitions/${p2.id}`)).status()).toBe(403);
  expect((await up.request.post('/api/etl/definitions', { data: { kind: 'pipeline', name: 'x' } })).status()).toBe(403);
  expect((await up.request.post('/api/etl/connections', { data: { name: 'c', dbType: 'postgres', host: 'h', port: 5432, username: 'u' } })).status()).toBe(403);
  // use 可运行、可看运行记录；预览与调度需 edit——仅持 use 授权时均为 403
  const runResp = await up.request.post(`/api/etl/definitions/${p1.id}/run`, { data: {} });
  expect(runResp.ok()).toBeTruthy();
  expect((await up.request.post('/api/etl/pipelines/preview', { data: { doc: {}, nodeId: 'g' } })).status()).toBe(403);
  expect((await up.request.post('/api/etl/schedules', { data: { targetId: p1.id, cron: '0 2 * * *' } })).status()).toBe(403);

  // ---- 追加 edit(p3)：写路径放开、预览放行、调度可建 ----
  await setEtlGrants(page, uname, [
    { definitionId: p1.id, accessMode: 'use' },
    { definitionId: p3.id, accessMode: 'edit' },
  ]);
  const scResp = await up.request.post('/api/etl/schedules', { data: { targetId: p3.id, cron: '0 3 * * *' } });
  expect(scResp.ok()).toBeTruthy();

  // edit 定义：设计器可保存
  const cur = await (await up.request.get(`/api/etl/definitions/${p3.id}`)).json();
  const put = await up.request.put(`/api/etl/definitions/${p3.id}`, { data: { revision: cur.revision, description: '由 edit 授权用户保存' } });
  expect(put.ok()).toBeTruthy();

  // ---- 撤销全部授权：API 立刻 403，会话刷新后 features.etl=false ----
  await setEtlGrants(page, uname, []);
  expect((await up.request.get('/api/etl/definitions')).status()).toBe(403);
  await up.reload();
  const sess2 = await (await up.request.get('/api/session')).json();
  expect(sess2.features.etl).toBeFalsy();
  await ctx.close();

  // 清理测试用户
  await page.request.delete(`/api/users/${encodeURIComponent(uname)}`);
  await page.request.delete(`/api/users/${encodeURIComponent(uname0)}`);
});
