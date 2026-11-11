import { test, expect } from '@playwright/test';

// 数据集成工程列表（P2.13）：文件夹分组/筛选、最近运行状态排序。
const ADMIN = { user: 'admin', pass: '1qaz@WSX' };
const usernameInput = (page) => page.locator('input[autocomplete="username"]');

async function login(page) {
  await page.goto('/');
  await usernameInput(page).fill(ADMIN.user);
  await page.locator('input[type="password"]').fill(ADMIN.pass);
  await page.getByRole('button', { name: '登录' }).click();
  await expect(usernameInput(page)).toHaveCount(0, { timeout: 10000 });
}

async function makePipeline(page, name, folder, nodes) {
  const doc = {
    format: 1, kind: 'pipeline', params: [],
    nodes: nodes || [
      { id: 'src', type: 'generate_rows', name: '生成', version: 1, config: { limit: 2 }, ui: { x: 96, y: 128 } },
      { id: 'dst', type: 'dummy', name: '空操作', version: 1, config: {}, ui: { x: 448, y: 128 } },
    ],
    hops: [{ id: 'h1', from: 'src', fromPort: 'main', to: 'dst', toPort: 'main' }],
    settings: {}, notes: [],
  };
  const resp = await page.request.post('/api/etl/definitions', { data: { kind: 'pipeline', name, folder, doc } });
  expect(resp.ok()).toBeTruthy();
  return await resp.json();
}

async function runAndWait(page, defId, want) {
  const run = await (await page.request.post(`/api/etl/definitions/${defId}/run`, { data: { params: {} } })).json();
  await expect.poll(async () => (await (await page.request.get(`/api/etl/runs/${run.id}`)).json()).run.status,
    { timeout: 20000, intervals: [500, 1000, 2000] }).toBe(want);
}

const rowNames = async (page) => await page.locator('.etl-row .etl-name-title').allTextContents();

test('工程列表：文件夹分组/筛选 + 最近运行状态排序', async ({ page }) => {
  await login(page);
  const tag = `P${Date.now() % 100000}`;
  const d1 = await makePipeline(page, `${tag}-甲`, `${tag}组`);
  const d2 = await makePipeline(page, `${tag}-乙`, `${tag}组`);
  await makePipeline(page, `${tag}-丙`, '');
  // 丁：table_input 引用不存在连接 → 运行必失败
  const d4 = await makePipeline(page, `${tag}-丁`, `${tag}组`, [
    { id: 'src', type: 'table_input', name: '表输入', version: 1, config: { connection: 'conn-missing', table: 't' }, ui: { x: 96, y: 128 } },
    { id: 'dst', type: 'dummy', name: '空操作', version: 1, config: {}, ui: { x: 448, y: 128 } },
  ]);

  // 跑 d1（成功）与 d4（失败），凑齐两种最近运行状态
  await runAndWait(page, d1.id, 'succeeded');
  await runAndWait(page, d4.id, 'failed');

  await page.getByRole('button', { name: '数据集成', exact: true }).click();
  await expect(page.locator('.etl-table')).toBeVisible();

  // ---- 分组：组头显示文件夹名与计数，未分组排最后
  const groups = page.locator('.etl-group');
  const groupTexts = await groups.allTextContents();
  const gi = groupTexts.findIndex((t) => t.includes(`${tag}组`));
  const ni = groupTexts.findIndex((t) => t.includes('未分组'));
  expect(gi).toBeGreaterThanOrEqual(0);
  expect(ni).toBeGreaterThan(gi); // 未分组在最后
  expect(groupTexts[gi]).toContain('3'); // 组内计数（甲乙丁）

  // ---- 文件夹筛选：只看该组 → 恰好 3 行
  const folderSel = page.locator('.etl-toolbar select.select-el').first();
  await folderSel.selectOption({ label: `${tag}组` });
  await expect(page.locator('.etl-row')).toHaveCount(3);
  await folderSel.selectOption({ label: '全部文件夹' });
  await expect(page.locator('.etl-row', { hasText: tag })).toHaveCount(4); // 复位后本用例 4 个定义都在

  // ---- 最近运行排序：点表头 → 失败优先（丁最前、未运行垫底）；再点 → 成功优先（甲最前）
  const th = page.locator('th.etl-sort-th');
  const idx = (names, n) => names.indexOf(`${tag}-${n}`);
  await th.click();
  let names = await rowNames(page);
  expect(idx(names, '丁')).toBeLessThan(idx(names, '甲'));
  expect(idx(names, '甲')).toBeLessThan(idx(names, '乙')); // 成功 < 从未运行
  await th.click();
  names = await rowNames(page);
  expect(idx(names, '甲')).toBeLessThan(idx(names, '丁'));
  expect(idx(names, '丁')).toBeLessThan(idx(names, '乙')); // 未运行仍垫底
  await th.click(); // 恢复分组视图
  await expect(groups.first()).toBeVisible();
});
