// 服务器工作台：独立路由，不写入 mc_route，避免与主标签互相覆盖。
// 布局：左侧文件 | 右侧终端；窄屏切换标签。
import React from 'react';
import { Btn, Spinner, EmptyState, Badge, toast } from '../components/primitives.jsx';
import { getServerResource, createServerSession, deleteServerSession } from '../lib/serverops-api.js';
import { PasswordDialog } from '../components/serverops/PasswordDialog.jsx';
import { TerminalPane } from '../components/serverops/TerminalPane.jsx';
import { FileTree } from '../components/serverops/FileTree.jsx';
import { TransferPanel } from '../components/serverops/TransferPanel.jsx';

function ServerWorkspacePage({ resourceId, user, onLogout, theme, onTheme }) {
  const [resource, setResource] = React.useState(null);
  const [loadErr, setLoadErr] = React.useState(null);
  const [loading, setLoading] = React.useState(true);
  const [sessionId, setSessionId] = React.useState(null);
  const [connecting, setConnecting] = React.useState(false);
  const [connErr, setConnErr] = React.useState(null);
  const [showPw, setShowPw] = React.useState(false);
  const [transfers, setTransfers] = React.useState([]);
  const [fileWidth, setFileWidth] = React.useState(300);
  const [mobileTab, setMobileTab] = React.useState('terminal'); // terminal | files
  const dragRef = React.useRef(null);

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
        } else {
          setShowPw(true);
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

  // 卸载时断开会话
  React.useEffect(() => {
    return () => {
      if (sessionId && resourceId) {
        deleteServerSession(resourceId, sessionId).catch(() => {});
      }
    };
  }, [sessionId, resourceId]);

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
      // SSH 认证失败为 422，不会触发全局退出
      setConnErr(e.message || '连接失败');
      setShowPw(true);
    } finally {
      setConnecting(false);
    }
  };

  const disconnect = async () => {
    if (sessionId) {
      try { await deleteServerSession(resourceId, sessionId); } catch (_) {}
    }
    setSessionId(null);
    setShowPw(true);
    setConnErr(null);
  };

  const onTransfer = (t) => {
    setTransfers((list) => {
      const i = list.findIndex((x) => x.id === t.id);
      if (i < 0) return [t, ...list].slice(0, 20);
      const next = list.slice();
      next[i] = t;
      return next;
    });
  };

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
        <div style={{ flex: 1 }} />
        <span style={{ fontSize: 12, color: 'var(--muted-fg)' }}>{user}</span>
        {sessionId ? (
          <Btn size="sm" variant="outline" onClick={disconnect}>断开</Btn>
        ) : (
          <Btn size="sm" variant="primary" icon="terminal" onClick={() => setShowPw(true)}
            disabled={!!loadErr && resource?.hostKeyStatus !== 'trusted'}>连接</Btn>
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
              onDisconnected={() => {
                setSessionId(null);
                toast('会话已结束', { tone: 'warn' });
              }}
            />
          ) : (
            <EmptyState icon="terminal" title="输入 SSH 密码以连接"
              desc="密码不会保存，仅用于本次会话"
              action={<Btn variant="primary" onClick={() => setShowPw(true)}>连接</Btn>} />
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
        open={showPw && !sessionId}
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
