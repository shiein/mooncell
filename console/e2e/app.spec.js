import { test, expect } from '@playwright/test';

// 关键前端流程的端到端验证(真实 Console + SQLite,浏览器驱动真实前端)。
const ADMIN = { user: 'admin', pass: 'jch@9388' };
const usernameInput = (page) => page.locator('input[autocomplete="username"]');

async function login(page) {
  await page.goto('/');
  await expect(usernameInput(page)).toBeVisible();
  await usernameInput(page).fill(ADMIN.user);
  await page.locator('input[type="password"]').fill(ADMIN.pass);
  await page.getByRole('button', { name: '登录' }).click();
  // 成功的稳定信号:离开登录页(用户名输入框消失)
  await expect(usernameInput(page)).toHaveCount(0, { timeout: 10000 });
}

test('登录页无任何前端绕过入口(演示向导已移除)', async ({ page }) => {
  await page.goto('/');
  await expect(usernameInput(page)).toBeVisible();
  // 不存在演示初始化向导入口;用户名/密码默认不预填(不泄露默认口令)
  await expect(page.getByText(/首次初始化向导|演示/)).toHaveCount(0);
  await expect(usernameInput(page)).toHaveValue('');
  await expect(page.locator('input[type="password"]')).toHaveValue('');
});

test('登录失败显示错误,不进入主壳', async ({ page }) => {
  await page.goto('/');
  await usernameInput(page).fill('admin');
  await page.locator('input[type="password"]').fill('wrong-pass');
  await page.getByRole('button', { name: '登录' }).click();
  await expect(page.getByText(/登录失败|用户名或密码/)).toBeVisible();
  await expect(usernameInput(page)).toBeVisible(); // 仍在登录页
});

test('登录 → 导航到应用列表 → 打开新建应用向导', async ({ page }) => {
  await login(page);
  await page.getByRole('button', { name: '应用 Applications' }).click();
  const create = page.getByRole('button', { name: /新建应用/ });
  await expect(create).toBeVisible();
  await create.click();
  // 向导第一步:选择部署类型(唯一标题)
  await expect(page.getByText('第 1 步 · 选择部署类型')).toBeVisible();
});

test('登录态在刷新后保持(httpOnly 会话)', async ({ page }) => {
  await login(page);
  await page.reload();
  await expect(usernameInput(page)).toHaveCount(0); // 仍在主壳,不回登录页
  await expect(page.getByRole('button', { name: '总览 Overview' })).toBeVisible();
});

test('登出回到登录页', async ({ page }) => {
  await login(page);
  await page.getByTitle('退出登录').click();
  await expect(usernameInput(page)).toBeVisible({ timeout: 8000 });
});

test('新建应用 Runner 按真实能力置灰(pm2 不可用)', async ({ page }) => {
  await login(page);
  await page.getByRole('button', { name: '应用 Applications' }).click();
  await page.getByRole('button', { name: /新建应用/ }).click();
  await expect(page.getByText('第 1 步 · 选择部署类型')).toBeVisible();
  // 选 go-binary(runners: systemd / pm2)→ 下一步到表单
  await page.getByRole('button', { name: /Go Binary/ }).click();
  await page.getByRole('button', { name: '下一步', exact: true }).click();
  // 假 Agent 报 pm2 不可用 → 该 option 置灰禁用;systemd 可用
  await expect(page.locator('option[value="pm2"]')).toBeDisabled();
  await expect(page.locator('option[value="systemd"]')).toBeEnabled();
});

test('真实应用备份接口失败显示错误态(不回退 mock)', async ({ page }) => {
  await login(page);
  // 经 API 建一个真实类型应用(假 Agent 的 backups 端点会 500)
  await page.request.put('/api/data/app/e2e-bak', {
    data: {
      id: 'e2e-bak', name: 'E2E 备份测试', type: 'go-binary', runner: 'systemd',
      status: 'running', version: 'v1', path: '/srv/apps/e2e-bak/app',
      backupKeep: 5, logPaths: ['/srv/apps/e2e-bak/logs/app.log'],
    },
  });
  await page.reload();
  await page.getByRole('button', { name: '应用 Applications' }).click();
  const row = page.getByText('E2E 备份测试');
  await expect(row).toBeVisible();
  await row.click();
  // 进入详情页后点「备份」标签(Tabs 渲染为 button.tab)
  const bakTab = page.locator('button.tab').filter({ hasText: '备份' });
  await expect(bakTab).toBeVisible();
  await bakTab.click();
  // 备份失败态:显示错误提示,不回退/不显示 mock 备份
  await expect(page.getByText(/无法读取真实备份|Agent 备份列表不可用|Agent 不可达/).first()).toBeVisible({ timeout: 8000 });
});
