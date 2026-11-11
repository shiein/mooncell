import { test, expect } from '@playwright/test';
import fs from 'node:fs';

// 数据集成端到端（P3.19）：
// ① 转换全链路：新建连接 → 新建转换 → 拖步骤 → 连线 → 校验 → 预览 → 保存 → 运行 → 详情成功。
//    连接走 UI 表单流程（SQLite 是内部测试方言，API 刻意拒绝 UI 创建；外部库不可用时数据通路用
//    无需连接的「生成记录 → CSV 输出」，落盘文件即真实结果）。
// ② 作业条件分支运行 + 调度保存与 cron 预览。
const ADMIN = { user: 'admin', pass: '1qaz@WSX' };
const usernameInput = (page) => page.locator('input[autocomplete="username"]');

async function login(page) {
  await page.goto('/');
  await usernameInput(page).fill(ADMIN.user);
  await page.locator('input[type="password"]').fill(ADMIN.pass);
  await page.getByRole('button', { name: '登录' }).click();
  await expect(usernameInput(page)).toHaveCount(0, { timeout: 10000 });
}

const nav = (page, name) => page.getByRole('navigation').getByRole('button', { name, exact: true }).click();

async function dragStep(page, label, x, y) {
  const item = page.locator('.etl-palette-item', { hasText: label }).first();
  await expect(item).toBeVisible();
  await item.dragTo(page.locator('.etl-canvas'), { targetPosition: { x, y } });
}

async function connect(page, fromName, toName) {
  const src = page.locator('.etl-node', { hasText: fromName }).locator('xpath=ancestor::div[contains(@class,"react-flow__node")]');
  const dst = page.locator('.etl-node', { hasText: toName }).locator('xpath=ancestor::div[contains(@class,"react-flow__node")]');
  const sh = src.locator('.react-flow__handle-right').first();
  const th = dst.locator('.react-flow__handle-left').first();
  const sb = await sh.boundingBox(), tb = await th.boundingBox();
  await page.mouse.move(sb.x + sb.width / 2, sb.y + sb.height / 2);
  await page.mouse.down();
  await page.mouse.move(tb.x + tb.width / 2, tb.y + tb.height / 2, { steps: 12 });
  await page.mouse.up();
}

test('转换全链路：连接→拖步骤→连线→校验→预览→保存→运行→详情成功', async ({ page }) => {
  await login(page);
  const tag = `e2e-${Date.now() % 100000}`;
  const csvPath = `/tmp/mc-${tag}.csv`;

  // ---- 新建连接（UI 表单流程；保存不强制连通性，测试连接是独立按钮）
  await nav(page, '数据集成');
  await page.getByRole('button', { name: '连接', exact: true }).click();
  await page.getByRole('button', { name: '新建连接' }).first().click();
  const dlg = page.getByRole('dialog');
  await dlg.locator('input.input').first().fill(`e2e-conn-${tag}`);
  await dlg.locator('select.select-el').first().selectOption({ label: 'PostgreSQL' });
  await dlg.getByText('主机', { exact: true }).locator('xpath=following::input[1]').fill('127.0.0.1');
  await dlg.getByText('端口', { exact: true }).locator('xpath=following::input[1]').fill('5432');
  await dlg.getByText('数据库', { exact: true }).locator('xpath=following::input[1]').fill('demo');
  await dlg.getByText('用户名', { exact: true }).locator('xpath=following::input[1]').fill('etl');
  await dlg.getByText('密码', { exact: true }).locator('xpath=following::input[1]').fill('secret');
  await dlg.getByRole('button', { name: '保存' }).click();
  await expect(page.locator('.card.etl-conn', { hasText: `e2e-conn-${tag}` })).toBeVisible({ timeout: 8000 });

  // ---- 新建转换 → 设计器
  await page.getByRole('button', { name: '工程', exact: true }).click();
  await page.getByRole('button', { name: '新建', exact: true }).click();
  await page.getByRole('menuitem', { name: '新建转换' }).click();
  await page.getByRole('dialog').locator('input.input').fill(`e2e-转换-${tag}`);
  await page.getByRole('button', { name: '创建并打开' }).click();
  await expect(page.locator('.etl-designer')).toBeVisible({ timeout: 10000 });

  // ---- 拖入 生成记录 + CSV 输出并连线
  await dragStep(page, '生成记录', 140, 140);
  await dragStep(page, 'CSV 输出', 480, 140);
  await expect(page.locator('.etl-node')).toHaveCount(2);
  await connect(page, '生成记录', 'CSV 输出');
  await expect(page.locator('.react-flow__edge')).toHaveCount(1);

  // ---- 配置 生成记录：行数默认 10 + 序号字段 seq
  await page.locator('.etl-node', { hasText: '生成记录' }).click();
  const insp = page.locator('.etl-inspector');
  await insp.locator('input[placeholder="如 seq"]').fill('seq');

  // ---- 配置 CSV 输出：文件路径
  await page.locator('.etl-node', { hasText: 'CSV 输出' }).click();
  await page.locator('.etl-inspector input.mono').first().fill(csvPath);

  // ---- 校验通过（防抖自动校验）
  await expect(page.getByText('校验通过').first()).toBeVisible({ timeout: 15000 });

  // ---- 预览：右键 生成记录 → 预览数据 → 抽屉出现 seq 列数据
  await page.locator('.etl-node', { hasText: '生成记录' }).click({ button: 'right' });
  await page.getByRole('menuitem', { name: '预览数据' }).click();
  const drawer = page.locator('.etl-drawer');
  await expect(drawer).toBeVisible({ timeout: 10000 });
  await expect(drawer.getByText('seq').first()).toBeVisible();
  await expect(drawer.getByText('10').first()).toBeVisible();

  // ---- 保存
  await page.getByRole('button', { name: '保存', exact: true }).click();
  await expect(page.locator('.etl-save-state[data-state="saved"], .etl-save-state[data-state="clean"]')).toBeVisible({ timeout: 8000 });

  // ---- 运行（无参数直接跑）
  await page.getByRole('button', { name: '运行', exact: true }).first().click();
  await expect(page.locator('.etl-node', { hasText: 'CSV 输出' })).toHaveAttribute('data-status', 'succeeded', { timeout: 20000 });

  // ---- 运行详情显示成功（设计器面板遮挡侧栏，走返回按钮回列表）
  await page.getByTitle('返回数据集成').click();
  await page.getByRole('button', { name: '运行记录' }).click();
  await page.locator('.etl-row', { hasText: `e2e-转换-${tag}` }).first().click();
  await expect(page.locator('.etl-run-head')).toBeVisible({ timeout: 10000 });
  await expect(page.locator('.etl-run-head')).toContainText('成功');

  // ---- 产物：csv 落盘且内容正确（表头 seq + 10 行）
  expect(fs.existsSync(csvPath)).toBeTruthy();
  const body = fs.readFileSync(csvPath, 'utf8');
  expect(body).toContain('seq');
  expect(body.trim().split('\n').length).toBe(11);
});

test('作业条件分支运行 + 调度保存与 cron 预览', async ({ page }) => {
  await login(page);
  const tag = `e2e-wf-${Date.now() % 100000}`;

  // ---- API 建作业：set_vars(n=10) → condition(n>5) → true→成功A / false→成功B
  const doc = {
    format: 1, kind: 'workflow', params: [],
    nodes: [
      { id: 'start', type: 'start', name: '开始', version: 1, config: {}, ui: { x: 80, y: 140 } },
      { id: 'sv', type: 'set_vars', name: '设变量', version: 1, config: { assign: [{ var: 'n', kind: 'integer', expr: '10' }] }, ui: { x: 320, y: 140 } },
      { id: 'cond', type: 'condition', name: '判断', version: 1, config: { expr: 'n > 5' }, ui: { x: 560, y: 140 } },
      { id: 'okA', type: 'success', name: '是分支', version: 1, config: {}, ui: { x: 800, y: 60 } },
      { id: 'okB', type: 'success', name: '否分支', version: 1, config: {}, ui: { x: 800, y: 220 } },
    ],
    hops: [
      { id: 'h1', from: 'start', fromPort: 'success', to: 'sv', toPort: 'main' },
      { id: 'h2', from: 'sv', fromPort: 'success', to: 'cond', toPort: 'main' },
      { id: 'h3', from: 'cond', fromPort: 'true', to: 'okA', toPort: 'main' },
      { id: 'h4', from: 'cond', fromPort: 'false', to: 'okB', toPort: 'main' },
    ],
    settings: {}, notes: [],
  };
  const resp = await page.request.post('/api/etl/definitions', { data: { kind: 'workflow', name: `e2e-作业-${tag}`, doc } });
  expect(resp.ok()).toBeTruthy();
  const wf = await resp.json();

  // ---- 运行并等结束
  const run = await (await page.request.post(`/api/etl/definitions/${wf.id}/run`, { data: { params: {} } })).json();
  await expect.poll(async () => (await (await page.request.get(`/api/etl/runs/${run.id}`)).json()).run.status, { timeout: 20000 }).toBe('succeeded');

  // ---- 运行详情：是分支 succeeded、否分支 skipped
  await nav(page, '数据集成');
  await page.getByRole('button', { name: '运行记录' }).click();
  await page.locator('.etl-row', { hasText: `e2e-作业-${tag}` }).first().click();
  await expect(page.locator('.etl-run-head')).toBeVisible({ timeout: 10000 });
  const rowA = page.locator('.etl-gantt-row', { hasText: '是分支' });
  const rowB = page.locator('.etl-gantt-row', { hasText: '否分支' });
  await expect(rowA).toContainText('成功');
  await expect(rowB).toContainText('跳过');

  // ---- 调度：新建 → 选作业 → 预设 cron → 预览 → 保存（运行详情是独立视图，先回列表）
  await nav(page, '数据集成');
  await page.getByRole('button', { name: '调度', exact: true }).click();
  await page.getByRole('button', { name: '新建调度' }).click();
  const sd = page.getByRole('dialog');
  await sd.locator('select.select-el').first().selectOption({ label: `作业 · e2e-作业-${tag}` });
  await sd.locator('.etl-cron-preset', { hasText: '每天 02:00' }).click();
  await expect(sd.locator('.etl-cron-desc')).not.toBeEmpty({ timeout: 8000 });
  await expect(sd.locator('.etl-cron-next span')).toHaveCount(5, { timeout: 8000 });
  await sd.getByRole('button', { name: '保存' }).click();
  await expect(page.locator('tr', { hasText: '0 2 * * *' }).filter({ hasText: `e2e-作业-${tag}` })).toBeVisible({ timeout: 8000 });
});
