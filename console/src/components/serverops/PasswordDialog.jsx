// SSH 密码对话框：密码只保存在本组件 state，连接结束立即清空。
// 禁止写入 URL、localStorage 或审计。
import React from 'react';
import { Btn, Field, Spinner, Dialog } from '../primitives.jsx';

export function PasswordDialog({ open, resource, busy, error, onConnect, onClose }) {
  const [password, setPassword] = React.useState('');
  const inputRef = React.useRef(null);

  React.useEffect(() => {
    if (open) {
      setPassword('');
      // 聚焦密码框
      setTimeout(() => inputRef.current && inputRef.current.focus(), 50);
    } else {
      setPassword('');
    }
  }, [open]);

  const submit = () => {
    if (!password || busy) return;
    const pw = password;
    setPassword(''); // 提交后立即清空 UI state
    onConnect(pw);
  };

  return (
    <Dialog open={open} onClose={busy ? undefined : onClose} width={400}
      title="连接服务器"
      desc={resource ? `${resource.username}@${resource.host}:${resource.port}` : ''}
      foot={<React.Fragment>
        <Btn variant="ghost" disabled={busy} onClick={onClose}>取消</Btn>
        <Btn variant="primary" icon="terminal" disabled={busy || !password} onClick={submit}>
          {busy ? <Spinner size={12} /> : '连接'}
        </Btn>
      </React.Fragment>}>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        <Field label="SSH 密码" hint="仅用于本次会话，不会保存">
          <input ref={inputRef} className="input" type="password" autoComplete="off"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            onKeyDown={(e) => { if (e.key === 'Enter') submit(); }}
            disabled={busy}
            placeholder="输入远端 SSH 密码" />
        </Field>
        {error ? (
          <div style={{ fontSize: 12.5, color: 'var(--error)', lineHeight: 1.45 }}>{error}</div>
        ) : null}
      </div>
    </Dialog>
  );
}
