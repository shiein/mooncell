// 服务器运维资源列表：admin CRUD + 所有登录用户打开工作台
import React from 'react';
import { useMC } from '../lib/data.js';
import { Btn, Badge, Spinner, EmptyState, toast, confirmDialog } from '../components/primitives.jsx';
import { PageHead } from '../components/Shell.jsx';
import { useAsync } from '../lib/async.js';
import { listServerResources, deleteServerResource } from '../lib/serverops-api.js';
import { ServerResourceDialog } from '../components/serverops/ServerResourceDialog.jsx';
import { HostKeyDialog } from '../components/serverops/HostKeyDialog.jsx';

function hostKeyBadge(r) {
  if (r.hostKeyStatus === 'trusted' && r.hostKeySha256) {
    return <Badge tone="success">已确认</Badge>;
  }
  return <Badge tone="warn">未确认</Badge>;
}

function accessLabel(mode) {
  if (mode === 'admin') return '管理员';
  if (mode === 'operate') return '可运维';
  return mode || '—';
}

function ServerResourcesPage() {
  const store = useMC();
  const isAdmin = store.can('admin');
  const { data, error, loading, retry } = useAsync(async () => {
    const r = await listServerResources();
    return r;
  }, []);
  const resources = (data && data.resources) || [];
  const cleanupPending = (data && data.cleanupPending) || 0;

  const [edit, setEdit] = React.useState(null); // null | {} create | resource
  const [hostKeyRes, setHostKeyRes] = React.useState(null);

  const openWorkspace = (r) => {
    // 独立新标签；hash 仅含资源 ID，不传密码或 session
    window.open(
      `/#/server-operations/${encodeURIComponent(r.id)}`,
      '_blank',
      'noopener',
    );
  };

  const onDelete = async (r) => {
    const ok = await confirmDialog({
      title: '删除服务器',
      message: `确定删除「${r.name}」？将终止全部活动会话并清除授权。`,
      tone: 'danger',
      confirmText: '删除',
    });
    if (!ok) return;
    try {
      await deleteServerResource(r.id);
      toast(`已删除 ${r.name}`, { icon: 'trash' });
      retry();
    } catch (e) {
      toast(e.message || '删除失败', { tone: 'error' });
    }
  };

  return (
    <div>
      <PageHead title="服务器运维" desc="远程 Linux 终端、SFTP 文件管理 · SSH 密码每次连接输入且不保存"
        actions={isAdmin ? (
          <Btn variant="primary" icon="plus" onClick={() => setEdit({})}>新建服务器</Btn>
        ) : null} />

      {isAdmin && cleanupPending > 0 ? (
        <div className="card" style={{ padding: '10px 14px', marginBottom: 12, fontSize: 12.5, color: 'var(--warn)' }}>
          有 {cleanupPending} 条远端临时上传文件待清理（下次有人成功连接对应服务器后按记录精确清理）。
        </div>
      ) : null}

      <div className="card" style={{ overflow: 'hidden' }}>
        <table className="table">
          <thead>
            <tr>
              <th>服务器名称</th>
              <th>地址</th>
              <th>用户名</th>
              <th>主机指纹</th>
              <th>我的权限</th>
              <th style={{ width: 200 }}></th>
            </tr>
          </thead>
          <tbody>
            {resources.map((r) => (
              <tr key={r.id}>
                <td style={{ fontWeight: 600 }}>{r.name}</td>
                <td className="mono" style={{ fontSize: 12.5 }}>{r.host}:{r.port}</td>
                <td className="mono" style={{ fontSize: 12.5 }}>{r.username}</td>
                <td>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                    {hostKeyBadge(r)}
                    {r.hostKeySha256 ? (
                      <span className="mono" style={{ fontSize: 10.5, color: 'var(--muted-fg)', maxWidth: 140, overflow: 'hidden', textOverflow: 'ellipsis' }}
                        title={r.hostKeySha256}>{r.hostKeySha256.replace(/^SHA256:/, '').slice(0, 12)}…</span>
                    ) : null}
                  </div>
                </td>
                <td><Badge tone={r.accessMode === 'admin' ? 'error' : 'info'}>{accessLabel(r.accessMode)}</Badge></td>
                <td>
                  <div style={{ display: 'flex', gap: 4, justifyContent: 'flex-end' }}>
                    <Btn size="sm" variant="primary" icon="terminal" title="打开工作台"
                      disabled={r.hostKeyStatus !== 'trusted' && !isAdmin}
                      onClick={() => {
                        if (r.hostKeyStatus !== 'trusted') {
                          toast('请先确认主机指纹', { tone: 'warn' });
                          return;
                        }
                        openWorkspace(r);
                      }}>打开</Btn>
                    {isAdmin ? (
                      <React.Fragment>
                        <Btn size="sm" variant="ghost" icon="shield" title="主机指纹"
                          onClick={() => setHostKeyRes(r)} />
                        <Btn size="sm" variant="ghost" icon="settings" title="编辑"
                          onClick={() => setEdit(r)} />
                        <Btn size="sm" variant="ghost" icon="trash" title="删除"
                          onClick={() => onDelete(r)} />
                      </React.Fragment>
                    ) : null}
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {loading ? <div style={{ padding: 24, textAlign: 'center' }}><Spinner size={16} /></div> : null}
        {!loading && error ? (
          <EmptyState icon="alert" title="加载失败" desc={error.message || '请稍后重试'}
            action={<Btn variant="primary" icon="rotate" onClick={retry}>重试</Btn>} />
        ) : null}
        {!loading && !error && resources.length === 0 ? (
          <EmptyState
            icon="server"
            title={isAdmin ? '暂无服务器' : '暂无已授权服务器，请联系管理员授权'}
            desc={isAdmin ? '点击右上角新建服务器，探测并确认主机指纹后即可连接' : undefined}
          />
        ) : null}
      </div>

      <ServerResourceDialog
        open={!!edit}
        resource={edit && edit.id ? edit : null}
        onClose={() => setEdit(null)}
        onSaved={() => { setEdit(null); retry(); }}
      />
      <HostKeyDialog
        open={!!hostKeyRes}
        resource={hostKeyRes}
        onClose={() => setHostKeyRes(null)}
        onConfirmed={() => { setHostKeyRes(null); retry(); }}
      />
    </div>
  );
}

export { ServerResourcesPage };
