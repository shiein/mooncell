import { defineConfig } from '@playwright/test';

// 前端全量 E2E:用真实 Console 二进制(go:embed 的前端 + SQLite 后端)起服务,
// 浏览器驱动真实前端跑关键流程(登录→导航→建应用→列表→登出)。
// Console 二进制由 e2e/run-console.sh 在临时目录起(全新空库),loopback + 默认管理员凭据。
const PORT = 8765;

export default defineConfig({
  testDir: './e2e',
  timeout: 30000,
  expect: { timeout: 8000 },
  fullyParallel: false,
  retries: 0,
  reporter: [['list']],
  use: {
    baseURL: `http://127.0.0.1:${PORT}`,
    headless: true,
    actionTimeout: 8000,
  },
  webServer: {
    command: `sh e2e/run-console.sh ${PORT}`,
    url: `http://127.0.0.1:${PORT}/api/session`,
    reuseExistingServer: true,
    timeout: 30000,
  },
});
