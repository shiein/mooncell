// Mooncell — 通用异步数据 hook(四态:loading / ready / error / stale)
// 解决旧模式「失败返回 null 被 `|| []` 折成空数组/空状态」的伪装问题:
//   - loading: 首次拉取中(data=null, error=null)
//   - ready:   拉取成功(data 有值, error=null)
//   - error:   拉取失败(data=null, error 有值,可 retry)
//   - stale:   重拉失败但保留上次成功 data(error 有值, data 仍是旧值)
// fetcher 返回 null/undefined 视为失败(与现有 api.js 约定一致:`!r.ok` 即 return null)。
import React from 'react';

function useAsync(fetcher, deps) {
  const [data, setData] = React.useState(null);
  const [error, setError] = React.useState(null);
  const [loading, setLoading] = React.useState(true);
  const [token, setToken] = React.useState(0);

  React.useEffect(() => {
    let alive = true;
    setLoading(true);
    fetcher()
      .then((d) => {
        if (!alive) return;
        if (d == null) {
          // 约定:null = 失败(非空数组/空数组都是成功)
          setError(new Error('加载失败'));
          setData(null);
        } else {
          setError(null);
          setData(d);
        }
        setLoading(false);
      })
      .catch((e) => {
        if (!alive) return;
        setError(e instanceof Error ? e : new Error(String(e)));
        setLoading(false);
      });
    return () => { alive = false; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...(deps || []), token]);

  const retry = React.useCallback(() => setToken((t) => t + 1), []);
  return { data, error, loading, retry };
}

export { useAsync };
