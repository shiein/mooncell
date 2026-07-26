// 探测与确认 SSH host key 指纹（admin）
import React from 'react';
import { Btn, Field, Spinner, Dialog, toast } from '../primitives.jsx';
import { probeHostKey, confirmHostKey } from '../../lib/serverops-api.js';

export function HostKeyDialog({ open, resource, onClose, onConfirmed }) {
  const [probe, setProbe] = React.useState(null);
  const [busy, setBusy] = React.useState(false);
  const [probing, setProbing] = React.useState(false);

  React.useEffect(() => {
    if (!open) {
      setProbe(null);
      setBusy(false);
      setProbing(false);
    }
  }, [open]);

  const doProbe = async () => {
    if (!resource) return;
    setProbing(true);
    setProbe(null);
    try {
      const r = await probeHostKey({ host: resource.host, port: resource.port });
      setProbe(r);
    } catch (e) {
      toast(e.message || '探测失败', { tone: 'error' });
    } finally {
      setProbing(false);
    }
  };

  const doConfirm = async () => {
    if (!resource || !probe) return;
    setBusy(true);
    try {
      await confirmHostKey(resource.id, {
        algorithm: probe.algorithm,
        sha256: probe.sha256,
        updatedAt: resource.updatedAt,
      });
      toast('主机指纹已确认');
      onConfirmed && onConfirmed();
    } catch (e) {
      toast(e.message || '确认失败', { tone: 'error' });
      setBusy(false);
    }
  };

  const oldFp = resource?.hostKeySha256 || '';
  const changed = probe && oldFp && oldFp !== probe.sha256;

  return (
    <Dialog open={open} onClose={onClose} width={520}
      title="主机指纹"
      desc={`${resource?.name || ''} · ${resource?.host || ''}:${resource?.port || 22}`}
      foot={<React.Fragment>
        <Btn variant="ghost" onClick={onClose}>关闭</Btn>
        <Btn variant="outline" icon="rotate" disabled={probing} onClick={doProbe}>
          {probing ? <Spinner size={12} /> : '探测'}
        </Btn>
        <Btn variant="primary" icon="check" disabled={!probe || busy} onClick={doConfirm}>
          {busy ? <Spinner size={12} /> : '确认指纹'}
        </Btn>
      </React.Fragment>}>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        <div style={{ fontSize: 12.5, color: 'var(--muted-fg)', lineHeight: 1.5 }}>
          请通过带外可信渠道核对指纹后再确认。普通用户无法连接未确认指纹的服务器。
        </div>
        {oldFp ? (
          <Field label="当前已确认">
            <div className="mono" style={{ fontSize: 12, wordBreak: 'break-all' }}>
              {resource.hostKeyAlgorithm} · {oldFp}
            </div>
          </Field>
        ) : (
          <div style={{ fontSize: 12.5, color: 'var(--warn)' }}>尚未确认主机指纹</div>
        )}
        {probe ? (
          <Field label={changed ? '新探测指纹（与当前不同）' : '探测结果'}>
            <div className="mono" style={{
              fontSize: 12, wordBreak: 'break-all',
              color: changed ? 'var(--warn)' : 'var(--fg)',
            }}>
              {probe.algorithm} · {probe.sha256}
            </div>
          </Field>
        ) : (
          <div style={{ fontSize: 12.5, color: 'var(--muted-fg)' }}>
            点击「探测」从目标主机获取当前指纹（无需密码）
          </div>
        )}
      </div>
    </Dialog>
  );
}
