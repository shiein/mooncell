// 极简假 Agent:供 E2E 让 Console 有「真实」能力清单与可控错误态。
// 不验 token(E2E 内网);只覆盖 ping/capabilities/system/backups/deploy 几个端点。
import http from 'node:http';

const PORT = Number(process.env.FAKE_AGENT_PORT || 9111);
const json = (res, code, obj) => { res.writeHead(code, { 'content-type': 'application/json' }); res.end(JSON.stringify(obj)); };

const srv = http.createServer((req, res) => {
  const p = new URL(req.url, 'http://x').pathname;
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
  // 备份列表:故意 500,用于验证前端「备份失败态」(不回退 mock)
  if (/\/api\/apps\/.+\/backups$/.test(p)) return json(res, 500, { error: 'fake backend error' });
  // 部署流:消费完 body 后回 SSE done success
  if (/\/api\/apps\/.+\/deploy\/stream$/.test(p)) {
    req.resume();
    req.on('end', () => {
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
