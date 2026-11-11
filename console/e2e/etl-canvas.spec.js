import { test, expect } from '@playwright/test';

// 数据集成设计器画布交互（P2.12）：复制/粘贴、多选、注释、节点/连线右键菜单。
const ADMIN = { user: 'admin', pass: '1qaz@WSX' };
const usernameInput = (page) => page.locator('input[autocomplete="username"]');
const MOD = process.platform === 'darwin' ? 'Meta' : 'Control';

async function login(page) {
  await page.goto('/');
  await usernameInput(page).fill(ADMIN.user);
  await page.locator('input[type="password"]').fill(ADMIN.pass);
  await page.getByRole('button', { name: '登录' }).click();
  await expect(usernameInput(page)).toHaveCount(0, { timeout: 10000 });
}

async function makePipeline(page, name) {
  const doc = {
    format: 1, kind: 'pipeline', params: [],
    nodes: [
      { id: 'src', type: 'generate_rows', name: '生成', version: 1, config: { limit: 3 }, ui: { x: 96, y: 128 } },
      { id: 'dst', type: 'dummy', name: '空操作', version: 1, config: {}, ui: { x: 448, y: 128 } },
    ],
    hops: [{ id: 'h1', from: 'src', fromPort: 'main', to: 'dst', toPort: 'main' }],
    settings: {}, notes: [],
  };
  const resp = await page.request.post('/api/etl/definitions', { data: { kind: 'pipeline', name, doc } });
  if (!resp.ok()) console.log('POST /api/etl/definitions →', resp.status(), await resp.text());
  expect(resp.ok()).toBeTruthy();
  return await resp.json();
}

async function openDesigner(page, name) {
  await page.getByRole('button', { name: '数据集成', exact: true }).click();
  await page.locator('.etl-row', { hasText: name }).first().click();
  await expect(page.locator('.etl-designer')).toBeVisible();
  await expect(page.locator('.etl-node')).toHaveCount(2);
}

const paneBox = async (page) => await page.locator('.etl-canvas').boundingBox();

test('画布：复制/粘贴、多选、注释、右键菜单、连线停用与插入步骤', async ({ page }) => {
  await login(page);
  const name = `画布用例${Date.now() % 100000}`;
  const def = await makePipeline(page, name);
  await openDesigner(page, name);
  const pane = await paneBox(page);

  // ---- 节点右键菜单
  await page.locator('.etl-node', { hasText: '空操作' }).click({ button: 'right' });
  const menu = page.getByRole('menu');
  await expect(menu).toBeVisible();
  await expect(page.getByRole('menuitem', { name: '复制' })).toBeVisible();
  await expect(page.getByRole('menuitem', { name: '创建副本' })).toBeVisible();
  await expect(page.getByRole('menuitem', { name: /删除节点/ })).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(menu).toHaveCount(0);

  // ---- 复制/粘贴（快捷键，跨定义走 localStorage+系统剪贴板）
  await page.locator('.etl-node', { hasText: '生成' }).click();
  await page.keyboard.press(`${MOD}+c`);
  await page.keyboard.press(`${MOD}+v`);
  await expect(page.locator('.etl-node')).toHaveCount(3);

  // ---- 框选多选（左键在空白处拖框，覆盖全部节点）
  await page.mouse.click(pane.x + pane.width - 60, pane.y + 60); // 点空白清除选择并记录粘贴点
  const boxes = await page.locator('.etl-node').evaluateAll((els) => els.map((e) => e.getBoundingClientRect().toJSON()));
  const x0 = Math.min(...boxes.map((b) => b.x)) - 40, y0 = Math.min(...boxes.map((b) => b.y)) - 60;
  const x1 = Math.max(...boxes.map((b) => b.x + b.width)) + 40, y1 = Math.max(...boxes.map((b) => b.y + b.height)) + 60;
  await page.mouse.move(x0, y0); await page.mouse.down();
  await page.mouse.move(x1, y1, { steps: 8 }); await page.mouse.up();
  await expect(page.locator('.etl-node[data-selected="true"]')).toHaveCount(3);
  await page.mouse.click(pane.x + pane.width - 60, pane.y + 60); // 清选区（选区覆盖层会吞连线右键）

  // ---- 连线右键：停用 → 虚线淡化；再插入步骤 → 节点+1 连线+1
  const edgePath = page.locator('.react-flow__edge-path').first();
  const edgeBox = await edgePath.boundingBox();
  const edgeAt = { x: edgeBox.x + edgeBox.width / 2, y: edgeBox.y + edgeBox.height / 2 };
  await page.mouse.click(edgeAt.x, edgeAt.y, { button: 'right' });
  await page.getByRole('menuitem', { name: '停用' }).click();
  await expect(edgePath).toHaveCSS('opacity', '0.45');

  await page.mouse.click(edgeAt.x, edgeAt.y, { button: 'right' });
  await page.getByRole('menuitem', { name: '插入步骤' }).click();
  await page.locator('.etl-menu input.input-sm').fill('值映射');
  await page.locator('.etl-cmenu-item').first().click();
  await expect(page.locator('.etl-node')).toHaveCount(4);
  await expect(page.locator('.react-flow__edge')).toHaveCount(2); // 仅原 1 条连线，插入节点后 1→2

  // ---- 画布右键：添加注释 → 输入文字
  await page.mouse.click(pane.x + 120, pane.y + pane.height - 140, { button: 'right' });
  await page.getByRole('menuitem', { name: '添加注释' }).click();
  await expect(page.locator('.etl-note')).toHaveCount(1);
  await page.locator('.etl-note-text').fill('说明：这是一条画布注释');
  await expect(page.locator('.etl-note-text')).toHaveValue('说明：这是一条画布注释');

  // ---- 节点右键：预览数据 → Drawer 出 3 行
  await page.locator('.etl-node', { hasText: '生成' }).first().click({ button: 'right' });
  await page.getByRole('menuitem', { name: '预览数据' }).click();
  await expect(page.locator('.etl-preview-table tbody tr')).toHaveCount(3, { timeout: 15000 });

  // ---- 保存 → 校验 doc 持久化（notes + 4 节点 + 2 连线）
  await page.keyboard.press(`${MOD}+s`);
  await expect(page.locator('.etl-save-state[data-state]')).toContainText('已保存', { timeout: 10000 });
  const saved = await (await page.request.get(`/api/etl/definitions/${def.id}`)).json();
  expect(saved.doc.nodes.length).toBe(4);
  expect(saved.doc.notes.length).toBe(1);
  expect(saved.doc.notes[0].text).toBe('说明：这是一条画布注释');
  expect(saved.doc.hops.length).toBe(2);
  expect(saved.doc.hops.filter((h) => h.disabled).length).toBe(1);

  // ---- 截图（亮 / 暗）
  await page.screenshot({ path: 'e2e/out/etl-canvas-light.png' });
  await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'dark'));
  await page.waitForTimeout(300);
  await page.screenshot({ path: 'e2e/out/etl-canvas-dark.png' });
});
