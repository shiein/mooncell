// xterm.js 终端面板：动态 import；连接 effect 仅依赖 resourceId/sessionId。
// onDisconnected 经 ref 读取，避免父组件重渲染重建 WebSocket/Shell。
import React from 'react';
import { Btn, Spinner, toast } from '../primitives.jsx';
import { terminalWsUrl } from '../../lib/serverops-api.js';
import { createZmodemBridge } from './zmodem-bridge.js';

export function TerminalPane({ resourceId, sessionId, onDisconnected, zmodemMaxMB = 512 }) {
  const hostRef = React.useRef(null);
  const termRef = React.useRef(null);
  const fitRef = React.useRef(null);
  const wsRef = React.useRef(null);
  const searchRef = React.useRef(null);
  const onDisconnectedRef = React.useRef(onDisconnected);
  const zmodemMaxRef = React.useRef(zmodemMaxMB);
  const [ready, setReady] = React.useState(false);
  const [search, setSearch] = React.useState('');
  const [status, setStatus] = React.useState('connecting'); // connecting | ready | closed
  const [zmInfo, setZmInfo] = React.useState(''); // 传输提示

  React.useEffect(() => { onDisconnectedRef.current = onDisconnected; }, [onDisconnected]);
  React.useEffect(() => { zmodemMaxRef.current = zmodemMaxMB; }, [zmodemMaxMB]);

  React.useEffect(() => {
    if (!resourceId || !sessionId || !hostRef.current) return;
    let disposed = false;
    let term;
    let fitAddon;
    let searchAddon;
    let ws;
    let zmBridge;
    let ro;
    let fitTimers = [];

    const fitAndNotify = () => {
      if (disposed || !fitAddon || !term) return null;
      try {
        fitAddon.fit();
        const dims = fitAddon.proposeDimensions();
        if (dims && dims.cols > 1 && dims.rows > 1) {
          if (ws && ws.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify({ type: 'resize', cols: dims.cols, rows: dims.rows }));
          }
          return dims;
        }
      } catch (_) { /* ignore */ }
      return null;
    };

    (async () => {
      const [{ Terminal }, { FitAddon }, { SearchAddon }] = await Promise.all([
        import('@xterm/xterm'),
        import('@xterm/addon-fit'),
        import('@xterm/addon-search'),
      ]);
      await import('@xterm/xterm/css/xterm.css');
      if (disposed || !hostRef.current) return;

      const styles = getComputedStyle(document.documentElement);
      term = new Terminal({
        cursorBlink: true,
        convertEol: false,
        // 依赖远端 ECHO；禁止本地回显，否则与 bash 回显叠加重影
        disableStdin: false,
        fontFamily: styles.getPropertyValue('--font-mono')?.trim()
          || 'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace',
        fontSize: 13,
        lineHeight: 1.2,
        theme: {
          background: styles.getPropertyValue('--console-bg')?.trim() || '#0f1419',
          foreground: styles.getPropertyValue('--console-fg')?.trim() || '#e6edf3',
          cursor: styles.getPropertyValue('--console-fg')?.trim() || '#e6edf3',
        },
        allowProposedApi: true,
        // 滚动缓冲，避免高速输出卡顿感
        scrollback: 5000,
      });
      fitAddon = new FitAddon();
      searchAddon = new SearchAddon();
      term.loadAddon(fitAddon);
      term.loadAddon(searchAddon);
      term.open(hostRef.current);
      // 布局稳定后再 fit：容器可能刚挂载或仍有连接中遮罩变化
      requestAnimationFrame(() => {
        if (!disposed) fitAndNotify();
      });
      termRef.current = term;
      fitRef.current = fitAddon;
      searchRef.current = searchAddon;

      const url = terminalWsUrl(resourceId, sessionId);
      ws = new WebSocket(url);
      ws.binaryType = 'arraybuffer';
      wsRef.current = ws;

      // 键盘输入：直达 ws.send，不经 Promise 链（低延迟关键路径）。
      // ZMODEM 大块发送：串行 + 背压，避免撑爆 bufferedAmount。
      const textEnc = new TextEncoder();
      let binarySendChain = Promise.resolve();
      const sendBinaryBackpressured = (u8) => {
        const data = u8.slice();
        const send = async () => {
          while (!disposed && ws.readyState === WebSocket.OPEN && ws.bufferedAmount > 512 * 1024) {
            await new Promise((resolve) => setTimeout(resolve, 8));
          }
          if (disposed || ws.readyState !== WebSocket.OPEN) {
            throw new Error('终端连接已关闭');
          }
          ws.send(data);
        };
        binarySendChain = binarySendChain.then(send);
        return binarySendChain;
      };
      const sendBinary = sendBinaryBackpressured;

      zmBridge = createZmodemBridge({
        sendToRemote: sendBinary,
        writeToTerm: (u8) => { if (!disposed) term.write(u8); },
        maxBytes: (zmodemMaxRef.current || 512) * 1024 * 1024,
        onStart: (info) => {
          const dir = info.direction === 'upload' ? 'rz 上传' : 'sz 下载';
          setZmInfo(`${dir}: ${info.name}`);
          toast(`${dir} 开始 · ${info.name}`);
        },
        onComplete: (info) => {
          setZmInfo('');
          toast(`${info.direction === 'upload' ? 'rz' : 'sz'} 完成 · ${info.name || ''}`);
        },
        onError: (err) => {
          setZmInfo('');
          if (err && err.message && err.message !== '已取消' && err.message !== '未选择文件') {
            toast(err.message || 'ZMODEM 失败', { tone: 'error' });
          }
        },
      });
      zmBridge.ensureLib().catch(() => {});

      const sendCtrl = (obj) => {
        if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj));
      };

      ws.onopen = () => {
        if (disposed) return;
        setStatus('connecting');
        // 多次 fit：初次布局、字体加载、遮罩消失后尺寸会变
        fitAndNotify();
        fitTimers.push(setTimeout(() => fitAndNotify(), 50));
        fitTimers.push(setTimeout(() => fitAndNotify(), 200));
      };

      ws.onmessage = (ev) => {
        if (disposed) return;
        if (typeof ev.data === 'string') {
          try {
            const msg = JSON.parse(ev.data);
            if (msg.type === 'ready') {
              setReady(true);
              setStatus('ready');
              // ready 后连接遮罩移除，必须再 fit 一次并通知远端 COLUMNS
              requestAnimationFrame(() => {
                fitAndNotify();
                fitTimers.push(setTimeout(() => fitAndNotify(), 100));
              });
            } else if (msg.type === 'exit' || msg.type === 'error') {
              setStatus('closed');
              if (msg.message) term.writeln(`\r\n\x1b[31m${msg.message}\x1b[0m`);
              const cb = onDisconnectedRef.current;
              cb && cb(msg);
            }
          } catch (_) { /* ignore */ }
          return;
        }
        const u8 = new Uint8Array(ev.data);
        if (zmBridge) {
          zmBridge.onTerminalOutput(u8);
        } else {
          term.write(u8);
        }
      };

      ws.onclose = () => {
        if (disposed) return;
        setStatus('closed');
        setReady(false);
        const cb = onDisconnectedRef.current;
        cb && cb({ type: 'exit' });
      };

      ws.onerror = () => {
        if (!disposed) toast('终端连接失败', { tone: 'error' });
      };

      term.onData((data) => {
        if (ws.readyState !== WebSocket.OPEN) return;
        // ZMODEM 进行中也允许 Ctrl+C 等；大流量时仍走背压路径
        if (zmBridge && zmBridge.isBusy()) {
          sendBinaryBackpressured(textEnc.encode(data)).catch(() => {});
          return;
        }
        try {
          ws.send(textEnc.encode(data));
        } catch (_) { /* ignore */ }
      });

      // resize 节流：避免 ResizeObserver 风暴打满控制帧
      let resizeTimer = null;
      let lastSentCols = 0;
      let lastSentRows = 0;
      term.onResize(({ cols, rows }) => {
        if (cols <= 1 || rows <= 1) return;
        if (cols === lastSentCols && rows === lastSentRows) return;
        if (resizeTimer) clearTimeout(resizeTimer);
        resizeTimer = setTimeout(() => {
          resizeTimer = null;
          if (cols === lastSentCols && rows === lastSentRows) return;
          lastSentCols = cols;
          lastSentRows = rows;
          sendCtrl({ type: 'resize', cols, rows });
        }, 50);
      });

      let fitRaf = 0;
      const onWinResize = () => {
        if (fitRaf) cancelAnimationFrame(fitRaf);
        fitRaf = requestAnimationFrame(() => {
          fitRaf = 0;
          fitAndNotify();
        });
      };
      window.addEventListener('resize', onWinResize);
      if (typeof ResizeObserver !== 'undefined' && hostRef.current) {
        ro = new ResizeObserver(onWinResize);
        ro.observe(hostRef.current);
      }
      term.__mcCleanup = () => {
        window.removeEventListener('resize', onWinResize);
        if (ro) ro.disconnect();
        if (resizeTimer) clearTimeout(resizeTimer);
        if (fitRaf) cancelAnimationFrame(fitRaf);
        fitTimers.forEach((t) => clearTimeout(t));
      };
    })().catch((e) => {
      console.error(e);
      toast('终端组件加载失败', { tone: 'error' });
    });

    return () => {
      disposed = true;
      try {
        if (zmBridge) zmBridge.cancel();
        if (term && term.__mcCleanup) term.__mcCleanup();
        if (ws) ws.close();
        if (term) term.dispose();
      } catch (_) { /* ignore */ }
      termRef.current = null;
      wsRef.current = null;
    };
  }, [resourceId, sessionId]);

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
        <span style={{
          fontSize: 12,
          color: status === 'ready' ? 'var(--success)' : status === 'closed' ? 'var(--error)' : 'var(--muted-fg)',
        }}>
          {status === 'ready' ? '已连接' : status === 'closed' ? '已断开' : '连接中…'}
        </span>
        {zmInfo ? (
          <span style={{ fontSize: 11.5, color: 'var(--primary)' }} title="ZMODEM 进行中">{zmInfo}</span>
        ) : null}
        <div style={{ flex: 1 }} />
        <input className="input" style={{ width: 160, padding: '4px 8px', fontSize: 12 }}
          placeholder="搜索终端…" value={search} onChange={(e) => setSearch(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter') doSearch(e.shiftKey ? -1 : 1); }} />
        <Btn size="sm" variant="ghost" onClick={() => doSearch(1)}>下一个</Btn>
        <Btn size="sm" variant="ghost" icon="trash" title="清屏" onClick={clearScreen} />
        <span style={{ fontSize: 11, color: 'var(--muted-fg)' }} title="远端需已安装 lrzsz；大文件请优先 SFTP">
          rz/sz
        </span>
      </div>
      {/* 终端容器独立：遮罩用 absolute，避免挤占 xterm 尺寸导致 COLUMNS 错误 */}
      <div style={{ flex: 1, minHeight: 0, position: 'relative', background: 'var(--console-bg)' }}>
        <div ref={hostRef} style={{ position: 'absolute', inset: 0, padding: 4 }} />
        {!ready && status === 'connecting' ? (
          <div style={{
            position: 'absolute', inset: 0, display: 'flex', alignItems: 'center', justifyContent: 'center',
            gap: 8, color: 'var(--muted-fg)', pointerEvents: 'none', zIndex: 1,
            background: 'var(--console-bg)',
          }}>
            <Spinner size={14} /> 正在建立终端…
          </div>
        ) : null}
      </div>
    </div>
  );
}
