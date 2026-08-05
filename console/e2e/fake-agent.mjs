// 极简假 Agent:供 E2E 让 Console 有「真实」能力清单与可控错误态。
// 校验与 E2E Console 配置一致的 token,覆盖 ping/capabilities/system/backups/deploy 几个端点。
import http from 'node:http';

const PORT = Number(process.env.FAKE_AGENT_PORT || 9111);
const json = (res, code, obj) => { res.writeHead(code, { 'content-type': 'application/json' }); res.end(JSON.stringify(obj)); };
const backupCounts = new Map([['e2e-refresh', 1]]);

const backupsOf = (id) => Array.from({ length: backupCounts.get(id) || 0 }, (_, i) => ({
  dir: `20260722_08000${i}.000000001`, version: i === 0 ? 'v-old' : 'v-new',
  time: 1784678400000 + i * 1000, size: 1024 + i,
}));

const srv = http.createServer((req, res) => {
  const p = new URL(req.url, 'http://x').pathname;
  if (req.headers.authorization !== 'Bearer tok') return json(res, 401, { error: 'token 校验失败' });
  if (p === '/api/ping') return json(res, 200, { ok: true });
  if (p === '/api/capabilities') return json(res, 200, {
    capabilities: [
      { key: 'systemd', label: 'systemd', ok: true, ver: '255' },
      { key: 'pm2', label: 'pm2', ok: false, ver: '未检测到' },
      { key: 'java', label: 'Java', ok: false, ver: '未检测到' },
      { key: 'node', label: 'Node', ok: false, ver: '未检测到' },
      { key: 'python', label: 'Python', ok: true, ver: '3.12' },
      { key: 'nginx', label: 'nginx', ok: false, ver: '未检测到' },
      // 故意不含 tomcat:验证 UI 对「caps 已加载但缺该 key」的 fail-closed(置灰)处理。
    ],
  });
  if (p === '/api/system') return json(res, 200, { cpuPercent: 10, memPercent: 20, disk: { usedPercent: 30 } });
  if (p === '/api/precheck') return json(res, 200, { checks: [{ label: '目标目录可写', ok: true, detail: '/srv/apps' }] });
  // e2e-bak 故意 500 验证失败态；e2e-refresh 返回可变列表验证部署后的主动刷新。
  const backupMatch = p.match(/^\/api\/apps\/([^/]+)\/backups$/);
  if (backupMatch) {
    if (backupMatch[1] === 'e2e-bak') return json(res, 500, { error: 'fake backend error' });
    return json(res, 200, { backups: backupsOf(backupMatch[1]) });
  }
  // 部署流:消费完 body 后回 SSE done success
  if (/\/api\/apps\/.+\/deploy\/stream$/.test(p)) {
    const appId = p.split('/')[3];
    req.resume();
    req.on('end', () => {
      if (backupCounts.has(appId)) backupCounts.set(appId, (backupCounts.get(appId) || 0) + 1);
      res.writeHead(200, { 'content-type': 'text/event-stream' });
      res.write('event: step\ndata: {"name":"替换制品","ok":true}\n\n');
      res.write('event: done\ndata: {"result":"success","version":"v1","steps":[{"name":"替换制品","ok":true}]}\n\n');
      res.end();
    });
    return;
  }
  json(res, 404, { error: 'not found' });
});
srv.listen(PORT, '127.0.0.1', () => console.log('[fake-agent] on', PORT));
