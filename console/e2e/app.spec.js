import { test, expect } from '@playwright/test';

// 关键前端流程的端到端验证(真实 Console + SQLite,浏览器驱动真实前端)。
const ADMIN = { user: 'admin', pass: '1qaz@WSX' };
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

test('Runner 不可用时预检提交首个可用 Runner(systemd 而非 pm2)', async ({ page }) => {
  await login(page);
  await page.getByRole('button', { name: '应用 Applications' }).click();
  await page.getByRole('button', { name: /新建应用/ }).click();
  // node 的 Runner 顺序是 pm2 / systemd;假 Agent 报 pm2 不可用 → 应回落 systemd
  await page.getByRole('button', { name: /Node\.js/ }).click();
  await page.getByRole('button', { name: '下一步', exact: true }).click();
  await page.getByPlaceholder(/数据查询平台后端/).fill('e2e-runner');
  // 拦截预检请求:UI 不手选 Runner 时,提交的应是 systemd(首个可用)而非 pm2(第一个但不可用)
  const reqPromise = page.waitForRequest((r) => r.url().includes('/api/agent/precheck'));
  await page.getByRole('button', { name: '执行预检' }).click();
  const req = await reqPromise;
  expect(req.url()).toContain('runner=systemd');
  expect(req.url()).not.toContain('runner=pm2');
});

test('tomcat-war 的 tomcat Runner 置灰(caps 缺 tomcat key → fail-closed)', async ({ page }) => {
  await login(page);
  await page.getByRole('button', { name: '应用 Applications' }).click();
  await page.getByRole('button', { name: /新建应用/ }).click();
  await page.getByRole('button', { name: /Tomcat WAR/ }).click();
  await page.getByRole('button', { name: '下一步', exact: true }).click();
  // 假 Agent 能力清单不含 tomcat key → fail-closed:tomcat Runner option 置灰(不再恒可用)
  await expect(page.locator('option[value="tomcat"]')).toBeDisabled();
});

test('切换类型后陈旧 Runner 自动纠正(systemd → 软链)', async ({ page }) => {
  await login(page);
  await page.getByRole('button', { name: '应用 Applications' }).click();
  await page.getByRole('button', { name: /新建应用/ }).click();
  // 先选 go-binary,手动选 systemd(可用)
  await page.getByRole('button', { name: /Go Binary/ }).click();
  await page.getByRole('button', { name: '下一步', exact: true }).click();
  const runnerSel = page.locator('select').filter({ has: page.locator('option[value="systemd"]') });
  await runnerSel.selectOption('systemd');
  await expect(runnerSel).toHaveValue('systemd');
  // 退回选 static-nginx(runner 仅 软链)→ 陈旧的 systemd 应被自动清空、回落 软链
  await page.getByRole('button', { name: '上一步', exact: true }).click();
  await page.getByRole('button', { name: /Static \/ Nginx/ }).click();
  await page.getByRole('button', { name: '下一步', exact: true }).click();
  const staticSel = page.locator('select').filter({ has: page.locator('option[value="软链"]') });
  await expect(staticSel).toHaveValue('软链');
});

test('真实应用部署弹窗无"示例制品演示"入口(必须真实文件)', async ({ page }) => {
  await login(page);
  await page.request.put('/api/apps/e2e-dep/config', {
    data: { id: 'e2e-dep', name: 'E2E 部署测试', type: 'go-binary', runner: 'systemd', status: 'running', version: 'v1', path: '/srv/apps/e2e-dep/app', backupKeep: 5, logPaths: [] },
  });
  await page.reload();
  await page.getByRole('button', { name: '应用 Applications' }).click();
  await page.getByText('E2E 部署测试').click();
  await page.getByRole('button', { name: /部署新版本/ }).click();
  await expect(page.getByText(/拖拽制品到此处/)).toBeVisible();
  await expect(page.getByRole('button', { name: '使用示例制品演示' })).toHaveCount(0);
});

test('配置页 Runner 按 Agent 能力置灰(pm2 不可用)', async ({ page }) => {
  await login(page);
  await page.request.put('/api/apps/e2e-cfg/config', {
    data: { id: 'e2e-cfg', name: 'E2E 配置测试', type: 'go-binary', runner: 'systemd', status: 'running', version: 'v1', path: '/srv/apps/e2e-cfg/app', backupKeep: 5, logPaths: [] },
  });
  await page.reload();
  await page.getByRole('button', { name: '应用 Applications' }).click();
  await page.getByText('E2E 配置测试').click();
  await page.locator('button.tab').filter({ hasText: '配置' }).click();
  await page.getByRole('button', { name: /编辑/ }).click();
  await expect(page.locator('option[value="pm2"]')).toBeDisabled();
});

test('真实应用日志流失败显示错误态(不伪造模拟日志)', async ({ page }) => {
  await login(page);
  await page.request.put('/api/apps/e2e-log/config', {
    data: { id: 'e2e-log', name: 'E2E 日志测试', type: 'go-binary', runner: 'systemd', status: 'running', version: 'v1', path: '/srv/apps/e2e-log/app', backupKeep: 5, logPaths: [] },
  });
  await page.reload();
  await page.getByRole('button', { name: '应用 Applications' }).click();
  await page.getByText('E2E 日志测试').click();
  await page.locator('button.tab').filter({ hasText: '实时日志' }).click();
  // 假 Agent 无 logs/stream 端点 → 流失败 → 错误态 + 重试,不出现模拟日志
  await expect(page.getByText('无法读取实时日志')).toBeVisible({ timeout: 8000 });
  await expect(page.getByRole('button', { name: /重试/ })).toBeVisible();
});

test('真实应用备份接口失败显示错误态(不回退 mock)', async ({ page }) => {
  await login(page);
  // 经 API 建一个真实类型应用(假 Agent 的 backups 端点会 500)
  await page.request.put('/api/apps/e2e-bak/config', {
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
