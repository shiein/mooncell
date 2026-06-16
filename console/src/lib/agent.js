// Mooncell — 实时 Agent 状态 hook
// 经 Console 代理拉取真实 Agent 的能力清单与资源水位;任一失败即回退到 mock 并标记离线,
// 保证 Agent 未运行 / 纯前端 dev 时页面仍 1:1 可用。
import React from 'react';
import { AGENT } from './data.js';
import { getAgentCapabilities, getAgentSystem, getAgentPing, listAgentNodes } from './api.js';

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

// useAgents — 多 Agent 总览:拉取已注册的全部 Agent,对每台并发轮询资源水位 + 能力 + 在线状态。
// 返回 null(加载中)或 [{ id, name, addr, system, caps, online }]。任一台失败只标记该台 online=false,
// 不拖累其它(各自独立 fetch)。Agent 列表拉不到时退化为内置 default 一台,保证纯前端 dev 仍可用。
function useAgents() {
  const [agents, setAgents] = React.useState(null);

  React.useEffect(() => {
    let alive = true;
    let iv = null;
    const patch = (id, p) => setAgents((prev) => prev ? prev.map((a) => a.id === id ? { ...a, ...p } : a) : prev);

    listAgentNodes().then((nodes) => {
      if (!alive) return;
      const list = (nodes && nodes.length) ? nodes : [{ id: 'default', name: '本机 Agent' }];
      setAgents(list.map((n) => ({ id: n.id, name: n.name, addr: n.addr || '', system: null, caps: AGENT.caps, online: null })));

      // 能力清单只需拉一次(不随资源轮询)
      list.forEach((n) => getAgentCapabilities(n.id).then((c) => {
        if (alive && c && c.capabilities) patch(n.id, { caps: c.capabilities });
      }));

      const poll = () => list.forEach((n) => getAgentSystem(n.id).then((s) => {
        if (!alive) return;
        if (s) patch(n.id, { system: s, online: true });
        else patch(n.id, { online: false });
      }));
      poll();
      iv = setInterval(poll, 2500);
    });

    return () => { alive = false; if (iv) clearInterval(iv); };
  }, []);

  return agents;
}

export { useAgent, useAgents };
