import { test, expect } from '@playwright/test';
import { writeFileSync, mkdirSync } from 'node:fs';

// csv 文件步骤的 path 相对 Console 的 file_root（e2e 配置为 /tmp/mc-e2e-etlroot）。
const FILE_ROOT = '/tmp/mc-e2e-etlroot';
mkdirSync(FILE_ROOT, { recursive: true });

// 数据集成运行详情（P2.14）：拒绝行采样查看 + 失败节点一键定位到设计器。
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

async function runAndWait(page, defId, want) {
  const run = await (await page.request.post(`/api/etl/definitions/${defId}/run`, { data: { params: {} } })).json();
  await expect.poll(async () => (await (await page.request.get(`/api/etl/runs/${run.id}`)).json()).run.status,
    { timeout: 20000, intervals: [500, 1000, 2000] }).toBe(want);
  return run.id;
}

async function openRunDetail(page, defName) {
  await page.getByRole('navigation').getByRole('button', { name: '数据集成' }).click();
  await page.getByRole('button', { name: '运行记录' }).click();
  await page.locator('.etl-row', { hasText: defName }).first().click();
  await expect(page.locator('.etl-run-head')).toBeVisible();
}

test('运行详情：拒绝行采样 + 失败节点定位', async ({ page }) => {
  await login(page);
  const tag = `R${Date.now() % 100000}`;

  // ---- 拒绝行采样：csv_input 声明 integer 字段，混入一条无法转换的行 → error 端口拒绝
  const csvPath = `mc-e2e-err-${tag}.csv`;
  writeFileSync(`${FILE_ROOT}/${csvPath}`, 'id,name\n1,甲\n2,乙\nbad,丙\n4,丁\n');
  const p1 = await makePipeline(page, `${tag}-采样`, [
    { id: 'csv', type: 'csv_input', name: '读文件', version: 1, config: { path: csvPath, hasHeader: true, fields: [{ name: 'id', kind: 'integer' }, { name: 'name', kind: 'string' }] }, ui: { x: 96, y: 128 } },
    { id: 'ok', type: 'dummy', name: '正常行', version: 1, config: {}, ui: { x: 448, y: 96 } },
    { id: 'err', type: 'dummy', name: '错误行', version: 1, config: {}, ui: { x: 448, y: 192 } },
  ], [
    { id: 'h1', from: 'csv', fromPort: 'main', to: 'ok', toPort: 'main' },
    { id: 'h2', from: 'csv', fromPort: 'error', to: 'err', toPort: 'main' },
  ]);
  const runId = await runAndWait(page, p1.id, 'succeeded');

  await openRunDetail(page, `${tag}-采样`);
  // 拒绝列显示可点击的 1（列序：步骤|状态|读|写|输入|输出|更新|拒绝|错误|耗时|速度）
  const rejCell = page.locator('.table tr', { hasText: '读文件' }).locator('td').nth(7);
  await expect(rejCell.locator('button')).toHaveText('1');
  await rejCell.locator('button').click();
  // 采样弹窗：字段含错误字段，行含坏值与错误码
  const dialog = page.locator('.dialog');
  await expect(dialog.getByText('拒绝行采样 · 读文件')).toBeVisible();
  await expect(dialog.locator('th', { hasText: '_error_code' })).toBeVisible();
  await expect(dialog.locator('td', { hasText: 'bad' }).first()).toBeVisible();
  await expect(dialog.locator('td', { hasText: 'convert_error' })).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(dialog).toHaveCount(0, { timeout: 3000 });

  // ---- 失败节点定位：csv_input 读不存在文件 → 运行时失败
  const p2 = await makePipeline(page, `${tag}-定位`, [
    { id: 'src', type: 'csv_input', name: '表输入', version: 1, config: { path: `mc-e2e-missing-${tag}.csv`, fields: [{ name: 'id', kind: 'integer' }] }, ui: { x: 96, y: 128 } },
    { id: 'dst', type: 'dummy', name: '空操作', version: 1, config: {}, ui: { x: 448, y: 128 } },
  ], [{ id: 'h1', from: 'src', fromPort: 'main', to: 'dst', toPort: 'main' }]);
  await runAndWait(page, p2.id, 'failed');

  await openRunDetail(page, `${tag}-定位`);
  const failRow = page.locator('.table tr', { hasText: '表输入' });
  await expect(failRow.getByText('定位')).toBeVisible();
  await failRow.getByText('定位').click();
  // 设计器打开且该节点被选中
  await expect(page.locator('.react-flow')).toBeVisible({ timeout: 10000 });
  await expect(page.locator('.react-flow__node.selected', { hasText: '表输入' })).toHaveCount(1, { timeout: 5000 });
});
