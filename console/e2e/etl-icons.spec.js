import { test, expect } from '@playwright/test';

// R1-m2 图标完整性：调色板中每个步骤/作业项图标都必须渲染出图形
//（IC 表缺键时 <Icon> 输出空 <svg>），并留亮/暗两套截图备查。
const ADMIN = { user: 'admin', pass: '1qaz@WSX' };
const usernameInput = (page) => page.locator('input[autocomplete="username"]');

async function login(page) {
  await page.goto('/');
  await usernameInput(page).fill(ADMIN.user);
  await page.locator('input[type="password"]').fill(ADMIN.pass);
  await page.getByRole('button', { name: '登录' }).click();
  await expect(usernameInput(page)).toHaveCount(0, { timeout: 10000 });
}

async function makeDef(page, kind, name) {
  const doc = {
    format: 1, kind, params: [],
    nodes: [{ id: 'src', type: kind === 'workflow' ? 'start' : 'generate_rows', name: '起点', version: 1, config: kind === 'workflow' ? {} : { limit: 1 }, ui: { x: 96, y: 128 } }],
    hops: [], settings: {}, notes: [],
  };
  const resp = await page.request.post('/api/etl/definitions', { data: { kind, name, doc } });
  expect(resp.ok()).toBeTruthy();
  return await resp.json();
}

async function checkPalette(page, kind, shot) {
  // 展开所有分类（默认全展开；防御性地点开折叠组）
  const items = page.locator('.etl-palette-item');
  await expect(items.first()).toBeVisible({ timeout: 10000 }); // catalog 异步加载
  const n = await items.count();
  console.log(`[icons] ${kind} 调色板条目数=${n}`);
  expect(n).toBeGreaterThan(10);
  const empty = [];
  for (let i = 0; i < n; i++) {
    const item = items.nth(i);
    const svgCount = await item.locator('.etl-chip svg').count();
    const childCount = svgCount ? await item.locator('.etl-chip svg > *').count() : 0;
    if (childCount === 0) empty.push(await item.locator('.etl-palette-label').textContent());
  }
  expect(empty, `空图标（IC 表缺键）：${empty.join(', ')}`).toEqual([]);
  await page.locator('.etl-palette').screenshot({ path: `e2e/out/${shot}` });
}

test('调色板步骤/作业项图标齐全（亮/暗）', async ({ page }) => {
  await login(page);
  await makeDef(page, 'pipeline', '图标验证-转换');
  await makeDef(page, 'workflow', '图标验证-作业');

  await page.getByRole('button', { name: '数据集成', exact: true }).click();
  await page.locator('.etl-row', { hasText: '图标验证-转换' }).first().click();
  await expect(page.locator('.etl-designer')).toBeVisible();
  await checkPalette(page, 'pipeline', 'icons-pipeline-light.png');
  await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'dark'));
  await checkPalette(page, 'pipeline', 'icons-pipeline-dark.png');
  await page.getByTitle('返回数据集成').click();
  await expect(page.locator('.etl-designer')).toHaveCount(0);

  await page.locator('.etl-row', { hasText: '图标验证-作业' }).first().click();
  await expect(page.locator('.etl-designer')).toBeVisible();
  await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'light'));
  await checkPalette(page, 'workflow', 'icons-workflow-light.png');
  await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'dark'));
  await checkPalette(page, 'workflow', 'icons-workflow-dark.png');
});
