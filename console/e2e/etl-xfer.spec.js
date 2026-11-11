import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';

// 数据集成导入/导出（P2.15）：JSON 包导出、版本校验、引用映射、同名冲突改名。
const ADMIN = { user: 'admin', pass: '1qaz@WSX' };
const usernameInput = (page) => page.locator('input[autocomplete="username"]');

async function login(page) {
  await page.goto('/');
  await usernameInput(page).fill(ADMIN.user);
  await page.locator('input[type="password"]').fill(ADMIN.pass);
  await page.getByRole('button', { name: '登录' }).click();
  await expect(usernameInput(page)).toHaveCount(0, { timeout: 10000 });
}

async function makePipeline(page, name, nodes, hops) {
  const doc = { format: 1, kind: 'pipeline', params: [], nodes, hops, settings: {}, notes: [] };
  const resp = await page.request.post('/api/etl/definitions', { data: { kind: 'pipeline', name, doc } });
  expect(resp.ok()).toBeTruthy();
  return await resp.json();
}

test('导入/导出：包格式校验、缺失连接映射、同名改名', async ({ page }) => {
  await login(page);
  const tag = `X${Date.now() % 100000}`;
  // 一个引用不存在连接的转换（草稿态可保存）+ 一个普通转换
  const p1 = await makePipeline(page, `${tag}-含悬空连接`, [
    { id: 'in', type: 'table_input', name: '源', version: 1, config: { connection: 'conn_ghost', sql: 'select 1' }, ui: { x: 96, y: 128 } },
    { id: 'd', type: 'dummy', name: '空', version: 1, config: {}, ui: { x: 448, y: 128 } },
  ], [{ id: 'h', from: 'in', fromPort: 'main', to: 'd', toPort: 'main' }]);
  await makePipeline(page, `${tag}-普通`, [
    { id: 'g', type: 'generate_rows', name: '生成', version: 1, config: { limit: 1 }, ui: { x: 96, y: 128 } },
    { id: 'd', type: 'dummy', name: '空', version: 1, config: {}, ui: { x: 448, y: 128 } },
  ], [{ id: 'h', from: 'g', fromPort: 'main', to: 'd', toPort: 'main' }]);

  // ---- 导出：行菜单触发下载，包内容符合格式
  await page.getByRole('navigation').getByRole('button', { name: '数据集成' }).click();
  const row = page.locator('.etl-row', { hasText: `${tag}-含悬空连接` });
  await row.locator('button[title="更多"]').click();
  const dl = page.waitForEvent('download');
  await page.getByText('导出', { exact: true }).click();
  const file = await dl;
  const pkg = JSON.parse(readFileSync(await file.path(), 'utf-8'));
  expect(pkg.format).toBe('mooncell-etl-export');
  expect(pkg.version).toBe(1);
  expect(pkg.definition.kind).toBe('pipeline');
  expect(pkg.definition.name).toBe(`${tag}-含悬空连接`);
  expect(pkg.references.connections).toHaveLength(1);
  expect(pkg.references.connections[0].id).toBe('conn_ghost');

  // ---- 导入：预览显示未匹配连接 → 保持原引用 → 导入后打开设计器
  const buffer = Buffer.from(JSON.stringify(pkg));
  await page.getByRole('button', { name: '导入', exact: true }).click();
  await page.locator('input[type="file"]').setInputFiles({ name: 'pkg.etl.json', mimeType: 'application/json', buffer });
  const dialog = page.locator('.dialog');
  await expect(dialog.getByText(`导入转换「${tag}-含悬空连接」`)).toBeVisible();
  await expect(dialog.getByText(/将保存为「.*\(导入\)」/)).toBeVisible(); // 同名冲突改名
  await expect(dialog.getByText('原引用已失效')).toBeVisible(); // conn_ghost 无名称信息
  // 保持原引用（默认空选）直接导入
  await dialog.getByRole('button', { name: '导入', exact: true }).click();
  await expect(page.locator('.react-flow')).toBeVisible({ timeout: 10000 });
  // 设计器标题输入框应为改名后的名称
  await expect(page.locator('input.etl-dz-name')).toHaveValue(`${tag}-含悬空连接 (导入)`);

  // ---- 非法文件：toast 报错不建定义
  await page.locator('button[title="返回数据集成"]').click();
  await page.getByRole('button', { name: '导入', exact: true }).click();
  await page.locator('input[type="file"]').setInputFiles({ name: 'bad.json', mimeType: 'application/json', buffer: Buffer.from('{"format":"other","version":1}') });
  await expect(page.getByText(/不是数据集成导出文件|格式/)).toBeVisible({ timeout: 5000 });

  // ---- 版本不符拒绝（preview 接口层验证）
  const badVer = await page.request.post('/api/etl/definitions/import', { data: { document: { ...pkg, version: 99 } } });
  expect(badVer.status()).toBe(400);

  // 列表中应存在改名后的定义
  await expect(page.locator('.etl-row', { hasText: `${tag}-含悬空连接 (导入)` })).toHaveCount(1);
});
