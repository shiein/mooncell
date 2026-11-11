// Mooncell — 实时 Agent 状态 hook
//
// 每台 Agent 只有一个共享轮询器（按 id 引用计数）：侧栏、总览等多个组件订阅同一台 Agent 时
// 不会各自发请求。页面隐藏时暂停，重新可见时立即补拉一次。
//
// 取不到就是取不到：caps/system/ping 默认 null，界面显示「未知 / 不可达」，绝不回退示例数据。
import React from 'react';
import { getAgentCapabilities, getAgentSystem, getAgentPing, listAgentNodes } from './api.js';

const POLL_MS = 2500;
const pollers = new Map(); // id -> { subs:Set, state, timer, inflight }

function emptyState() {
  return { system: null, online: null, caps: null, ping: null, lastOkAt: 0 };
}

function setState(p, patch) {
  p.state = { ...p.state, ...patch };
  for (const cb of p.subs) cb(p.state);
}

async function refreshMeta(id, p) {
  const [ping, c] = await Promise.all([getAgentPing(id), getAgentCapabilities(id)]);
  if (pollers.get(id) !== p) return;
  setState(p, {
    ping: ping && ping.ok ? { version: ping.version || '', os: ping.os || '', host: ping.agent || '' } : p.state.ping,
    caps: c && c.capabilities ? c.capabilities : p.state.caps,
    online: ping ? !!ping.ok : false,
  });
}

async function pollOnce(id, p) {
  if (p.inflight || (typeof document !== 'undefined' && document.hidden)) return;
  p.inflight = true;
  try {
    const s = await getAgentSystem(id);
    if (pollers.get(id) !== p) return;
    const wasOffline = p.state.online === false;
    if (s) {
      setState(p, { system: s, online: true, lastOkAt: Date.now() });
      // 从离线恢复：版本/能力可能已变（例如刚自更新），补拉一次
      if (wasOffline || !p.state.caps) refreshMeta(id, p);
    } else {
      setState(p, { online: false });
    }
  } finally {
    p.inflight = false;
  }
}

function subscribe(id, cb) {
  let p = pollers.get(id);
  if (!p) {
    p = { subs: new Set(), state: emptyState(), timer: null, inflight: false };
    pollers.set(id, p);
    refreshMeta(id, p);
    pollOnce(id, p);
    p.timer = setInterval(() => pollOnce(id, p), POLL_MS);
  }
  p.subs.add(cb);
  cb(p.state);
  return () => {
    p.subs.delete(cb);
    if (p.subs.size === 0) {
      clearInterval(p.timer);
      pollers.delete(id);
    }
  };
}

if (typeof document !== 'undefined') {
  document.addEventListener('visibilitychange', () => {
    if (document.hidden) return;
    for (const [id, p] of pollers) pollOnce(id, p);
  });
}

// useAgent — 内置默认 Agent 的状态（侧栏/顶栏）。enabled=false 时不发任何请求（普通用户）。
function useAgent(enabled = true) {
  const [state, setLocal] = React.useState(emptyState);
  React.useEffect(() => {
    if (!enabled) {
      setLocal(emptyState());
      return undefined;
    }
    return subscribe('default', setLocal);
  }, [enabled]);
  return state;
}

// useAgents — 多 Agent 总览：拉取已注册的全部 Agent，逐台订阅共享轮询器。
// 返回 null（加载中）或 [{ id, name, addr, system, caps, online, ping, lastOkAt }]。
// 列表拉不到时以内置 default 一台展示其真实状态，并标记 listError。
function useAgents() {
  const [nodes, setNodes] = React.useState(null);
  const [states, setStates] = React.useState({});
  React.useEffect(() => {
    let alive = true;
    const unsubs = [];
    listAgentNodes().then((list) => {
      if (!alive) return;
      const ns = list && list.length ? list : [{ id: 'default', name: '本机 Agent', addr: '', listError: !list }];
      setNodes(ns);
      for (const n of ns) {
        unsubs.push(subscribe(n.id, (st) => setStates((prev) => ({ ...prev, [n.id]: st }))));
      }
    });
    return () => {
      alive = false;
      unsubs.forEach((u) => u());
    };
  }, []);
  if (!nodes) return null;
  return nodes.map((n) => ({
    id: n.id, name: n.name, addr: n.addr || '', listError: !!n.listError, ...(states[n.id] || emptyState()),
  }));
}

// useAgentNode — 单台 Agent 的注册信息（名称/地址），侧栏展示用；只在 enabled 时请求一次。
function useAgentNode(id, enabled) {
  const [node, setNode] = React.useState(null);
  React.useEffect(() => {
    if (!enabled) return undefined;
    let alive = true;
    listAgentNodes().then((list) => {
      if (alive && list) setNode(list.find((n) => n.id === id) || null);
    });
    return () => { alive = false; };
  }, [id, enabled]);
  return node;
}

export { useAgent, useAgents, useAgentNode };
