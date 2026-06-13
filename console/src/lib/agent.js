// Mooncell — 实时 Agent 状态 hook
// 经 Console 代理拉取真实 Agent 的能力清单与资源水位;任一失败即回退到 mock 并标记离线,
// 保证 Agent 未运行 / 纯前端 dev 时页面仍 1:1 可用。
import React from 'react';
import { AGENT } from './data.js';
import { getAgentCapabilities, getAgentSystem, getAgentPing } from './api.js';

function useAgent() {
  const [caps, setCaps] = React.useState(AGENT.caps); // 默认 mock,取到真实后覆盖
  const [system, setSystem] = React.useState(null);   // {cpuPercent, memPercent, disk...}
  const [online, setOnline] = React.useState(null);   // null=探测中, true/false

  React.useEffect(() => {
    let alive = true;
    Promise.all([getAgentPing(), getAgentCapabilities()]).then(([ping, c]) => {
      if (!alive) return;
      setOnline(!!ping);
      if (c && c.capabilities) setCaps(c.capabilities);
    });
    const poll = () => getAgentSystem().then((s) => {
      if (!alive) return;
      if (s) { setSystem(s); setOnline(true); } else setOnline(false);
    });
    poll();
    const iv = setInterval(poll, 2500);
    return () => { alive = false; clearInterval(iv); };
  }, []);

  return { caps, system, online };
}

export { useAgent };
