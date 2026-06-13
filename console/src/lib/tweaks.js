// Mooncell — 轻量 useTweaks
// 原型自带的 TweaksPanel 是托管编辑器的悬浮调试面板(向 window.parent postMessage),
// 正常运行时永不显示,属于编辑器 chrome,迁移时丢弃。这里保留 App 真正用到的部分:
// 暗色模式 + 日志字号,落地为本地状态并持久化到 localStorage。
import React from 'react';

export function useTweaks(defaults) {
  const [values, setValues] = React.useState(() => {
    try {
      const saved = JSON.parse(localStorage.getItem('mc_tweaks') || '{}');
      return { ...defaults, ...saved };
    } catch (e) {
      return defaults;
    }
  });
  const setTweak = React.useCallback((key, val) => {
    setValues((prev) => {
      const next = { ...prev, [key]: val };
      try { localStorage.setItem('mc_tweaks', JSON.stringify(next)); } catch (e) {}
      return next;
    });
  }, []);
  return [values, setTweak];
}
