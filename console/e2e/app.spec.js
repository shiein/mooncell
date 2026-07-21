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
  await page.getByRole('button', { name: '应用', exact: true }).click();
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
  await expect(page.getByRole('button', { name: '总览', exact: true })).toBeVisible();
});

test('登出回到登录页', async ({ page }) => {
  await login(page);
  await page.getByTitle('退出登录').click();
  await expect(usernameInput(page)).toBeVisible({ timeout: 8000 });
});

test('admin 切换普通用户后只展示授权应用且不显示 Agent 功能', async ({ page }) => {
  await login(page);
  for (const app of [
    { id: 'acl-allowed', name: 'ACL 已授权应用' },
    { id: 'acl-denied', name: 'ACL 未授权应用' },
  ]) {
    await page.request.put(`/api/apps/${app.id}/config`, {
      data: { ...app, type: 'native-binary', runner: 'systemd', status: 'stopped', version: 'v1', path: `/srv/apps/${app.id}/app`, backupKeep: 10, logPaths: [] },
    });
  }
  const created = await page.request.post('/api/users', {
    data: { username: 'acl-user', password: 'acl-pass', appIds: ['acl-allowed'] },
  });
  expect(created.ok()).toBeTruthy();

  await page.getByTitle('退出登录').click();
  await usernameInput(page).fill('acl-user');
  await page.locator('input[type="password"]').fill('acl-pass');
  await page.getByRole('button', { name: '登录' }).click();
  await expect(usernameInput(page)).toHaveCount(0);

  await expect(page.getByText('ACL 已授权应用')).toBeVisible();
  await expect(page.getByText('ACL 未授权应用')).toHaveCount(0);
  await expect(page.getByRole('button', { name: /新建应用/ })).toHaveCount(0);
  await expect(page.getByText('本机 Agent')).toHaveCount(0);
  await expect(page.getByText(/Agent 在线|Agent 不可达|Agent 探测中/)).toHaveCount(0);
});

test('新建应用 Runner 按真实能力置灰(pm2 不可用)', async ({ page }) => {
  await login(page);
  await page.getByRole('button', { name: '应用', exact: true }).click();
  await page.getByRole('button', { name: /新建应用/ }).click();
  await expect(page.getByText('第 1 步 · 选择部署类型')).toBeVisible();
  // 选 native-binary(runners: systemd / pm2)→ 下一步到表单
  await page.getByRole('button', { name: /原生二进制/ }).click();
  await page.getByRole('button', { name: '下一步', exact: true }).click();
  // 假 Agent 报 pm2 不可用 → 该 option 置灰禁用;systemd 可用
  await expect(page.locator('option[value="pm2"]')).toBeDisabled();
  await expect(page.locator('option[value="systemd"]')).toBeEnabled();
});

test('Runner 不可用时预检提交首个可用 Runner(systemd 而非 pm2)', async ({ page }) => {
  await login(page);
  await page.getByRole('button', { name: '应用', exact: true }).click();
  await page.getByRole('button', { name: /新建应用/ }).click();
  // node 的 Runner 顺序是 pm2 / systemd;假 Agent 报 pm2 不可用 → 应回落 systemd
  await page.getByRole('button', { name: /Node\.js/ }).click();
  await page.getByRole('button', { name: '下一步', exact: true }).click();
  await page.getByPlaceholder(/数据查询平台后端/).fill('e2e-runner');
  // node 必填:入口文件 + 端口(端口探活默认口径),否则「执行预检」按钮 disabled(requiredMissing 守卫)。
  await page.getByPlaceholder('server.js').fill('server.js');
  await page.getByPlaceholder('8080').fill('8090');
  // 拦截预检请求:UI 不手选 Runner 时,提交的应是 systemd(首个可用)而非 pm2(第一个但不可用)
  const reqPromise = page.waitForRequest((r) => r.url().includes('/api/agent/precheck'));
  await page.getByRole('button', { name: '执行预检' }).click();
  const req = await reqPromise;
  expect(req.url()).toContain('runner=systemd');
  expect(req.url()).not.toContain('runner=pm2');
});

test('tomcat-war 的 tomcat Runner 置灰(caps 缺 tomcat key → fail-closed)', async ({ page }) => {
  await login(page);
  await page.getByRole('button', { name: '应用', exact: true }).click();
  await page.getByRole('button', { name: /新建应用/ }).click();
  await page.getByRole('button', { name: /Tomcat WAR/ }).click();
  await page.getByRole('button', { name: '下一步', exact: true }).click();
  // 假 Agent 能力清单不含 tomcat key → fail-closed:tomcat Runner option 置灰(不再恒可用)
  await expect(page.locator('option[value="tomcat"]')).toBeDisabled();
});

test('切换类型后陈旧 Runner 自动纠正(systemd → 软链)', async ({ page }) => {
  await login(page);
  await page.getByRole('button', { name: '应用', exact: true }).click();
  await page.getByRole('button', { name: /新建应用/ }).click();
  // 先选 native-binary,手动选 systemd(可用)
  await page.getByRole('button', { name: /原生二进制/ }).click();
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
    data: { id: 'e2e-dep', name: 'E2E 部署测试', type: 'native-binary', runner: 'systemd', status: 'running', version: 'v1', path: '/srv/apps/e2e-dep/app', backupKeep: 5, logPaths: [] },
  });
  await page.reload();
  await page.getByRole('button', { name: '应用', exact: true }).click();
  await page.getByText('E2E 部署测试').click();
  await page.getByRole('button', { name: /部署新版本/ }).click();
  await expect(page.getByText(/拖拽制品到此处/)).toBeVisible();
  await expect(page.getByRole('button', { name: '使用示例制品演示' })).toHaveCount(0);
});

test('配置页 Runner 按 Agent 能力置灰(pm2 不可用)', async ({ page }) => {
  await login(page);
  await page.request.put('/api/apps/e2e-cfg/config', {
    data: { id: 'e2e-cfg', name: 'E2E 配置测试', type: 'native-binary', runner: 'systemd', status: 'running', version: 'v1', path: '/srv/apps/e2e-cfg/app', backupKeep: 5, logPaths: [] },
  });
  await page.reload();
  await page.getByRole('button', { name: '应用', exact: true }).click();
  await page.getByText('E2E 配置测试').click();
  await page.locator('button.tab').filter({ hasText: '配置' }).click();
  await page.getByRole('button', { name: /编辑/ }).click();
  await expect(page.locator('option[value="pm2"]')).toBeDisabled();
});

test('配置页编辑端口保存成功(数值归一化;备份保留固定 10 只读)', async ({ page }) => {
  await login(page);
  await page.request.put('/api/apps/e2e-save/config', {
    data: { id: 'e2e-save', name: 'E2E 保存测试', type: 'native-binary', runner: 'systemd', status: 'running', version: 'v1', path: '/srv/apps/e2e-save/app', port: 8080, backupKeep: 10, logPaths: [] },
  });
  await page.reload();
  await page.getByRole('button', { name: '应用', exact: true }).click();
  await page.getByText('E2E 保存测试').click();
  await page.locator('button.tab').filter({ hasText: '配置' }).click();
  await page.getByRole('button', { name: /编辑/ }).click();
  // 备份保留份数固定 10、只读;改端口应归一化为数值并保存成功
  const keepInput = page.locator('xpath=//label[contains(@class,"field-label") and contains(.,"备份保留份数")]/following-sibling::input[1]');
  await expect(keepInput).toBeDisabled();
  await expect(keepInput).toHaveValue('10');
  const portInput = page.locator('xpath=//label[contains(@class,"field-label") and contains(.,"端口")]/following-sibling::input[1]');
  await portInput.fill('9090');
  await page.getByRole('button', { name: /保存配置/ }).click();
  await expect(page.getByText('配置已保存')).toBeVisible({ timeout: 8000 });
});

test('tomcat 不显示启停按钮(容器托管,无 systemd 单元)', async ({ page }) => {
  await login(page);
  await page.request.put('/api/apps/e2e-tc/config', {
    data: { id: 'e2e-tc', name: 'E2E Tomcat', type: 'tomcat-war', runner: 'tomcat', status: 'running', version: 'v1', path: '/opt/tomcat/webapps/app.war', backupKeep: 5, logPaths: [] },
  });
  await page.reload();
  await page.getByRole('button', { name: '应用', exact: true }).click();
  await page.getByText('E2E Tomcat').click();
  // tomcat 无 systemd 单元,点启停必 500 → 不应显示启停按钮
  await expect(page.getByRole('button', { name: '停止', exact: true })).toHaveCount(0);
  await expect(page.getByRole('button', { name: '启动', exact: true })).toHaveCount(0);
  // 但「部署新版本」仍应有
  await expect(page.getByRole('button', { name: /部署新版本/ })).toBeVisible();
});

test('真实应用日志流失败显示错误态(不伪造模拟日志)', async ({ page }) => {
  await login(page);
  await page.request.put('/api/apps/e2e-log/config', {
    data: { id: 'e2e-log', name: 'E2E 日志测试', type: 'native-binary', runner: 'systemd', status: 'running', version: 'v1', path: '/srv/apps/e2e-log/app', backupKeep: 5, logPaths: [] },
  });
  await page.reload();
  await page.getByRole('button', { name: '应用', exact: true }).click();
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
      id: 'e2e-bak', name: 'E2E 备份测试', type: 'native-binary', runner: 'systemd',
      status: 'running', version: 'v1', path: '/srv/apps/e2e-bak/app',
      backupKeep: 5, logPaths: ['/srv/apps/e2e-bak/logs/app.log'],
    },
  });
  await page.reload();
  await page.getByRole('button', { name: '应用', exact: true }).click();
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

test('真实部署结束后主动刷新备份列表与角标', async ({ page }) => {
  await login(page);
  await page.request.put('/api/apps/e2e-refresh/config', {
    data: { id: 'e2e-refresh', name: 'E2E 备份刷新', type: 'native-binary', runner: 'systemd', status: 'running', version: 'v0', path: '/srv/apps/e2e-refresh/app', backupKeep: 10, logPaths: [] },
  });
  await page.reload();
  await page.getByRole('button', { name: '应用', exact: true }).click();
  await page.getByText('E2E 备份刷新').click();
  const bakTab = page.locator('button.tab').filter({ hasText: '备份' });
  await bakTab.click();
  await expect(page.getByText('v-old')).toBeVisible();

  await page.getByRole('button', { name: /部署新版本/ }).click();
  await page.locator('input[type="file"]').setInputFiles({ name: 'app.bin', mimeType: 'application/octet-stream', buffer: Buffer.from('e2e-artifact') });
  await page.getByRole('button', { name: /开始部署/ }).click();
  await expect(page.getByText('部署成功 · v1 已上线', { exact: true })).toBeVisible({ timeout: 8000 });
  await page.getByText('关闭', { exact: true }).click();

  await expect(page.getByText('v-new')).toBeVisible({ timeout: 8000 });
  await expect(bakTab).toContainText('2');
});

// 回归:static-nginx 新建必须真正落库、刷新后仍在。曾因两处叠加变成「假建」——
// (1) 向导对 static 默认 healthType=端口探活,被后端 validateAppConfig 拒绝(static 无独立进程,端口探活须有效端口);
// (2) addApp 乐观 fire-and-forget,不 await 落库结果就 toast「创建成功」并跳转,刷新后应用消失。
test('新建 static-nginx 应用真正落库、刷新后仍在(默认不做端口探活)', async ({ page }) => {
  await login(page);
  await page.getByRole('button', { name: '应用', exact: true }).click();
  await page.getByRole('button', { name: /新建应用/ }).click();
  // 第 1 步:选 Static / Nginx(runner 仅「软链」,无运行时依赖)
  await page.getByRole('button', { name: /Static \/ Nginx/ }).click();
  await page.getByRole('button', { name: '下一步', exact: true }).click();
  // 第 2 步:应用名 + 目标目录(static 必填,否则「执行预检」按钮 disabled)
  await page.locator('xpath=//label[contains(@class,"field-label") and contains(.,"应用名")]/following-sibling::input[1]').fill('E2E 静态站点');
  await page.locator('xpath=//label[contains(@class,"field-label") and contains(.,"目标目录")]/following-sibling::input[1]').fill('/data/web/e2e-static');
  await page.getByRole('button', { name: '执行预检' }).click();
  // 第 3 步:fake-agent 预检恒通过 → 创建应用
  await page.getByRole('button', { name: '创建应用' }).click();
  await expect(page.getByText(/创建成功/)).toBeVisible({ timeout: 8000 });
  // 关键回归:刷新后应用仍在列表——证明真的落库了,不是「假建」。
  // 创建成功会跳到详情页,刷新后面包屑与侧栏都有「应用」,用侧栏导航消歧。
  await page.reload();
  await page.getByRole('navigation').getByRole('button', { name: '应用', exact: true }).click();
  await expect(page.getByText('E2E 静态站点')).toBeVisible({ timeout: 8000 });
});
