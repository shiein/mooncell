// 服务器工作台：独立路由，不写入 mc_route，避免与主标签互相覆盖。
// 布局：左侧文件 | 右侧终端；窄屏切换标签。
import React from 'react';
import { Btn, Spinner, EmptyState, Badge, toast } from '../components/primitives.jsx';
import { getServerResource, createServerSession, deleteServerSession } from '../lib/serverops-api.js';
import { PasswordDialog } from '../components/serverops/PasswordDialog.jsx';
import { TerminalPane } from '../components/serverops/TerminalPane.jsx';
import { FileTree } from '../components/serverops/FileTree.jsx';
import { TransferPanel } from '../components/serverops/TransferPanel.jsx';

/** 连接/运维错误的可操作提示（参考 DataWorkspace friendlyWorkspaceError）。 */
function friendlyServerOpsError(e) {
  const code = e && e.code;
  const fallback = (e && e.message) || '操作失败';
  switch (code) {
    case 'HOST_KEY_MISMATCH':
      return '主机指纹与已确认值不一致。可能主机重装或存在中间人风险，请管理员重新探测并确认指纹，然后刷新此工作台。';
    case 'HOST_KEY_UNCONFIRMED':
      return '主机指纹尚未确认，请管理员先探测并确认后再连接。';
    case 'SSH_AUTH_FAILED':
      return 'SSH 用户名或密码错误，请重试（不会退出 Mooncell 登录）。';
    case 'PASSWORD_REQUIRED':
      return e.message || '请输入 SSH 密码。';
    case 'SSH_AUTH_RATE_LIMITED':
      return '密码尝试过于频繁，请稍后再试。';
    case 'SESSION_LIMIT_REACHED':
      return 'SSH 会话数已达上限，请关闭其他工作台后再连。';
    case 'TRANSFER_LIMIT_REACHED':
      return e.message || '文件传输数已达上限，可在文件面板丢弃未完成上传后重试。';
    case 'REMOTE_TARGET_EXISTS':
      return '目标文件已存在。可勾选「覆盖同名文件」后重试，或先在远端删除。';
    case 'MOONCELL_SESSION_EXPIRED':
      return 'Mooncell 登录已过期，请重新登录。';
    case 'SSH_CONNECT_TIMEOUT':
      return '连接或握手超时，请检查网络与目标主机。';
    case 'SSH_CONNECTION_FAILED':
      return 'SSH 连接失败，请检查地址、端口与网络。';
    default:
      return fallback;
  }
}

function ServerWorkspacePage({ resourceId, user, onLogout, theme, onTheme, zmodemMaxMB = 512 }) {
  const [resource, setResource] = React.useState(null);
  const [loadErr, setLoadErr] = React.useState(null);
  const [loading, setLoading] = React.useState(true);
  const [sessionId, setSessionId] = React.useState(null);
  const [connecting, setConnecting] = React.useState(false);
  const [connErr, setConnErr] = React.useState(null);
  const [showPw, setShowPw] = React.useState(false);
  /** host key 类错误：阻断密码重试，避免用户把密码往可疑主机上送。 */
  const [hostKeyBlock, setHostKeyBlock] = React.useState(null);
  const [transfers, setTransfers] = React.useState([]);
  const [fileWidth, setFileWidth] = React.useState(300);
  const [mobileTab, setMobileTab] = React.useState('terminal'); // terminal | files
  const dragRef = React.useRef(null);
  const autoConnectRef = React.useRef(null);

  React.useEffect(() => {
    let alive = true;
    setLoading(true);
    getServerResource(resourceId)
      .then((r) => {
        if (!alive) return;
        setResource(r);
        setLoadErr(null);
        if (r.hostKeyStatus !== 'trusted') {
          setLoadErr('主机指纹未确认，请管理员先探测并确认后再连接');
        }
      })
      .catch((e) => {
        if (!alive) return;
        // 404 统一无权/不存在
        setLoadErr(e.status === 404 ? '无权访问或不存在' : (e.message || '加载失败'));
      })
      .finally(() => { if (alive) setLoading(false); });
    return () => { alive = false; };
  }, [resourceId]);

  const connect = async (password) => {
    setConnecting(true);
    setConnErr(null);
    try {
      const res = await createServerSession(resourceId, { password, cols: 120, rows: 36 });
      // password 参数离开本函数后由 PasswordDialog 清空 state
      setSessionId(res.sessionId);
      setShowPw(false);
      toast('已连接');
    } catch (e) {
      const code = e && e.code;
      const msg = friendlyServerOpsError(e);
      // 指纹类错误：关闭密码框、禁止重试，避免训练用户忽略中间人告警。
      if (code === 'HOST_KEY_MISMATCH' || code === 'HOST_KEY_UNCONFIRMED') {
        setShowPw(false);
        setHostKeyBlock(msg);
        setConnErr(null);
        toast(msg, { tone: 'error', icon: 'alert' });
      } else {
        // SSH 认证失败为 422，不会触发全局退出
        setConnErr(msg);
        setShowPw(true);
      }
    } finally {
      setConnecting(false);
    }
  };

  // 首次进入优先复用活动 SSH 会话或当前 Mooncell 登录会话的内存凭据租约。
  React.useEffect(() => {
    if (!resource || resource.hostKeyStatus !== 'trusted' || sessionId || autoConnectRef.current === resourceId) return;
    autoConnectRef.current = resourceId;
    connect('');
  }, [resource, resourceId, sessionId]);

  const disconnect = async () => {
    if (sessionId) {
      try { await deleteServerSession(resourceId, sessionId); } catch (_) {}
    }
    setSessionId(null);
    setShowPw(false);
    setConnErr(null);
    setHostKeyBlock(null);
  };

  const onTransfer = React.useCallback((t) => {
    setTransfers((list) => {
      const i = list.findIndex((x) => x.id === t.id);
      if (i < 0) return [t, ...list].slice(0, 20);
      const next = list.slice();
      next[i] = t;
      return next;
    });
  }, []);

  // 稳定回调：避免 TerminalPane 因父组件重渲染而重建 WebSocket/Shell
  const onTerminalDisconnected = React.useCallback(() => {
    setSessionId(null);
    setShowPw(false);
    toast('会话已结束', { tone: 'warn' });
  }, []);

  // 拖动分隔条
  const onDragStart = (e) => {
    e.preventDefault();
    const startX = e.clientX;
    const startW = fileWidth;
    const onMove = (ev) => {
      const w = Math.min(520, Math.max(220, startW + (ev.clientX - startX)));
      setFileWidth(w);
    };
    const onUp = () => {
      window.removeEventListener('mousemove', onMove);
      window.removeEventListener('mouseup', onUp);
    };
    window.addEventListener('mousemove', onMove);
    window.addEventListener('mouseup', onUp);
  };

  if (loading) {
    return (
      <div style={{ height: '100%', display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
        <Spinner size={20} />
      </div>
    );
  }

  if (loadErr && !resource) {
    return (
      <div style={{ height: '100%', display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
        <EmptyState icon="shield" title="无法打开工作台" desc={loadErr}
          action={<Btn variant="primary" onClick={() => window.close()}>关闭</Btn>} />
      </div>
    );
  }

  return (
    <div style={{ height: '100%', display: 'flex', flexDirection: 'column', background: 'var(--bg)' }}>
      {/* 顶栏：不使用主 Shell，避免改 mc_route */}
      <header style={{
        display: 'flex', alignItems: 'center', gap: 12, padding: '8px 14px',
        borderBottom: '1px solid var(--border)', background: 'var(--card)', flex: 'none',
      }}>
        <div style={{ fontWeight: 650, fontSize: 14 }}>Mooncell</div>
        <span style={{ color: 'var(--border)' }}>|</span>
        <div style={{ minWidth: 0 }}>
          <div style={{ fontWeight: 600, fontSize: 13.5 }}>{resource?.name || '服务器'}</div>
          <div className="mono" style={{ fontSize: 11, color: 'var(--muted-fg)' }}>
            {resource ? `${resource.username}@${resource.host}:${resource.port}` : ''}
          </div>
        </div>
        <Badge tone={sessionId ? 'success' : 'warn'} dot>
          {sessionId ? '已连接' : '未连接'}
        </Badge>
        {loadErr ? <span style={{ fontSize: 12, color: 'var(--warn)' }}>{loadErr}</span> : null}
        {hostKeyBlock ? <span style={{ fontSize: 12, color: 'var(--error)', maxWidth: 360 }}>{hostKeyBlock}</span> : null}
        <div style={{ flex: 1 }} />
        <span style={{ fontSize: 12, color: 'var(--muted-fg)' }}>{user}</span>
        {sessionId ? (
          <Btn size="sm" variant="outline" onClick={disconnect}>断开</Btn>
        ) : (
          <Btn size="sm" variant="primary" icon="terminal"
            onClick={() => { setConnErr(null); connect(''); }}
            disabled={connecting || !!hostKeyBlock || (!!loadErr && resource?.hostKeyStatus !== 'trusted')}>连接</Btn>
        )}
        <Btn size="sm" variant="ghost" icon={theme === 'dark' ? 'sun' : 'moon'} onClick={onTheme} />
        <Btn size="sm" variant="ghost" icon="logout" onClick={onLogout} title="退出登录" />
      </header>

      {/* 窄屏标签 */}
      <div className="serverops-mobile-tabs" style={{
        display: 'none', borderBottom: '1px solid var(--border)', flex: 'none',
      }}>
        <button type="button" className="nav-item" data-active={String(mobileTab === 'terminal')}
          onClick={() => setMobileTab('terminal')} style={{ flex: 1, justifyContent: 'center' }}>终端</button>
        <button type="button" className="nav-item" data-active={String(mobileTab === 'files')}
          onClick={() => setMobileTab('files')} style={{ flex: 1, justifyContent: 'center' }}>文件</button>
      </div>

      <div style={{ flex: 1, display: 'flex', minHeight: 0 }} className="serverops-workbench">
        <div className="serverops-files" style={{
          width: fileWidth, flex: 'none', borderRight: '1px solid var(--border)',
          display: 'flex', flexDirection: 'column', minHeight: 0, background: 'var(--card)',
        }}>
          {sessionId ? (
            <React.Fragment>
              <div style={{ flex: 1, minHeight: 0 }}>
                <FileTree resourceId={resourceId} sessionId={sessionId} onTransfer={onTransfer} />
              </div>
              <TransferPanel items={transfers} />
            </React.Fragment>
          ) : (
            <EmptyState icon="folder" title="连接后浏览文件" desc="SFTP 目录与上传下载" />
          )}
        </div>
        <div
          ref={dragRef}
          onMouseDown={onDragStart}
          style={{ width: 5, cursor: 'col-resize', flex: 'none', background: 'var(--border)' }}
          className="serverops-splitter"
          title="拖动调整宽度"
        />
        <div className="serverops-term" style={{ flex: 1, minWidth: 0, minHeight: 0 }}>
          {sessionId ? (
            <TerminalPane
              resourceId={resourceId}
              sessionId={sessionId}
              onDisconnected={onTerminalDisconnected}
              zmodemMaxMB={zmodemMaxMB}
            />
          ) : hostKeyBlock ? (
            <EmptyState icon="shield" title="主机指纹异常" desc={hostKeyBlock}
              action={null} />
          ) : (
            <EmptyState icon="terminal" title="连接服务器"
              desc="优先使用当前 Mooncell 登录会话的内存凭据；凭据失效时再提示输入"
              action={<Btn variant="primary" onClick={() => connect('')} disabled={connecting || !!loadErr}>连接</Btn>} />
          )}
        </div>
      </div>

      <style>{`
        @media (max-width: 900px) {
          .serverops-mobile-tabs { display: flex !important; }
          .serverops-splitter { display: none !important; }
          .serverops-workbench { flex-direction: column !important; }
          .serverops-files { width: 100% !important; max-height: 45%; border-right: none !important; border-bottom: 1px solid var(--border); }
          .serverops-files { display: ${mobileTab === 'files' ? 'flex' : 'none'} !important; max-height: none; flex: 1 !important; }
          .serverops-term { display: ${mobileTab === 'terminal' ? 'block' : 'none'} !important; flex: 1 !important; }
        }
      `}</style>

      <PasswordDialog
        open={showPw && !sessionId && !hostKeyBlock}
        resource={resource}
        busy={connecting}
        error={connErr}
        onConnect={connect}
        onClose={() => setShowPw(false)}
      />
    </div>
  );
}

export { ServerWorkspacePage };
