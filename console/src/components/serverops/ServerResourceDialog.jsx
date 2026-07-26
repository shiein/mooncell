// 新建/编辑服务器资源对话框（admin）
import React from 'react';
import { Btn, Field, Spinner, Dialog, toast } from '../primitives.jsx';
import { createServerResource, updateServerResource } from '../../lib/serverops-api.js';

const EMPTY = { name: '', host: '', port: 22, username: '' };

export function ServerResourceDialog({ open, resource, onClose, onSaved }) {
  const isEdit = !!(resource && resource.id);
  const [form, setForm] = React.useState(EMPTY);
  const [busy, setBusy] = React.useState(false);

  React.useEffect(() => {
    if (!open) return;
    if (isEdit) {
      setForm({
        name: resource.name || '',
        host: resource.host || '',
        port: resource.port || 22,
        username: resource.username || '',
      });
    } else {
      setForm(EMPTY);
    }
    setBusy(false);
  }, [open, isEdit, resource]);

  const set = (k, v) => setForm((s) => ({ ...s, [k]: v }));

  const submit = async () => {
    const name = String(form.name || '').trim();
    const host = String(form.host || '').trim();
    const username = String(form.username || '').trim();
    const port = Number(form.port);
    if (!name || !host || !username) {
      toast('请填写名称、地址与用户名', { tone: 'warn' });
      return;
    }
    if (!Number.isFinite(port) || port < 1 || port > 65535) {
      toast('端口必须在 1–65535', { tone: 'warn' });
      return;
    }
    setBusy(true);
    try {
      const payload = { name, host, port, username };
      if (isEdit) {
        payload.updatedAt = resource.updatedAt;
        await updateServerResource(resource.id, payload);
        toast('服务器已更新');
      } else {
        await createServerResource(payload);
        toast('服务器已创建 · 请探测并确认主机指纹');
      }
      onSaved && onSaved();
    } catch (e) {
      toast(e.message || '保存失败', { tone: 'error' });
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onClose={onClose} width={480}
      title={isEdit ? '编辑服务器' : '新建服务器'}
      desc="仅保存连接摘要；SSH 密码由用户在工作台每次输入，不落库"
      foot={<React.Fragment>
        <Btn variant="ghost" onClick={onClose}>取消</Btn>
        <Btn variant="primary" icon="check" disabled={busy} onClick={submit}>
          {busy ? <Spinner size={12} /> : (isEdit ? '保存' : '创建')}
        </Btn>
      </React.Fragment>}>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 13 }}>
        <Field label="服务器名称">
          <input className="input" value={form.name} onChange={(e) => set('name', e.target.value)}
            placeholder="如 生产应用服务器-01" autoComplete="off" />
        </Field>
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 100px', gap: 10 }}>
          <Field label="主机地址">
            <input className="input" value={form.host} onChange={(e) => set('host', e.target.value)}
              placeholder="IP 或主机名" autoComplete="off" />
          </Field>
          <Field label="端口">
            <input className="input mono" type="number" min={1} max={65535}
              value={form.port} onChange={(e) => set('port', e.target.value)} />
          </Field>
        </div>
        <Field label="SSH 用户名" hint="固定账号；密码不在此配置">
          <input className="input" value={form.username} onChange={(e) => set('username', e.target.value)}
            placeholder="如 ops" autoComplete="off" />
        </Field>
        {isEdit && (resource.host !== form.host || resource.port !== Number(form.port) || resource.username !== form.username) ? (
          <div style={{ fontSize: 12.5, color: 'var(--warn)', lineHeight: 1.45 }}>
            修改主机/端口/用户名将清空已确认指纹，并断开该服务器上全部活动会话。
          </div>
        ) : null}
      </div>
    </Dialog>
  );
}
