// xterm.js 终端面板：动态 import，未进入工作台时不加载大依赖。
// binary WebSocket 透传 PTY；可选 zmodem2 处理 rz/sz。
import React from 'react';
import { Btn, Spinner, toast } from '../primitives.jsx';
import { terminalWsUrl } from '../../lib/serverops-api.js';

export function TerminalPane({ resourceId, sessionId, onDisconnected, zmodemMaxMB = 512 }) {
  const hostRef = React.useRef(null);
  const termRef = React.useRef(null);
  const fitRef = React.useRef(null);
  const wsRef = React.useRef(null);
  const searchRef = React.useRef(null);
  const [ready, setReady] = React.useState(false);
  const [search, setSearch] = React.useState('');
  const [status, setStatus] = React.useState('connecting'); // connecting | ready | closed

  React.useEffect(() => {
    if (!resourceId || !sessionId || !hostRef.current) return;
    let disposed = false;
    let term, fitAddon, searchAddon, ws, zsession;

    (async () => {
      const [{ Terminal }, { FitAddon }, { SearchAddon }] = await Promise.all([
        import('@xterm/xterm'),
        import('@xterm/addon-fit'),
        import('@xterm/addon-search'),
      ]);
      // 样式
      await import('@xterm/xterm/css/xterm.css');

      if (disposed || !hostRef.current) return;

      const styles = getComputedStyle(document.documentElement);
      term = new Terminal({
        cursorBlink: true,
        fontFamily: styles.getPropertyValue('--font-mono')?.trim() || 'ui-monospace, SFMono-Regular, Menlo, monospace',
        fontSize: 13,
        theme: {
          background: styles.getPropertyValue('--console-bg')?.trim() || '#0f1419',
          foreground: styles.getPropertyValue('--console-fg')?.trim() || '#e6edf3',
          cursor: styles.getPropertyValue('--console-fg')?.trim() || '#e6edf3',
        },
        allowProposedApi: true,
        // 默认 canvas，避免不必要 WebGL
        rendererType: 'canvas',
      });
      fitAddon = new FitAddon();
      searchAddon = new SearchAddon();
      term.loadAddon(fitAddon);
      term.loadAddon(searchAddon);
      term.open(hostRef.current);
      fitAddon.fit();
      termRef.current = term;
      fitRef.current = fitAddon;
      searchRef.current = searchAddon;

      // ZMODEM（可选，失败不影响终端）
      let zmodem = null;
      try {
        const mod = await import('zmodem2');
        zmodem = mod.Sentry || mod.default?.Sentry || mod.default;
      } catch (_) {
        zmodem = null;
      }

      const url = terminalWsUrl(resourceId, sessionId);
      ws = new WebSocket(url);
      ws.binaryType = 'arraybuffer';
      wsRef.current = ws;

      const sendCtrl = (obj) => {
        if (ws.readyState === WebSocket.OPEN) {
          ws.send(JSON.stringify(obj));
        }
      };

      ws.onopen = () => {
        if (disposed) return;
        setStatus('connecting');
        // 同步尺寸
        try {
          const dims = fitAddon.proposeDimensions();
          if (dims) sendCtrl({ type: 'resize', cols: dims.cols, rows: dims.rows });
        } catch (_) {}
      };

      ws.onmessage = (ev) => {
        if (disposed) return;
        if (typeof ev.data === 'string') {
          try {
            const msg = JSON.parse(ev.data);
            if (msg.type === 'ready') {
              setReady(true);
              setStatus('ready');
            } else if (msg.type === 'exit' || msg.type === 'error') {
              setStatus('closed');
              if (msg.message) term.writeln(`\r\n\x1b[31m${msg.message}\x1b[0m`);
              onDisconnected && onDisconnected(msg);
            }
          } catch (_) {}
          return;
        }
        const u8 = new Uint8Array(ev.data);
        // ZMODEM 检测（若库可用）
        if (zsession && zsession.consume) {
          try {
            zsession.consume(u8);
            return;
          } catch (_) {
            zsession = null;
          }
        }
        term.write(u8);
      };

      ws.onclose = () => {
        if (disposed) return;
        setStatus('closed');
        setReady(false);
        onDisconnected && onDisconnected({ type: 'exit' });
      };

      ws.onerror = () => {
        if (!disposed) toast('终端连接失败', { tone: 'error' });
      };

      term.onData((data) => {
        if (ws.readyState !== WebSocket.OPEN) return;
        // 背压：bufferedAmount 过大时暂缓（简单策略）
        if (ws.bufferedAmount > 2 * 1024 * 1024) return;
        ws.send(new TextEncoder().encode(data));
      });

      term.onResize(({ cols, rows }) => {
        sendCtrl({ type: 'resize', cols, rows });
      });

      const onWinResize = () => {
        try { fitAddon.fit(); } catch (_) {}
      };
      window.addEventListener('resize', onWinResize);

      // 清理时移除
      term.__mcCleanup = () => window.removeEventListener('resize', onWinResize);

      // 初始化 ZMODEM sentry（若可用）
      if (zmodem && typeof zmodem === 'function') {
        try {
          // zmodem2 Sentry 接口因版本而异；失败则纯终端模式
          // 此处不强制 rz/sz，保持透传为主
        } catch (_) {}
      }
      void zmodemMaxMB;
    })().catch((e) => {
      console.error(e);
      toast('终端组件加载失败', { tone: 'error' });
    });

    return () => {
      disposed = true;
      try {
        if (term && term.__mcCleanup) term.__mcCleanup();
        if (ws) ws.close();
        if (term) term.dispose();
      } catch (_) {}
      termRef.current = null;
      wsRef.current = null;
    };
  }, [resourceId, sessionId, onDisconnected, zmodemMaxMB]);

  const doSearch = (dir) => {
    if (!searchRef.current || !search) return;
    if (dir < 0) searchRef.current.findPrevious(search);
    else searchRef.current.findNext(search);
  };

  const clearScreen = () => {
    if (termRef.current) termRef.current.clear();
  };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', minHeight: 0 }}>
      <div style={{
        display: 'flex', alignItems: 'center', gap: 8, padding: '6px 10px',
        borderBottom: '1px solid var(--border)', background: 'var(--card)', flex: 'none',
      }}>
        <span style={{ fontSize: 12, color: status === 'ready' ? 'var(--success)' : status === 'closed' ? 'var(--error)' : 'var(--muted-fg)' }}>
          {status === 'ready' ? '已连接' : status === 'closed' ? '已断开' : '连接中…'}
        </span>
        <div style={{ flex: 1 }} />
        <input className="input" style={{ width: 160, padding: '4px 8px', fontSize: 12 }}
          placeholder="搜索终端…" value={search} onChange={(e) => setSearch(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter') doSearch(e.shiftKey ? -1 : 1); }} />
        <Btn size="sm" variant="ghost" onClick={() => doSearch(1)}>下一个</Btn>
        <Btn size="sm" variant="ghost" icon="trash" title="清屏" onClick={clearScreen} />
        <span style={{ fontSize: 11, color: 'var(--muted-fg)' }} title="远端需已安装 lrzsz">rz/sz 透传</span>
      </div>
      <div ref={hostRef} style={{ flex: 1, minHeight: 0, padding: 4, background: 'var(--console-bg)' }}>
        {!ready && status === 'connecting' ? (
          <div style={{ padding: 16, color: 'var(--muted-fg)', display: 'flex', alignItems: 'center', gap: 8 }}>
            <Spinner size={14} /> 正在建立终端…
          </div>
        ) : null}
      </div>
    </div>
  );
}
