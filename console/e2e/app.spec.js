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
