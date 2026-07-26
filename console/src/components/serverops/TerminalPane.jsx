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

  // 始终指向最新回调/配置，不参与连接 effect 依赖。
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
        fontFamily: styles.getPropertyValue('--font-mono')?.trim() || 'ui-monospace, SFMono-Regular, Menlo, monospace',
        fontSize: 13,
        theme: {
          background: styles.getPropertyValue('--console-bg')?.trim() || '#0f1419',
          foreground: styles.getPropertyValue('--console-fg')?.trim() || '#e6edf3',
          cursor: styles.getPropertyValue('--console-fg')?.trim() || '#e6edf3',
        },
        allowProposedApi: true,
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

      const url = terminalWsUrl(resourceId, sessionId);
      ws = new WebSocket(url);
      ws.binaryType = 'arraybuffer';
      wsRef.current = ws;

      let binarySendChain = Promise.resolve();
      const sendBinary = (u8) => {
        // 拷贝后串行发送；缓冲过高时等待下降，不能静默丢弃 ZMODEM 帧。
        const data = u8.slice();
        const send = async () => {
          while (!disposed && ws.readyState === WebSocket.OPEN && ws.bufferedAmount > 4 * 1024 * 1024) {
            await new Promise((resolve) => setTimeout(resolve, 10));
          }
          if (disposed || ws.readyState !== WebSocket.OPEN) {
            throw new Error('终端连接已关闭');
          }
          ws.send(data);
        };
        binarySendChain = binarySendChain.then(send);
        return binarySendChain;
      };

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
      // 预加载库（失败不阻断终端）
      zmBridge.ensureLib().catch(() => {});

      const sendCtrl = (obj) => {
        if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj));
      };

      ws.onopen = () => {
        if (disposed) return;
        setStatus('connecting');
        try {
          const dims = fitAddon.proposeDimensions();
          if (dims) sendCtrl({ type: 'resize', cols: dims.cols, rows: dims.rows });
        } catch (_) { /* ignore */ }
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
              const cb = onDisconnectedRef.current;
              cb && cb(msg);
            }
          } catch (_) { /* ignore */ }
          return;
        }
        const u8 = new Uint8Array(ev.data);
        // ZMODEM 桥接：识别后接管，否则写终端
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
        // ZMODEM 传输期间仍允许键盘（取消等）；协议数据由 bridge 发 binary
        sendBinary(new TextEncoder().encode(data)).catch(() => {});
      });

      term.onResize(({ cols, rows }) => {
        sendCtrl({ type: 'resize', cols, rows });
      });

      const onWinResize = () => {
        try { fitAddon.fit(); } catch (_) { /* ignore */ }
      };
      window.addEventListener('resize', onWinResize);
      term.__mcCleanup = () => window.removeEventListener('resize', onWinResize);
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
    // 仅资源/会话变化时重建连接；回调经 ref，进度更新不会拆 Shell。
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
